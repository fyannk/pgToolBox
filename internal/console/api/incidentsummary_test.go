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

package api

import (
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

var updateSummary = flag.Bool("update", false, "rewrite the incident summary golden files")

const summaryURL = "https://console.example.com/c/team-a/"

// unknownSummary is a Cluster the console has only started reading:
// nothing is judged, diagnostics have not run.
func unknownSummary() IncidentSummary {
	since := fixtures.Now.Add(-5 * time.Second)
	return IncidentSummary{
		Overview: viewmodel.ClusterOverview{
			Ref: viewmodel.ClusterRef{Namespace: "team-a", Name: "ledger"}, GeneratedAt: fixtures.Now, Present: true,
			Verdict: viewmodel.Verdict{Health: viewmodel.HealthUnknown, Contributors: []viewmodel.Contributor{
				{ID: "source:pods", Health: viewmodel.HealthUnknown, Label: "Pods", Detail: "not observed yet", Provenance: viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: "Pod"}},
			}},
			Sources: []viewmodel.SourceState{
				{Source: "clusters", State: viewmodel.SourceLive, Since: &since},
				{Source: "pods", State: viewmodel.SourcePending, Since: &since},
				{Source: "instanceStatus", State: viewmodel.SourceUnavailable, Since: &since, Reason: "dial tcp 10.0.0.12:8000: connection refused"},
			},
		},
		Diagnostics: &viewmodel.Diagnostics{Runner: viewmodel.SourceState{Source: "diagnostics", State: viewmodel.SourcePending}},
		ConsoleURL:  summaryURL + "ledger",
		Now:         fixtures.Now,
	}
}

func TestIncidentSummaryGolden(t *testing.T) {
	t.Parallel()
	healthyDiag := fixtures.Diagnostics()
	healthyDiag.Ref = fixtures.HealthyOverview().Ref
	healthyDiag.Incidents, healthyDiag.Counts = nil, viewmodel.FindingCounts{RunAt: healthyDiag.RunAt}
	degradedDiag := fixtures.Diagnostics()
	for _, tc := range []struct {
		name string
		in   IncidentSummary
	}{
		{"healthy", IncidentSummary{Overview: fixtures.HealthyOverview(), Diagnostics: &healthyDiag, ConsoleURL: summaryURL + "orders", Now: fixtures.Now}},
		{"degraded", IncidentSummary{Overview: fixtures.FailingOverOverview(), Diagnostics: &degradedDiag, ConsoleURL: summaryURL + "billing", Now: fixtures.Now}},
		{"unknown", unknownSummary()},
		// The diagnostics could not be read and the console's origin is not known.
		{"unread", IncidentSummary{Overview: fixtures.FailingOverOverview(), Now: fixtures.Now}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RenderIncidentSummary(tc.in)
			path := filepath.Join("testdata", "summary", tc.name+".md")
			if *updateSummary {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if got != string(want) {
				t.Fatalf("%s differs from the golden file (go test -update):\n%s", path, got)
			}
		})
	}
}

// A finding, a source reason or an object name from the cluster cannot
// open a link, a mention, a code span or a new line in the channel, and
// cannot carry a credential into it.
func TestIncidentSummaryEscapesUntrustedText(t *testing.T) {
	t.Parallel()
	evil := "see [x](http://evil.example) @here @channel <!here> `rm -rf` **bold** ~~gone~~ &amp; <img src=x>\n- injected item\r\n# heading\u2028line\u202Edesrever password=hunter2 www.evil.example"
	run := fixtures.Now.Add(-10 * time.Second)
	in := unknownSummary()
	in.Overview.Ref.Name = "ledger`[x](http://evil)"
	in.Overview.Phase.Text = evil
	in.Overview.Verdict.Contributors[0].Detail = evil
	in.Overview.Sources[2].Reason = evil
	in.Diagnostics = &viewmodel.Diagnostics{RunAt: &run, Runner: viewmodel.SourceState{State: viewmodel.SourceLive}, Incidents: []viewmodel.Incident{{
		Root: viewmodel.Finding{
			Check: "evil`check", Severity: viewmodel.SeverityCritical, Summary: evil, Relation: evil,
			Evidence: []viewmodel.Evidence{{Origin: evil, Object: "Pod/``x``"}},
		},
	}}}
	got := RenderIncidentSummary(in)

	for _, bad := range []string{
		"@here", "@channel", "\n- injected", "\n# heading", "hunter2", "http://", "www.evil", "\u2028", "\u202E",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("the summary carries %q:\n%s", bad, got)
		}
	}
	// Outside code spans, every character that opens an inline construct
	// is escaped.
	if c, ok := unescapedOutsideCode(got); ok {
		t.Errorf("an unescaped %q outside a code span:\n%s", c, got)
	}
	// Every line is one the renderer wrote: none starts with a construct
	// of the untrusted text.
	for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "**") && !strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "  - ") {
			t.Errorf("a line the renderer did not start: %q", l)
		}
	}
	// A code span is fenced longer than any backtick run inside it.
	if !strings.Contains(got, "``ledger`[x](http:/\u2060/evil)``") {
		t.Errorf("the Cluster name is not fenced as code:\n%s", got)
	}
	if !strings.Contains(got, "``` Pod/``x`` ```") {
		t.Errorf("an object name with backtick runs is not fenced as code:\n%s", got)
	}
	if !strings.Contains(got, `\[redacted\]`) {
		t.Errorf("the credential was not redacted:\n%s", got)
	}
}

