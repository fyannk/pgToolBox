// Copyright 2026 The pgToolBox Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command readonlycheck is the console's read-only boundary, checked on the
// typed syntax tree rather than by grep. It loads
// the console's packages (cmd/console, internal/console, pkg) and lists
// every call of a mutating method (Create, Update, Patch, Delete,
// DeleteAllOf, DeleteCollection, Apply, ...) on a Kubernetes client, or on
// any method that takes a Kubernetes object, so that a wrapper cannot hide
// one. Each call site, by enclosing function, must be in the allowlist below
// with its exact count, and every allowlist entry must still exist.
//
// It also holds the console's own-identity writer to its one use: the
// restore-drill reaper's client (ops.NewDrillReaperClient) is built in
// internal/console/wiring only, and the reaper's only call is the
// UID-preconditioned Delete of DrillReaper.Teardown.
//
// Run from the module root: go run ./hack/readonlycheck (make check-repo).
package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

const module = "github.com/fyannk/pgToolBox/"

// patterns are the console's packages and the shared libraries it reads
// through. The operator writes to the API server by design and is out of
// scope; the evidence agent is a nested module with no client-go client.
var patterns = []string{"./cmd/console/...", "./internal/console/...", "./pkg/..."}

// mutating are the method names that change state on an API server.
var mutating = map[string]bool{
	"Create": true, "Update": true, "Patch": true, "Delete": true, "DeleteAllOf": true,
	"DeleteCollection": true, "Apply": true, "ApplyStatus": true, "UpdateStatus": true,
	"Evict": true, "EvictV1": true, "EvictV1beta1": true, "Bind": true,
}

// kubePackages are the packages whose types are Kubernetes clients or
// objects.
var kubePackages = []string{
	"sigs.k8s.io/controller-runtime/", "k8s.io/client-go/", "k8s.io/api/", "k8s.io/apimachinery/",
	"github.com/cloudnative-pg/",
}

// Site is one allowed mutating call: the enclosing function (package path
// relative to the module, then the function as go doc prints it), the
// method, and how many such calls the function holds.
type Site struct {
	Function string
	Method   string
}

// allowed is the console's whole mutation surface. Every write but one is
// made with the signed-in person's own token (ops.UserWriter and the
// writers built the same way); the review Create asks the API server a
// question and changes nothing; the own-identity writes are the drill
// teardown, the console's own state ConfigMap, which its
// Role names by resourceNames and nothing else, and the digest and MCP
// Leases.
var allowed = map[Site]int{
	// Reviews (SelfSubjectReview, SelfSubjectAccessReview): questions put
	// to the API server, with the person's token or the console's; they
	// change no state.
	{"internal/console/auth.(*SelfSubjectReviewer).Review", "Create"}: 1,
	{"internal/console/auth.SelfReviewer.Review", "Create"}:           1,
	// PgAccessRequest writes, with the person's token.
	{"internal/console/ops.(*AccessWriter).CreateAccessRequest", "Create"}: 1,
	{"internal/console/ops.(*AccessWriter).CreateGrant", "Create"}:         1,
	{"internal/console/ops.(*AccessWriter).RecordDecision", "Update"}:      1,
	// The operations, with the person's token.
	{"internal/console/ops.(*UserWriter).CreatePersonalDatabaseRole", "Create"}: 1,
	{"internal/console/ops.(*UserWriter).CreateDrillCluster", "Create"}:         1,
	{"internal/console/ops.(*UserWriter).CreateBackup", "Create"}:               1,
	{"internal/console/ops.(*UserWriter).AnnotateCluster", "Patch"}:             1,
	{"internal/console/ops.(*UserWriter).Promote", "Patch"}:                     1,
	// An own-identity write: the drill teardown, UID-preconditioned.
	{"internal/console/ops.(*DrillReaper).Teardown", "Delete"}: 1,
	// The console's own state ConfigMap (sign-outs, "since" times, digest
	// delivery marks), resourceVersion-preconditioned; the Role grants
	// get, watch and update on that one name.
	{"internal/console/sharedstate.(*Store).flush", "Update"}: 1,
	// The same ConfigMap, written through for a spent MCP confirmation
	// (SpendCode, MarkCode), resourceVersion-preconditioned.
	{"internal/console/sharedstate.(*Store).writeThrough", "Update"}: 1,
	// The digest Lease (DIGEST_LEASE): created by the first replica that
	// stands (create cannot be scoped by name), then renewed and handed
	// over with the resourceVersion read; the Role grants get and update
	// on that one name, and create on leases.
	{"internal/console/leader.(*leaseLock).Create", "Create"}: 1,
	{"internal/console/leader.(*leaseLock).Update", "Update"}: 1,
}

// resourcelockPackage holds client-go's own election locks. They write
// through a client of their own, inside client-go, where the allowlist
// cannot see the calls: the console elects with internal/console/leader's
// lock instead, and may use only that package's conversions.
const resourcelockPackage = "k8s.io/client-go/tools/leaderelection/resourcelock"

