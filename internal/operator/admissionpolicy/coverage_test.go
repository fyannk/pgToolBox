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

	refRe = `'n': string\(\{\{ \$o \}\}\.spec\.([A-Za-z0-9.]+)\.name\)`
	// 'd' runs to the end of its map; "{{ $o }}" is always followed by
	// ".spec", so the first "}" followed by "]" or ")" closes the entry.
	destRe          = `'d': (.*?)\}`
	singleEntryRe   = regexp.MustCompile(`\{'p': '([^']+)', ` + refRe + `, ` + destRe + `\]`)
	listEntryRe     = regexp.MustCompile(`\{\{ \$o \}\}\.spec\.([A-Za-z0-9.]+)\.map\(s, \{'p': '([^']+)', 'n': string\(s(\.name)?\), ` + destRe + `\)`)
	hasRe           = regexp.MustCompile(`has\(\{\{ \$o \}\}\.spec\.[A-Za-z0-9.]+\)`)
	readRe          = regexp.MustCompile(`\{\{ \$o \}\}\.spec\.([A-Za-z0-9.]+)`)
	resourceSplitRe = regexp.MustCompile(`request\.resource\.resource == '([a-z]+)' \?`)
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

// policyEntry is one extraction of the Secret-reference helper.
type policyEntry struct {
	line string
	dest []string // the spec paths the destination is read from, sorted; none for ''
	name bool     // reads a bare string (string(s)), not s.name
}

