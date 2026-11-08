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

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// TestRelationHoldsOnScopeAndWindow pins the terms a relation is
// allowed to claim: the named cause only, the same pod where the scope
// says so, and a bounded gap where both findings carry a time. Where
// one side has no time, the window is not enforced — the relation rests
// on scope alone rather than inventing an instant.
func TestRelationHoldsOnScopeAndWindow(t *testing.T) {
	t.Parallel()
	pod1 := EntityRef{Kind: "Pod", Name: "orders-1"}
	pod2 := EntityRef{Kind: "Pod", Name: "orders-2"}
	for name, tc := range map[string]struct {
		relation      Relation
		effect, cause Finding
		want          bool
		reason        string
	}{
		"wrong cause": {
			Relation{Cause: "disk"}, Finding{Check: "crash"}, Finding{Check: "oom"}, false, "not the named cause",
		},
		"cluster scope joins any objects": {
			Relation{Cause: "disk"}, Finding{Check: "crash", Subject: pod1}, Finding{Check: "disk", Subject: pod2}, true, "",
		},
		"pod scope joins the same pod": {
			Relation{Cause: "disk", Scope: ScopePod}, Finding{Check: "crash", Subject: pod1}, Finding{Check: "disk", Subject: pod1}, true, "",
		},
		"pod scope refuses another pod": {
			Relation{Cause: "disk", Scope: ScopePod}, Finding{Check: "crash", Subject: pod1}, Finding{Check: "disk", Subject: pod2}, false, "Pod/orders-2",
		},
		"pod scope refuses an unnamed side": {
			Relation{Cause: "disk", Scope: ScopePod}, Finding{Check: "crash", Subject: pod1}, Finding{Check: "disk", Subject: clusterSubject}, false, "names none",
		},
		"window admits a close pair": {
			Relation{Cause: "disk", Within: time.Hour}, Finding{Check: "crash", At: now}, Finding{Check: "disk", At: now.Add(-30 * time.Minute)}, true, "",
		},
		"window refuses a distant pair either way round": {
			Relation{Cause: "disk", Within: time.Hour}, Finding{Check: "crash", At: now.Add(-3 * time.Hour)}, Finding{Check: "disk", At: now}, false, "3h0m0s apart",
		},
		"window is not enforced without both times": {
			Relation{Cause: "disk", Within: time.Hour}, Finding{Check: "crash"}, Finding{Check: "disk", At: now.Add(-3 * time.Hour)}, true, "",
		},
	} {
		got, because := tc.relation.Holds(tc.effect, tc.cause)
		if got != tc.want {
			t.Errorf("%s: holds = %v (%q), want %v", name, got, because, tc.want)
		}
		if !strings.Contains(because, tc.reason) {
			t.Errorf("%s: reason %q does not say %q", name, because, tc.reason)
		}
	}
	if s := (Relation{Cause: "x", Scope: ScopePod, Within: time.Hour, Strength: StrengthPlausible}).String(); s != "plausible · same pod · within 1h0m0s" {
		t.Errorf("relation terms = %q", s)
	}
}

// TestRelationTermsStateOnlyWhatWasEnforced proves the label beneath a
// nested finding describes the admission, not the declaration: a window
// appears only when both findings carried a time and it was applied,
// and otherwise the label says the window was not enforced and why.
func TestRelationTermsStateOnlyWhatWasEnforced(t *testing.T) {
	t.Parallel()
	timed := Relation{Cause: "x", Scope: ScopePod, Within: time.Hour}
	for name, tc := range map[string]struct {
		relation      Relation
		effect, cause Finding
		want          string
	}{
		"no window declared": {
			Relation{Cause: "x"},
			Finding{},
			Finding{},
			"established mechanism · same cluster",
		},
		"both timed": {
			timed,
			Finding{At: now},
			Finding{At: now.Add(-time.Minute)},
			"established mechanism · same pod · within 1h0m0s",
		},
		"one untimed": {
			timed,
			Finding{At: now},
			Finding{},
			"established mechanism · same pod · window not enforced: one finding carries no time",
		},
		"neither timed": {
			timed,
			Finding{},
			Finding{},
			"established mechanism · same pod · window not enforced: neither finding carries a time",
		},
	} {
		if got := tc.relation.Terms(tc.effect, tc.cause); got != tc.want {
			t.Errorf("%s: terms = %q, want %q", name, got, tc.want)
		}
	}
}

