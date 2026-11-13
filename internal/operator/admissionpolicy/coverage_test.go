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

package admissionpolicy

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// secretField is a Secret reference the API types declare, at a path below
// spec (JSON names, "[]" for a list element, "{}" for a map value), with the
// JSON names of the optional fields along the way.
type secretField struct {
	path     string
	optional map[string]bool // path prefix -> the field ending there is optional
	list     bool            // the reference itself is a list (pullSecrets)
	name     bool            // a bare Secret name (string), not a reference struct
}

var (
	apiPkg       = reflect.TypeFor[pgtoolboxv1alpha1.LocalObjectReference]().PkgPath()
	localRefType = []reflect.Type{reflect.TypeFor[pgtoolboxv1alpha1.LocalObjectReference](), reflect.TypeFor[corev1.LocalObjectReference]()}
	// Types that name a Secret whatever the field is called.
	secretKeyTypes = []reflect.Type{
		reflect.TypeFor[pgtoolboxv1alpha1.SecretKeyReference](), reflect.TypeFor[pgtoolboxv1alpha1.SecretKeySelector](),
		reflect.TypeFor[corev1.SecretKeySelector](), reflect.TypeFor[corev1.SecretReference](),
		reflect.TypeFor[corev1.SecretEnvSource](), reflect.TypeFor[corev1.SecretVolumeSource](),
		reflect.TypeFor[corev1.SecretProjection](),
	}
	// String fields whose JSON name says "secret" but which name no Secret
	// (path below spec -> why). Empty today; a new entry needs its reason.
	notSecretNames = map[string]string{}
	// String fields that name a Secret although their JSON name does not say
	// so (path below spec -> what they name). Empty today.
	secretNamesByDeclaration = map[string]string{}

	singleEntryRe = regexp.MustCompile(`\[string\(object\.spec\.([A-Za-z0-9.]+)\.name\)\]`)
	listEntryRe   = regexp.MustCompile(`object\.spec\.([A-Za-z0-9.]+)\.map\(s, string\(s(\.name)?\)\)`)
	resourceRe    = regexp.MustCompile(`request\.resource\.resource == '([a-z]+)' \?`)
)

// secretFields walks a spec type: every SecretKeyReference and
// SecretKeySelector (ours or core/v1's, and core/v1's other Secret-naming
// types), every LocalObjectReference (ours or core/v1's, or a list of them)
// whose JSON name says it names a Secret, every string or list of strings
// whose JSON name says "secret" (minus notSecretNames, plus
// secretNamesByDeclaration), through pointers, lists and map values.
func secretFields(t reflect.Type) []secretField {
	var out []secretField
	var walk func(t reflect.Type, prefix string, optional map[string]bool, inList bool)
	walk = func(t reflect.Type, prefix string, optional map[string]bool, inList bool) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" || !f.IsExported() {
				continue
			}
			path := prefix
			if name != "" {
				path = strings.TrimPrefix(prefix+"."+name, ".")
			}
			opt := copyOptional(optional)
			if name != "" && (f.Type.Kind() == reflect.Pointer || f.Type.Kind() == reflect.Map || strings.Contains(opts, "omitempty")) {
				opt[path] = true
			}
			ft, list := f.Type, false
			var marks strings.Builder // "[]" per list, "{}" per map along the way
			for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Map {
				if ft.Kind() == reflect.Slice {
					list = true
					marks.WriteString("[]")
				} else if ft.Kind() == reflect.Map {
					marks.WriteString("{}")
				}
				ft = ft.Elem()
			}
			suffix := marks.String()
			secretish := strings.Contains(strings.ToLower(name), "secret")
			switch {
			case slices.Contains(secretKeyTypes, ft):
				out = append(out, secretField{path: path + suffix, optional: opt, list: inList})
			case slices.Contains(localRefType, ft):
				if secretish {
					// A list of references is read with .map() by the policy.
					out = append(out, secretField{path: path + strings.TrimSuffix(suffix, "[]"), optional: opt, list: list || inList})
				}
			case ft.Kind() == reflect.String:
				_, declared := secretNamesByDeclaration[path]
				if _, excluded := notSecretNames[path]; (secretish && !excluded) || declared {
					out = append(out, secretField{path: path + strings.TrimSuffix(suffix, "[]"), optional: opt, list: list || inList, name: true})
				}
			case ft.Kind() == reflect.Struct && ft.PkgPath() == apiPkg:
				walk(ft, path+suffix, opt, inList || list)
			}
		}
	}
	walk(t, "", map[string]bool{}, false)
	return out
}

