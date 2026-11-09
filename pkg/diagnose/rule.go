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
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// Rule is one catalog diagnostic, written as data: on the pinned
// versions, this observation means this finding. The prose a hand-written
// detector carries in code — what it looks for, what a match means,
// where the reader goes next — a rule carries in fields, which is what
// makes adding one a declaration rather than a program.
//
// Versioning is first-class because the claim is version-scoped whether
// the author says so or not: upstream can reword a log line or change a
// behaviour in any release. A pin makes that scope explicit, and the
// evaluator turns it into one of three honest answers per rule — it
// matched, it does not apply to the observed versions, or it could not
// be evaluated because a pinned version is not observed.
type Rule struct {
	// ID is the stable identifier findings are reported under. Unique
	// across the catalog and the hand-written detectors.
	ID string
	// Component is the part of the stack the rule is about. It groups
	// the catalog files and prefixes nothing: applicability comes only
	// from Requires.
	Component Component
	// Layer is where in the stack the rule looks, for the screen's
	// per-layer summary. See Layer.
	Layer Layer
	// Requires are the version pins, all of which must hold. Empty means
	// the rule applies to every version the console encounters — a claim
	// the author makes by leaving it empty, not a default.
	Requires []Requirement
	// Pinned lists the upstream source strings the pin was verified
	// against, when the condition's own literals are not verbatim source
	// strings — a metric name the exporter assembles at runtime, a JSON
	// envelope the log pipe emits — or when the condition carries none,
	// such as a comparison of two status fields. The pin verification
	// greps these in each verified release's tree instead of the
	// condition's literals. Empty for the common rule whose phase,
	// condition, event reason, or log line is itself the source string.
	Pinned []string
	// Severity of the finding when the rule matches.
	Severity Severity
	// Describes states what the rule looks for, for the check row.
	Describes string
	// Summary is the finding's one-sentence statement of what is wrong.
	Summary string
	// Detail expands on the consequence. Empty when the summary says
	// everything.
	Detail string
	// When is the observation that must hold. Nil means the version pins
	// alone are the finding — the rule fires whenever it applies, which
	// is how "this version is itself the problem" is written.
	When Condition
	// ReviewBy is the date, as YYYY-MM-DD, by which this rule's
	// console-pinned knowledge stops being safe to assert unreviewed.
	// It is for the claims that go stale on a calendar rather than on a
	// release: a support boundary moves when upstream retires the next
	// version, and nothing in the observed world changes to say so.
	//
	// The console verifies the operator's strings against the operator's
	// source, and this is the same discipline turned on its own claims:
	// the catalog's review gate fails CI once the date passes (it runs
	// with PGTOOLBOX_ENFORCE_REVIEW_DATES=1; a plain go test of a released
	// tree only logs it), so a stale boundary stops the pipeline instead
	// of quietly telling operators that a
	// supported version is unsupported, or worse, saying nothing about
	// one that is. Empty means the rule states nothing that expires —
	// a phase, an event reason and a log line are true until the string
	// changes, which the pin verification already catches.
	ReviewBy string
	// NextSteps is the console-pinned guidance carried onto every
	// finding this rule produces. See Finding.NextSteps.
	NextSteps string
	// ConsequenceOf names the checks this rule's findings follow from,
	// with the scope and window of each relation. See
	// Finding.ConsequenceOf.
	ConsequenceOf []Relation
	// Link and LinkLabel are where the reader goes next. A condition may
	// override them per match with something more specific.
	Link      string
	LinkLabel string
}

// Requirement is one version pin: a component and the constraint its
// observed version must satisfy, such as ">=1.24 <1.27".
type Requirement struct {
	Component  Component
	Constraint string
}

// String renders the pin for the check row, so the screen states the
// scope of a clear result: "clear" from a rule pinned to CNPG 1.24 rules
// nothing out on 1.27.
func (r Requirement) String() string {
	return string(r.Component) + " " + r.Constraint
}

// Satisfied reports whether an observed version meets the constraint.
// Exported for the pin verification, which asks the same question of
// each verified release that the evaluator asks of the observed one.
func (r Requirement) Satisfied(version string) bool {
	return satisfies(version, r.Constraint)
}

// Condition is one observation a rule can wait for. Implementations are
// declarative structs, so a rule stays data; each knows which input it
// reads and answers "could not run" when that input is unobserved,
// keeping the per-rule accounting as honest as the hand-written
// detectors'.
type Condition interface {
	// describe states what is looked for, folded into the check row.
	describe() string
	// evaluate returns one entry per matched finding, or the reason the
	// condition could not be evaluated. The rule's ID is passed through
	// so stream-backed conditions can find their own observations.
	evaluate(ruleID string, in Input) (matches []conditionMatch, unavailable string)
}

// conditionMatch is one matched finding as a condition reports it: the
// evidence, plus the parts only the condition can know.
type conditionMatch struct {
	// idSuffix distinguishes findings when one rule matches in several
	// places; empty when the rule's ID alone identifies the finding.
	idSuffix string
	// summary overrides the rule's summary when the match can state it
	// more precisely; empty keeps the rule's.
	summary string
	// evidence is the quoted claims, in reading order.
	evidence []Evidence
	// subject is the object the match is about, when it is about one.
	subject EntityRef
	// at is when the matched observation was made, when the source
	// carries a time.
	at time.Time
	// link and linkLabel override the rule's when set.
	link, linkLabel string
}

// clusterSubject is the subject of a finding about the cluster as a
// whole: the console watches one, so the kind alone names it.
var clusterSubject = EntityRef{Kind: "Cluster"}

// LogContains matches a line in a followed container log carrying every
// substring. Substrings rather than patterns, deliberately: a rule is a
// claim that a fixed message means a fixed thing, and the fixed strings
// are the claim's own words.
//
// The matching itself happens continuously in the logstream matcher —
// LogRules derives the matcher's rule set from the catalog — and this
// condition reads back what that matcher retained. A stream is best
// effort: it breaks on every container restart and Kubernetes cannot say
// what was missed, so a match reports what was seen, counts are floors,
// and no absence here is evidence of anything.
type LogContains struct {
	// Substrings must all appear in one line.
	Substrings []string
	// Except withdraws a line containing any of these, for a rule whose
	// marker is broader than its meaning: PostgreSQL stamps routine
	// lifecycle refusals with the same severity word as real faults, and
	// the carve-outs name those known-benign messages exactly.
	Except []string
}

