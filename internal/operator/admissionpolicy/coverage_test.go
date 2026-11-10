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
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// secretField is a Secret reference the API types declare, at a path below
// spec (JSON names, "[]" for a list element), with the JSON names of the
// optional fields along the way.
type secretField struct {
	path     string
	optional map[string]bool // path prefix -> the field ending there is optional
	list     bool            // the reference itself is a list (pullSecrets)
}

var (
	apiPkg          = reflect.TypeFor[pgtoolboxv1alpha1.LocalObjectReference]().PkgPath()
	localRefType    = reflect.TypeFor[pgtoolboxv1alpha1.LocalObjectReference]()
	secretKeyTypes  = []reflect.Type{reflect.TypeFor[pgtoolboxv1alpha1.SecretKeyReference](), reflect.TypeFor[pgtoolboxv1alpha1.SecretKeySelector]()}
	singleEntryRe   = regexp.MustCompile(`\{'p': '([^']+)', 'n': string\(\{\{ \$o \}\}\.spec\.([A-Za-z0-9.]+)\.name\)\}`)
	listEntryRe     = regexp.MustCompile(`\{\{ \$o \}\}\.spec\.([A-Za-z0-9.]+)\.map\(s, \{'p': '([^']+)', 'n': string\(s\.name\)\}\)`)
	resourceSplitRe = regexp.MustCompile(`request\.resource\.resource == '([a-z]+)' \?`)
)

// secretFields walks a spec type: every SecretKeyReference and
// SecretKeySelector, and every LocalObjectReference (or list of them) whose
// JSON name says it names a Secret.
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
			opt := maps(optional)
			if name != "" && (f.Type.Kind() == reflect.Pointer || strings.Contains(opts, "omitempty")) {
				opt[path] = true
			}
			ft, list := f.Type, false
			for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
				if ft.Kind() == reflect.Slice {
					list = true
				}
				ft = ft.Elem()
			}
			switch {
			case slices.Contains(secretKeyTypes, ft):
				p := path
				if list {
					p += "[]"
				}
				out = append(out, secretField{path: p, optional: opt, list: inList})
			case ft == localRefType:
				if strings.Contains(strings.ToLower(name), "secret") {
					out = append(out, secretField{path: path, optional: opt, list: list || inList})
				}
			case ft.Kind() == reflect.Struct && ft.PkgPath() == apiPkg:
				p := path
				if list && name != "" {
					p += "[]"
				}
				walk(ft, p, opt, inList || list)
			}
		}
	}
	walk(t, "", map[string]bool{}, false)
	return out
}

func maps(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// policyEntries reads the Secret-reference helper of the operator chart: per
// resource, each path it extracts and the source line that extracts it.
func policyEntries(t *testing.T) map[string]map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "pgtoolbox", "templates", "_admissionpolicy.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	resource := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if m := resourceSplitRe.FindStringSubmatch(line); m != nil {
			resource = m[1]
			out[resource] = map[string]string{}
			continue
		}
		if resource == "" {
			continue
		}
		for _, m := range singleEntryRe.FindAllStringSubmatch(line, -1) {
			if m[1] != m[2] {
				t.Errorf("%s: entry %q reads spec.%s", resource, m[1], m[2])
			}
			out[resource][m[1]] = line
		}
		for _, m := range listEntryRe.FindAllStringSubmatch(line, -1) {
			if m[1] != m[2] {
				t.Errorf("%s: list entry %q maps spec.%s", resource, m[2], m[1])
			}
			out[resource][m[2]] = line
		}
	}
	return out
}

// TestSecretRefPolicyCoversTheAPI fails when a Secret reference of a
// PgToolBoxConsole or a PgAdmin is missing from the admission policy's
// Secret rule, or is read without a has() guard on every optional field
// along its path (a missing guard is an evaluation error, which
// failurePolicy Fail turns into a refusal of every object without it).
func TestSecretRefPolicyCoversTheAPI(t *testing.T) {
	entries := policyEntries(t)
	for resource, spec := range map[string]reflect.Type{
		"pgtoolboxconsoles": reflect.TypeFor[pgtoolboxv1alpha1.PgToolBoxConsoleSpec](),
		"pgadmins":          reflect.TypeFor[pgtoolboxv1alpha1.PgAdminSpec](),
	} {
		got := entries[resource]
		if len(got) == 0 {
			t.Fatalf("the policy has no Secret references for %s", resource)
		}
		fields := secretFields(spec)
		want := map[string]bool{}
		for _, f := range fields {
			want[f.path] = true
			line, ok := got[f.path]
			if !ok {
				t.Errorf("%s: spec.%s names a Secret and the admission policy does not check it (charts/pgtoolbox/templates/_admissionpolicy.tpl)", resource, f.path)
				continue
			}
			for prefix := range f.optional {
				if !strings.Contains(line, "has({{ $o }}.spec."+prefix+")") {
					t.Errorf("%s: spec.%s is read without has() on the optional spec.%s", resource, f.path, prefix)
				}
			}
			if f.list != strings.Contains(line, ".map(s,") {
				t.Errorf("%s: spec.%s is a list (%v) but the policy reads it as one (%v)", resource, f.path, f.list, !f.list)
			}
		}
		for p := range got {
			if !want[p] {
				t.Errorf("%s: the policy checks spec.%s, which the API does not declare as a Secret reference", resource, p)
			}
		}
	}
}

// TestEvidenceAgentSecretRefsAreKnown: the Secret rule does not cover
// EvidenceAgents (docs/site/docs/security.md says so). Each Secret reference
// of an EvidenceAgent is listed here with why; a new one fails until it is
// either added to the policy or listed with its reason.
func TestEvidenceAgentSecretRefsAreKnown(t *testing.T) {
	known := map[string]string{
		// The agent trusts the bundle; it sends nothing of it anywhere, and
		// the agent reads it only when spec.allowedSecrets lists it too.
		"pgbackrest.endpointCA": "a CA bundle the agent trusts, also gated by allowedSecrets",
		// Only the kubelet uses it, against spec.image's registry, which only
		// the imageOverride allow-list may set.
		"image.pullSecrets": "used by the kubelet for spec.image, which the imageOverride rule gates",
		// The CRD's CEL keeps an agent clusterIP-only and certificateSecretRef
		// ingress-only: the field cannot be set.
		"exposure.tls.certificateSecretRef": "unsettable: exposure must be clusterIP, certificateSecretRef is ingress-only",
	}
	if entries := policyEntries(t); len(entries["evidenceagents"]) != 0 {
		t.Fatalf("the policy now covers EvidenceAgents: fold them into TestSecretRefPolicyCoversTheAPI")
	}
	seen := map[string]bool{}
	for _, f := range secretFields(reflect.TypeFor[pgtoolboxv1alpha1.EvidenceAgentSpec]()) {
		seen[f.path] = true
		if _, ok := known[f.path]; !ok {
			t.Errorf("EvidenceAgent spec.%s names a Secret: add it to the admission policy or list it here with the reason it needs none", f.path)
		}
	}
	for p := range known {
		if !seen[p] {
			t.Errorf("EvidenceAgent spec.%s is listed but no longer declared", p)
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
}
