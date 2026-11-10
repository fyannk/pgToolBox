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
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// denyEntry is one entry of pgtoolbox.admission.exposureDenyList, spelled as
// the security documentation's table spells it: "*snippet*" for a substring
// of any key, "<domain><name>*" for a prefix, "<domain><name>" for a key.
type denyEntry string

// example returns an annotation key the entry denies.
func (e denyEntry) example() string {
	s := string(e)
	switch {
	case strings.HasPrefix(s, "*"):
		return "example.com/x-" + strings.Trim(s, "*")
	case strings.HasSuffix(s, "*"):
		return strings.TrimSuffix(s, "*") + "x"
	default:
		return s
	}
}

// literal is the CEL string literal the entry renders to.
func (e denyEntry) literal() string { return "'" + strings.Trim(string(e), "*") + "'" }

var denyListDefineRe = regexp.MustCompile(`(?s)\{\{- define "pgtoolbox.admission.exposureDenyList" -\}\}\n(.*?)\{\{- end \}\}`)

// exposureDenyList reads the data list of _admissionpolicy.tpl.
func exposureDenyList(t *testing.T) []denyEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "pgtoolbox", "templates", "_admissionpolicy.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	m := denyListDefineRe.FindSubmatch(raw)
	if m == nil {
		t.Fatal("_admissionpolicy.tpl defines no pgtoolbox.admission.exposureDenyList")
	}
	var list struct {
		Contains    []string `json:"contains"`
		Controllers []struct {
			Domain     string   `json:"domain"`
			Controller string   `json:"controller"`
			Prefixes   []string `json:"prefixes"`
			Keys       []string `json:"keys"`
		} `json:"controllers"`
	}
	if err := yaml.UnmarshalStrict(m[1], &list); err != nil {
		t.Fatalf("exposureDenyList: %v", err)
	}
	var out []denyEntry
	for _, c := range list.Contains {
		out = append(out, denyEntry("*"+c+"*"))
	}
	for _, c := range list.Controllers {
		if !strings.HasSuffix(c.Domain, "/") {
			t.Errorf("domain %q does not end with /", c.Domain)
		}
		for _, p := range c.Prefixes {
			out = append(out, denyEntry(c.Domain+p+"*"))
		}
		for _, k := range c.Keys {
			out = append(out, denyEntry(c.Domain+k))
		}
	}
	if len(out) < 40 {
		t.Fatalf("exposureDenyList has %d entries; the parser lost some", len(out))
	}
	return out
}

var codeSpanRe = regexp.MustCompile("`([^`]+)`")

// documentedDenyList reads the table of the security documentation's
// section on exposure annotations.
func documentedDenyList(t *testing.T) []denyEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "site", "docs", "security.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(raw), "## Exposure annotations that configure the ingress controller")
	if !ok {
		t.Fatal("security.md has no section on exposure annotations")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	var out []denyEntry
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 || !strings.HasPrefix(line, "|") {
			continue
		}
		domain, keys := strings.TrimSpace(cells[1]), cells[2]
		switch {
		case domain == "(any)":
			domain = ""
		case strings.HasPrefix(domain, "`"):
			domain = strings.Trim(domain, "`")
		default:
			continue // the header and the separator
		}
		for _, m := range codeSpanRe.FindAllStringSubmatch(keys, -1) {
			out = append(out, denyEntry(domain+m[1]))
		}
	}
	return out
}

func sorted(in []denyEntry) []denyEntry {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// TestExposureDenyListDocumented: the security documentation's table lists
// exactly the keys the admission policy denies.
func TestExposureDenyListDocumented(t *testing.T) {
	policy, docs := sorted(exposureDenyList(t)), sorted(documentedDenyList(t))
	if len(slices.Compact(slices.Clone(policy))) != len(policy) {
		t.Errorf("exposureDenyList has duplicates: %v", policy)
	}
	for _, e := range policy {
		if !slices.Contains(docs, e) {
			t.Errorf("security.md does not document %q", e)
		}
	}
	for _, e := range docs {
		if !slices.Contains(policy, e) {
			t.Errorf("security.md documents %q, which the policy does not deny", e)
		}
	}
}

var annotationLiteralRe = regexp.MustCompile(`'[a-z0-9.-]+\.[a-z]+/[^']*'`)

// TestExposureDenyListRendered: the rendered policy tests every entry of the
// list, on spec.exposure.annotations and on metadataPropagation, and no
// annotation key that is not on it.
func TestExposureDenyListRendered(t *testing.T) {
	policy, _ := renderPolicy(t)
	expr := ""
	for _, v := range policy.Spec.Variables {
		if v.Name == "exposureAnnotationsKept" {
			expr = v.Expression
		}
	}
	if expr == "" {
		t.Fatal("no exposureAnnotationsKept variable")
	}
	entries := exposureDenyList(t)
	want := map[string]bool{}
	for _, e := range entries {
		want[e.literal()] = true
		// once for spec.exposure.annotations, once for metadataPropagation
		if n := strings.Count(expr, ", "+e.literal()) + strings.Count(expr, "["+e.literal()) + strings.Count(expr, "("+e.literal()); n != 2 {
			t.Errorf("%s is tested %d times, want 2", e.literal(), n)
		}
	}
	for _, lit := range annotationLiteralRe.FindAllString(expr, -1) {
		if !want[lit] && !strings.HasSuffix(lit, "/'") { // a domain
			t.Errorf("the policy tests %s, which is not on exposureDenyList", lit)
		}
	}
	if !strings.Contains(expr, "r.target in ['All', 'Exposure', 'Service']") {
		t.Error("metadataPropagation rules for the Service are not checked")
	}
}

func TestDenyEntryExample(t *testing.T) {
	for _, tc := range []struct{ entry, want string }{
		{"*snippet*", "example.com/x-snippet"},
		{"nginx.ingress.kubernetes.io/canary*", "nginx.ingress.kubernetes.io/canaryx"},
		{"konghq.com/plugins", "konghq.com/plugins"},
	} {
		if got := denyEntry(tc.entry).example(); got != tc.want {
			t.Errorf("%s: example %q, want %q", tc.entry, got, tc.want)
		}
	}
}