func (c LogContains) describe() string {
	quoted := make([]string, len(c.Substrings))
	for i, s := range c.Substrings {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	described := "a followed log line containing " + strings.Join(quoted, " and ")
	if len(c.Except) > 0 {
		described += fmt.Sprintf(", excluding %d known-benign messages", len(c.Except))
	}
	return described
}

func (c LogContains) evaluate(ruleID string, in Input) ([]conditionMatch, string) {
	return logMatches(ruleID, in)
}

// LogFields matches a line of a followed container log by the values of
// its named fields rather than by searching the whole line. The operator
// writes JSON, so a rule that knows which field carries the string it
// looks for can say so: a severity belongs in the severity field, and a
// line quoting that severity inside some other field is not the server
// reporting it.
//
// It is the more precise of the two log conditions and the less widely
// usable: a rule can only be written this way once the field carrying
// its string has actually been read out of the emitting component's
// source. Where that is not established, LogContains stays the honest
// choice.
type LogFields struct {
	// Fields are the tests, all of which must hold.
	Fields []LogField
	// Except are substrings any one of which withdraws the match, tested
	// against the whole line exactly as LogContains tests them.
	Except []string
}

// LogField is one test against a named field: its exact value, or a
// substring of it where the component formats a value into the message.
// Exactly one of Equals and Contains is set; a field stating neither or
// both is malformed, matches nothing, and says so where it is
// described. The catalog's tests refuse one before it can ship.
type LogField struct {
	Path     string
	Equals   string
	Contains string
}

// Valid reports whether the field states exactly one question about one
// named field.
func (f LogField) Valid() bool {
	return logstream.FieldTest{
		Path: f.Path, Equals: f.Equals, Contains: f.Contains,
	}.Valid()
}

func (c LogFields) describe() string {
	parts := make([]string, len(c.Fields))
	for i, field := range c.Fields {
		switch {
		case !field.Valid():
			// Named rather than rendered as one of the two halves: a
			// check row that read "contains" of an empty string would
			// describe a test the rule never made.
			parts[i] = orUnnamedField(field.Path) + " is tested by a malformed field, which matches nothing"
		case field.Equals != "":
			parts[i] = fmt.Sprintf("%s is %q", field.Path, field.Equals)
		default:
			parts[i] = fmt.Sprintf("%s contains %q", field.Path, field.Contains)
		}
	}
	described := "a followed log line whose " + strings.Join(parts, " and ")
	if len(c.Except) > 0 {
		described += fmt.Sprintf(", excluding %d known-benign messages", len(c.Except))
	}
	return described
}

func (c LogFields) evaluate(ruleID string, in Input) ([]conditionMatch, string) {
	return logMatches(ruleID, in)
}

// orUnnamedField names a field whose path is empty.
func orUnnamedField(path string) string {
	if path == "" {
		return "an unnamed field"
	}
	return path
}

// logMatches is what both log conditions do with a run: read what the
// continuous matcher already retained under this rule's ID. Neither
// condition re-tests a line here — the lines are long gone by the time a
// run happens, which is the whole reason the matcher is continuous.
func logMatches(ruleID string, in Input) ([]conditionMatch, string) {
	if reason := logsUnavailable(in); reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, observation := range in.Logs.Observations() {
		if observation.RuleID != ruleID {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + observation.Pod + "/" + observation.Container,
			subject:  EntityRef{Kind: "Pod", Name: observation.Pod},
			at:       observation.LastSeen,
			evidence: []Evidence{
				{
					Origin: "container log (best effort)",
					Object: fmt.Sprintf("Pod/%s container %s", observation.Pod, observation.Container),
					Detail: observation.Line,
				},
				{
					Origin: "console-observed",
					Object: "matching window",
					Detail: fmt.Sprintf("first seen %s, most recently %s, at least %d matching lines",
						observation.FirstSeen.UTC().Format("15:04:05Z"),
						observation.LastSeen.UTC().Format("15:04:05Z"),
						observation.Count),
				},
			},
			link:      "/logs/" + observation.Pod + "/" + observation.Container,
			linkLabel: "Log tail",
		})
	}
	// Matches stand whatever else went unread -- a line that was seen was
	// seen. It is the empty result that cannot be published as a clear
	// while a container is going unheard.
	if len(matches) == 0 {
		return nil, logsIncomplete(in)
	}
	return matches, ""
}

// EventMatch matches events by reason, optionally narrowed to messages
// carrying every substring. It quotes the events rather than reasoning
// about them: the API server — or the operator, through its recorder —
// already stated the refusal in its own words.
//
// Only events on the Cluster object and its member pods are in the
// observed window, so a rule here must be about one of those. An event
// the operator records on a Backup or ScheduledBackup object is not
// observable and belongs to a status-backed condition instead.
type EventMatch struct {
	// Reasons are the event reasons accepted, any of which matches.
	Reasons []string
	// Types are the event types accepted; empty means Warning only.
	// Named explicitly because the operator's type is not a reliable
	// severity signal — some real failures are recorded as Normal.
	Types []string
	// MessageContains narrows to messages carrying every substring.
	// Empty accepts any message under a matching reason.
	MessageContains []string
	// Kinds are the involved-object kinds accepted; empty means the
	// Cluster and its pods, the window's original scope. An event on a
	// Backup, ScheduledBackup or Pooler is admitted only when the
	// respective catalog lists the object as this cluster's — the
	// namespace can hold another cluster's — and cannot be judged at
	// all while that catalog is unreadable.
	Kinds []string
}

func (c EventMatch) describe() string {
	described := "an event with reason " + strings.Join(c.Reasons, " or ")
	if len(c.Kinds) > 0 {
		described += " on a " + strings.Join(c.Kinds, " or ")
	}
	return described
}

// eventAdmitted decides whether an event of a secondary kind is this
// cluster's: admitted, or the reason it cannot be told.
func eventAdmitted(in Input, event observe.EventFacts) (bool, string) {
	switch event.Kind {
	case "Backup", "ScheduledBackup":
		if !in.HasBackups {
			return false, "the backup catalog has not been observed yet, so events on " + event.Kind + " objects cannot be attributed to this cluster"
		}
		if in.Backups.Stale {
			return false, "the backup catalog is stale, so events on " + event.Kind + " objects cannot be attributed to this cluster"
		}
		if event.Kind == "Backup" {
			for _, backup := range in.Backups.Backups {
				if backup.Name == event.Object {
					return true, ""
				}
			}
			return false, ""
		}
		for _, schedule := range in.Backups.ScheduledBackups {
			if schedule.Name == event.Object {
				return true, ""
			}
		}
		return false, ""
	case "Pooler":
		if reason := poolersUnavailable(in); reason != "" {
			return false, reason + ", so events on Pooler objects cannot be attributed to this cluster"
		}
		for _, pooler := range in.Poolers.Poolers {
			if pooler.Name == event.Object {
				return true, ""
			}
		}
		return false, ""
	}
	return true, ""
}

