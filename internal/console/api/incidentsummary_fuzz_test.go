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
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// FuzzIncidentSummary puts one untrusted string into every field of the
// summary that comes from the cluster, as prose and as an identifier, and
// holds the rendering to the invariants of the escaping test whatever the
// bytes: every line is one the renderer started (a bold line or a list
// item), no inline construct is left unescaped outside a code span, no
// mention survives ("@" is always followed by the word joiner), no format
// character but the renderer's own word joiner is emitted, and a code span
// is fenced longer than any backtick run inside it.
func FuzzIncidentSummary(f *testing.F) {
	for _, seed := range []string{
		"",
		"plain",
		"see [x](http://evil.example) @here @channel <!here> `rm -rf` **bold** ~~gone~~ &amp; <img src=x>",
		"a\n- injected item\r\n# heading\u2028line\u202Edesrever password=hunter2 www.evil.example",
		"`", "``", "```a```", "`a", "a`", "\\", "\\`", "\\\\`", "@", "@@here", "://", "www.",
		"\u2060@\u200b", "\x00\xff\xfe", "    code", "> quote", "1. item", "***", "---", "|a|b|",
		strings.Repeat("`", 400), strings.Repeat("a", 301) + "`", strings.Repeat("\\", 301),
	} {
		f.Add(seed, seed)
	}
	f.Fuzz(func(t *testing.T, prose, ident string) {
		run := fixtures.Now.Add(-10 * time.Second)
		since := fixtures.Now.Add(-5 * time.Second)
		in := IncidentSummary{
			Overview: viewmodel.ClusterOverview{
				Ref:         viewmodel.ClusterRef{Namespace: ident, Name: ident},
				GeneratedAt: fixtures.Now, Present: true,
				Phase: viewmodel.Phase{Text: prose},
				Verdict: viewmodel.Verdict{Health: viewmodel.Health(ident), Contributors: []viewmodel.Contributor{{
					ID: "x", Health: viewmodel.HealthWarning, Label: prose, Detail: prose,
					Provenance: viewmodel.Provenance{Origin: viewmodel.Origin(prose), Source: prose, Stale: true, StaleReason: prose},
				}}},
				Sources: []viewmodel.SourceState{{
					Source: ident, State: viewmodel.SourceStateKind(prose), Since: &since, Reason: prose,
				}},
			},
			Diagnostics: &viewmodel.Diagnostics{RunAt: &run, Runner: viewmodel.SourceState{State: viewmodel.SourceStateKind(prose)}, Incidents: []viewmodel.Incident{{
				Root: viewmodel.Finding{
					Check: ident, Severity: viewmodel.Severity(prose), Summary: prose, Relation: prose, At: &run,
					Evidence: []viewmodel.Evidence{{Origin: prose, Object: ident}},
				},
				Consequences: []viewmodel.Finding{{Check: ident, Severity: viewmodel.SeverityNote, Summary: prose, Relation: prose}},
			}}},
			Now: fixtures.Now,
		}
		got := RenderIncidentSummary(in)

		for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			if l != "" && !strings.HasPrefix(l, "**") && !strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "  - ") {
				t.Fatalf("a line the renderer did not start: %q\nin:\n%s", l, got)
			}
		}
		if c, ok := unescapedOutsideCode(got); ok {
			t.Fatalf("an unescaped %q outside a code span:\n%s", c, got)
		}
		for i, r := range got {
			if r == '@' && !strings.HasPrefix(got[i+1:], wordJoiner) {
				t.Fatalf("a bare @ at %d:\n%s", i, got)
			}
			if r != '\n' && unicode.IsControl(r) {
				t.Fatalf("control character %U in:\n%s", r, got)
			}
			if unicode.Is(unicode.Cf, r) && string(r) != wordJoiner {
				t.Fatalf("format character %U in:\n%s", r, got)
			}
		}
		if strings.Contains(got, "://") || strings.Contains(strings.ToLower(got), "www.") {
			// Only inside a code span could a URL stay whole, and mdClean breaks
			// them before the span is built.
			t.Fatalf("a bare URL start survived:\n%s", got)
		}

		span := mdCode(ident)
		fence := span[:len(span)-len(strings.TrimLeft(span, "`"))]
		if span != "(none)" {
			if fence == "" || !strings.HasSuffix(span, fence) || len(span) < 2*len(fence) {
				t.Fatalf("mdCode(%q) = %q is not fenced", ident, span)
			}
			inner := span[len(fence) : len(span)-len(fence)]
			if strings.Contains(inner, fence) {
				t.Fatalf("mdCode(%q) = %q holds a run as long as its fence", ident, span)
			}
		}
	})
}