func copyOptional(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// policyEntry is one Secret reference the policy's secretRefs reads.
type policyEntry struct {
	line string
	list bool // read with .map()
	name bool // reads a bare string (string(s)), not s.name
}

// policyFile is the operator chart's admission policy template.
var policyFile = filepath.Join("..", "..", "..", "charts", "pgtoolbox", "templates", "operator", "admissionpolicy.yaml")

// policyEntries reads the secretRefs variable of the operator chart's
// policy: per resource, each spec path it extracts and the line that does.
func policyEntries(t *testing.T) map[string]map[string]policyEntry {
	t.Helper()
	raw, err := os.ReadFile(policyFile)
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(raw), "- name: secretRefs\n")
	if !ok {
		t.Fatal("the policy has no secretRefs variable")
	}
	body, _, _ = strings.Cut(body, "    - name: ")
	out := map[string]map[string]policyEntry{}
	resource := ""
	for _, line := range strings.Split(body, "\n") {
		if m := resourceRe.FindStringSubmatch(line); m != nil {
			resource = m[1]
			out[resource] = map[string]policyEntry{}
		}
		if resource == "" {
			continue
		}
		for _, m := range singleEntryRe.FindAllStringSubmatch(line, -1) {
			out[resource][m[1]] = policyEntry{line: line}
		}
		for _, m := range listEntryRe.FindAllStringSubmatch(line, -1) {
			out[resource][m[1]] = policyEntry{line: line, list: true, name: m[2] == ""}
		}
	}
	return out
}

// fieldsBeside lists the leaf fields of the struct at parent (a path below
// spec, "" for spec itself): JSON paths below spec, through pointers and
// structs of the API package; lists, maps and other types are leaves.
func fieldsBeside(t *testing.T, spec reflect.Type, parent string) []string {
	t.Helper()
	cur := spec
	if parent != "" {
		for _, seg := range strings.Split(parent, ".") {
			for cur.Kind() == reflect.Pointer {
				cur = cur.Elem()
			}
			f, ok := fieldByJSON(cur, seg)
			if !ok {
				t.Fatalf("spec.%s: no field %q in %s", parent, seg, cur)
			}
			cur = f.Type
		}
	}
	var out []string
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" || name == "" || !f.IsExported() {
				continue
			}
			path := strings.TrimPrefix(prefix+"."+name, ".")
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft.PkgPath() == apiPkg {
				walk(ft, path)
				continue
			}
			out = append(out, path)
		}
	}
	walk(cur, parent)
	return out
}

func fieldByJSON(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if n, _, _ := strings.Cut(f.Tag.Get("json"), ","); n == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// underAny reports whether path is one of prefixes or below one.
func underAny(path string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return path == p || strings.HasPrefix(path, p+".") })
}

// notCovered: the Secret references of an EvidenceAgent the policy does not
// check, each with why.
var notCovered = map[string]map[string]string{
	"evidenceagents": {
		// The agent trusts the bundle; it sends nothing of it anywhere, and
		// reads it only when spec.allowedSecrets, which the policy checks,
		// lists it too.
		"pgbackrest.endpointCA": "a CA bundle the agent trusts, also gated by allowedSecrets",
		// Only the kubelet uses it, against spec.image's registry, which only
		// the imageOverride allow-list may set.
		"image.pullSecrets": "used by the kubelet for spec.image, which the imageOverride rule gates",
		// The CRD's CEL keeps an agent clusterIP-only and certificateSecretRef
		// ingress-only: the field cannot be set.
		"exposure.tls.certificateSecretRef": "unsettable: exposure must be clusterIP, certificateSecretRef is ingress-only",
	},
}

