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
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// The framework is tested with synthetic rules only: the real catalog
// lives in its own packages and carries its own tests. What is proven
// here is the machinery — gating, condition evaluation, evidence
// assembly — independent of any actual claim.

type staticObservations []logstream.Observation

func (s staticObservations) Observations() []logstream.Observation { return s }

func (s staticObservations) Unread() []logstream.Unread { return nil }

// clusterInput is an input whose operator status reports the given
// PostgreSQL major version.
func clusterInput(major int) Input {
	return Input{
		Now:        now,
		HasCluster: true,
		Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{
			Present:              true,
			PostgresMajorVersion: &major,
		}},
	}
}

// logRule is a synthetic unpinned log rule.
func logRule() Rule {
	return Rule{
		ID:       "example-log",
		Severity: SeverityCritical,
		Summary:  "Example log finding.",
		When:     LogContains{Substrings: []string{"boom"}},
	}
}

// TestLogRuleReportsWhatTheMatcherSaw proves a log-backed rule quotes
// the matched line, marks the stream best effort, states the count as a
// floor, and links to the tail of the exact container.
func TestLogRuleReportsWhatTheMatcherSaw(t *testing.T) {
	t.Parallel()
	in := Input{Now: now, Logs: staticObservations{{
		RuleID:    "example-log",
		Pod:       "orders-1",
		Container: "postgres",
		Line:      "something went boom",
		FirstSeen: now.Add(-time.Hour),
		LastSeen:  now,
		Count:     7,
	}}}

	finding := findingByID(t, Run(in, logRule()), "example-log/orders-1/postgres")
	if finding.Evidence[0].Detail != "something went boom" {
		t.Errorf("line not quoted verbatim: %q", finding.Evidence[0].Detail)
	}
	// The origin must say the evidence is best effort: a stream cannot
	// promise completeness the way a snapshot can.
	if !strings.Contains(finding.Evidence[0].Origin, "best effort") {
		t.Errorf("origin does not mark the stream best effort: %q", finding.Evidence[0].Origin)
	}
	if !strings.Contains(finding.Evidence[1].Detail, "at least 7") {
		t.Errorf("count is not stated as a floor: %q", finding.Evidence[1].Detail)
	}
	if finding.Link != "/logs/orders-1/postgres" {
		t.Errorf("link = %q, want the tail of the container it came from", finding.Link)
	}
}

// TestLogRuleWithFollowingOffCannotBeClear is the honesty property. A
// console that is not reading logs has ruled nothing out in them, so
// every log-backed rule reports that it could not run.
func TestLogRuleWithFollowingOffCannotBeClear(t *testing.T) {
	t.Parallel()
	check, _ := evaluateRule(logRule(), Input{Now: now})
	if check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with following off, want could-not-run", check.Outcome)
	}
}

// TestLogRuleFollowingOnWithNoMatchIsClear proves the other side: with
// following on, the roster readable and every container being read,
// nothing matched is genuinely clear.
func TestLogRuleFollowingOnWithNoMatchIsClear(t *testing.T) {
	t.Parallel()
	check, _ := evaluateRule(logRule(), Input{Now: now, HasPods: true, Logs: staticObservations{}})
	if check.Outcome != CheckClear {
		t.Errorf("outcome = %v with following on and no match, want clear", check.Outcome)
	}
}

// pinnedRule is a synthetic version-only rule: no condition, so the pin
// alone decides.
func pinnedRule() Rule {
	return Rule{
		ID:        "example-pinned",
		Component: ComponentPostgreSQL,
		Requires:  []Requirement{{Component: ComponentPostgreSQL, Constraint: "<14"}},
		Severity:  SeverityWarning,
		Summary:   "Example pinned finding.",
		Describes: "an example version-only rule",
	}
}

// TestPinnedRuleWithUnobservedVersionCannotRun proves the framework's
// own honesty rule: a rule pinned to a version nobody observed answers
// "could not run" with the pin in the reason — never clear, never
// matched.
func TestPinnedRuleWithUnobservedVersionCannotRun(t *testing.T) {
	t.Parallel()
	check, _ := evaluateRule(pinnedRule(), Input{Now: now})
	if check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with no observed version, want could-not-run", check.Outcome)
	}
	if !strings.Contains(check.Because, "PostgreSQL <14") {
		t.Errorf("reason does not state the pin: %q", check.Because)
	}
}

