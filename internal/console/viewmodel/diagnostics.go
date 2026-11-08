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

package viewmodel

import "time"

// Diagnostics is the /diagnostics screen: the latest background run of
// pkg/diagnose for one Cluster. Checks are the half that keeps the
// screen honest — they turn an empty finding list into "these checks
// were looked at, and these could not run".
type Diagnostics struct {
	Ref ClusterRef `json:"ref"`
	// RunAt is when the published run happened; absent before the first.
	RunAt *time.Time `json:"runAt,omitempty"`
	// InputGeneration identifies the snapshot set the run read, so the
	// client can tell a re-render from a new run.
	InputGeneration uint64      `json:"inputGeneration"`
	Runner          SourceState `json:"runner" doc:"State of the background runner for this Cluster."`
	// CatalogPins names the CNPG minors the rule catalog is pinned to,
	// and Applied the one used for this Cluster ("" when none matched).
	CatalogPins []string      `json:"catalogPins"`
	Applied     string        `json:"applied,omitempty"`
	Counts      FindingCounts `json:"counts"`
	Incidents   []Incident    `json:"incidents" doc:"Findings grouped by catalog-pinned causal relations, worst first."`
	Checks      []Check       `json:"checks"`
}

// Severity is a finding's severity.
type Severity string

// The severities. Critical and high read as health critical, warning as
// warning, note as info.
const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityWarning  Severity = "warning"
	SeverityNote     Severity = "note"
)

// Incident is a root finding and the findings that follow from it.
type Incident struct {
	Root         Finding   `json:"root"`
	Consequences []Finding `json:"consequences,omitempty"`
	// Severity is the worst across root and consequences.
	Severity Severity `json:"severity" enum:"critical,high,warning,note"`
}

// Finding is one correlated statement about what is wrong. Evidence is
// quoted, never paraphrased; NextSteps is console guidance and rendered
// apart from it.
type Finding struct {
	ID       string    `json:"id"`
	Check    string    `json:"check" doc:"Detector or catalog rule that produced it."`
	Subject  ObjectRef `json:"subject"`
	Severity Severity  `json:"severity" enum:"critical,high,warning,note"`
	Summary  string    `json:"summary"`
	Detail   string    `json:"detail,omitempty"`
	// At is the instant the underlying observation carries (an event's
	// last occurrence); absent for a current state.
	At *time.Time `json:"at,omitempty"`
	// Since is the "state that lasts" clock: when the console first saw
	// this finding in consecutive runs. A floor, reset when it clears.
	Since     *time.Time `json:"since,omitempty" doc:"First run in an unbroken series that reported this finding."`
	Evidence  []Evidence `json:"evidence"`
	NextSteps string     `json:"nextSteps,omitempty"`
	// Relation states, for a consequence, how it follows from its root.
	Relation string `json:"relation,omitempty"`
	// Link is a cluster-relative route, never an action.
	Link      string `json:"link,omitempty"`
	LinkLabel string `json:"linkLabel,omitempty"`
}

// Evidence is one verbatim claim a finding rests on.
type Evidence struct {
	Origin string `json:"origin" doc:"Who made the claim, as the catalog names it."`
	Object string `json:"object,omitempty" untrusted:"true"`
	Detail string `json:"detail"`
}

// CheckOutcome is what became of a check in a run.
type CheckOutcome string

// The outcomes.
const (
	CheckMatched       CheckOutcome = "matched"
	CheckClear         CheckOutcome = "clear"
	CheckUnavailable   CheckOutcome = "unavailable"
	CheckNotApplicable CheckOutcome = "notApplicable"
)

// Check is one detector's account of itself in a run.
type Check struct {
	Name      string       `json:"name"`
	Layer     string       `json:"layer" doc:"Kubernetes, Operator, PostgreSQL, Replication, Backup, ..."`
	Describes string       `json:"describes"`
	Outcome   CheckOutcome `json:"outcome" enum:"matched,clear,unavailable,notApplicable"`
	Because   string       `json:"because,omitempty"`
	// SourceOff marks an unavailable check whose input is switched off
	// by configuration (a decision) rather than failing (a fault).
	SourceOff bool `json:"sourceOff,omitempty"`
}

// Triage is the /triage screen: playbooks walked against the same run.
type Triage struct {
	Ref       ClusterRef `json:"ref"`
	RunAt     *time.Time `json:"runAt,omitempty"`
	Playbooks []Playbook `json:"playbooks"`
}

// TriageOutcome is the answer to one triage step.
type TriageOutcome string

// The outcomes, after pkg/diagnose/triage.
const (
	TriageFound         TriageOutcome = "found"
	TriageRuledOut      TriageOutcome = "ruledOut"
	TriageUnknown       TriageOutcome = "unknown"
	TriageSourceOff     TriageOutcome = "sourceOff"
	TriageNotApplicable TriageOutcome = "notApplicable"
	TriageAsk           TriageOutcome = "ask"
)

// Playbook is one symptom-driven walk.
type Playbook struct {
	ID        string `json:"id"`
	Symptom   string `json:"symptom"`
	Describes string `json:"describes,omitempty"`
	Upstream  string `json:"upstream,omitempty" doc:"Upstream documentation the playbook follows."`
	// StartAt is the index of the first step not ruled out.
	StartAt int          `json:"startAt"`
	Steps   []TriageStep `json:"steps"`
}

// TriageStep is one question of a playbook and its answer.
type TriageStep struct {
	Question string        `json:"question"`
	Outcome  TriageOutcome `json:"outcome" enum:"found,ruledOut,unknown,sourceOff,notApplicable,ask"`
	Explain  string        `json:"explain,omitempty"`
	// Yourself is the "look yourself" command for a DBA.
	Yourself string     `json:"yourself,omitempty"`
	Because  string     `json:"because,omitempty"`
	Findings []string   `json:"findings,omitempty" doc:"IDs of the findings that answered the step."`
	Evidence []Evidence `json:"evidence,omitempty"`
}