// resourcelockAllowed are the resourcelock names the console may use:
// the record type, the lock interface and the spec conversions, none of
// which writes.
var resourcelockAllowed = map[string]bool{
	"Interface": true, "LeaderElectionRecord": true,
	"LeaseSpecToLeaderElectionRecord": true, "LeaderElectionRecordToLeaseSpec": true,
}

// reaperClient is the constructor of the console's own-identity writer, and
// where it may be used.
const (
	reaperClient        = "internal/console/ops.NewDrillReaperClient"
	reaperClientUsedIn  = "internal/console/wiring"
	reaperClientMaxUses = 1
	// The client goes only into this constructor, which keeps it in the
	// reaper's field; the field is read by these functions only.
	reaperConstructor = "internal/console/ops.NewDrillReaper"
	reaperType        = "DrillReaper"
	reaperField       = "c"
)

// reaperFieldUses pins every use of the reaper's client field: set in the
// constructor, read by the one Delete.
var reaperFieldUses = map[string]int{
	"internal/console/ops.NewDrillReaper":          1,
	"internal/console/ops.(*DrillReaper).Teardown": 1,
}

// Finding is one violation.
type Finding struct {
	Pos     token.Position
	Message string
}

func main() {
	findings, err := run(".", nil, allowed, reaperClientUsedIn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "readonlycheck:", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "%s: %s\n", f.Pos, f.Message)
	}
	if len(findings) > 0 {
		fmt.Fprintln(os.Stderr, "the console's mutation surface changed: see hack/readonlycheck")
		os.Exit(1)
	}
	fmt.Println("console mutation surface: as pinned")
}

// run loads the packages from dir (with overlay, the tests' planted files)
// and checks them against allow, with the reaper client permitted in usedIn.
func run(dir string, overlay map[string][]byte, allow map[Site]int, usedIn string) ([]Finding, error) {
	pkgs, err := packages.Load(&packages.Config{
		Dir:     dir,
		Overlay: overlay,
		Mode:    packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}, patterns...)
	if err != nil {
		return nil, err
	}
	var loadErrors []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrors = append(loadErrors, e.Error())
		}
	})
	if len(loadErrors) > 0 {
		return nil, fmt.Errorf("load: %s", strings.Join(loadErrors, "; "))
	}
	return check(pkgs, allow, usedIn), nil
}

// check is the analysis proper.
func check(pkgs []*packages.Package, allow map[Site]int, usedIn string) []Finding {
	var findings []Finding
	seen := map[Site]int{}
	var reaperUses []token.Position
	field := reaperClientField(pkgs)
	if field == nil {
		findings = append(findings, Finding{Message: fmt.Sprintf("the reaper's client field %s.%s is gone: update hack/readonlycheck", reaperType, reaperField)})
	}
	fieldUses := map[string]int{}
	// reaperVars are the variables NewDrillReaperClient's result is
	// assigned to.
	reaperVars := map[types.Object]bool{}
	for _, p := range pkgs {
		rel := strings.TrimPrefix(p.PkgPath, module)
		for _, file := range p.Syntax {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				name := rel + "." + funcName(fn)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CallExpr:
						method, ok := mutatingCall(p.TypesInfo, n)
						if !ok {
							return true
						}
						site := Site{Function: name, Method: method}
						seen[site]++
						if _, ok := allow[site]; !ok {
							findings = append(findings, Finding{
								p.Fset.Position(n.Pos()),
								fmt.Sprintf("%s calls %s: not in the console's mutation allowlist", name, method),
							})
						}
					case *ast.AssignStmt:
						// v, err := ops.NewDrillReaperClient(...)
						if len(n.Rhs) == 1 && len(n.Lhs) > 0 && calls(p.TypesInfo, n.Rhs[0], reaperClient) {
							if ident, ok := n.Lhs[0].(*ast.Ident); ok {
								if obj := p.TypesInfo.ObjectOf(ident); obj != nil {
									reaperVars[obj] = true
								}
							}
						}
					case *ast.Ident:
						if obj := p.TypesInfo.Uses[n]; obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == resourcelockPackage &&
							obj.Parent() == obj.Pkg().Scope() && !resourcelockAllowed[obj.Name()] {
							findings = append(findings, Finding{
								p.Fset.Position(n.Pos()),
								fmt.Sprintf("%s uses resourcelock.%s: client-go's locks write where this check cannot see; use internal/console/leader", name, obj.Name()),
							})
						}
						if obj, ok := p.TypesInfo.Uses[n].(*types.Func); ok && objName(obj) == reaperClient {
							pos := p.Fset.Position(n.Pos())
							reaperUses = append(reaperUses, pos)
							if rel != usedIn {
								findings = append(findings, Finding{pos, fmt.Sprintf("%s uses %s: the console's own-identity writer is built in %s only", name, reaperClient, usedIn)})
							}
						}
						if field != nil && p.TypesInfo.Uses[n] == field {
							fieldUses[name]++
						}
					}
					return true
				})
				// The reaper client's variable is only ever an argument of
				// NewDrillReaper.
				findings = append(findings, reaperVarMisuse(p, fn, name, reaperVars)...)
			}
		}
	}
	for site, want := range allow {
		if got := seen[site]; got != want {
			findings = append(findings, Finding{Message: fmt.Sprintf("%s calls %s %d times, the allowlist pins %d", site.Function, site.Method, got, want)})
		}
	}
	if field != nil {
		for function, want := range reaperFieldUses {
			if fieldUses[function] != want {
				findings = append(findings, Finding{Message: fmt.Sprintf("%s uses the reaper's client %d times, pinned %d", function, fieldUses[function], want)})
			}
		}
		for function, got := range fieldUses {
			if _, ok := reaperFieldUses[function]; !ok {
				findings = append(findings, Finding{Message: fmt.Sprintf("%s uses the reaper's client (%d times): only the teardown may", function, got)})
			}
		}
	}
	if len(reaperUses) > reaperClientMaxUses {
		findings = append(findings, Finding{reaperUses[len(reaperUses)-1], fmt.Sprintf("%s is used %d times, at most %d", reaperClient, len(reaperUses), reaperClientMaxUses)})
	}
	slices.SortFunc(findings, func(a, b Finding) int { return strings.Compare(a.Pos.String()+a.Message, b.Pos.String()+b.Message) })
	return findings
}