// TestPinnedRuleOutsideItsVersionsDoesNotApply proves the third answer:
// on versions the pin excludes, the rule rules itself out and says so,
// stating both the pin and the observed version.
func TestPinnedRuleOutsideItsVersionsDoesNotApply(t *testing.T) {
	t.Parallel()
	check, findings := evaluateRule(pinnedRule(), clusterInput(17))
	if check.Outcome != CheckNotApplicable {
		t.Errorf("outcome = %v on PostgreSQL 17, want does-not-apply", check.Outcome)
	}
	if !strings.Contains(check.Because, "17") || !strings.Contains(check.Because, "<14") {
		t.Errorf("reason states neither observed version nor pin: %q", check.Because)
	}
	if len(findings) != 0 {
		t.Error("a rule that does not apply reported findings")
	}
}

// TestVersionOnlyRuleFiresOnThePinAlone proves a rule with no condition
// matches whenever it applies, and its finding quotes the version fact
// it rests on.
func TestVersionOnlyRuleFiresOnThePinAlone(t *testing.T) {
	t.Parallel()
	check, findings := evaluateRule(pinnedRule(), clusterInput(13))
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v, findings = %d, want one match", check.Outcome, len(findings))
	}
	finding := findings[0]
	if len(finding.Evidence) == 0 {
		t.Fatal("finding carries no evidence")
	}
	if !strings.Contains(finding.Evidence[0].Detail, "13") {
		t.Errorf("evidence does not quote the observed version: %+v", finding.Evidence[0])
	}
	if finding.Evidence[0].Origin != "operator-reported" {
		t.Errorf("evidence origin = %q", finding.Evidence[0].Origin)
	}
}

// TestPinnedLogRuleQuotesTheVersionBeneathTheFinding proves the two
// evidence kinds compose: a pinned, condition-bearing rule quotes the
// observation first and the version fact that made the rule apply after.
func TestPinnedLogRuleQuotesTheVersionBeneathTheFinding(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:        "example-pinned-log",
		Component: ComponentPostgreSQL,
		Requires:  []Requirement{{Component: ComponentPostgreSQL, Constraint: ">=13"}},
		Severity:  SeverityCritical,
		Summary:   "Example.",
		When:      LogContains{Substrings: []string{"boom"}},
	}
	in := clusterInput(17)
	in.Logs = staticObservations{{RuleID: "example-pinned-log", Pod: "p", Container: "c", Line: "boom", Count: 1}}

	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched {
		t.Fatalf("outcome = %v, want matched", check.Outcome)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	last := findings[0].Evidence[len(findings[0].Evidence)-1]
	if !strings.Contains(last.Detail, "17") {
		t.Errorf("version fact not quoted beneath the finding: %+v", findings[0].Evidence)
	}
}

// TestEventConditionQuotesTheEvent proves the declarative event
// condition behaves like the hand-written event-backed detectors: the
// finding quotes the API server's words, and an unobserved event window
// is could-not-run.
func TestEventConditionQuotesTheEvent(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:       "example-event",
		Severity: SeverityWarning,
		Summary:  "Example.",
		When:     EventMatch{Reasons: []string{"FailedMount"}, MessageContains: []string{"secret"}},
	}

	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with events unobserved, want could-not-run", check.Outcome)
	}

	in := Input{Now: now, HasEvents: true, Events: observe.EventsSnapshot{Events: []observe.EventFacts{
		{
			Type: "Warning", Reason: "FailedMount", Kind: "Pod", Object: "orders-1",
			Message: `MountVolume.SetUp failed for volume "certs" : secret "tls" not found`,
		},
		{
			Type: "Warning", Reason: "FailedMount", Kind: "Pod", Object: "orders-1",
			Message: "unrelated",
		},
	}}}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched {
		t.Fatalf("outcome = %v, want matched", check.Outcome)
	}
	if len(findings) != 1 || len(findings[0].Evidence) != 1 {
		t.Fatalf("want one finding quoting the one narrowed event, got %+v", findings)
	}
	if !strings.Contains(findings[0].Evidence[0].Detail, `secret "tls" not found`) {
		t.Errorf("event not quoted verbatim: %q", findings[0].Evidence[0].Detail)
	}
}