// unescapedOutsideCode finds an inline-construct character the renderer
// did not write and did not escape: outside the code spans, and outside
// the "**" its bold lines start with.
func unescapedOutsideCode(md string) (rune, bool) {
	for _, l := range strings.Split(md, "\n") {
		if rest, ok := strings.CutPrefix(l, "**"); ok {
			if j := strings.LastIndex(rest, "**"); j >= 0 {
				l = rest[:j] + rest[j+2:]
			}
		}
		for i := 0; i < len(l); i++ {
			switch c := l[i]; c {
			case '\\':
				i++
			case '`':
				n := 1
				for i+n < len(l) && l[i+n] == '`' {
					n++
				}
				fence := strings.Repeat("`", n)
				end := strings.Index(l[i+n:], fence)
				if end < 0 {
					return '`', true
				}
				i += n + end + n - 1
			case '[', ']', '<', '>', '*', '_', '~', '&':
				return rune(c), true
			}
		}
	}
	return 0, false
}

func TestMdText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"plain sentence.", "plain sentence."},
		{"a_b*c", `a\_b\*c`},
		{"x\n\n  y\tz", "x y z"},
		{"  lead and trail  ", "lead and trail"},
		{"mail ops@example.com", "mail ops@\u2060example.com"},
		{"https://x", "https:/\u2060/x"},
		{"zero\u200bwidth", "zerowidth"},
		{strings.Repeat("a", maxSummaryText+5), strings.Repeat("a", maxSummaryText) + "…"},
	} {
		if got := mdText(tc.in); got != tc.want {
			t.Errorf("mdText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A no-break or zero-width character inside a credential does not hide it
// from redaction: the value is folded before it is redacted.
func TestMdTextRedactsAfterFolding(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"pass\u200bword=hunter2",
		"password=\u00a0hunter2",
		"PASSWORD\u2060 'hunter2'",
	} {
		if got := mdText(in); strings.Contains(got, "hunter2") {
			t.Errorf("mdText(%q) = %q: the credential got through", in, got)
		}
	}
}

func TestMdCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"pods", "`pods`"},
		{"a`b", "``a`b``"},
		{"`a", "`` `a ``"},
		{"", "(none)"},
	} {
		if got := mdCode(tc.in); got != tc.want {
			t.Errorf("mdCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClusterPageURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ origin, want string }{
		{"https://console.example.com", "https://console.example.com/c/team-a/orders"},
		{"https://console.example.com/", "https://console.example.com/c/team-a/orders"},
		{"http://localhost:8080", "http://localhost:8080/c/team-a/orders"},
		{"", ""},
		{"javascript:alert(1)", ""},
		{"https://user:pw@console.example.com", ""},
		{"https://console.example.com/?q=1", ""},
		{"//console.example.com", ""},
	} {
		if got := clusterPageURL(tc.origin, "team-a", "orders"); got != tc.want {
			t.Errorf("clusterPageURL(%q) = %q, want %q", tc.origin, got, tc.want)
		}
	}
}

func TestAgoText(t *testing.T) {
	t.Parallel()
	now := fixtures.Now
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	for _, tc := range []struct {
		at   *time.Time
		want string
	}{
		{nil, "at an unknown time"},
		{at(-time.Second), "just now"},
		{at(15 * time.Second), "15s ago"},
		{at(4 * time.Minute), "4m ago"},
		{at(100 * time.Second), "1m40s ago"},
		{at(26 * time.Hour), "26h ago"},
		{at(5*time.Hour + 3*time.Minute), "5h3m ago"},
		{at(9 * 24 * time.Hour), "9d ago"},
	} {
		if got := agoText(now, tc.at); got != tc.want {
			t.Errorf("agoText(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
}

// The route serves Markdown with the console page under PUBLIC_URL.
func TestClusterSummaryRoute(t *testing.T) {
	t.Parallel()
	h, _ := New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}, PublicURL: "https://console.example.com", Now: func() time.Time { return fixtures.Now }})
	w := call(t, h, FixturePrincipal(), "/api/v1/namespaces/team-a/clusters/billing/summary")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		"**Cluster `billing` in namespace `team-a`: critical**",
		"- Console: https://console.example.com/c/team-a/billing",
		"- Rendered: 2026-10-29T12:00:00Z (UTC)",
		"critical · `pod-crashloop` · Instance billing-1 is restarting in a loop.",
		"- `pods`: stale since",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the summary lacks %q:\n%s", want, body)
		}
	}
}