// TestConditionsNameTheirSubjectAndTime proves each condition hands the
// finding the object it is about and the time its source reported, so
// a relation can ask "same pod, and when" instead of assuming.
func TestConditionsNameTheirSubjectAndTime(t *testing.T) {
	t.Parallel()
	seen := now.Add(-5 * time.Minute)
	// A scraped reading is judged against the sweep cadence, so this one
	// is a sweep old rather than five minutes: the subject and time it
	// carries are what this test is about, and a refused reading would
	// never get far enough to carry them.
	swept := now.Add(-30 * time.Second)
	moved := now.Add(-20 * time.Minute)
	created := now.Add(-time.Hour)

	logs := clusterInput(17)
	logs.Logs = staticObservations{{
		RuleID: "subject", Pod: "orders-1", Container: "postgres",
		Line: "boom", FirstSeen: seen, LastSeen: seen, Count: 1,
	}}
	events := withEvents(warning("Pod", "orders-2", "Evicted", "The node was low on resource"))
	events.Events.Events[0].LastSeen = seen
	backups := Input{Now: now, HasBackups: true, Backups: observe.BackupsSnapshot{
		Backups: []observe.BackupFacts{{Name: "orders-1", Phase: "failed", CreatedAt: created}},
	}}
	cluster := clusterInput(17)
	cluster.Cluster.Cluster.Phase = "Waiting for user action"
	primary := clusterInput(17)
	primary.Cluster.Cluster.CurrentPrimary = "orders-1"
	primary.Cluster.Cluster.TargetPrimary = "orders-2"
	primary.Cluster.Cluster.TargetPrimaryTimestamp = &moved
	pods := Input{Now: now, HasPods: true, Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
		Name: "orders-3", Containers: []observe.ContainerFacts{{Name: "postgres", Reason: "OOMKilled"}},
	}}}}
	metric := Input{Now: now, Metrics: staticWindow{"orders-2": {"fencing-on": {At: swept.Unix(), Value: 1}}}}
	objects := Input{Now: now, HasDatabaseObjects: true, DatabaseObjects: observe.DatabaseObjectsSnapshot{
		Databases: []observe.DatabaseFacts{{Name: "app", Declared: observe.Declared{Applied: boolPtr(false)}}},
	}}
	failing := clusterInput(17)
	failing.Cluster.Cluster.CurrentPrimary = "orders-1"
	failing.Cluster.Cluster.PrimaryFailingSince = &moved
	four := 4
	held := heldInput(observe.ClusterFacts{
		Phase: "Creating a new replica", FailedInstances: []string{"orders-4"}, DanglingPVCs: []string{"orders-4"},
		TimelineID: &four, InstanceTimelines: []observe.InstanceTimeline{{Instance: "orders-2", TimelineID: 3}},
		CertificateExpirations: []observe.CertificateExpiry{{Secret: "orders-ca", ExpiresAt: now.Add(-time.Hour)}},
		UnreconcilableRoles:    []observe.RoleProblem{{Role: "app"}},
		Tablespaces:            []observe.TablespaceFacts{{Name: "fast", Error: "disk missing"}},
		Conditions:             []observe.Condition{{Type: "Ready", Status: "False", LastTransition: &moved}},
	}, now.Add(-time.Hour))
	poolers := Input{Now: now, HasPoolers: true, Poolers: observe.PoolersSnapshot{
		Poolers: []observe.PoolerFacts{{Name: "orders-rw", Phase: "failed"}},
	}}
	status := statusInput(
		instancestatus.Reading{
			Instance: "orders-1", IsPrimary: true, PendingRestart: true, LastFailedWALTime: &moved,
			ReadyWALFiles: 40, InstanceManagerVersion: "1.30.0", Slots: []instancestatus.Slot{{Name: "debezium"}},
		},
		instancestatus.Reading{Instance: "orders-2", InstanceManagerVersion: "1.29.2"})
	fifteen := int32(15)
	lease := leaseInput(observe.PrimaryLeaseFacts{Present: true, Holder: "orders-2", RenewedAt: &moved, DurationSeconds: &fifteen},
		"orders-1", "orders-1")
	for name, tc := range map[string]struct {
		when    Condition
		in      Input
		subject EntityRef
		at      time.Time
	}{
		"log":       {LogContains{Substrings: []string{"boom"}}, logs, EntityRef{Kind: "Pod", Name: "orders-1"}, seen},
		"event":     {EventMatch{Reasons: []string{"Evicted"}}, events, EntityRef{Kind: "Pod", Name: "orders-2"}, seen},
		"backup":    {BackupPhase{AnyOf: []string{"failed"}}, backups, EntityRef{Kind: "Backup", Name: "orders-1"}, created},
		"phase":     {ClusterPhase{AnyOf: []string{"Waiting for user action"}}, cluster, clusterSubject, time.Time{}},
		"primary":   {PrimaryMismatch{MinAge: time.Minute}, primary, clusterSubject, moved},
		"container": {ContainerState{Reasons: []string{"OOMKilled"}}, pods, EntityRef{Kind: "Pod", Name: "orders-3"}, time.Time{}},
		"metric":    {InstantNonZero{Key: "fencing-on"}, metric, EntityRef{Kind: "Pod", Name: "orders-2"}, swept},
		"declared":  {DeclaredObjectFailed{}, objects, EntityRef{Kind: "Database", Name: "app"}, time.Time{}},
		"held-phase": {
			ClusterPhaseHeld{AnyOf: []string{"Creating a new replica"}, MinAge: time.Minute},
			held, clusterSubject,
			time.Time{},
		},
		"held-condition": {
			ClusterCondition{Type: "Ready", Status: "False", MinAge: time.Minute},
			held, clusterSubject,
			time.Time{},
		},
		"failed-instance": {StatusListed{List: ListFailedInstances}, held, EntityRef{Kind: "Pod", Name: "orders-4"}, time.Time{}},
		"dangling-pvc": {
			StatusListed{List: ListDanglingPVC, MinAge: time.Minute},
			held,
			EntityRef{Kind: "PersistentVolumeClaim", Name: "orders-4"},
			time.Time{},
		},
		"primary-failing": {PrimaryFailing{MinAge: time.Minute}, failing, EntityRef{Kind: "Pod", Name: "orders-1"}, moved},
		"timeline":        {TimelineDivergence{MinAge: time.Minute}, held, EntityRef{Kind: "Pod", Name: "orders-2"}, time.Time{}},
		"certificate":     {CertificateExpiring{}, held, EntityRef{Kind: "Secret", Name: "orders-ca"}, time.Time{}},
		"role":            {RoleUnreconcilable{}, held, EntityRef{Kind: "ManagedRole", Name: "app"}, time.Time{}},
		"tablespace":      {TablespaceError{}, held, EntityRef{Kind: "Tablespace", Name: "fast"}, time.Time{}},
		"pooler-phase":    {PoolerPhase{AnyOf: []string{"failed"}}, poolers, EntityRef{Kind: "Pooler", Name: "orders-rw"}, time.Time{}},
		"lease-expired":   {PrimaryLeaseExpired{Grace: time.Minute}, lease, EntityRef{Kind: "Pod", Name: "orders-2"}, moved},
		"lease-holder":    {PrimaryLeaseHolderMismatch{}, lease, EntityRef{Kind: "Pod", Name: "orders-2"}, time.Time{}},
		"status-flag":     {InstanceFlagSet{Flag: FlagPendingRestart}, status, EntityRef{Kind: "Pod", Name: "orders-1"}, now},
		"status-archive":  {InstanceArchiveFailing{}, status, EntityRef{Kind: "Pod", Name: "orders-1"}, moved},
		"status-wal":      {InstanceReadyWAL{Threshold: 32}, status, EntityRef{Kind: "Pod", Name: "orders-1"}, now},
		"status-slot":     {InstanceSlotInactive{}, status, EntityRef{Kind: "Pod", Name: "orders-1"}, now},
		"status-drift":    {InstanceManagerDrift{}, status, clusterSubject, time.Time{}},
	} {
		rule := Rule{ID: "subject", Summary: "Subject.", When: tc.when}
		check, findings := evaluateRule(rule, tc.in)
		if check.Outcome != CheckMatched || len(findings) != 1 {
			t.Fatalf("%s: outcome %v with %d findings (%s), want one match", name, check.Outcome, len(findings), check.Because)
		}
		if findings[0].Subject != tc.subject {
			t.Errorf("%s: subject = %+v, want %+v", name, findings[0].Subject, tc.subject)
		}
		// PodSubjectOf is a hand-written classification, and a
		// hand-written classification drifts. Holding it against what
		// the condition actually produced keeps it grounded in
		// behaviour: a condition classified as always naming a pod that
		// names something else is caught here rather than by a relation
		// that quietly stops holding.
		switch naming := PodSubjectOf(tc.when); naming {
		case PodSubjectAlways:
			if tc.subject.Kind != "Pod" {
				t.Errorf("%s: classified as always naming a pod, but named %s", name, tc.subject)
			}
		case PodSubjectNever:
			if tc.subject.Kind == "Pod" {
				t.Errorf("%s: classified as never naming a pod, but named %s", name, tc.subject)
			}
		case PodSubjectSometimes:
			// Nothing to hold: either kind is consistent with it.
		}
		if !findings[0].At.Equal(tc.at) {
			t.Errorf("%s: at = %v, want %v", name, findings[0].At, tc.at)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// TestAllOfComposesTheHonestyRules proves the combinator: every branch
// must match for a finding, a branch that could not run makes the whole
// condition one that could not run rather than one that came back
// clear, and the finding takes the first branch's subject and quotes
// every branch's evidence.
func TestAllOfComposesTheHonestyRules(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "corroborated", Summary: "Corroborated.", When: AllOf{Of: []Condition{
		ClusterCondition{Type: "ContinuousArchiving", Status: "False"},
		EventMatch{Reasons: []string{"RetentionPolicyFailed"}},
	}}}

	failing := clusterInput(17)
	failing.Cluster.Cluster.Conditions = []observe.Condition{{Type: "ContinuousArchiving", Status: "False", Reason: "Failing"}}

	// The condition holds, the events were never observed: not clear.
	if check, _ := evaluateRule(rule, failing); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with one branch unreadable, want could-not-run", check.Outcome)
	}
	// Both readable, only one matching: clear.
	quiet := failing
	quiet.HasEvents = true
	if check, _ := evaluateRule(rule, quiet); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with one branch unmatched, want clear", check.Outcome)
	}
	// Both matching: one finding, the cluster's subject, both quotes.
	both := failing
	both.HasEvents = true
	both.Events = observe.EventsSnapshot{Events: []observe.EventFacts{
		warning("Cluster", "orders", "RetentionPolicyFailed", "retention failed: AccessDenied"),
	}}
	check, findings := evaluateRule(rule, both)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v with %d findings, want one match", check.Outcome, len(findings))
	}
	if findings[0].Subject != clusterSubject {
		t.Errorf("subject = %+v, want the first branch's", findings[0].Subject)
	}
	var condition, event bool
	for _, evidence := range findings[0].Evidence {
		condition = condition || strings.Contains(evidence.Detail, "Failing")
		event = event || strings.Contains(evidence.Detail, "AccessDenied")
	}
	if !condition || !event {
		t.Errorf("evidence does not quote both branches: %+v", findings[0].Evidence)
	}
	if !strings.Contains(check.Describes, "together with") {
		t.Errorf("describes = %q, want both branches named", check.Describes)
	}
}