func (c EventMatch) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := eventsUnavailable(in); reason != "" {
		return nil, reason
	}
	types := c.Types
	if len(types) == 0 {
		types = []string{"Warning"}
	}
	kinds := c.Kinds
	if len(kinds) == 0 {
		kinds = []string{"Cluster", "Pod"}
	}
	// Bounded like warningEvents, so one flapping object cannot fill a
	// finding with the same line.
	const maxQuoted = 3
	var evidence []Evidence
	// The newest matching event names the subject and the time: the
	// list is newest first, and one finding quotes up to three.
	var newest *observe.EventFacts
	for _, event := range in.Events.Events {
		if len(evidence) >= maxQuoted {
			break
		}
		typeAccepted := false
		for _, accepted := range types {
			if event.Type == accepted {
				typeAccepted = true
				break
			}
		}
		if !typeAccepted {
			continue
		}
		kindAccepted := false
		for _, accepted := range kinds {
			if event.Kind == accepted {
				kindAccepted = true
				break
			}
		}
		if !kindAccepted {
			continue
		}
		reasonAccepted := false
		for _, reason := range c.Reasons {
			if event.Reason == reason {
				reasonAccepted = true
				break
			}
		}
		if !reasonAccepted {
			continue
		}
		carriesAll := true
		for _, needle := range c.MessageContains {
			if !strings.Contains(event.Message, needle) {
				carriesAll = false
				break
			}
		}
		if !carriesAll {
			continue
		}
		admitted, unattributable := eventAdmitted(in, event)
		if unattributable != "" {
			return nil, unattributable
		}
		if admitted {
			evidence = append(evidence, eventEvidence(event))
			if newest == nil {
				first := event
				newest = &first
			}
		}
	}
	if len(evidence) == 0 {
		return nil, ""
	}
	// One finding quoting every matched event: the events are the same
	// refusal repeating, not separate incidents.
	return []conditionMatch{{
		evidence: evidence,
		subject:  EntityRef{Kind: newest.Kind, Name: newest.Object},
		at:       newest.LastSeen,
	}}, ""
}

// BackupPhase matches Backup objects sitting in one of the given phases
// for at least MinAge, quoting the operator's own phase. The age bound
// keeps ordinary progress out: every backup passes through its early
// phases, and only staying there is a finding.
type BackupPhase struct {
	// AnyOf are the phase strings that match, compared case-insensitively
	// because the operator has written both "failed" and "Failed" across
	// versions.
	AnyOf []string
	// MinAge is how long the backup must have existed before its phase
	// counts. Zero matches immediately.
	MinAge time.Duration
}

func (c BackupPhase) describe() string {
	return "a Backup whose reported phase is " + strings.Join(c.AnyOf, " or ")
}

func (c BackupPhase) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if !in.HasBackups {
		return nil, "the backup catalog has not been observed yet"
	}
	if in.Backups.Stale {
		return nil, "the backup catalog is stale, so current phases are unknown"
	}
	var matches []conditionMatch
	for _, backup := range in.Backups.Backups {
		phaseAccepted := false
		for _, phase := range c.AnyOf {
			if strings.EqualFold(backup.Phase, phase) {
				phaseAccepted = true
				break
			}
		}
		if !phaseAccepted {
			continue
		}
		if c.MinAge > 0 && in.Now.Sub(backup.CreatedAt) < c.MinAge {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + backup.Name,
			subject:  EntityRef{Kind: "Backup", Name: backup.Name},
			at:       backup.CreatedAt,
			evidence: []Evidence{{
				Origin: "operator-reported",
				Object: "Backup/" + backup.Name,
				Detail: fmt.Sprintf("phase %s, created %s", backup.Phase,
					backup.CreatedAt.UTC().Format("2006-01-02 15:04:05Z")),
			}},
			link:      "/backups",
			linkLabel: "Backups",
		})
	}
	return matches, ""
}

// ClusterCondition matches an operator-reported condition on the
// Cluster at a given status, quoting the operator's own reason and
// message. It is the status-backed condition: nothing here depends on
// log text, so rules built on it survive rewordings that would silence
// a LogContains rule.
type ClusterCondition struct {
	// Type is the condition type, such as "ContinuousArchiving".
	Type string
	// Status is the status that matches, such as "False".
	Status string
	// Reason, when set, narrows to that reason. It matters when one
	// status carries two meanings: LastBackupSucceeded goes False both
	// for a backup that failed and for one that just started, and only
	// the reason tells them apart.
	Reason string
	// MinAge, when set, requires the condition to have held that long
	// by the operator's own transition time. A condition the operator
	// reports without one cannot be judged and says so.
	MinAge time.Duration
}

func (c ClusterCondition) describe() string {
	described := fmt.Sprintf("the operator reporting condition %s as %s", c.Type, c.Status)
	if c.Reason != "" {
		described += " with reason " + c.Reason
	}
	if c.MinAge > 0 {
		described += fmt.Sprintf(" for at least %s", c.MinAge)
	}
	return described
}

func (c ClusterCondition) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	for _, condition := range in.Cluster.Cluster.Conditions {
		if condition.Type != c.Type || condition.Status != c.Status {
			continue
		}
		if c.Reason != "" && condition.Reason != c.Reason {
			continue
		}
		detail := fmt.Sprintf("status %s, reason %s", condition.Status, condition.Reason)
		if condition.Message != "" {
			detail += ": " + condition.Message
		}
		if c.MinAge > 0 {
			if condition.LastTransition == nil {
				return nil, fmt.Sprintf(
					"condition %s carries no transition time, so how long it has held is unknown", c.Type)
			}
			held := in.Now.Sub(*condition.LastTransition)
			if held < c.MinAge {
				return nil, ""
			}
			detail += fmt.Sprintf("; last transition %s — %s ago",
				condition.LastTransition.UTC().Format(time.RFC3339), held.Round(time.Minute))
		}
		return []conditionMatch{{subject: clusterSubject, evidence: []Evidence{{
			Origin: "operator-reported",
			Object: "Cluster condition " + c.Type,
			Detail: detail,
		}}}}, ""
	}
	// An absent condition is not the sought status; it is also not
	// evidence of the opposite, which is why this reads as clear only
	// under a check row that names the exact condition it waited for.
	return nil, ""
}

