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
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// maxSummaryFindings bounds the findings an incident summary lists; the
// rest are counted and left to the diagnostics screen.
const maxSummaryFindings = 25

// maxSummaryText bounds each value an incident summary quotes, in runes.
const maxSummaryText = 300

// IncidentSummary is what the incident summary renders: the overview,
// the diagnostics run (nil when it could not be read), the console page
// of the Cluster ("" when the console's origin is not known) and the
// rendering instant.
type IncidentSummary struct {
	Overview    viewmodel.ClusterOverview
	Diagnostics *viewmodel.Diagnostics
	ConsoleURL  string
	Now         time.Time
}

// RenderIncidentSummary renders s as Markdown for an incident channel,
// in the words of the overview and diagnostics screens. Every value
// that comes from the cluster (names, operator and Kubernetes text,
// finding sentences, source reasons) is redacted (pkg/redact), folded
// onto one line, bounded, stripped of format characters and escaped,
// so it can neither open a link, a mention or a block nor carry a
// credential into the channel.
func RenderIncidentSummary(s IncidentSummary) string {
	ov := s.Overview
	now := s.Now.UTC()
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}

	line("**Cluster %s in namespace %s: %s**", mdCode(ov.Ref.Name), mdCode(ov.Ref.Namespace), healthWord(ov.Verdict.Health))
	line("")
	verdict := healthWord(ov.Verdict.Health)
	if ov.Verdict.Stale {
		verdict += " (rests on a stale source)"
	}
	line("- Verdict: %s", verdict)
	if !ov.Present {
		line("- The Cluster no longer exists: this is its last observation.")
	}
	if ov.Phase.Text != "" {
		line("- Phase: %s", mdText(ov.Phase.Text))
	}
	if s.ConsoleURL != "" {
		line("- Console: %s", s.ConsoleURL)
	}
	line("- Rendered: %s (UTC); the view was assembled %s", now.Format(time.RFC3339), agoText(now, &ov.GeneratedAt))

	var notOK []viewmodel.Contributor
	for _, c := range ov.Verdict.Contributors {
		if c.Health != viewmodel.HealthOK {
			notOK = append(notOK, c)
		}
	}
	if len(notOK) > 0 {
		line("")
		line("**Why**")
		for _, c := range notOK {
			text := mdText(c.Label)
			if c.Detail != "" {
				text += ": " + mdText(c.Detail)
			}
			line("- %s · %s (%s)", healthWord(c.Health), text, provenanceText(now, c.Provenance))
		}
	}

	line("")
	writeFindings(&b, now, s.Diagnostics)

	var off []viewmodel.SourceState
	for _, src := range ov.Sources {
		if src.State != viewmodel.SourceLive {
			off = append(off, src)
		}
	}
	line("")
	switch {
	case len(ov.Sources) == 0:
		line("**Sources**: none reported; nothing on this page is confirmed.")
	case len(off) == 0:
		line("**Sources**: all %d live.", len(ov.Sources))
	default:
		line("**Sources not live** (what rests on them is unknown, not healthy)")
		for _, src := range off {
			text := fmt.Sprintf("- %s: %s", mdCode(src.Source), sourceWord(src.State))
			if src.Since != nil {
				text += " since " + src.Since.UTC().Format(time.RFC3339) + " (" + agoText(now, src.Since) + ")"
			}
			if src.Reason != "" {
				text += ": " + mdText(src.Reason)
			}
			line("%s", text)
		}
	}
	return b.String()
}

// writeFindings renders the diagnostics part: each finding of the run,
// worst first, or why there is none to show.
func writeFindings(b *strings.Builder, now time.Time, d *viewmodel.Diagnostics) {
	line := func(format string, args ...any) {
		fmt.Fprintf(b, format, args...)
		b.WriteByte('\n')
	}
	switch {
	case d == nil:
		line("**Findings**: the diagnostics could not be read; no finding is not a clean bill of health.")
		return
	case d.RunAt == nil:
		line("**Findings**: diagnostics have not run yet (%s); no finding is not a clean bill of health.", sourceWord(d.Runner.State))
		return
	}
	run := fmt.Sprintf("diagnostics run %s, %s", d.RunAt.UTC().Format(time.RFC3339), agoText(now, d.RunAt))
	if d.Runner.State != viewmodel.SourceLive {
		run += "; the runner is " + sourceWord(d.Runner.State)
	}
	if n := d.Counts.Unavailable; n > 0 {
		run += fmt.Sprintf("; %d %s could not run", n, plural(n, "check", "checks"))
	}
	var findings []string
	for _, inc := range d.Incidents {
		findings = append(findings, "- "+findingText(now, inc.Root))
		for _, c := range inc.Consequences {
			findings = append(findings, "  - "+findingText(now, c))
		}
	}
	if len(findings) == 0 {
		line("**Findings**: none (%s).", run)
		return
	}
	line("**Findings** (%s)", run)
	shown := findings
	if len(shown) > maxSummaryFindings {
		shown = shown[:maxSummaryFindings]
	}
	for _, f := range shown {
		line("%s", f)
	}
	if rest := len(findings) - len(shown); rest > 0 {
		line("- and %d more on the diagnostics screen", rest)
	}
}