// TestLogConditionIsFoundInsideAllOf proves the matcher is fed a log
// condition wherever the rule carries it, so a corroborating rule's log
// line is followed continuously like any other.
func TestLogConditionIsFoundInsideAllOf(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		{ID: "plain", Summary: "Plain.", When: LogContains{Substrings: []string{"a"}}},
		{ID: "nested", Summary: "Nested.", When: AllOf{Of: []Condition{
			ClusterPhase{AnyOf: []string{"x"}}, LogContains{Substrings: []string{"b"}},
		}}},
		{ID: "none", Summary: "None.", When: ClusterPhase{AnyOf: []string{"y"}}},
	}
	derived := LogRules(rules)
	if len(derived) != 2 || derived[0].ID != "plain" || derived[1].ID != "nested" ||
		derived[1].Contains[0] != "b" {
		t.Errorf("derived = %+v, want the plain and the nested log conditions", derived)
	}
	// A matched nested log condition reaches the finding under the
	// rule's own ID.
	in := clusterInput(17)
	in.Cluster.Cluster.Phase = "x"
	in.Logs = staticObservations{{RuleID: "nested", Pod: "orders-1", Container: "postgres", Line: "b", Count: 1}}
	if check, findings := evaluateRule(rules[1], in); check.Outcome != CheckMatched || len(findings) != 1 {
		t.Errorf("nested log rule = %v (%s)", check.Outcome, check.Because)
	}
	if _, ok := LogRuleOf(rules[2].When); ok {
		t.Error("a rule without a log condition reported one")
	}
}

