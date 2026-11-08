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

// Package triage walks a cluster's diagnostics from the symptom an
// operator arrives with, rather than from whatever happened to match.
//
// The diagnostics screen answers "what matched"; it is signal-driven,
// and an operator whose cluster refuses connections still has to find
// the finding that explains it among the others, or notice that none
// does. A playbook is the other direction: one symptom, an ordered list
// of questions in the order the upstream troubleshooting guide asks
// them, and each question answered by the checks that already ran —
// never by a check of its own. A playbook adds no observation and no
// judgement to the catalog; it is a reading order over it, written as
// data, so the two can never disagree about a fact.
//
// Every step answers one of five ways, and the honesty rules of the
// catalog carry through unchanged: a step is found when a check behind
// it matched, ruled out when every check behind it ran and cleared,
// unknown when one could not run, and switched off or inapplicable
// when the checks behind it were. A step with nothing behind it — a
// question the console cannot observe, such as a NetworkPolicy — says
// so and hands the reader the command that would answer it.
package triage

import (
	"fmt"
	"strings"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// Outcome is how one step answered.
type Outcome string

const (
	// OutcomeFound means a check behind the step matched, or its fact
	// answered yes: this is a cause worth reading first.
	OutcomeFound Outcome = "found"
	// OutcomeRuledOut means every check behind the step ran and found
	// nothing — which rules out exactly what those checks describe.
	OutcomeRuledOut Outcome = "ruled out"
	// OutcomeUnknown means a check behind the step could not run, and
	// none matched: the step rules nothing out.
	OutcomeUnknown Outcome = "could not be judged"
	// OutcomeOff means every check behind the step needs a source this
	// deployment has switched off.
	OutcomeOff Outcome = "needs a switched-off source"
	// OutcomeNotApplicable means every check behind the step is pinned
	// to versions other than the observed ones.
	OutcomeNotApplicable Outcome = "does not apply"
	// OutcomeAsk means the console observes nothing that answers the
	// step: the reader has to look.
	OutcomeAsk Outcome = "not observable here"
)

// Fact is a question answered from the snapshots directly rather than
// through a check: an absence the catalog has no rule for, such as no
// primary being named at all. Like a condition, it says when it cannot
// answer.
type Fact interface {
	// Describe states the question in the console's words.
	Describe() string
	// Answer reports yes with the evidence it rests on, no, or the
	// reason it cannot tell.
	Answer(in diagnose.Input) (yes bool, evidence []diagnose.Evidence, unknown string)
}

// Step is one question in a playbook.
type Step struct {
	// Question is the step as the reader sees it.
	Question string
	// Checks are the check IDs whose match answers the question yes.
	// Every name must be a check the catalog or the detectors declare;
	// the tests refuse one that is not.
	Checks []string
	// Fact, when set, is asked before the checks.
	Fact Fact
	// Explain says what a found step means for this symptom.
	Explain string
	// Yourself is what to run when the console cannot tell: the
	// upstream guide's own command, for a step that is unknown, off,
	// or unobservable.
	Yourself string
}

// Playbook is one symptom and its reading order.
type Playbook struct {
	// ID is the stable identifier, used in links.
	ID string
	// Symptom is what the reader is seeing, in their words.
	Symptom string
	// Describes says what the playbook walks through.
	Describes string
	// Upstream is the upstream guide page the order follows.
	Upstream string
	// Steps are asked in order; every one is answered, and the first
	// found is where to start.
	Steps []Step
}

// StepResult is one step answered.
type StepResult struct {
	Step
	Outcome Outcome
	// Findings are the matched findings behind a found step, most
	// severe first as the run ordered them.
	Findings []diagnose.Finding
	// Evidence is a found fact's own evidence.
	Evidence []diagnose.Evidence
	// Because accounts for the outcome in words: which checks cleared,
	// which could not run and why, which were off.
	Because string
}

// Walk is one playbook answered against one run.
type Walk struct {
	Playbook
	Steps []StepResult
	// StartAt is the index of the first found step, or -1 when none.
	StartAt int
}

// Evaluate answers every step of the playbook from the run's checks
// and findings and the input they were computed from. It runs no
// check of its own.
func Evaluate(playbook Playbook, in diagnose.Input, result diagnose.Result) Walk {
	checks := map[string]diagnose.Check{}
	for _, check := range result.Checks {
		checks[check.Name] = check
	}
	findings := map[string][]diagnose.Finding{}
	for _, finding := range result.Findings {
		findings[finding.Check] = append(findings[finding.Check], finding)
	}
	walk := Walk{Playbook: playbook, StartAt: -1}
	for _, step := range playbook.Steps {
		answered := answer(step, in, checks, findings)
		if answered.Outcome == OutcomeFound && walk.StartAt < 0 {
			walk.StartAt = len(walk.Steps)
		}
		walk.Steps = append(walk.Steps, answered)
	}
	return walk
}

// answer resolves one step: the fact first, then the checks.
func answer(step Step, in diagnose.Input, checks map[string]diagnose.Check, findings map[string][]diagnose.Finding) StepResult {
	out := StepResult{Step: step}
	if step.Fact != nil {
		yes, evidence, unknown := step.Fact.Answer(in)
		switch {
		case unknown != "":
			out.Outcome, out.Because = OutcomeUnknown, unknown
			return out
		case yes:
			out.Outcome, out.Evidence = OutcomeFound, evidence
			return out
		}
	}
	if len(step.Checks) == 0 {
		if step.Fact != nil {
			out.Outcome, out.Because = OutcomeRuledOut, step.Fact.Describe()+": no"
			return out
		}
		out.Outcome = OutcomeAsk
		return out
	}
	var matched, clear, off, na []string
	var unavailable []string
	for _, name := range step.Checks {
		check, known := checks[name]
		if !known {
			// A check the run did not carry is a catalog drift the
			// tests catch; at run time it is honestly unjudgeable.
			unavailable = append(unavailable, name+": not in this run")
			continue
		}
		switch check.Outcome {
		case diagnose.CheckMatched:
			matched = append(matched, name)
			out.Findings = append(out.Findings, findings[name]...)
		case diagnose.CheckClear:
			clear = append(clear, name)
		case diagnose.CheckNotApplicable:
			na = append(na, name)
		case diagnose.CheckUnavailable:
			if check.SourceOff {
				off = append(off, name)
			} else {
				unavailable = append(unavailable, name+": "+check.Because)
			}
		}
	}
	var because []string
	switch {
	case len(matched) > 0:
		out.Outcome = OutcomeFound
		because = append(because, "matched: "+strings.Join(matched, ", "))
	case len(unavailable) > 0:
		out.Outcome = OutcomeUnknown
		because = append(because, "could not run — "+strings.Join(unavailable, "; "))
	case len(clear) > 0 && len(off) > 0:
		// A clear beside a switched-off check is not every check having
		// run: the step rules out only what the clear ones describe, and
		// says so rather than reading as settled.
		out.Outcome = OutcomeUnknown
		because = append(because, "clear: "+strings.Join(clear, ", "))
	case len(clear) > 0:
		out.Outcome = OutcomeRuledOut
		because = append(because, "clear: "+strings.Join(clear, ", "))
	case len(off) > 0:
		out.Outcome = OutcomeOff
	default:
		out.Outcome = OutcomeNotApplicable
	}
	if len(off) > 0 && out.Outcome != OutcomeOff {
		because = append(because, "could not be judged in full — switched off: "+strings.Join(off, ", "))
	} else if len(off) > 0 {
		because = append(because, "every check here needs a switched-off source: "+strings.Join(off, ", "))
	}
	if len(na) > 0 && out.Outcome != OutcomeNotApplicable {
		because = append(because, "do not apply to the observed versions: "+strings.Join(na, ", "))
	} else if len(na) > 0 {
		because = append(because, "pinned to other versions: "+strings.Join(na, ", "))
	}
	if step.Fact != nil && out.Outcome == OutcomeRuledOut {
		because = append([]string{step.Fact.Describe() + ": no"}, because...)
	}
	out.Because = strings.Join(because, ". ")
	return out
}

// String renders a step for logs and tests.
func (s StepResult) String() string {
	return fmt.Sprintf("%s → %s (%s)", s.Question, s.Outcome, s.Because)
}