// ClusterPhase matches the operator-reported phase, quoting the phase
// and its reason. It is for phases that mean the cluster is waiting on
// something — a phase the operator can sit in indefinitely is the
// closest thing it has to saying "stuck" out loud.
type ClusterPhase struct {
	// AnyOf are the phase strings that match, exactly as the operator
	// writes them.
	AnyOf []string
}

func (c ClusterPhase) describe() string {
	return "the operator reporting phase " + strings.Join(c.AnyOf, " or ")
}

func (c ClusterPhase) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	for _, phase := range c.AnyOf {
		if in.Cluster.Cluster.Phase != phase {
			continue
		}
		detail := "phase " + phase
		if in.Cluster.Cluster.PhaseReason != "" {
			detail += ": " + in.Cluster.Cluster.PhaseReason
		}
		return []conditionMatch{{subject: clusterSubject, evidence: []Evidence{{
			Origin: "operator-reported",
			Object: "Cluster",
			Detail: detail,
		}}}}, ""
	}
	return nil, ""
}

// InstantNonZero matches an instance whose latest reading of one
// point-in-time metric is non-zero. The keys are the metric catalog's
// instant keys, such as "fencing-on"; the exporter's own metric name is
// quoted in the evidence.
//
// It reads the scraped window, so it is a claim about what the exporter
// last reported. A reading the sweep has stopped refreshing is refused
// rather than taken at face value, and the read time is quoted on the
// ones that pass so the reader sees how current the claim is.
type InstantNonZero struct {
	// Key is the instant key in the instance metric catalog.
	Key string
}

func (c InstantNonZero) describe() string {
	return fmt.Sprintf("an instance whose exporter reports %s as non-zero", instantMetricName(c.Key))
}

func (c InstantNonZero) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := metricsUnavailable(in); reason != "" {
		return nil, reason
	}
	fresh := freshnessOf(in.Metrics, in.Now, "of "+instantMetricName(c.Key))
	readings := in.Metrics.InstantReadings()
	var matches []conditionMatch
	for _, instance := range readingInstances(readings) {
		reading, reported := readings[instance][c.Key]
		// Age is judged before value, so a stale zero is withheld
		// rather than read as the flag being off.
		if !reported || !fresh.current(reading.At) || reading.Value == 0 {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + instance,
			subject:  EntityRef{Kind: "Pod", Name: instance},
			at:       time.Unix(reading.At, 0),
			evidence: []Evidence{{
				Origin: "console-scraped from the instance exporter",
				Object: "instance " + instance,
				Detail: fmt.Sprintf("%s = %g, read %s", instantMetricName(c.Key), reading.Value,
					time.Unix(reading.At, 0).UTC().Format("15:04:05Z")),
			}},
			link:      "/cluster/metrics",
			linkLabel: "Metrics",
		})
	}
	if len(matches) == 0 {
		return nil, fresh.unavailable()
	}
	return matches, ""
}

// InstantZero matches an instance whose latest reading of one
// point-in-time flag is zero — the exact inverse of InstantNonZero, for
// the flags where the fault is an absence rather than a presence. An
// instance whose exporter does not report the flag is skipped, never
// counted as zero: unreported is not off.
type InstantZero struct {
	Key string
}

func (c InstantZero) describe() string {
	return fmt.Sprintf("an instance whose exporter reports %s as zero", instantMetricName(c.Key))
}

func (c InstantZero) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := metricsUnavailable(in); reason != "" {
		return nil, reason
	}
	fresh := freshnessOf(in.Metrics, in.Now, "of "+instantMetricName(c.Key))
	readings := in.Metrics.InstantReadings()
	var matches []conditionMatch
	for _, instance := range readingInstances(readings) {
		reading, reported := readings[instance][c.Key]
		if !reported || !fresh.current(reading.At) || reading.Value != 0 {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + instance,
			subject:  EntityRef{Kind: "Pod", Name: instance},
			at:       time.Unix(reading.At, 0),
			evidence: []Evidence{{
				Origin: "console-scraped from the instance exporter",
				Object: "instance " + instance,
				Detail: fmt.Sprintf("%s = 0, read %s", instantMetricName(c.Key),
					time.Unix(reading.At, 0).UTC().Format("15:04:05Z")),
			}},
			link:      "/cluster/metrics",
			linkLabel: "Metrics",
		})
	}
	if len(matches) == 0 {
		return nil, fresh.unavailable()
	}
	return matches, ""
}

// InstantShortfall matches an instance reporting fewer of something
// than it reports expecting: two readings the exporter publishes under
// one metric name and different labels, compared against each other.
// Both must be reported for an instance to be judged — one without the
// other is a number with nothing to compare it to — and an instance
// reporting neither is one where the feature is not configured, which
// is a clear result rather than a silent skip.
type InstantShortfall struct {
	// Expected and Observed are the instant keys of the two readings.
	Expected, Observed string
	// Noun names what is counted, for the finding's own summary.
	Noun string
}

func (c InstantShortfall) describe() string {
	return fmt.Sprintf("an instance reporting fewer %s than it expects", c.Noun)
}

func (c InstantShortfall) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := metricsUnavailable(in); reason != "" {
		return nil, reason
	}
	fresh := freshnessOf(in.Metrics, in.Now, "of "+instantMetricName(c.Observed))
	readings := in.Metrics.InstantReadings()
	var matches []conditionMatch
	for _, instance := range readingInstances(readings) {
		expected, hasExpected := readings[instance][c.Expected]
		observed, hasObserved := readings[instance][c.Observed]
		if !hasExpected || !hasObserved {
			continue
		}
		// Both sides must be current: comparing a fresh count against a
		// frozen expectation, or the reverse, invents a shortfall out of
		// the gap between two sweeps. Both are judged rather than
		// short-circuited, so the count the reason carries is a true
		// count of what this check refused.
		currentObserved, currentExpected := fresh.current(observed.At), fresh.current(expected.At)
		if !currentObserved || !currentExpected {
			continue
		}
		if expected.Value <= 0 || observed.Value >= expected.Value {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + instance,
			subject:  EntityRef{Kind: "Pod", Name: instance},
			at:       time.Unix(observed.At, 0),
			summary: fmt.Sprintf("Instance %s reports %g of the %g %s it expects.",
				instance, observed.Value, expected.Value, c.Noun),
			evidence: []Evidence{{
				Origin: "console-scraped from the instance exporter",
				Object: "instance " + instance,
				Detail: fmt.Sprintf("%s: expected %g, observed %g, read %s",
					instantMetricName(c.Expected), expected.Value, observed.Value,
					time.Unix(observed.At, 0).UTC().Format("15:04:05Z")),
			}},
			link:      "/cluster/metrics",
			linkLabel: "Metrics",
		})
	}
	if len(matches) == 0 {
		return nil, fresh.unavailable()
	}
	return matches, ""
}