// funcName is the function as go doc prints it: F, T.M or (*T).M.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	pointer := false
	if star, ok := recv.(*ast.StarExpr); ok {
		pointer, recv = true, star.X
	}
	if index, ok := recv.(*ast.IndexExpr); ok {
		recv = index.X
	}
	if index, ok := recv.(*ast.IndexListExpr); ok {
		recv = index.X
	}
	typeName := fmt.Sprint(recv)
	if ident, ok := recv.(*ast.Ident); ok {
		typeName = ident.Name
	}
	if pointer {
		return "(*" + typeName + ")." + fn.Name.Name
	}
	return typeName + "." + fn.Name.Name
}

// mutatingCall reports whether call is a method call with a mutating name,
// either on a Kubernetes client type or taking a Kubernetes object.
func mutatingCall(info *types.Info, call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !mutating[sel.Sel.Name] {
		return "", false
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	if !ok {
		return "", false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return "", false
	}
	if fn.Pkg() != nil && isKube(fn.Pkg().Path()) {
		return sel.Sel.Name, true
	}
	params := sig.Params()
	for i := range params.Len() {
		if kubeType(params.At(i).Type()) {
			return sel.Sel.Name, true
		}
	}
	return "", false
}

func isKube(path string) bool {
	return slices.ContainsFunc(kubePackages, func(prefix string) bool { return strings.HasPrefix(path, prefix) })
}

// kubeType reports whether t is, points to, or is a slice of a named type
// of a Kubernetes package.
func kubeType(t types.Type) bool {
	for {
		switch u := t.(type) {
		case *types.Pointer:
			t = u.Elem()
			continue
		case *types.Slice:
			t = u.Elem()
			continue
		case *types.Named:
			return u.Obj().Pkg() != nil && isKube(u.Obj().Pkg().Path())
		case *types.Alias:
			t = types.Unalias(u)
			continue
		}
		return false
	}
}

func objName(obj types.Object) string {
	if obj.Pkg() == nil {
		return obj.Name()
	}
	return strings.TrimPrefix(obj.Pkg().Path(), module) + "." + obj.Name()
}

// calls reports whether expr is a call of the function named want.
func calls(info *types.Info, expr ast.Expr, want string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	var ident *ast.Ident
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		ident = fun
	case *ast.SelectorExpr:
		ident = fun.Sel
	default:
		return false
	}
	obj, ok := info.Uses[ident].(*types.Func)
	return ok && objName(obj) == want
}

// reaperVarMisuse lists the uses, in fn, of a variable holding the reaper
// client other than its declaration and an argument of NewDrillReaper.
func reaperVarMisuse(p *packages.Package, fn *ast.FuncDecl, name string, vars map[types.Object]bool) []Finding {
	if len(vars) == 0 {
		return nil
	}
	allowedUse := map[*ast.Ident]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calls(p.TypesInfo, call, reaperConstructor) {
			for _, arg := range call.Args {
				if ident, ok := arg.(*ast.Ident); ok {
					allowedUse[ident] = true
				}
			}
		}
		return true
	})
	var findings []Finding
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok || !vars[p.TypesInfo.Uses[ident]] || allowedUse[ident] {
			return true
		}
		findings = append(findings, Finding{
			p.Fset.Position(ident.Pos()),
			fmt.Sprintf("%s uses the drill reaper's client other than to build the reaper (%s)", name, reaperConstructor),
		})
		return true
	})
	return findings
}

// reaperClientField is the DrillReaper's client field.
func reaperClientField(pkgs []*packages.Package) *types.Var {
	var field *types.Var
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if field != nil || p.Types == nil || strings.TrimPrefix(p.PkgPath, module) != "internal/console/ops" {
			return
		}
		obj := p.Types.Scope().Lookup(reaperType)
		if obj == nil {
			return
		}
		st, ok := obj.Type().Underlying().(*types.Struct)
		if !ok {
			return
		}
		for i := range st.NumFields() {
			if st.Field(i).Name() == reaperField {
				field = st.Field(i)
			}
		}
	})
	return field
}