// TestEventConditionAcceptsDeclaredTypes proves the Types field: some
// operator failures are recorded as Normal events, and a rule declaring
// that must see them while the default stays Warning-only.
func TestEventConditionAcceptsDeclaredTypes(t *testing.T) {
	t.Parallel()
	in := Input{Now: now, HasEvents: true, Events: observe.EventsSnapshot{Events: []observe.EventFacts{
		{Type: "Normal", Reason: "Failed", Kind: "Cluster", Object: "orders", Message: "Backup failed"},
	}}}

	defaulted := Rule{
		ID: "warning-only", Severity: SeverityWarning, Summary: "Example.",
		When: EventMatch{Reasons: []string{"Failed"}},
	}
	if check, _ := evaluateRule(defaulted, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v, want clear: a Normal event must not match the Warning default", check.Outcome)
	}

	declared := Rule{
		ID: "normal-too", Severity: SeverityWarning, Summary: "Example.",
		When: EventMatch{Reasons: []string{"Failed"}, Types: []string{"Normal"}},
	}
	if check, _ := evaluateRule(declared, in); check.Outcome != CheckMatched {
		t.Errorf("outcome = %v, want matched with the type declared", check.Outcome)
	}
}

// TestClusterConditionQuotesTheOperator proves the status-backed
// condition: it matches only the exact type, status and — when set —
// reason, quotes reason and message, treats an absent condition as
// clear, and an unobserved or absent Cluster as could-not-run.
func TestClusterConditionQuotesTheOperator(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:       "example-condition",
		Severity: SeverityCritical,
		Summary:  "Example.",
		When:     ClusterCondition{Type: "ContinuousArchiving", Status: "False"},
	}

	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with no Cluster observed, want could-not-run", check.Outcome)
	}
	if check, _ := evaluateRule(rule, Input{Now: now, HasCluster: true}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with the Cluster absent, want could-not-run", check.Outcome)
	}

	in := clusterInput(17)
	in.Cluster.Cluster.Conditions = []observe.Condition{
		{Type: "Ready", Status: "True"},
		{
			Type: "ContinuousArchiving", Status: "False", Reason: "Failing",
			Message: "unexpected failure invoking barman-cloud-wal-archive",
		},
	}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched {
		t.Fatalf("outcome = %v, want matched", check.Outcome)
	}
	detail := findings[0].Evidence[0].Detail
	if !strings.Contains(detail, "Failing") || !strings.Contains(detail, "barman-cloud-wal-archive") {
		t.Errorf("reason and message not quoted: %q", detail)
	}

	// The same condition at True is not the sought status.
	in.Cluster.Cluster.Conditions[1].Status = "True"
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with the condition True, want clear", check.Outcome)
	}

	// A declared reason narrows: the same status under another reason is
	// not a match.
	narrowed := rule
	narrowed.When = ClusterCondition{Type: "ContinuousArchiving", Status: "False", Reason: "Other"}
	in.Cluster.Cluster.Conditions[1].Status = "False"
	if check, _ := evaluateRule(narrowed, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with another reason, want clear", check.Outcome)
	}
}

// TestClusterPhaseQuotesPhaseAndReason proves the phase condition
// matches exactly and carries the operator's reason.
func TestClusterPhaseQuotesPhaseAndReason(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:       "example-phase",
		Severity: SeverityWarning,
		Summary:  "Example.",
		When:     ClusterPhase{AnyOf: []string{"Cluster in unrecoverable state", "Setting up primary"}},
	}
	in := clusterInput(17)
	in.Cluster.Cluster.Phase = "Cluster in unrecoverable state"
	in.Cluster.Cluster.PhaseReason = "the instances are unrecoverable"

	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched {
		t.Fatalf("outcome = %v, want matched", check.Outcome)
	}
	detail := findings[0].Evidence[0].Detail
	if !strings.Contains(detail, "unrecoverable state") || !strings.Contains(detail, "the instances are unrecoverable") {
		t.Errorf("phase and reason not quoted: %q", detail)
	}

	in.Cluster.Cluster.Phase = "Cluster in healthy state"
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v on another phase, want clear", check.Outcome)
	}
}

// staticWindow is a MetricsWindow serving fixed instant readings.
type staticWindow map[string]map[string]metrics.Instant