// readingInstances is the instances a window holds instant readings
// for, in a stable order.
func readingInstances(readings map[string]map[string]metrics.Instant) []string {
	instances := make([]string, 0, len(readings))
	for instance := range readings {
		instances = append(instances, instance)
	}
	sort.Strings(instances)
	return instances
}

// instantMetricName resolves an instant key to the exporter's metric
// name, so evidence quotes the exporter's vocabulary rather than the
// console's.
func instantMetricName(key string) string {
	for _, def := range metrics.Instance.Instants {
		if def.Key == key {
			if len(def.Names) > 0 {
				return def.Names[0]
			}
			break
		}
	}
	return key
}

// ContainerState matches containers whose kubelet-reported state
// reason is in the set — CrashLoopBackOff, OOMKilled, and the like. It
// reads the same container facts the pods screen renders: the reason is
// the kubelet's own word, quoted, never inferred from restarts or
// timing.
//
// Instance pods are always scanned; pooler pods join when observed,
// because a crash-looping PgBouncer breaks applications just as surely
// while looking nothing like an instance failure.
type ContainerState struct {
	// Reasons are the kubelet reasons accepted, any of which matches.
	Reasons []string
}

func (c ContainerState) describe() string {
	return "a container whose state reason is " + strings.Join(c.Reasons, " or ")
}

func (c ContainerState) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := podsUnavailable(in); reason != "" {
		return nil, reason
	}
	if reason := poolerPodsUnavailable(in); reason != "" {
		return nil, reason
	}
	pods := in.Pods.Pods
	if in.HasPoolerPods {
		pods = append(append([]observe.PodFacts{}, pods...), in.PoolerPods.Pods...)
	}
	var matches []conditionMatch
	for _, pod := range pods {
		for _, container := range pod.Containers {
			accepted := false
			for _, reason := range c.Reasons {
				if container.Reason == reason {
					accepted = true
					break
				}
			}
			if !accepted {
				continue
			}
			detail := fmt.Sprintf("container %q: %s", container.Name, container.Reason)
			if container.State != "" {
				detail += ", state " + container.State
			}
			if container.Restarts != nil {
				detail += fmt.Sprintf(", %d restarts", *container.Restarts)
			}
			if container.ExitCode != nil {
				detail += fmt.Sprintf(", last exit code %d", *container.ExitCode)
			}
			matches = append(matches, conditionMatch{
				idSuffix: "/" + pod.Name + "/" + container.Name,
				subject:  EntityRef{Kind: "Pod", Name: pod.Name},
				evidence: []Evidence{{
					Origin: "Kubernetes-observed",
					Object: "Pod/" + pod.Name,
					Detail: detail,
				}},
			})
		}
	}
	return matches, ""
}

// PrimaryMismatch matches a primary move still in flight after MinAge:
// the operator's current and target primaries disagree, and the
// operator's own request timestamp is at least that old. Both bounds
// come from the operator — without the timestamp there is no honest way
// to call the move stuck, so an unreported timestamp matches nothing.
type PrimaryMismatch struct {
	// MinAge is how long the move must have been in flight.
	MinAge time.Duration
}

func (c PrimaryMismatch) describe() string {
	return fmt.Sprintf("a primary move still in flight after %s", c.MinAge)
}

func (c PrimaryMismatch) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	cluster := in.Cluster.Cluster
	if cluster.CurrentPrimary == "" || cluster.TargetPrimary == "" ||
		cluster.CurrentPrimary == cluster.TargetPrimary {
		return nil, ""
	}
	if cluster.TargetPrimaryTimestamp == nil {
		return nil, ""
	}
	age := in.Now.Sub(*cluster.TargetPrimaryTimestamp)
	if age < c.MinAge {
		return nil, ""
	}
	detail := fmt.Sprintf("currentPrimary %s, targetPrimary %s, requested %s (%s ago)",
		cluster.CurrentPrimary, cluster.TargetPrimary,
		cluster.TargetPrimaryTimestamp.UTC().Format("15:04:05Z"),
		age.Round(time.Minute))
	if cluster.TargetPrimary == "pending" {
		detail += `; "pending" is the operator's marker for a failover decided with no candidate chosen yet`
	}
	return []conditionMatch{{subject: clusterSubject, at: *cluster.TargetPrimaryTimestamp, evidence: []Evidence{{
		Origin: "operator-reported",
		Object: "Cluster primaries",
		Detail: detail,
	}}}}, ""
}

// ScheduledBackupSuspended matches suspended backup schedules.
type ScheduledBackupSuspended struct{}

func (ScheduledBackupSuspended) describe() string {
	return "a ScheduledBackup whose suspend flag is set"
}

func (ScheduledBackupSuspended) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if !in.HasBackups {
		return nil, "the backup catalog has not been observed yet"
	}
	if in.Backups.Stale {
		return nil, "the backup catalog is stale, so current schedules are unknown"
	}
	var matches []conditionMatch
	for _, schedule := range in.Backups.ScheduledBackups {
		if schedule.Suspended == nil || !*schedule.Suspended {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + schedule.Name,
			subject:  EntityRef{Kind: "ScheduledBackup", Name: schedule.Name},
			evidence: []Evidence{{
				Origin: "operator-reported",
				Object: "ScheduledBackup/" + schedule.Name,
				Detail: "suspend is true",
			}},
			link:      "/backups",
			linkLabel: "Backups",
		})
	}
	return matches, ""
}

// ScheduledBackupOverdue matches schedules whose operator-reported next
// run is at least Grace in the past — the operator advances that field
// every time it schedules, so a stale one means scheduling itself has
// stopped, whatever the cause. Suspended schedules are excluded: those
// are the other rule's finding.
type ScheduledBackupOverdue struct {
	// Grace is how far past the reported next run counts as stopped.
	Grace time.Duration
}

func (c ScheduledBackupOverdue) describe() string {
	return fmt.Sprintf("a ScheduledBackup whose reported next run is over %s past", c.Grace)
}