// TestSecretRefPolicyCoversTheAPI fails when a Secret reference of a
// PgToolBoxConsole, a PgAdmin or an EvidenceAgent is missing from the
// admission policy's Secret rule (unless notCovered says why), is read
// without a has() guard on every optional field along its path (a missing
// guard is an evaluation error, which failurePolicy Fail turns into a refusal
// of every object without it).
func TestSecretRefPolicyCoversTheAPI(t *testing.T) {
	entries := policyEntries(t)
	for resource, spec := range map[string]reflect.Type{
		"pgtoolboxconsoles": reflect.TypeFor[pgtoolboxv1alpha1.PgToolBoxConsoleSpec](),
		"pgadmins":          reflect.TypeFor[pgtoolboxv1alpha1.PgAdminSpec](),
		"evidenceagents":    reflect.TypeFor[pgtoolboxv1alpha1.EvidenceAgentSpec](),
	} {
		got := entries[resource]
		if len(got) == 0 {
			t.Fatalf("the policy has no Secret references for %s", resource)
		}
		fields := secretFields(spec)
		want := map[string]bool{}
		for _, f := range fields {
			if _, skip := notCovered[resource][f.path]; skip {
				if _, ok := got[f.path]; ok {
					t.Errorf("%s: spec.%s is both checked and listed as not covered", resource, f.path)
				}
				continue
			}
			want[f.path] = true
			e, ok := got[f.path]
			if !ok {
				t.Errorf("%s: spec.%s names a Secret and the admission policy does not check it (charts/pgtoolbox/templates/operator/admissionpolicy.yaml, secretRefs)", resource, f.path)
				continue
			}
			for prefix := range f.optional {
				if !strings.Contains(e.line, "has(object.spec."+prefix+")") {
					t.Errorf("%s: spec.%s is read without has() on the optional spec.%s", resource, f.path, prefix)
				}
			}
			if f.list != e.list {
				t.Errorf("%s: spec.%s is a list (%v) but the policy reads it as one (%v)", resource, f.path, f.list, !f.list)
			}
			if f.name != e.name {
				t.Errorf("%s: spec.%s is a bare Secret name (%v) but the policy reads it as one (%v)", resource, f.path, f.name, e.name)
			}
		}
		for p := range got {
			if !want[p] {
				t.Errorf("%s: the policy checks spec.%s, which the API does not declare as a Secret reference", resource, p)
			}
		}
		for p := range notCovered[resource] {
			if !slices.ContainsFunc(fields, func(f secretField) bool { return f.path == p }) {
				t.Errorf("%s: spec.%s is listed as not covered but no longer declared", resource, p)
			}
		}
	}
}

// TestSecretFieldsWalker pins the walker on the shapes it must find, so a
// broken walker cannot make the coverage test pass on nothing.
func TestSecretFieldsWalker(t *testing.T) {
	type inner struct {
		Ref   pgtoolboxv1alpha1.SecretKeySelector      `json:"clientSecretRef"`
		Opt   *pgtoolboxv1alpha1.SecretKeyReference    `json:"headersSecretRef,omitempty"`
		Cfg   *pgtoolboxv1alpha1.LocalObjectReference  `json:"caConfigMapRef,omitempty"`
		Pulls []pgtoolboxv1alpha1.LocalObjectReference `json:"pullSecrets,omitempty"`
	}
	type spec struct {
		Auth *pgtoolboxv1alpha1.PgToolBoxConsoleOIDCSpec `json:"oidc,omitempty"`
		Img  pgtoolboxv1alpha1.ImageSpec                 `json:"image"`
	}
	got := map[string]secretField{}
	for _, f := range secretFields(reflect.TypeFor[spec]()) {
		got[f.path] = f
	}
	for path, wantList := range map[string]bool{
		"oidc.clientSecretRef":             false,
		"oidc.grantClient.clientSecretRef": false,
		"image.pullSecrets":                true,
	} {
		f, ok := got[path]
		if !ok || f.list != wantList {
			t.Errorf("walker: %s = %+v (found %v), want list=%v; all: %v", path, f, ok, wantList, got)
		}
	}
	if !got["oidc.grantClient.clientSecretRef"].optional["oidc.grantClient"] || got["oidc.clientSecretRef"].optional["oidc.clientSecretRef"] {
		t.Errorf("walker optional marks wrong: %+v", got)
	}
	if len(got) != 3 {
		t.Errorf("walker found %d fields, want 3: %v", len(got), got)
	}
	// A ConfigMap reference is not a Secret reference.
	if fs := secretFields(reflect.TypeFor[inner]()); len(fs) != 3 {
		t.Errorf("walker on a flat struct found %v, want clientSecretRef, headersSecretRef, pullSecrets", fs)
	}

	// The shapes the first walker missed: bare Secret names (a string or a
	// list of them), core/v1's Secret-naming types, map values.
	type blind struct {
		Allowed  []string                                              `json:"allowedSecrets,omitempty"`
		One      string                                                `json:"tokenSecret,omitempty"`
		NotOne   string                                                `json:"serverName,omitempty"`
		Core     *corev1.SecretKeySelector                             `json:"password,omitempty"`
		CoreRef  corev1.SecretReference                                `json:"bootstrap"`
		CoreLoc  []corev1.LocalObjectReference                         `json:"imagePullSecrets,omitempty"`
		CoreCM   *corev1.LocalObjectReference                          `json:"configMapRef,omitempty"`
		ByName   map[string]corev1.SecretKeySelector                   `json:"env,omitempty"`
		Tenants  map[string]pgtoolboxv1alpha1.PgToolBoxConsoleOIDCSpec `json:"tenants,omitempty"`
		Resource *corev1.ResourceRequirements                          `json:"resources,omitempty"`
	}
	gotBlind := map[string]secretField{}
	for _, f := range secretFields(reflect.TypeFor[blind]()) {
		gotBlind[f.path] = f
	}
	for path, want := range map[string]secretField{
		"allowedSecrets":                        {list: true, name: true},
		"tokenSecret":                           {name: true},
		"password":                              {},
		"bootstrap":                             {},
		"imagePullSecrets":                      {list: true},
		"env{}":                                 {},
		"tenants{}.clientSecretRef":             {},
		"tenants{}.grantClient.clientSecretRef": {},
	} {
		f, ok := gotBlind[path]
		if !ok || f.list != want.list || f.name != want.name {
			t.Errorf("walker: %s = %+v (found %v), want list=%v name=%v; all: %v", path, f, ok, want.list, want.name, gotBlind)
		}
	}
	if len(gotBlind) != 8 {
		t.Errorf("walker found %d fields, want 8 (serverName and configMapRef name no Secret): %v", len(gotBlind), gotBlind)
	}
	if !gotBlind["allowedSecrets"].optional["allowedSecrets"] || !gotBlind["tenants{}.clientSecretRef"].optional["tenants"] {
		t.Errorf("walker optional marks wrong: %+v", gotBlind)
	}
}