// TestPrimaryDisagreementComparesTwoSources proves the one condition
// that reads two sources: it cannot run without either, it is clear
// when they agree or while a move is in flight, and it reports the
// contradiction — quoting both — when an instance's own recovery state
// contradicts the operator's primary.
func TestPrimaryDisagreementComparesTwoSources(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "disagree", Summary: "Disagree.", When: PrimaryDisagreement{}}
	readAt := now.Add(-time.Minute).Unix()
	agree := staticWindow{
		"orders-1": {"in-recovery": {At: readAt, Value: 0}},
		"orders-2": {"in-recovery": {At: readAt, Value: 1}},
	}
	in := clusterInput(17)
	in.Cluster.Cluster.CurrentPrimary = "orders-1"
	in.Cluster.Cluster.TargetPrimary = "orders-1"

	if check, _ := evaluateRule(rule, Input{Now: now, Metrics: agree}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v without the Cluster, want could-not-run", check.Outcome)
	}
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v without metrics, want could-not-run", check.Outcome)
	}
	in.Metrics = agree
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v when the sources agree, want clear", check.Outcome)
	}

	// The replica says it accepts writes: two writers.
	in.Metrics = staticWindow{
		"orders-1": {"in-recovery": {At: readAt, Value: 0}},
		"orders-2": {"in-recovery": {At: readAt, Value: 0}},
	}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v with %d findings, want one on the extra writer", check.Outcome, len(findings))
	}
	if findings[0].ID != "disagree/orders-2" || findings[0].Subject.Name != "orders-2" {
		t.Errorf("finding = %s on %+v, want the replica", findings[0].ID, findings[0].Subject)
	}
	if !strings.Contains(findings[0].Summary, "orders-2") || !strings.Contains(findings[0].Summary, "orders-1") {
		t.Errorf("summary names neither side: %q", findings[0].Summary)
	}
	var operator, exporter bool
	for _, evidence := range findings[0].Evidence {
		operator = operator || strings.Contains(evidence.Detail, "currentPrimary orders-1")
		exporter = exporter || strings.Contains(evidence.Detail, "cnpg_pg_replication_in_recovery = 0")
	}
	if !operator || !exporter {
		t.Errorf("evidence does not quote both sources: %+v", findings[0].Evidence)
	}

	// The named primary says it is in recovery: no primary at all.
	in.Metrics = staticWindow{"orders-1": {"in-recovery": {At: readAt, Value: 1}}}
	if _, findings := evaluateRule(rule, in); len(findings) != 1 || findings[0].Subject.Name != "orders-1" {
		t.Errorf("a primary in recovery was not reported: %+v", findings)
	}

	// A move in flight explains the lag: clear until it settles.
	in.Cluster.Cluster.TargetPrimary = "orders-2"
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v during a primary move, want clear", check.Outcome)
	}
}