func (c ScheduledBackupOverdue) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if !in.HasBackups {
		return nil, "the backup catalog has not been observed yet"
	}
	if in.Backups.Stale {
		return nil, "the backup catalog is stale, so current schedules are unknown"
	}
	var matches []conditionMatch
	for _, schedule := range in.Backups.ScheduledBackups {
		if schedule.Suspended != nil && *schedule.Suspended {
			continue
		}
		if schedule.NextScheduleTime == nil {
			continue
		}
		overdue := in.Now.Sub(*schedule.NextScheduleTime)
		if overdue < c.Grace {
			continue
		}
		detail := fmt.Sprintf("next run reported for %s, now %s past",
			schedule.NextScheduleTime.UTC().Format("2006-01-02 15:04:05Z"),
			overdue.Round(time.Minute))
		if schedule.LastScheduleTime != nil {
			detail += ", last scheduled " + schedule.LastScheduleTime.UTC().Format("2006-01-02 15:04:05Z")
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + schedule.Name,
			subject:  EntityRef{Kind: "ScheduledBackup", Name: schedule.Name},
			at:       *schedule.NextScheduleTime,
			evidence: []Evidence{{
				Origin: "operator-reported",
				Object: "ScheduledBackup/" + schedule.Name,
				Detail: detail,
			}},
			link:      "/backups",
			linkLabel: "Backups",
		})
	}
	return matches, ""
}

// SeriesAbove matches an instance whose latest reading of one retained
// metric series is at or above the threshold. Raw samples are preferred;
// the rollup tier answers when the raw window holds nothing.
type SeriesAbove struct {
	// Key is the series key in the instance metric catalog, or in the
	// pooler catalog when Pooler is set.
	Key string
	// Threshold is the inclusive lower bound that matches.
	Threshold float64
	// For requires the breach to be sustained: every sample the console
	// retains from the trailing window is at or above the threshold, and
	// the window holds at least two of them. Zero matches on the latest
	// sample alone. It is what separates a threshold worth reporting
	// from a spike — a lag of five minutes for one scrape is traffic,
	// the same lag held for a quarter of an hour is a replica falling
	// behind — and an instance whose window is too short to show either
	// is reported as one the check could not judge, never as a match.
	For time.Duration
	// Pooler reads the PgBouncer exporter's window and catalog instead
	// of the instance's.
	Pooler bool
}

// window is the metrics window and catalog the condition reads, or the
// reason neither is scraped.
func (c SeriesAbove) window(in Input) (MetricsWindow, metrics.Catalog, string) {
	if c.Pooler {
		if reason := poolerMetricsUnavailable(in); reason != "" {
			return nil, metrics.Pooler, reason
		}
		return in.PoolerMetrics, metrics.Pooler, ""
	}
	if reason := metricsUnavailable(in); reason != "" {
		return nil, metrics.Instance, reason
	}
	return in.Metrics, metrics.Instance, ""
}

func (c SeriesAbove) describe() string {
	subject, catalog := "an instance", metrics.Instance
	if c.Pooler {
		subject, catalog = "a pooler instance", metrics.Pooler
	}
	described := fmt.Sprintf("%s whose %s reading is at least %g", subject, seriesMetricName(catalog, c.Key), c.Threshold)
	if c.For > 0 {
		described += ", held for " + c.For.String()
	}
	return described
}

func (c SeriesAbove) evaluate(_ string, in Input) ([]conditionMatch, string) {
	window, catalog, unavailable := c.window(in)
	if unavailable != "" {
		return nil, unavailable
	}
	origin, object, link, label := "console-scraped from the instance exporter", "instance ", "/cluster/metrics", "Metrics"
	if c.Pooler {
		origin, object, link, label = "console-scraped from the pooler exporter", "pooler instance ", "/poolers/metrics", "Pooler metrics"
	}
	readings := map[string][]seriesReading{}
	for _, tier := range [...]metrics.Tier{metrics.TierRaw, metrics.TierRollup} {
		times, byInstance := window.Range(c.Key, tier)
		for instance, column := range byInstance {
			if _, done := readings[instance]; done {
				continue
			}
			var samples []seriesReading
			for i, value := range column {
				if value != nil && i < len(times) {
					samples = append(samples, seriesReading{value: *value, at: times[i]})
				}
			}
			if len(samples) > 0 {
				readings[instance] = samples
			}
		}
	}
	instances := make([]string, 0, len(readings))
	for instance := range readings {
		instances = append(instances, instance)
	}
	sort.Strings(instances)
	fresh := freshnessOf(window, in.Now, "of "+seriesMetricName(catalog, c.Key))
	var matches []conditionMatch
	unjudged := 0
	for _, instance := range instances {
		samples := readings[instance]
		reading := samples[len(samples)-1]
		// The newest retained sample is the only one that could speak
		// for now, so its age decides whether this instance can be
		// judged at all — before the threshold, so that an instance
		// whose exporter froze below it is withheld rather than
		// counted as being under the line.
		if !fresh.current(reading.at) {
			continue
		}
		if reading.value < c.Threshold {
			continue
		}
		held := ""
		if c.For > 0 {
			trailing := sustained(samples, in.Now, c.For)
			if len(trailing) < 2 {
				// One sample cannot show that anything was held, and
				// matching on it would make this a spike detector.
				unjudged++
				continue
			}
			breached := true
			for _, sample := range trailing {
				if sample.value < c.Threshold {
					breached = false
					break
				}
			}
			if !breached {
				continue
			}
			held = fmt.Sprintf(", and every one of the %d samples since %s", len(trailing),
				time.Unix(trailing[0].at, 0).UTC().Format("15:04:05Z"))
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + instance,
			subject:  EntityRef{Kind: "Pod", Name: instance},
			at:       time.Unix(reading.at, 0),
			evidence: []Evidence{{
				Origin: origin,
				Object: object + instance,
				Detail: fmt.Sprintf("%s = %.0f, read %s%s", seriesMetricName(catalog, c.Key), reading.value,
					time.Unix(reading.at, 0).UTC().Format("15:04:05Z"), held),
			}},
			link:      link,
			linkLabel: label,
		})
	}
	if len(matches) == 0 {
		// Age first: "the sweep stopped" is the more specific account of
		// a short window, and the one an operator can act on.
		if reason := fresh.unavailable(); reason != "" {
			return nil, reason
		}
		if unjudged > 0 {
			return nil, fmt.Sprintf(
				"the retained window holds fewer than two samples of %s, so nothing can be shown to have held for %s",
				seriesMetricName(catalog, c.Key), c.For)
		}
	}
	return matches, ""
}