func (w staticWindow) Interval() time.Duration { return metrics.DefaultInterval }

func (w staticWindow) Instances() []string { return nil }
func (w staticWindow) Range(string, metrics.Tier) ([]int64, map[string][]*float64) {
	return nil, nil
}
func (w staticWindow) InstantReadings() map[string]map[string]metrics.Instant { return w }

// TestInstantNonZeroReadsTheScrapedFlag proves the metric-backed
// condition: per-instance findings for non-zero readings, the exporter's
// own metric name and the read time in the evidence, zero and unreported
// readings clear, and no window at all could-not-run.
func TestInstantNonZeroReadsTheScrapedFlag(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:       "example-instant",
		Severity: SeverityWarning,
		Summary:  "Example.",
		When:     InstantNonZero{Key: "fencing-on"},
	}

	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with no metrics window, want could-not-run", check.Outcome)
	}

	in := Input{Now: now, Metrics: staticWindow{
		"orders-1": {"fencing-on": {At: now.Unix(), Value: 1}},
		"orders-2": {"fencing-on": {At: now.Unix(), Value: 0}},
		"orders-3": {},
	}}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched {
		t.Fatalf("outcome = %v, want matched", check.Outcome)
	}
	if len(findings) != 1 || findings[0].ID != "example-instant/orders-1" {
		t.Fatalf("want one finding for the fenced instance, got %+v", findings)
	}
	detail := findings[0].Evidence[0].Detail
	if !strings.Contains(detail, "cnpg_collector_fencing_on") {
		t.Errorf("evidence does not name the exporter's metric: %q", detail)
	}
	if !strings.Contains(detail, "read ") {
		t.Errorf("evidence does not date the reading: %q", detail)
	}

	in.Metrics = staticWindow{"orders-2": {"fencing-on": {At: now.Unix(), Value: 0}}}
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with the flag at zero, want clear", check.Outcome)
	}
}

// TestContainerStateReadsTheKubeletsWord proves the container-state
// condition: it matches the kubelet's exact reason, quotes state,
// restarts and exit code, spans pooler pods when they are observed, and
// reports could-not-run when no pods have been.
func TestContainerStateReadsTheKubeletsWord(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:       "example-container",
		Severity: SeverityCritical,
		Summary:  "Example.",
		When:     ContainerState{Reasons: []string{"CrashLoopBackOff", "OOMKilled"}},
	}

	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with no pods observed, want could-not-run", check.Outcome)
	}

	restarts, exit := 7, int32(137)
	in := Input{
		Now:     now,
		HasPods: true,
		Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
			Name: "orders-1",
			Containers: []observe.ContainerFacts{
				{Name: "postgres", State: "running"},
				{
					Name: "plugin-barman-cloud", State: "waiting", Reason: "CrashLoopBackOff",
					Restarts: &restarts,
				},
			},
		}}},
		HasPoolerPods: true,
		PoolerPods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
			Name: "orders-pool-rw-abc",
			Containers: []observe.ContainerFacts{
				{Name: "pgbouncer", State: "terminated", Reason: "OOMKilled", ExitCode: &exit},
			},
		}}},
	}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched || len(findings) != 2 {
		t.Fatalf("outcome = %v, findings = %d, want two matches", check.Outcome, len(findings))
	}
	sidecar := findingByID(t, Result{Findings: findings}, "example-container/orders-1/plugin-barman-cloud")
	if !strings.Contains(sidecar.Evidence[0].Detail, "CrashLoopBackOff") ||
		!strings.Contains(sidecar.Evidence[0].Detail, "7 restarts") {
		t.Errorf("kubelet reason and restarts not quoted: %q", sidecar.Evidence[0].Detail)
	}
	pooler := findingByID(t, Result{Findings: findings}, "example-container/orders-pool-rw-abc/pgbouncer")
	if !strings.Contains(pooler.Evidence[0].Detail, "OOMKilled") ||
		!strings.Contains(pooler.Evidence[0].Detail, "exit code 137") {
		t.Errorf("pooler container state not quoted: %q", pooler.Evidence[0].Detail)
	}
}