// pgadminClusterFields: every field of a PgAdmin that names a Cluster, and
// what the credentials rule does with it. A Cluster of the namespace is a
// credential source: the operator hands pgAdmin a login of it (the
// credentials offered, or the settings database's own role), so the rule
// reads the field; one of another namespace is the PgAdminAccess grant's.
var pgadminClusterFields = map[string]string{
	"clusterRefs":               "source",
	"clusterSelector":           "source",
	"configDatabase.clusterRef": "source",
	"crossNamespaceClusterRefs": "grant",
}

// TestPgAdminClusterFieldsAreCredentialSources fails when a field of a
// PgAdmin that names a Cluster is not classified, or is a source the
// credentials rule does not read: a new field through which the operator acts
// on a Cluster of the namespace cannot slip past the rule, as configDatabase
// once did.
func TestPgAdminClusterFieldsAreCredentialSources(t *testing.T) {
	policy, err := os.ReadFile(policyFile)
	if err != nil {
		t.Fatal(err)
	}
	before, _, ok := strings.Cut(string(policy), "offers the credentials of Clusters of its namespace")
	if !ok {
		t.Fatal("no credentials validation in the policy")
	}
	rule := before[strings.LastIndex(before, "- expression:"):]

	spec := reflect.TypeFor[pgtoolboxv1alpha1.PgAdminSpec]()
	for _, leaf := range fieldsBeside(t, spec, "") {
		if !strings.Contains(strings.ToLower(leaf), "cluster") {
			continue
		}
		if !underAny(leaf, slices.Collect(maps.Keys(pgadminClusterFields))) {
			t.Errorf("spec.%s names a Cluster: classify it in pgadminClusterFields (a credential source of the credentials rule, or a grant's)", leaf)
		}
	}
	for path, kind := range pgadminClusterFields {
		if !slices.ContainsFunc(fieldsBeside(t, spec, ""), func(l string) bool { return underAny(l, []string{path}) }) {
			t.Errorf("pgadminClusterFields lists spec.%s, which the API does not declare", path)
		}
		top, _, _ := strings.Cut(path, ".")
		reads := strings.Contains(rule, ".spec."+top)
		switch {
		case kind == "source" && !reads:
			t.Errorf("the credentials rule does not read spec.%s, a credential source", path)
		case kind == "grant" && reads:
			t.Errorf("the credentials rule reads spec.%s, which the PgAdminAccess grant decides", path)
		}
	}
}
