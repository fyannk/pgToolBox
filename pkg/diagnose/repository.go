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
	"strings"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/evidence"
)

// RepositoryAspect names one typed state the evidence agent
// reports about the backup repository, whichever its format
// (barman-cloud or pgBackRest): the rules read the format-neutral facts.
type RepositoryAspect string

// The aspects the evidence agent reports as four-state facts.
const (
	// RepositoryWAL is the archived WAL sequence's continuity.
	RepositoryWAL RepositoryAspect = "WAL continuity"
	// RepositoryCoverage is the observed recovery coverage.
	RepositoryCoverage RepositoryAspect = "recovery coverage"
	// RepositoryRetention is the retention comparison.
	RepositoryRetention RepositoryAspect = "retention"
)

// RepositoryState matches when the evidence agent reports
// one aspect of the repository in one of the given states. It restates
// the evidence agent's typed state and stable reason code and nothing else:
// the evidence agent's free-text diagnostics never reach this package, and the
// finding never merges the observation into an operator claim or says
// what the repository would or would not restore.
type RepositoryState struct {
	Aspect RepositoryAspect
	// States are the evidence agent's states that match; "unhealthy" alone when
	// empty.
	States []string
}

func (c RepositoryState) describe() string {
	states := c.States
	if len(states) == 0 {
		states = []string{"unhealthy"}
	}
	described := "the evidence agent reporting " + string(c.Aspect) + " as " + states[0]
	var describedSb56 strings.Builder
	for _, state := range states[1:] {
		describedSb56.WriteString(" or " + state)
	}
	described += describedSb56.String()
	return described
}

// fact picks the aspect's state from a recognised report. reason is set
// when the aspect cannot be read: the catalog names one this console
// does not know, or the repository's format does not report it.
func (c RepositoryAspect) fact(facts *evidence.RepositoryFacts) (fact evidence.StateFact, reason string) {
	switch c {
	case RepositoryWAL:
		return facts.WAL, ""
	case RepositoryCoverage:
		return facts.Coverage, ""
	case RepositoryRetention:
		if facts.Retention == nil {
			return evidence.StateFact{}, "the evidence agent reports no retention comparison for a " + facts.Format + " repository"
		}
		return facts.Retention.Result, ""
	}
	return evidence.StateFact{}, "the catalog names a repository aspect this console does not read"
}

func (c RepositoryState) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := evidenceUnavailable(in); reason != "" {
		return nil, reason
	}
	report := in.Evidence.Snapshot.Report
	fact, reason := c.Aspect.fact(report.Facts)
	if reason != "" {
		return nil, reason
	}
	states := c.States
	if len(states) == 0 {
		states = []string{"unhealthy"}
	}
	matched := false
	for _, state := range states {
		if fact.State == state {
			matched = true
			break
		}
	}
	if !matched {
		return nil, ""
	}
	detail := fmt.Sprintf("state %s, reason code %s; evidence generation %d", fact.State, fact.Code, report.EvidenceGeneration)
	if report.CompletedAt != nil {
		detail += ", scan completed " + report.CompletedAt.UTC().Format("2006-01-02 15:04:05Z")
	}
	if report.Completeness == "incomplete" {
		detail += " (the scan is incomplete, so the state rests on what was examined)"
	}
	match := conditionMatch{
		subject: EntityRef{Kind: "Repository", Name: report.ScopeName},
		evidence: []Evidence{{
			Origin: "repository-evidence",
			Object: "repository " + string(c.Aspect),
			Detail: detail,
		}},
		link:      "/backups/evidence",
		linkLabel: "Repository evidence",
	}
	if report.CompletedAt != nil {
		match.at = *report.CompletedAt
	}
	return []conditionMatch{match}, ""
}

// RepositoryFinding matches when the evidence agent raised a
// finding of the given code (pgBackRest's reader raises them: StanzaSplit,
// ChecksumMismatch, ...). It restates the code, severity and subject; the
// evidence agent's message stays in the evidence agent.
type RepositoryFinding struct {
	Code string
}

func (c RepositoryFinding) describe() string {
	return "the evidence agent raising the " + c.Code + " finding"
}

func (c RepositoryFinding) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := evidenceUnavailable(in); reason != "" {
		return nil, reason
	}
	report := in.Evidence.Snapshot.Report
	var matches []conditionMatch
	for _, finding := range report.Facts.Findings {
		if finding.Code != c.Code {
			continue
		}
		detail := fmt.Sprintf("finding %s (%s) on %s; evidence generation %d", finding.Code, finding.Severity, finding.Subject, report.EvidenceGeneration)
		match := conditionMatch{
			subject: EntityRef{Kind: "Repository", Name: report.ScopeName},
			evidence: []Evidence{{
				Origin: "repository-evidence",
				Object: "repository finding " + finding.Code,
				Detail: detail,
			}},
			link:      "/backups/evidence",
			linkLabel: "Repository evidence",
		}
		if report.CompletedAt != nil {
			match.at = *report.CompletedAt
		}
		matches = append(matches, match)
	}
	if len(matches) == 0 && report.Facts.FindingsTruncated {
		return nil, "the evidence agent cut its findings list, so the absence of " + c.Code + " is unknown"
	}
	return matches, ""
}