// findingText is one finding: severity, check, its sentence, and who
// made the claim it rests on with how long it has been reported.
func findingText(now time.Time, f viewmodel.Finding) string {
	text := fmt.Sprintf("%s · %s · %s", severityWord(f.Severity), mdCode(f.Check), mdText(f.Summary))
	if f.Relation != "" {
		text += " (follows: " + mdText(f.Relation) + ")"
	}
	sources := make([]string, 0, len(f.Evidence))
	for _, e := range f.Evidence {
		s := mdText(e.Origin)
		if e.Object != "" {
			s += " " + mdCode(e.Object)
		}
		sources = append(sources, s)
	}
	if len(sources) > 0 {
		text += " Source: " + strings.Join(sources, ", ") + "."
	}
	switch {
	case f.Since != nil:
		text += " Reported since " + f.Since.UTC().Format(time.RFC3339) + " (" + agoText(now, f.Since) + ")."
	case f.At != nil:
		text += " Observed " + f.At.UTC().Format(time.RFC3339) + " (" + agoText(now, f.At) + ")."
	}
	return text
}

// provenanceText names who made a claim, where it was read and its age.
func provenanceText(now time.Time, p viewmodel.Provenance) string {
	parts := []string{mdText(string(p.Origin))}
	if p.Source != "" {
		parts = append(parts, mdText(p.Source))
	}
	if p.ObservedAt != nil {
		parts = append(parts, "read "+agoText(now, p.ObservedAt))
	} else {
		parts = append(parts, "never read")
	}
	if p.Stale {
		stale := "stale"
		if p.StaleReason != "" {
			stale += ": " + mdText(p.StaleReason)
		}
		parts = append(parts, stale)
	}
	return strings.Join(parts, ", ")
}

// agoText is the age of at relative to now, as "12s ago".
func agoText(now time.Time, at *time.Time) string {
	if at == nil || at.IsZero() {
		return "at an unknown time"
	}
	d := now.Sub(*at)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		m, s := int(d/time.Minute), int(d%time.Minute/time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm ago", m)
		}
		return fmt.Sprintf("%dm%ds ago", m, s)
	case d < 48*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh ago", h)
		}
		return fmt.Sprintf("%dh%dm ago", h, m)
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// healthWord is the screen's word for a health state (ui/messages en).
func healthWord(h viewmodel.Health) string {
	switch h {
	case viewmodel.HealthProgressing:
		return "in progress"
	case "":
		return "unknown"
	default:
		return mdText(string(h))
	}
}

// severityWord is the screen's word for a severity.
func severityWord(s viewmodel.Severity) string {
	if s == "" {
		return "unknown"
	}
	return mdText(string(s))
}

// sourceWord is the screen's word for a source state (ui sources.ts).
func sourceWord(s viewmodel.SourceStateKind) string {
	switch s {
	case viewmodel.SourceLive:
		return "live"
	case viewmodel.SourcePending:
		return "starting"
	case viewmodel.SourceStale:
		return "stale"
	case viewmodel.SourceUnavailable:
		return "unavailable"
	case viewmodel.SourceForbidden:
		return "not permitted to the console"
	case viewmodel.SourceNotInstalled:
		return "not installed"
	case viewmodel.SourceDisabled:
		return "switched off"
	default:
		return mdText(string(s))
	}
}

// wordJoiner is U+2060: invisible, it keeps a chat client from reading
// "@here" as a mention or "http://" as the start of a link.
const wordJoiner = "\u2060"

var (
	autolinkStart = regexp.MustCompile(`(?i)(://|www\.)`)
	mdSpecial     = strings.NewReplacer(
		`\`, `\\`, "`", "\\`", `*`, `\*`, `_`, `\_`, `[`, `\[`, `]`, `\]`,
		`<`, `\<`, `>`, `\>`, `~`, `\~`, `&`, `\&`,
	)
)

// mdClean is the boundary every quoted value crosses: every line break,
// control and format character (bidi overrides, zero-width characters)
// folded into one space, then credentials removed (after the folding, so
// a no-break or zero-width character inside a credential cannot hide it
// from redaction), then bounded, and mentions and bare-URL autolinks
// broken by a word joiner.
func mdClean(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError, unicode.Is(unicode.Cf, r):
			continue
		case unicode.IsSpace(r), unicode.IsControl(r): // U+2028 and U+2029 are spaces
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	out := redact.Text(b.String())
	if runes := []rune(out); len(runes) > maxSummaryText {
		out = string(runes[:maxSummaryText]) + "…"
	}
	out = strings.ReplaceAll(out, "@", "@"+wordJoiner)
	return autolinkStart.ReplaceAllStringFunc(out, func(m string) string {
		return m[:len(m)-1] + wordJoiner + m[len(m)-1:]
	})
}

// mdText is an untrusted value as Markdown inline text: cleaned, then
// every character that opens an inline construct (emphasis, code, link,
// image, autolink, raw HTML, strikethrough, entity) backslash-escaped.
// Line-start constructs cannot occur: the value never starts a line.
func mdText(s string) string {
	return mdSpecial.Replace(mdClean(s))
}

// mdCode is an untrusted identifier as a Markdown code span, whose
// content no renderer interprets: cleaned, then fenced with one more
// backtick than its longest backtick run.
func mdCode(s string) string {
	s = mdClean(s)
	if s == "" {
		return "(none)"
	}
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// clusterPageURL is the console page of a Cluster under origin, or ""
// when origin is not an absolute http(s) URL.
func clusterPageURL(origin, namespace, name string) string {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return strings.TrimSuffix(u.String(), "/") + "/c/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)
}