// sustained is the samples inside the trailing window, oldest first.
func sustained(samples []seriesReading, now time.Time, span time.Duration) []seriesReading {
	cutoff := now.Add(-span).Unix()
	var trailing []seriesReading
	for _, sample := range samples {
		if sample.at >= cutoff {
			trailing = append(trailing, sample)
		}
	}
	return trailing
}

// seriesReading is one sample of one instance's series.
type seriesReading struct {
	value float64
	at    int64
}

// seriesMetricName resolves a series key to the exporter's metric name,
// so evidence quotes the exporter's vocabulary rather than the
// console's.
func seriesMetricName(catalog metrics.Catalog, key string) string {
	if def, ok := catalog.SeriesByKey(key); ok && len(def.Names) > 0 {
		return def.Names[0]
	}
	return key
}

// DeclaredObjectFailed matches declarative database objects — Database,
// DatabaseRole, Publication, Subscription — whose operator
// reconciliation report says failed. An object the operator has not
// reported on yet matches nothing: unreported is not failed.
type DeclaredObjectFailed struct{}

func (DeclaredObjectFailed) describe() string {
	return "a declared database object whose reconciliation the operator reports as failed"
}

func (DeclaredObjectFailed) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if !in.HasDatabaseObjects {
		return nil, "the declared database objects have not been observed yet"
	}
	if in.DatabaseObjects.Stale {
		return nil, "the declared database objects are stale, so current reports are unknown"
	}
	var matches []conditionMatch
	failed := func(kind, name string, declared observe.Declared) {
		if declared.Applied == nil || *declared.Applied {
			return
		}
		detail := "applied false"
		if declared.Message != "" {
			detail += ": " + declared.Message
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + strings.ToLower(kind) + "/" + name,
			subject:  EntityRef{Kind: kind, Name: name},
			summary:  fmt.Sprintf("The operator cannot apply the declared %s %q.", kind, name),
			evidence: []Evidence{{
				Origin: "operator-reported",
				Object: kind + "/" + name,
				Detail: detail,
			}},
			link:      "/databases",
			linkLabel: "Databases",
		})
	}
	for _, object := range in.DatabaseObjects.Databases {
		failed("Database", object.Name, object.Declared)
	}
	for _, object := range in.DatabaseObjects.Roles {
		failed("DatabaseRole", object.Name, object.Declared)
	}
	for _, object := range in.DatabaseObjects.Publications {
		failed("Publication", object.Name, object.Declared)
	}
	for _, object := range in.DatabaseObjects.Subscriptions {
		failed("Subscription", object.Name, object.Declared)
	}
	return matches, ""
}

// evaluateRule turns one rule into its check row and findings. The
// outcomes, in the order they are decided:
//
//   - a pinned component's version is unobserved → could not run,
//   - the observed versions fall outside the pins → does not apply,
//   - the condition's input is unobserved → could not run,
//   - the condition matched → findings, each quoting its evidence and,
//     when the rule is pinned, the version facts that made it apply,
//   - otherwise → clear, scoped by the pins the check row states.
func evaluateRule(rule Rule, in Input) (Check, []Finding) {
	check := Check{Name: rule.ID, Layer: rule.Layer, Describes: ruleDescribes(rule)}
	facts := versionFacts(in)

	var pins []Evidence
	for _, requirement := range rule.Requires {
		observed, known := facts[requirement.Component]
		if !known {
			check.Outcome = CheckUnavailable
			check.Because = fmt.Sprintf("the %s version is not observed, and this check applies only to %s",
				requirement.Component, requirement)
			return check, nil
		}
		if !satisfies(observed.Version, requirement.Constraint) {
			check.Outcome = CheckNotApplicable
			check.Because = fmt.Sprintf("applies to %s; observed %s", requirement, observed.Version)
			return check, nil
		}
		pins = append(pins, observed.evidence())
	}

	matches := []conditionMatch{{}}
	if rule.When != nil {
		var unavailable string
		matches, unavailable = rule.When.evaluate(rule.ID, in)
		if unavailable != "" {
			check.Outcome, check.Because = CheckUnavailable, unavailable
			check.SourceOff = sourceOff(unavailable)
			return check, nil
		}
		if len(matches) == 0 {
			check.Outcome = CheckClear
			return check, nil
		}
	}

	check.Outcome = CheckMatched
	findings := make([]Finding, 0, len(matches))
	for _, match := range matches {
		finding := Finding{
			ID:            rule.ID + match.idSuffix,
			Check:         rule.ID,
			Subject:       match.subject,
			At:            match.at,
			Severity:      rule.Severity,
			Summary:       rule.Summary,
			Detail:        rule.Detail,
			Evidence:      append(match.evidence, pins...),
			NextSteps:     rule.NextSteps,
			ConsequenceOf: rule.ConsequenceOf,
			Link:          rule.Link,
			LinkLabel:     rule.LinkLabel,
		}
		if match.summary != "" {
			finding.Summary = match.summary
		}
		if match.link != "" {
			finding.Link, finding.LinkLabel = match.link, match.linkLabel
		}
		findings = append(findings, finding)
	}
	return check, findings
}

// ruleDescribes folds the version pins into the check row, so a clear
// result states its own scope.
func ruleDescribes(rule Rule) string {
	describes := rule.Describes
	if describes == "" && rule.When != nil {
		describes = rule.When.describe()
	}
	if len(rule.Requires) == 0 {
		return describes
	}
	pins := make([]string, len(rule.Requires))
	for i, requirement := range rule.Requires {
		pins[i] = requirement.String()
	}
	return describes + " (applies to " + strings.Join(pins, ", ") + ")"
}

// LogRules derives the continuous matcher's rule set from a catalog:
// every rule watching for a log line is declared once, there, and the
// matcher learns its substrings from the same declaration the evaluator
// reads. The matcher is deliberately version-blind — it retains matches
// whatever the versions are, and the evaluator applies the pins when the
// findings are assembled, so a version fact that arrives late does not
// need lines re-read that are already gone.
func LogRules(rules []Rule) []logstream.Rule {
	var derived []logstream.Rule
	for _, rule := range rules {
		if matcher, ok := LogRuleOf(rule.When); ok {
			matcher.ID = rule.ID
			matcher.Summary = rule.Summary
			derived = append(derived, matcher)
		}
	}
	return derived
}