// TestPrimaryMismatchNeedsTheOperatorsClock proves the stuck-move
// condition rests entirely on operator-reported facts: disagreeing
// primaries alone are not enough, the operator's own request timestamp
// must be old enough, and without that timestamp nothing matches
// because no honest duration exists.
func TestPrimaryMismatchNeedsTheOperatorsClock(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:       "example-mismatch",
		Severity: SeverityCritical,
		Summary:  "Example.",
		When:     PrimaryMismatch{MinAge: 10 * time.Minute},
	}

	in := clusterInput(17)
	in.Cluster.Cluster.CurrentPrimary = "orders-1"
	in.Cluster.Cluster.TargetPrimary = "pending"
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v without the operator's timestamp, want clear", check.Outcome)
	}

	young := now.Add(-time.Minute)
	in.Cluster.Cluster.TargetPrimaryTimestamp = &young
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v on a young move, want clear", check.Outcome)
	}

	old := now.Add(-30 * time.Minute)
	in.Cluster.Cluster.TargetPrimaryTimestamp = &old
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched {
		t.Fatalf("outcome = %v on a thirty-minute move, want matched", check.Outcome)
	}
	detail := findings[0].Evidence[0].Detail
	if !strings.Contains(detail, "orders-1") || !strings.Contains(detail, "30m0s ago") {
		t.Errorf("primaries and age not quoted: %q", detail)
	}
	if !strings.Contains(detail, "no candidate chosen") {
		t.Errorf("the pending marker is not explained: %q", detail)
	}

	in.Cluster.Cluster.TargetPrimary = "orders-1"
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with primaries agreeing, want clear", check.Outcome)
	}
}

// TestScheduledBackupConditions proves the two schedule conditions: a
// suspended schedule matches one rule and is excluded from the other,
// an overdue next run matches with the grace honoured, and a stale
// catalog is could-not-run for both.
func TestScheduledBackupConditions(t *testing.T) {
	t.Parallel()
	suspended := true
	pastNext := now.Add(-2 * time.Hour)
	futureNext := now.Add(time.Hour)
	in := Input{Now: now, HasBackups: true, Backups: observe.BackupsSnapshot{
		ScheduledBackups: []observe.ScheduledBackupFacts{
			{Name: "paused", Suspended: &suspended, NextScheduleTime: &pastNext},
			{Name: "stopped", NextScheduleTime: &pastNext},
			{Name: "healthy", NextScheduleTime: &futureNext},
		},
	}}

	suspendedRule := Rule{
		ID: "example-suspended", Severity: SeverityWarning, Summary: "Example.",
		When: ScheduledBackupSuspended{},
	}
	check, findings := evaluateRule(suspendedRule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].ID != "example-suspended/paused" {
		t.Fatalf("suspended: outcome %v, findings %+v", check.Outcome, findings)
	}

	overdueRule := Rule{
		ID: "example-overdue", Severity: SeverityWarning, Summary: "Example.",
		When: ScheduledBackupOverdue{Grace: 30 * time.Minute},
	}
	check, findings = evaluateRule(overdueRule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].ID != "example-overdue/stopped" {
		t.Fatalf("overdue: outcome %v, findings %+v", check.Outcome, findings)
	}
	if !strings.Contains(findings[0].Evidence[0].Detail, "past") {
		t.Errorf("overdue evidence does not state how late: %q", findings[0].Evidence[0].Detail)
	}

	in.Backups.Stale = true
	if check, _ := evaluateRule(overdueRule, in); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v on a stale catalog, want could-not-run", check.Outcome)
	}
}

// seriesWindow is a MetricsWindow serving one fixed series per tier.
type seriesWindow struct {
	raw, rollup map[string][]*float64
	times       []int64
}

func (w seriesWindow) Interval() time.Duration { return metrics.DefaultInterval }

func (w seriesWindow) Instances() []string { return nil }
func (w seriesWindow) Range(_ string, tier metrics.Tier) ([]int64, map[string][]*float64) {
	if tier == metrics.TierRaw {
		return w.times, w.raw
	}
	return w.times, w.rollup
}
func (w seriesWindow) InstantReadings() map[string]map[string]metrics.Instant { return nil }