// destPaths is the spec paths a destination expression reads, has() guards
// aside, sorted and without duplicates.
func destPaths(expr string) []string {
	var out []string
	for _, m := range readRe.FindAllStringSubmatch(hasRe.ReplaceAllString(expr, ""), -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// policyEntries reads the Secret-reference helper of the operator chart: per
// resource, each path it extracts, the source line that extracts it and the
// destination it pairs the reference with.
func policyEntries(t *testing.T) map[string]map[string]policyEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "pgtoolbox", "templates", "_admissionpolicy.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	body, _, _ := strings.Cut(string(raw), "{{- end }}") // the secretRefs helper only
	out := map[string]map[string]policyEntry{}
	resource := ""
	for _, line := range strings.Split(body, "\n") {
		if m := resourceSplitRe.FindStringSubmatch(line); m != nil {
			resource = m[1]
			out[resource] = map[string]policyEntry{}
			continue
		}
		if resource == "" {
			continue
		}
		for _, m := range singleEntryRe.FindAllStringSubmatch(line, -1) {
			if m[1] != m[2] {
				t.Errorf("%s: entry %q reads spec.%s", resource, m[1], m[2])
			}
			out[resource][m[1]] = policyEntry{line: line, dest: destPaths(m[3])}
		}
		for _, m := range listEntryRe.FindAllStringSubmatch(line, -1) {
			if m[1] != m[2] {
				t.Errorf("%s: list entry %q maps spec.%s", resource, m[2], m[1])
			}
			out[resource][m[2]] = policyEntry{line: line, dest: destPaths(m[4]), name: m[3] == ""}
		}
	}
	return out
}

// destinations: everything that decides where each workload sends a Secret
// it is given, and whom it trusts there, for every reference whose
// destination the spec names. The policy pairs the reference with all of
// it, so a kept reference sent somewhere else is checked again. A reference
// not listed here has no destination in the spec, and noDestination says
// why.
var destinations = map[string]map[string][]string{
	"pgtoolboxconsoles": {
		"auth.oidc.clientSecretRef":              {"auth.oidc.issuerURL", "auth.oidc.caConfigMapRef.name"},
		"auth.oidc.grantClient.clientSecretRef":  {"auth.oidc.issuerURL", "auth.oidc.caConfigMapRef.name"},
		"auth.openshift.clientSecretRef":         {"auth.openshift.caConfigMapRef.name"},
		"prometheus.tokenSecretRef":              {"prometheus.url", "prometheus.caConfigMapRef.name"},
		"digest.delivery.smtp.passwordSecretRef": {"digest.delivery.smtp.address", "digest.delivery.smtp.tls"},
		"tracing.headersSecretRef":               {"tracing.endpoint"},
		"image.pullSecrets":                      {"image.repository"},
	},
	"pgadmins": {
		"auth.oauth2.clientSecretRef":                {"auth.oauth2.issuerURL"},
		"auth.openshiftOAuthProxy.image.pullSecrets": {"auth.openshiftOAuthProxy.image.repository"},
		"image.pullSecrets":                          {"image.repository"},
	},
	"evidenceagents": {
		"allowedSecrets": {
			"clusterRef.name", "objectStoreRef.name", "allowInsecureEndpoint",
			"pgbackrest.repository", "pgbackrest.endpoint", "pgbackrest.endpointCA.name", "pgbackrest.endpointCA.key",
		},
	},
}

var noDestination = map[string]string{
	"digest.delivery.chatWebhookSecretRef": "the Secret holds the URL itself",
	"digest.delivery.webhookSecretRef":     "the Secret holds the URL itself",
	"restoreDrills.sealKeySecretRef":       "a key the console keeps, sent nowhere",
	"exposure.tls.certificateSecretRef":    "served by the Ingress, sent nowhere",
}

// notDestination: per reference with a destination, the fields beside it
// (below the struct that holds it; a path prefix covers what is below it)
// that do not decide where it is sent, each with why. Together with
// destinations it classifies every such field, so a new field beside a
// reference (an insecure switch, a second CA) fails the coverage test until
// somebody says whether it moves the Secret.
var notDestination = map[string]map[string]map[string]string{
	"pgtoolboxconsoles": {
		"auth.oidc.clientSecretRef": {
			"auth.oidc.clientID":             "names the client to the same issuer",
			"auth.oidc.redirectURL":          "where the browser returns with a code; the client secret is not sent there",
			"auth.oidc.scopes":               "what is asked of the same issuer",
			"auth.oidc.requireEmailVerified": "a check of the ID token, not a destination",
			"auth.oidc.grantClient":          "a reference of its own",
		},
		"auth.oidc.grantClient.clientSecretRef": {
			"auth.oidc.grantClient.clientID": "names the client to the same issuer",
		},
		"auth.openshift.clientSecretRef": {},
		"prometheus.tokenSecretRef": {
			"prometheus.projectedToken": "the other token source, refused beside tokenSecretRef by the CRD",
			"prometheus.tokenAudience":  "applies to the projected token only",
			"prometheus.namespaced":     "a query parameter",
			"prometheus.range":          "the charts' window",
		},
		"digest.delivery.smtp.passwordSecretRef": {
			"digest.delivery.smtp.username": "sent with the password, to the same server",
			"digest.delivery.smtp.from":     "a mail header",
			"digest.delivery.smtp.to":       "mail recipients; the password is not in the mail",
		},
		"tracing.headersSecretRef": {
			"tracing.serviceName":     "a span attribute",
			"tracing.sampler":         "which spans are sent, not where",
			"tracing.samplerArg":      "which spans are sent, not where",
			"tracing.includePayloads": "span content, not where it is sent",
		},
		"image.pullSecrets": {
			"image.tag":        "which image of the same repository",
			"image.digest":     "which image of the same repository",
			"image.pullPolicy": "when the kubelet pulls, not from where",
		},
	},
	"pgadmins": {
		"auth.oauth2.clientSecretRef": {
			"auth.oauth2.clientID":       "names the client to the same issuer",
			"auth.oauth2.requiredClaims": "who may sign in: the credentials rule's audience check",
			"auth.oauth2.usernameClaim":  "who may sign in: the credentials rule's audience check",
			"auth.oauth2.scopes":         "what is asked of the same issuer",
			"auth.oauth2.displayName":    "a button label",
		},
		"auth.openshiftOAuthProxy.image.pullSecrets": {
			"auth.openshiftOAuthProxy.image.tag":        "which image of the same repository",
			"auth.openshiftOAuthProxy.image.digest":     "which image of the same repository",
			"auth.openshiftOAuthProxy.image.pullPolicy": "when the kubelet pulls, not from where",
		},
		"image.pullSecrets": {
			"image.tag":        "which image of the same repository",
			"image.digest":     "which image of the same repository",
			"image.pullPolicy": "when the kubelet pulls, not from where",
		},
	},
	"evidenceagents": {
		"allowedSecrets": {
			"serverName":       "a key prefix inside the repository the Cluster and ObjectStore name",
			"image":            "the agent image: the imageOverride rule and the operator's repository allow-list",
			"resources":        "compute resources",
			"refreshInterval":  "how often, not where",
			"exposure":         "how the console reaches the agent (clusterIP only)",
			"tls":              "the agent's serving certificate",
			"workloadIdentity": "the Pod's cloud identity: the workloadIdentity rule",
		},
	},
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

// optionalAlong returns the JSON paths of the optional fields along path
// (dot-separated JSON names below spec), the field itself included.
func optionalAlong(t *testing.T, spec reflect.Type, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	cur, prefix := spec, ""
	for _, seg := range strings.Split(path, ".") {
		for cur.Kind() == reflect.Pointer {
			cur = cur.Elem()
		}
		found := false
		for i := range cur.NumField() {
			f := cur.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != seg {
				continue
			}
			prefix = strings.TrimPrefix(prefix+"."+seg, ".")
			if f.Type.Kind() == reflect.Pointer || strings.Contains(opts, "omitempty") {
				out[prefix] = true
			}
			cur, found = f.Type, true
			break
		}
		if !found {
			t.Fatalf("spec.%s: no field %q in %s", path, seg, cur)
		}
	}
	return out
}

// TestSecretRefPolicyCoversTheAPI fails when a Secret reference of a
// PgToolBoxConsole, a PgAdmin or an EvidenceAgent is missing from the
// admission policy's Secret rule (unless notCovered says why), is read
// without a has() guard on every optional field along its path (a missing
// guard is an evaluation error, which failurePolicy Fail turns into a refusal
// of every object without it), or is not paired with the destination the
// spec sends it to.
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
				t.Errorf("%s: spec.%s names a Secret and the admission policy does not check it (charts/pgtoolbox/templates/_admissionpolicy.tpl)", resource, f.path)
				continue
			}
			for prefix := range f.optional {
				if !strings.Contains(e.line, "has({{ $o }}.spec."+prefix+")") {
					t.Errorf("%s: spec.%s is read without has() on the optional spec.%s", resource, f.path, prefix)
				}
			}
			if f.list != strings.Contains(e.line, ".map(s,") {
				t.Errorf("%s: spec.%s is a list (%v) but the policy reads it as one (%v)", resource, f.path, f.list, !f.list)
			}
			if f.name != e.name {
				t.Errorf("%s: spec.%s is a bare Secret name (%v) but the policy reads it as one (%v)", resource, f.path, f.name, e.name)
			}
			wantDest, hasDest := destinations[resource][f.path]
			_, why := noDestination[f.path]
			switch {
			case hasDest && !slices.Equal(e.dest, slices.Sorted(slices.Values(wantDest))):
				t.Errorf("%s: spec.%s is sent to %v, the policy pairs it with %v", resource, f.path, wantDest, e.dest)
			case !hasDest && !why:
				t.Errorf("%s: spec.%s: say where it is sent (destinations) or why the spec names no destination (noDestination)", resource, f.path)
			case !hasDest && len(e.dest) != 0:
				t.Errorf("%s: spec.%s has no destination but the policy pairs it with %v", resource, f.path, e.dest)
			}
			if !hasDest {
				continue
			}
			// The destination is read beside the reference: every optional
			// field on its way must be guarded on that line too.
			for _, d := range wantDest {
				for prefix := range optionalAlong(t, spec, d) {
					if !strings.Contains(e.line, "has({{ $o }}.spec."+prefix+")") {
						t.Errorf("%s: the destination spec.%s of spec.%s is read without has() on the optional spec.%s", resource, d, f.path, prefix)
					}
				}
			}
			// Every field beside the reference is a destination or says why not.
			parent := ""
			if i := strings.LastIndex(f.path, "."); i >= 0 {
				parent = f.path[:i]
			}
			beside, classified := notDestination[resource][f.path]
			if !classified {
				t.Errorf("%s: spec.%s: classify the fields beside it (notDestination)", resource, f.path)
			}
			reasons := slices.Collect(maps.Keys(beside))
			for _, leaf := range fieldsBeside(t, spec, parent) {
				if underAny(leaf, []string{f.path}) || underAny(leaf, wantDest) || underAny(leaf, reasons) {
					continue
				}
				t.Errorf("%s: spec.%s is beside spec.%s: say whether it decides where that Secret is sent (destinations) or why not (notDestination)", resource, leaf, f.path)
			}
			for p := range beside {
				if !slices.ContainsFunc(fieldsBeside(t, spec, parent), func(l string) bool { return underAny(l, []string{p}) }) {
					t.Errorf("%s: notDestination lists spec.%s beside spec.%s, which the API does not declare", resource, p, f.path)
				}
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

// pgadminAudience: the fields of a PgAdmin that decide who may sign in to
// pgAdmin, or who becomes its administrator, and so who holds the Cluster
// credentials it offers. The credentials rule compares each (a path prefix
// covers what is below it) between the old and the new object.
var pgadminAudience = []string{
	"auth.oauth2.issuerURL",
	"auth.oauth2.clientID",
	"auth.oauth2.clientSecretRef",
	"auth.oauth2.requiredClaims",
	"auth.oauth2.usernameClaim",
	"auth.oauth2.scopes",
	"auth.openshiftOAuthProxy.image",
	// Raising it keeps a revoked person in pgAdmin longer; the check lets
	// it go down only.
	"auth.openshiftOAuthProxy.sessionMaxAge",
	"admins",
}

// pgadminNotAudience: every other field of a PgAdmin, each with why it does
// not decide who reaches the credentials, or which rule does.
var pgadminNotAudience = map[string]string{
	"auth.oauth2.displayName":   "the login button's label",
	"clusterRefs":               "what is offered: the credentials rule's sources",
	"clusterSelector":           "what is offered: the credentials rule's sources",
	"credentials":               "what is offered: the credentials rule's kinds",
	"crossNamespaceClusterRefs": "the PgAdminAccess grant of the target namespace decides",
	"endpoints":                 "which Services of the same Clusters",
	"image":                     "the imageOverride rule",
	"storage":                   "pgAdmin's volume",
	"configDatabase":            "a credential source of the credentials rule (pgadminClusterFields), immutable by CEL",
	"replicas":                  "how many Pods",
	"exposure":                  "how pgAdmin is reached; spec.auth still decides who signs in",
	"networkPolicy":             "which addresses reach the port; spec.auth still decides who signs in",
	"resources":                 "compute resources",
	"metadataPropagation":       "labels and annotations",
}

// TestPgAdminAudienceFieldsClassified fails when a field of a PgAdmin is
// neither compared by the credentials rule's audience check
// (pgtoolbox.admission.pgadminAudienceKept) nor listed with why it does not
// widen who holds the credentials: a new sign-in field cannot slip past the
// rule unclassified.
func TestPgAdminAudienceFieldsClassified(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "pgtoolbox", "templates", "_admissionpolicy.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(raw), `{{- define "pgtoolbox.admission.pgadminAudienceKept" -}}`)
	if !ok {
		t.Fatal("no pgtoolbox.admission.pgadminAudienceKept helper")
	}
	body, _, _ = strings.Cut(body, "{{- end }}")

	spec := reflect.TypeFor[pgtoolboxv1alpha1.PgAdminSpec]()
	for _, leaf := range fieldsBeside(t, spec, "") {
		audience := underAny(leaf, pgadminAudience)
		_, other := pgadminNotAudience[leaf]
		other = other || underAny(leaf, slices.Collect(maps.Keys(pgadminNotAudience)))
		switch {
		case audience && other:
			t.Errorf("spec.%s is both an audience field and not one", leaf)
		case !audience && !other:
			t.Errorf("spec.%s: say whether it decides who signs in to pgAdmin (pgadminAudience) or why not (pgadminNotAudience)", leaf)
		}
	}
	for _, p := range pgadminAudience {
		optional := optionalAlong(t, spec, p)
		for _, side := range []string{"object", "oldObject"} {
			read := side + ".spec." + p
			if !strings.Contains(body, read+" ") && !strings.Contains(body, read+")") {
				t.Errorf("the audience check does not compare %s", read)
			}
			if optional[p] && !strings.Contains(body, "has("+read+")") {
				t.Errorf("the audience check reads the optional %s without has()", read)
			}
		}
	}
	for _, p := range []string{"auth.oauth2", "auth.openshiftOAuthProxy"} {
		if !strings.Contains(body, "has(object.spec."+p+") == has(oldObject.spec."+p+")") {
			t.Errorf("the audience check does not compare whether spec.%s is set", p)
		}
	}
	for p := range pgadminNotAudience {
		if !slices.ContainsFunc(fieldsBeside(t, spec, ""), func(l string) bool { return underAny(l, []string{p}) }) {
			t.Errorf("pgadminNotAudience lists spec.%s, which the API does not declare", p)
		}
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
// credentials rule (its validation and the credentialSources helper) does
// not read: a new field through which the operator acts on a Cluster of the
// namespace cannot slip past the rule, as configDatabase once did.
func TestPgAdminClusterFieldsAreCredentialSources(t *testing.T) {
	tpl, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "pgtoolbox", "templates", "_admissionpolicy.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	_, sources, ok := strings.Cut(string(tpl), `{{- define "pgtoolbox.admission.credentialSources" -}}`)
	if !ok {
		t.Fatal("no pgtoolbox.admission.credentialSources helper")
	}
	sources, _, _ = strings.Cut(sources, "{{- end }}")
	policy, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "pgtoolbox", "templates", "operator", "admissionpolicy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	before, _, ok := strings.Cut(string(policy), "offers the credentials of Clusters of its namespace (spec.credentials)")
	if !ok {
		t.Fatal("no credentials validation in the policy")
	}
	validation := before[strings.LastIndex(before, "- expression:"):]
	rule := sources + validation

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
