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

package diagnose

import (
	"fmt"
	"sort"
	"time"
)

// Lifecycle is the end-of-life table: for each component, the release
// lines it knows and the day each leaves upstream support. It is data,
// not code: the console ships a built-in table and a deployment may
// supply its own (a ConfigMap), so a support boundary moves without a
// release. Each line carries its own date and the rule compares that date
// with the run's clock, so the table does not go stale as time passes:
// it only needs a new line when a component releases one.
type Lifecycle struct {
	// Reviewed is the day, as YYYY-MM-DD, the table was last checked
	// against the projects' own support pages.
	Reviewed string `json:"reviewed"`
	// Source says where this table came from, quoted under findings:
	// "built-in" or the ConfigMap that supplied it.
	Source string `json:"-"`
	// Lines are the release lines per component, any order.
	Lines map[Component][]ReleaseLine `json:"lines"`
}

// ReleaseLine is one release line ("14", "1.34", "0.15") and the day it
// leaves upstream support.
type ReleaseLine struct {
	Line string `json:"line"`
	// EOL is the YYYY-MM-DD the line leaves support; empty when no date
	// is announced yet.
	EOL string `json:"eol,omitempty"`
}

// LineStatus is where one observed version stands in the table.
type LineStatus struct {
	// Listed is false when the version's line is not in the table and is
	// newer than every listed line: nothing is claimed about it.
	Listed bool
	// Line is the matching line; for a version older than every listed
	// line, the oldest one.
	Line string
	// EOL is the day support ends, when one is known.
	EOL    time.Time
	HasEOL bool
	// Older is true for a version older than every listed line: it is
	// taken to be out of support, like the oldest line.
	Older bool
}

// Past reports whether support has ended at now.
func (s LineStatus) Past(now time.Time) bool {
	return s.Listed && (s.Older || (s.HasEOL && !now.Before(s.EOL)))
}

// Status places a version in the table. A line matches on its own number
// of fields: "1.34" matches "1.34.2", "14" matches "14.11".
func (l Lifecycle) Status(c Component, version string) LineStatus {
	v, ok := parseVersion(version)
	if !ok {
		return LineStatus{}
	}
	type parsed struct {
		fields []int
		line   ReleaseLine
	}
	var lines []parsed
	for _, rl := range l.Lines[c] {
		if f, ok := parseVersion(rl.Line); ok {
			lines = append(lines, parsed{f, rl})
		}
	}
	if len(lines) == 0 {
		return LineStatus{}
	}
	sort.Slice(lines, func(i, j int) bool { return compareVersions(lines[i].fields, lines[j].fields) < 0 })
	status := func(p parsed) LineStatus {
		s := LineStatus{Listed: true, Line: p.line.Line}
		if t, err := time.Parse(time.DateOnly, p.line.EOL); err == nil {
			s.EOL, s.HasEOL = t, true
		}
		return s
	}
	for _, p := range lines {
		if len(v) >= len(p.fields) && compareVersions(v[:len(p.fields)], p.fields) == 0 {
			return status(p)
		}
	}
	if oldest := lines[0]; compareVersions(v, oldest.fields) < 0 {
		s := status(oldest)
		s.Older = true
		return s
	}
	return LineStatus{}
}

// EndOfLife matches when the observed version of Component has left
// upstream support, or, with Within, will leave it inside that window
// (and has not yet).
type EndOfLife struct {
	Component Component
	Within    time.Duration
}

func (c EndOfLife) describe() string {
	if c.Within > 0 {
		return fmt.Sprintf("the %s release line leaves upstream support within %d days, per the lifecycle table",
			c.Component, int(c.Within.Hours()/24))
	}
	return fmt.Sprintf("the %s release line is past upstream end of life, per the lifecycle table", c.Component)
}

func (c EndOfLife) evaluate(_ string, in Input) ([]conditionMatch, string) {
	observed, known := versionFacts(in)[c.Component]
	if !known {
		return nil, fmt.Sprintf("the %s version is not observed", c.Component)
	}
	if len(in.Lifecycle.Lines[c.Component]) == 0 {
		return nil, fmt.Sprintf("the lifecycle table lists no %s release lines", c.Component)
	}
	s := in.Lifecycle.Status(c.Component, observed.Version)
	table := Evidence{Origin: "console-pinned", Object: "lifecycle table (" + in.Lifecycle.Source + ")"}
	reviewed := ""
	if in.Lifecycle.Reviewed != "" {
		reviewed = "; table reviewed " + in.Lifecycle.Reviewed
	}
	switch {
	case !s.Listed:
		return nil, ""
	case c.Within == 0 && s.Past(in.Now):
		if s.Older {
			table.Detail = fmt.Sprintf("%s %s is older than every line listed, the oldest being %s%s",
				c.Component, observed.Version, s.Line, reviewed)
		} else {
			table.Detail = fmt.Sprintf("%s %s left upstream support on %s%s",
				c.Component, s.Line, s.EOL.Format(time.DateOnly), reviewed)
		}
	case c.Within > 0 && s.HasEOL && !s.Past(in.Now) && s.EOL.Sub(in.Now) <= c.Within:
		days := int(s.EOL.Sub(in.Now).Hours()/24) + 1
		table.Detail = fmt.Sprintf("%s %s leaves upstream support on %s, in %d days%s",
			c.Component, s.Line, s.EOL.Format(time.DateOnly), days, reviewed)
	default:
		return nil, ""
	}
	return []conditionMatch{{evidence: []Evidence{observed.evidence(), table}}}, ""
}

// ParseLine reports whether a release line is dotted numerals ("1.34").
func ParseLine(line string) ([]int, bool) {
	f, ok := parseVersion(line)
	if !ok || joinVersion(f) != line {
		return nil, false
	}
	return f, true
}