// TestSeriesAboveReadsTheLatestSample proves the series condition: the
// latest non-nil sample decides, the rollup tier answers when raw holds
// nothing, and the evidence quotes the exporter's metric name.
func TestSeriesAboveReadsTheLatestSample(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:       "example-series",
		Severity: SeverityCritical,
		Summary:  "Example.",
		When:     SeriesAbove{Key: "xid-age", Threshold: 1_600_000_000},
	}

	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with no metrics window, want could-not-run", check.Outcome)
	}

	high, low, older := 1_700_000_000.0, 12_000.0, 1_900_000_000.0
	in := Input{Now: now, Metrics: seriesWindow{
		times: []int64{now.Unix() - 60, now.Unix()},
		raw: map[string][]*float64{
			"orders-1": {&older, &high}, // latest sample decides
			"orders-2": {nil, &low},
		},
	}}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].ID != "example-series/orders-1" {
		t.Fatalf("outcome %v, findings %+v", check.Outcome, findings)
	}
	if !strings.Contains(findings[0].Evidence[0].Detail, "cnpg_pg_database_xid_age") {
		t.Errorf("evidence does not name the exporter's metric: %q", findings[0].Evidence[0].Detail)
	}

	// An empty raw window falls back to the rollup tier.
	in.Metrics = seriesWindow{
		times:  []int64{now.Unix()},
		raw:    map[string][]*float64{},
		rollup: map[string][]*float64{"orders-1": {&high}},
	}
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckMatched {
		t.Errorf("outcome = %v from the rollup tier, want matched", check.Outcome)
	}
}

// TestDeclaredObjectFailedQuotesTheOperator proves the declarative
// condition: only a reported failure matches — never an unreported
// object — and the operator's message rides the evidence.
func TestDeclaredObjectFailedQuotesTheOperator(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID: "example-declared", Severity: SeverityWarning, Summary: "Example.",
		When: DeclaredObjectFailed{},
	}

	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with objects unobserved, want could-not-run", check.Outcome)
	}

	applied, failed := true, false
	in := Input{Now: now, HasDatabaseObjects: true, DatabaseObjects: observe.DatabaseObjectsSnapshot{
		Databases: []observe.DatabaseFacts{
			{Name: "orders-db", Declared: observe.Declared{
				Applied: &failed,
				Message: `role "app_owner" does not exist`,
			}},
			{Name: "fine-db", Declared: observe.Declared{Applied: &applied}},
			{Name: "new-db"}, // unreported is not failed
		},
		Subscriptions: []observe.SubscriptionFacts{
			{Name: "orders-sub", Declared: observe.Declared{
				Applied: &failed,
				Message: "could not connect to the publisher",
			}},
		},
	}}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched || len(findings) != 2 {
		t.Fatalf("outcome %v, findings %d, want two failures", check.Outcome, len(findings))
	}
	db := findingByID(t, Result{Findings: findings}, "example-declared/database/orders-db")
	if !strings.Contains(db.Evidence[0].Detail, `role "app_owner" does not exist`) {
		t.Errorf("operator message not quoted: %q", db.Evidence[0].Detail)
	}
	if !strings.Contains(db.Summary, "Database") || !strings.Contains(db.Summary, "orders-db") {
		t.Errorf("summary does not name the object: %q", db.Summary)
	}
}

// TestBackupPhaseHonoursAgeAndStaleness proves the backup condition:
// phases match case-insensitively, young backups are exempted by
// MinAge, and a stale catalog is could-not-run rather than a claim
// about current phases.
func TestBackupPhaseHonoursAgeAndStaleness(t *testing.T) {
	t.Parallel()
	rule := Rule{
		ID:       "example-backup",
		Severity: SeverityWarning,
		Summary:  "Example.",
		When:     BackupPhase{AnyOf: []string{"pending"}, MinAge: 30 * time.Minute},
	}

	in := Input{Now: now, HasBackups: true, Backups: observe.BackupsSnapshot{Backups: []observe.BackupFacts{
		{Name: "old-pending", Phase: "Pending", CreatedAt: now.Add(-time.Hour)},
		{Name: "new-pending", Phase: "pending", CreatedAt: now.Add(-time.Minute)},
	}}}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched {
		t.Fatalf("outcome = %v, want matched", check.Outcome)
	}
	if len(findings) != 1 || findings[0].ID != "example-backup/old-pending" {
		t.Fatalf("want the old pending backup only, got %+v", findings)
	}

	in.Backups.Stale = true
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v on a stale catalog, want could-not-run", check.Outcome)
	}
}