// LogRuleOf derives the matcher rule a condition asks for, from either
// log condition, at the top or as one branch of an AllOf. The returned
// rule carries what to match and not the identity, which only the
// catalog rule around it knows.
//
// A rule carries at most one log condition: the matcher keys
// observations by rule ID, so two in one rule would be
// indistinguishable when the rule is evaluated. The catalog's tests
// hold that line.
func LogRuleOf(when Condition) (logstream.Rule, bool) {
	switch condition := when.(type) {
	case LogContains:
		return logstream.Rule{Contains: condition.Substrings, Except: condition.Except}, true
	case LogFields:
		fields := make([]logstream.FieldTest, len(condition.Fields))
		for i, field := range condition.Fields {
			fields[i] = logstream.FieldTest{
				Path: field.Path, Equals: field.Equals, Contains: field.Contains,
			}
		}
		return logstream.Rule{Fields: fields, Except: condition.Except}, true
	case AllOf:
		for _, branch := range condition.Of {
			if found, ok := LogRuleOf(branch); ok {
				return found, true
			}
		}
	}
	return logstream.Rule{}, false
}

// AllOf matches when every branch matches, which is how a rule states a
// corroboration: an operator condition and the log line that explains
// it, on the same run. The honesty rules compose the obvious way — a
// branch that could not run makes the whole condition one that could
// not run, never one that came back clear, because "A and B" cannot be
// ruled out while B is unreadable.
//
// The finding takes its subject, time, ID suffix and link from the
// first branch's first match, and quotes every branch's evidence in
// order: the first branch is the observation the rule is about, the
// rest are what corroborate it.
type AllOf struct {
	Of []Condition
}

func (c AllOf) describe() string {
	parts := make([]string, len(c.Of))
	for i, branch := range c.Of {
		parts[i] = branch.describe()
	}
	return strings.Join(parts, ", together with ")
}

func (c AllOf) evaluate(ruleID string, in Input) ([]conditionMatch, string) {
	if len(c.Of) == 0 {
		return nil, ""
	}
	lead, unavailable := c.Of[0].evaluate(ruleID, in)
	if unavailable != "" {
		return nil, unavailable
	}
	if len(lead) == 0 {
		return nil, ""
	}
	rest := make([][]conditionMatch, 0, len(c.Of)-1)
	for _, branch := range c.Of[1:] {
		matches, unavailable := branch.evaluate(ruleID, in)
		if unavailable != "" {
			return nil, unavailable
		}
		if len(matches) == 0 {
			return nil, ""
		}
		rest = append(rest, matches)
	}
	var out []conditionMatch
	for _, candidate := range lead {
		corroboration, agreed := make([]Evidence, 0), true
		for _, matches := range rest {
			agreeing, found := agreeingMatch(candidate, matches)
			if !found {
				agreed = false
				break
			}
			corroboration = append(corroboration, agreeing.evidence...)
		}
		if !agreed {
			continue
		}
		one := candidate
		one.evidence = append(append([]Evidence{}, candidate.evidence...), corroboration...)
		out = append(out, one)
	}
	return out, ""
}

// agreeingMatch is the first match about the same thing as the
// candidate. Corroboration has to be about one subject: two branches
// matching on different instances are two facts, not one finding, and
// joining them would state a correlation neither source reported.
func agreeingMatch(candidate conditionMatch, matches []conditionMatch) (conditionMatch, bool) {
	for _, match := range matches {
		if subjectsAgree(candidate.subject, match.subject) {
			return match, true
		}
	}
	return conditionMatch{}, false
}

// subjectsAgree reports whether two subjects can belong to one finding.
// A subject that names no object — a cluster-wide condition, a phase —
// corroborates any instance, because it is a fact about all of them.
func subjectsAgree(a, b EntityRef) bool {
	if a.Name == "" || b.Name == "" {
		return true
	}
	return a == b
}

// PrimaryDisagreement matches an instance whose own account of its
// role contradicts the operator's: PostgreSQL's pg_is_in_recovery(),
// read from the exporter, says one thing about which instance accepts
// writes, and the Cluster's currentPrimary says another. It is the one
// condition that compares two sources instead of reading one, and it
// reports the contradiction as such — neither side is presumed right.
//
// A primary move in flight is not a disagreement: while currentPrimary
// and targetPrimary differ, the operator itself says the roles are
// changing, and the instance's answer is expected to lag. The condition
// stays clear until the move settles.
type PrimaryDisagreement struct{}

func (PrimaryDisagreement) describe() string {
	return "an instance whose own recovery state contradicts the operator's current primary"
}

func (PrimaryDisagreement) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	if reason := metricsUnavailable(in); reason != "" {
		return nil, reason
	}
	cluster := in.Cluster.Cluster
	if cluster.CurrentPrimary == "" {
		return nil, "the operator reports no current primary"
	}
	if cluster.TargetPrimary != "" && cluster.TargetPrimary != cluster.CurrentPrimary {
		return nil, ""
	}
	fresh := freshnessOf(in.Metrics, in.Now, "of "+instantMetricName("in-recovery"))
	readings := in.Metrics.InstantReadings()
	var matches []conditionMatch
	for _, instance := range readingInstances(readings) {
		reading, reported := readings[instance]["in-recovery"]
		// This condition sets a live operator report against a scraped
		// one, so a frozen reading is not merely weak evidence: it
		// manufactures the disagreement. An instance that was a standby
		// before a failover still reads as one until the sweep resumes,
		// and the operator has already moved on.
		if !reported || !fresh.current(reading.At) {
			continue
		}
		writable := reading.Value == 0
		if writable == (instance == cluster.CurrentPrimary) {
			continue
		}
		role := "in recovery, so not accepting writes"
		if writable {
			role = "not in recovery, so accepting writes"
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + instance,
			subject:  EntityRef{Kind: "Pod", Name: instance},
			at:       time.Unix(reading.At, 0),
			summary: fmt.Sprintf("Instance %s reports itself %s, while the operator names %s as the primary.",
				instance, role, cluster.CurrentPrimary),
			evidence: []Evidence{
				{
					Origin: "operator-reported",
					Object: "Cluster",
					Detail: "currentPrimary " + cluster.CurrentPrimary,
				},
				{
					Origin: "console-scraped from the instance exporter",
					Object: "instance " + instance,
					Detail: fmt.Sprintf("%s = %g (%s), read %s", instantMetricName("in-recovery"),
						reading.Value, role, time.Unix(reading.At, 0).UTC().Format("15:04:05Z")),
				},
			},
			link:      "/cluster/metrics",
			linkLabel: "Metrics",
		})
	}
	if len(matches) == 0 {
		return nil, fresh.unavailable()
	}
	return matches, ""
}
