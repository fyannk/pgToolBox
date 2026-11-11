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

// heldInput is a cluster input whose store has been publishing long
// enough to have a record of the given states.
func heldInput(facts observe.ClusterFacts, since time.Time) Input {
	facts.Present = true
	facts.ObservedSince = map[string]time.Time{}
	for _, key := range facts.HeldKeys() {
		facts.ObservedSince[key] = since
	}
	return Input{Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: facts}}
}

func evaluateOnce(t *testing.T, when Condition, in Input) (Check, []Finding) {
	t.Helper()
	return evaluateRule(Rule{ID: "held", Summary: "Held.", When: when}, in)
}

// TestAHeldPhaseNeedsTheConsolesClock proves the three answers a held
// check gives: matched once the state has held long enough, clear
// while it is younger, and could-not-run when the console has no
// record of its age at all.
func TestAHeldPhaseNeedsTheConsolesClock(t *testing.T) {
	t.Parallel()
	creating := observe.ClusterFacts{Phase: "Creating a new replica", PhaseReason: "Creating replica orders-2-join"}
	when := ClusterPhaseHeld{AnyOf: []string{"Creating a new replica"}, MinAge: 30 * time.Minute}

	check, findings := evaluateOnce(t, when, heldInput(creating, now.Add(-time.Hour)))
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("held an hour: %v %q", check.Outcome, check.Because)
	}
	if e := findings[0].Evidence; len(e) != 2 || e[1].Origin != "console-observed" ||
		!strings.Contains(e[1].Detail, "since at least") || !strings.Contains(e[0].Detail, "Creating replica orders-2-join") {
		t.Errorf("evidence = %+v", e)
	}

	check, _ = evaluateOnce(t, when, heldInput(creating, now.Add(-5*time.Minute)))
	if check.Outcome != CheckClear {
		t.Errorf("held five minutes: %v %q, want clear", check.Outcome, check.Because)
	}

	untracked := Input{Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{
		Present: true, Phase: "Creating a new replica",
	}}}
	check, _ = evaluateOnce(t, when, untracked)
	if check.Outcome != CheckUnavailable || !strings.Contains(check.Because, "no record yet") {
		t.Errorf("untracked: %v %q, want could not run naming the missing record", check.Outcome, check.Because)
	}

	check, _ = evaluateOnce(t, when, heldInput(observe.ClusterFacts{Phase: "Cluster in healthy state"}, now.Add(-time.Hour)))
	if check.Outcome != CheckClear {
		t.Errorf("other phase: %v, want clear", check.Outcome)
	}
}

// TestAHeldConditionUsesTheOperatorsTransitionTime proves the age bound
// on a condition reads the operator's clock, and refuses to judge a
// condition reported without one.
func TestAHeldConditionUsesTheOperatorsTransitionTime(t *testing.T) {
	t.Parallel()
	old := now.Add(-time.Hour)
	facts := observe.ClusterFacts{Present: true, Conditions: []observe.Condition{
		{Type: "Ready", Status: "False", Reason: "ClusterIsNotReady", LastTransition: &old},
	}}
	in := Input{Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: facts}}
	when := ClusterCondition{Type: "Ready", Status: "False", MinAge: 10 * time.Minute}

	check, findings := evaluateOnce(t, when, in)
	if check.Outcome != CheckMatched || !strings.Contains(findings[0].Evidence[0].Detail, "1h0m0s ago") {
		t.Fatalf("held an hour: %v %+v", check.Outcome, findings)
	}

	recent := now.Add(-time.Minute)
	facts.Conditions[0].LastTransition = &recent
	if check, _ := evaluateOnce(t, when, in); check.Outcome != CheckClear {
		t.Errorf("transitioned a minute ago: %v, want clear", check.Outcome)
	}

	facts.Conditions[0].LastTransition = nil
	if check, _ := evaluateOnce(t, when, in); check.Outcome != CheckUnavailable ||
		!strings.Contains(check.Because, "no transition time") {
		t.Errorf("no transition time: %v %q", check.Outcome, check.Because)
	}
}

// TestStatusListsNameTheirObjects proves each list yields one finding
// per listed name with the right subject kind, and that the age bound
// applies per name.
func TestStatusListsNameTheirObjects(t *testing.T) {
	t.Parallel()
	facts := observe.ClusterFacts{
		FailedInstances: []string{"orders-3"},
		UnusablePVCs:    []string{"orders-1-wal"},
		DanglingPVCs:    []string{"orders-2", "orders-4"},
	}
	in := heldInput(facts, now.Add(-time.Hour))
	// orders-4 was listed only just now.
	in.Cluster.Cluster.ObservedSince["danglingPVC=orders-4"] = now.Add(-time.Minute)

	for _, tc := range []struct {
		when StatusListed
		want []EntityRef
	}{
		{StatusListed{List: ListFailedInstances}, []EntityRef{{Kind: "Pod", Name: "orders-3"}}},
		{StatusListed{List: ListUnusablePVC}, []EntityRef{{Kind: "PersistentVolumeClaim", Name: "orders-1-wal"}}},
		{
			StatusListed{List: ListDanglingPVC, MinAge: 15 * time.Minute},
			[]EntityRef{{Kind: "PersistentVolumeClaim", Name: "orders-2"}},
		},
		{StatusListed{List: ListResizingPVC}, nil},
	} {
		_, findings := evaluateOnce(t, tc.when, in)
		if len(findings) != len(tc.want) {
			t.Errorf("%s: %d findings, want %d", tc.when.List, len(findings), len(tc.want))
			continue
		}
		for i, want := range tc.want {
			if findings[i].Subject != want {
				t.Errorf("%s: subject %v, want %v", tc.when.List, findings[i].Subject, want)
			}
		}
	}
}

// TestTimelineDivergenceComparesEachInstanceToTheCluster proves an
// instance on another timeline is named, one on the cluster's is not,
// one that reported none is not, and the check cannot run without a
// cluster timeline to compare against.
func TestTimelineDivergenceComparesEachInstanceToTheCluster(t *testing.T) {
	t.Parallel()
	four := 4
	facts := observe.ClusterFacts{TimelineID: &four, InstanceTimelines: []observe.InstanceTimeline{
		{Instance: "orders-1", TimelineID: 4, IsPrimary: true},
		{Instance: "orders-2", TimelineID: 3},
		{Instance: "orders-3"},
	}}
	check, findings := evaluateOnce(t, TimelineDivergence{MinAge: 10 * time.Minute}, heldInput(facts, now.Add(-time.Hour)))
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].Subject.Name != "orders-2" {
		t.Fatalf("outcome %v, findings %+v", check.Outcome, findings)
	}
	if !strings.Contains(findings[0].Summary, "timeline 3 while the cluster is on timeline 4") {
		t.Errorf("summary = %q", findings[0].Summary)
	}
	facts.TimelineID = nil
	if check, _ := evaluateOnce(t, TimelineDivergence{}, heldInput(facts, now)); check.Outcome != CheckUnavailable {
		t.Errorf("no cluster timeline: %v, want could not run", check.Outcome)
	}
}

// TestCertificateExpiryIsSplitBetweenExpiringAndExpired proves the two
// spans never report the same certificate.
func TestCertificateExpiryIsSplitBetweenExpiringAndExpired(t *testing.T) {
	t.Parallel()
	facts := observe.ClusterFacts{Present: true, CertificateExpirations: []observe.CertificateExpiry{
		{Secret: "orders-ca", ExpiresAt: now.Add(-time.Hour)},
		{Secret: "orders-server", ExpiresAt: now.Add(2 * 24 * time.Hour)},
		{Secret: "orders-replication", ExpiresAt: now.Add(60 * 24 * time.Hour)},
	}}
	in := Input{Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: facts}}
	_, expired := evaluateOnce(t, CertificateExpiring{}, in)
	_, expiring := evaluateOnce(t, CertificateExpiring{Within: 3 * 24 * time.Hour}, in)
	if len(expired) != 1 || expired[0].Subject != (EntityRef{Kind: "Secret", Name: "orders-ca"}) {
		t.Errorf("expired = %+v", expired)
	}
	if len(expiring) != 1 || expiring[0].Subject.Name != "orders-server" {
		t.Errorf("expiring = %+v", expiring)
	}
}

// TestPrimaryFailingReadsTheOperatorsStamp proves the finding names the
// current primary, carries the operator's instant as its time, and
// waits out the minimum age.
func TestPrimaryFailingReadsTheOperatorsStamp(t *testing.T) {
	t.Parallel()
	since := now.Add(-5 * time.Minute)
	facts := observe.ClusterFacts{Present: true, CurrentPrimary: "orders-1", PrimaryFailingSince: &since}
	in := Input{Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: facts}}
	_, findings := evaluateOnce(t, PrimaryFailing{MinAge: time.Minute}, in)
	if len(findings) != 1 || findings[0].Subject != (EntityRef{Kind: "Pod", Name: "orders-1"}) || !findings[0].At.Equal(since) {
		t.Fatalf("findings = %+v", findings)
	}
	if check, _ := evaluateOnce(t, PrimaryFailing{MinAge: 10 * time.Minute}, in); check.Outcome != CheckClear {
		t.Errorf("younger than the bound: %v, want clear", check.Outcome)
	}
}

// TestRolesTablespacesAndPoolersQuoteTheOperator covers the three
// simple restatements.
func TestRolesTablespacesAndPoolersQuoteTheOperator(t *testing.T) {
	t.Parallel()
	facts := observe.ClusterFacts{
		Present:             true,
		UnreconcilableRoles: []observe.RoleProblem{{Role: "app", Errors: []string{"role \"app\" cannot be dropped"}}},
		Tablespaces: []observe.TablespaceFacts{
			{Name: "fast", State: "pending", Error: "disk missing"}, {Name: "ok", State: "reconciled"},
		},
	}
	in := Input{
		Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: facts},
		HasPoolers: true, Poolers: observe.PoolersSnapshot{Poolers: []observe.PoolerFacts{
			{Name: "orders-rw", Phase: "failed", PhaseReason: "image not found"}, {Name: "orders-ro", Phase: "active"},
		}},
	}
	_, roles := evaluateOnce(t, RoleUnreconcilable{}, in)
	if len(roles) != 1 || roles[0].Subject != (EntityRef{Kind: "ManagedRole", Name: "app"}) ||
		!strings.Contains(roles[0].Evidence[0].Detail, "cannot be dropped") {
		t.Errorf("roles = %+v", roles)
	}
	_, tablespaces := evaluateOnce(t, TablespaceError{}, in)
	if len(tablespaces) != 1 || tablespaces[0].Subject != (EntityRef{Kind: "Tablespace", Name: "fast"}) {
		t.Errorf("tablespaces = %+v", tablespaces)
	}
	_, poolers := evaluateOnce(t, PoolerPhase{AnyOf: []string{"failed", "inactive"}}, in)
	if len(poolers) != 1 || poolers[0].Subject != (EntityRef{Kind: "Pooler", Name: "orders-rw"}) ||
		!strings.Contains(poolers[0].Evidence[0].Detail, "image not found") {
		t.Errorf("poolers = %+v", poolers)
	}
}

// TestInstancesShortHoldsBeforeReporting proves a shortfall reads the
// operator's own counts and waits out the bound, and that a fresh
// shortfall — a different pair of numbers — starts a new clock.
func TestInstancesShortHoldsBeforeReporting(t *testing.T) {
	t.Parallel()
	three, two := 3, 2
	facts := observe.ClusterFacts{DesiredInstances: &three, ReadyInstances: &two}
	check, findings := evaluateOnce(t, InstancesShort{MinAge: 10 * time.Minute}, heldInput(facts, now.Add(-time.Hour)))
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].Summary != "2 of 3 declared instances are ready." {
		t.Fatalf("outcome %v, findings %+v", check.Outcome, findings)
	}
	if check, _ := evaluateOnce(t, InstancesShort{MinAge: 10 * time.Minute}, heldInput(facts, now.Add(-time.Minute))); check.Outcome != CheckClear {
		t.Errorf("young shortfall: %v, want clear", check.Outcome)
	}
	facts.ReadyInstances = &three
	if check, _ := evaluateOnce(t, InstancesShort{}, heldInput(facts, now.Add(-time.Hour))); check.Outcome != CheckClear {
		t.Errorf("no shortfall: %v, want clear", check.Outcome)
	}
}

// TestEventMatchAttributesSecondaryKindsThroughTheirCatalog proves an
// event on a Backup, ScheduledBackup or Pooler counts only when the
// respective catalog lists the object as this cluster's, cannot be
// judged while that catalog is unreadable, and is ignored by a rule
// scoped to the Cluster and its pods.
func TestEventMatchAttributesSecondaryKindsThroughTheirCatalog(t *testing.T) {
	t.Parallel()
	in := withEvents(
		warning("Backup", "orders-20260917", "Error", "Backup exit with error: boom"),
		warning("Backup", "billing-20260917", "Error", "Backup exit with error: not ours"),
		warning("Pooler", "orders-rw", "ImageCatalogError", "no image"),
	)
	on := EventMatch{Reasons: []string{"Error"}, Kinds: []string{"Backup"}}

	check, _ := evaluateOnce(t, on, in)
	if check.Outcome != CheckUnavailable || !strings.Contains(check.Because, "backup catalog") {
		t.Fatalf("no backup catalog: %v %q, want could not run naming it", check.Outcome, check.Because)
	}

	in.HasBackups = true
	in.Backups = observe.BackupsSnapshot{Backups: []observe.BackupFacts{{Name: "orders-20260917"}}}
	check, findings := evaluateOnce(t, on, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || len(findings[0].Evidence) != 1 ||
		findings[0].Subject != (EntityRef{Kind: "Backup", Name: "orders-20260917"}) {
		t.Fatalf("catalogued backup: %v %+v", check.Outcome, findings)
	}

	// The default scope never reads a Backup event, catalog or not.
	if check, _ := evaluateOnce(t, EventMatch{Reasons: []string{"Error"}}, in); check.Outcome != CheckClear {
		t.Errorf("default scope read a Backup event: %v", check.Outcome)
	}

	pooler := EventMatch{Reasons: []string{"ImageCatalogError"}, Kinds: []string{"Pooler"}}
	if check, _ := evaluateOnce(t, pooler, in); check.Outcome != CheckUnavailable {
		t.Errorf("no pooler catalog: %v, want could not run", check.Outcome)
	}
	in.HasPoolers = true
	in.Poolers = observe.PoolersSnapshot{Poolers: []observe.PoolerFacts{{Name: "orders-rw"}}}
	if check, findings := evaluateOnce(t, pooler, in); check.Outcome != CheckMatched ||
		findings[0].Subject != (EntityRef{Kind: "Pooler", Name: "orders-rw"}) {
		t.Errorf("catalogued pooler: %v %+v", check.Outcome, findings)
	}
	if got := PodSubjectOf(pooler); got != PodSubjectNever {
		t.Errorf("a Pooler-scoped event rule classified as %v, want never naming a pod", got)
	}
}

func leaseInput(lease observe.PrimaryLeaseFacts, current, target string) Input {
	return Input{
		Now:        now,
		HasCluster: true, Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{
			Present: true, CurrentPrimary: current, TargetPrimary: target,
		}},
		HasPrimaryLease: true, PrimaryLease: observe.PrimaryLeaseSnapshot{Lease: lease},
	}
}

// TestPrimaryLeaseExpiryReadsTheLeasesOwnClock proves the lease is
// judged by its renewal time and duration, that a released lease is a
// finding only with a primary named and no move in flight, and that
// absence and a move in flight both read as clear.
func TestPrimaryLeaseExpiryReadsTheLeasesOwnClock(t *testing.T) {
	t.Parallel()
	fifteen := int32(15)
	fresh, stale := now.Add(-5*time.Second), now.Add(-5*time.Minute)
	when := PrimaryLeaseExpired{Grace: time.Minute}

	held := observe.PrimaryLeaseFacts{Present: true, Holder: "orders-1", RenewedAt: &fresh, DurationSeconds: &fifteen}
	if check, _ := evaluateOnce(t, when, leaseInput(held, "orders-1", "orders-1")); check.Outcome != CheckClear {
		t.Errorf("freshly renewed: %v %q, want clear", check.Outcome, check.Because)
	}
	held.RenewedAt = &stale
	check, findings := evaluateOnce(t, when, leaseInput(held, "orders-1", "orders-1"))
	if check.Outcome != CheckMatched || findings[0].Subject != (EntityRef{Kind: "Pod", Name: "orders-1"}) || !findings[0].At.Equal(stale) {
		t.Fatalf("stale renewal: %v %+v", check.Outcome, findings)
	}
	held.DurationSeconds = nil
	if check, _ := evaluateOnce(t, when, leaseInput(held, "orders-1", "orders-1")); check.Outcome != CheckUnavailable {
		t.Errorf("no duration: %v, want could not run", check.Outcome)
	}

	released := observe.PrimaryLeaseFacts{Present: true, RenewedAt: &stale}
	check, findings = evaluateOnce(t, when, leaseInput(released, "orders-1", "orders-1"))
	if check.Outcome != CheckMatched || !strings.Contains(findings[0].Summary, "released while the operator names orders-1") {
		t.Fatalf("released with a primary named: %v %+v", check.Outcome, findings)
	}
	if check, _ := evaluateOnce(t, when, leaseInput(released, "orders-1", "orders-2")); check.Outcome != CheckClear {
		t.Errorf("released during a move: %v, want clear", check.Outcome)
	}
	if check, _ := evaluateOnce(t, when, leaseInput(released, "", "")); check.Outcome != CheckClear {
		t.Errorf("released with no primary: %v, want clear", check.Outcome)
	}
	if check, _ := evaluateOnce(t, when, leaseInput(observe.PrimaryLeaseFacts{Present: false}, "orders-1", "orders-1")); check.Outcome != CheckClear {
		t.Errorf("no lease at all: %v, want clear", check.Outcome)
	}
	in := leaseInput(held, "orders-1", "orders-1")
	in.HasPrimaryLease = false
	if check, _ := evaluateOnce(t, when, in); check.Outcome != CheckUnavailable {
		t.Errorf("lease unobserved: %v, want could not run", check.Outcome)
	}
}

// TestPrimaryLeaseHolderMismatchIsTwoWritersDisagreeing proves the
// finding names the holder, quotes both claims, and stays clear during
// a primary move.
func TestPrimaryLeaseHolderMismatchIsTwoWritersDisagreeing(t *testing.T) {
	t.Parallel()
	held := observe.PrimaryLeaseFacts{Present: true, Holder: "orders-2"}
	check, findings := evaluateOnce(t, PrimaryLeaseHolderMismatch{}, leaseInput(held, "orders-1", "orders-1"))
	if check.Outcome != CheckMatched || findings[0].Subject != (EntityRef{Kind: "Pod", Name: "orders-2"}) ||
		len(findings[0].Evidence) != 2 || findings[0].Evidence[1].Origin != "operator-reported" {
		t.Fatalf("mismatch: %v %+v", check.Outcome, findings)
	}
	if check, _ := evaluateOnce(t, PrimaryLeaseHolderMismatch{}, leaseInput(held, "orders-1", "orders-2")); check.Outcome != CheckClear {
		t.Errorf("during a move: %v, want clear", check.Outcome)
	}
	if check, _ := evaluateOnce(t, PrimaryLeaseHolderMismatch{}, leaseInput(held, "orders-2", "orders-2")); check.Outcome != CheckClear {
		t.Errorf("holder is the primary: %v, want clear", check.Outcome)
	}
}

type staticStatus struct {
	snap  instancestatus.Snapshot
	swept bool
}

func (s staticStatus) CurrentInstanceStatus() (instancestatus.Snapshot, bool) { return s.snap, s.swept }

func statusInput(readings ...instancestatus.Reading) Input {
	snap := instancestatus.Snapshot{Interval: 10 * time.Second, SweptAt: now, Readings: map[string]instancestatus.Reading{}, Failing: map[string]string{}}
	for _, r := range readings {
		if r.ObservedAt.IsZero() {
			r.ObservedAt = now
		}
		snap.Readings[r.Instance] = r
	}
	return Input{Now: now, InstanceStatus: staticStatus{snap: snap, swept: true}}
}

// TestInstanceStatusIsJudgedPerInstance proves the sweep's honesty
// rules: a report older than three sweeps is refused rather than read,
// an instance the sweep could not read withholds a clear, a match on a
// fresh report stands whatever else went unread, and a switched-off or
// unswept source says so.
func TestInstanceStatusIsJudgedPerInstance(t *testing.T) {
	t.Parallel()
	fresh := instancestatus.Reading{Instance: "orders-1", IsPrimary: true, PendingRestart: true}
	stale := instancestatus.Reading{Instance: "orders-2", ObservedAt: now.Add(-5 * time.Minute), PendingRestart: true}
	in := statusInput(fresh, stale)
	when := InstanceFlagSet{Flag: FlagPendingRestart}
	check, findings := evaluateOnce(t, when, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].Subject.Name != "orders-1" {
		t.Fatalf("fresh match beside a stale report: %v %+v", check.Outcome, findings)
	}
	if findings[0].Evidence[0].Origin != statusOrigin || !strings.Contains(findings[0].Evidence[0].Detail, "pendingRestart true") {
		t.Errorf("evidence = %+v", findings[0].Evidence)
	}

	only := statusInput(stale)
	if check, _ := evaluateOnce(t, when, only); check.Outcome != CheckUnavailable || !strings.Contains(check.Because, "refused") {
		t.Errorf("stale-only: %v %q, want could not run naming the refusal", check.Outcome, check.Because)
	}
	failing := statusInput(instancestatus.Reading{Instance: "orders-1"})
	failing.InstanceStatus.(staticStatus).snap.Failing["orders-3"] = "unavailable"
	if check, _ := evaluateOnce(t, when, failing); check.Outcome != CheckUnavailable || !strings.Contains(check.Because, "orders-3") {
		t.Errorf("an unread instance did not withhold the clear: %v %q", check.Outcome, check.Because)
	}
	if check, _ := evaluateOnce(t, when, Input{Now: now}); check.Outcome != CheckUnavailable || !check.SourceOff {
		t.Errorf("switched off: %v off=%v", check.Outcome, check.SourceOff)
	}
	unswept := Input{Now: now, InstanceStatus: staticStatus{}}
	if check, _ := evaluateOnce(t, when, unswept); check.Outcome != CheckUnavailable || check.SourceOff || !strings.Contains(check.Because, "not been swept") {
		t.Errorf("unswept: %v %q off=%v", check.Outcome, check.Because, check.SourceOff)
	}
	if check, _ := evaluateOnce(t, InstanceFlagSet{Flag: FlagPendingRestart, ReplicaOnly: true}, statusInput(fresh)); check.Outcome != CheckClear {
		t.Errorf("a primary matched a replica-only flag: %v", check.Outcome)
	}
}

// TestInstanceStatusConditionsReadTheReport covers the archiver
// comparison, the WAL backlog, the unmanaged inactive slot, and the
// manager-version drift.
func TestInstanceStatusConditionsReadTheReport(t *testing.T) {
	t.Parallel()
	earlier, later := now.Add(-2*time.Hour), now.Add(-time.Hour)
	primary := instancestatus.Reading{
		Instance: "orders-1", IsPrimary: true, IsArchivingWAL: true,
		LastArchivedWAL: "0005", LastArchivedWALTime: &earlier, LastFailedWAL: "0006", LastFailedWALTime: &later,
		ReadyWALFiles: 40, InstanceManagerVersion: "1.30.0",
		Slots: []instancestatus.Slot{{Name: "_cnpg_orders_2", Active: false}, {Name: "debezium", Type: "logical", Plugin: "pgoutput", Active: false}},
	}
	replica := instancestatus.Reading{Instance: "orders-2", InstanceManagerVersion: "1.29.2", ReadyWALFiles: 0}
	in := statusInput(primary, replica)

	_, archive := evaluateOnce(t, InstanceArchiveFailing{}, in)
	if len(archive) != 1 || archive[0].Subject.Name != "orders-1" || !archive[0].At.Equal(later) {
		t.Errorf("archive = %+v", archive)
	}
	healthy := primary
	healthy.LastFailedWALTime = &earlier
	healthy.LastArchivedWALTime = &later
	if check, _ := evaluateOnce(t, InstanceArchiveFailing{}, statusInput(healthy)); check.Outcome != CheckClear {
		t.Errorf("success after failure: %v, want clear", check.Outcome)
	}
	_, backlog := evaluateOnce(t, InstanceReadyWAL{Threshold: 32}, in)
	if len(backlog) != 1 || !strings.Contains(backlog[0].Summary, "40 WAL segments") {
		t.Errorf("backlog = %+v", backlog)
	}
	// CNPG P3-1: a standby's count is no reading, even at the
	// threshold, and with no fresh primary report the rule does not
	// clear: a replica cluster's designated primary reports none.
	designated := primary
	designated.IsPrimary, designated.ReadyWALFiles = false, 0
	if check, matches := evaluateOnce(t, InstanceReadyWAL{Threshold: 32}, statusInput(designated, replica)); check.Outcome == CheckClear || len(matches) != 0 || !strings.Contains(check.Because, "only on a primary") {
		t.Errorf("no primary reading: %+v %+v, want unjudged with a reason", check, matches)
	}
	standby := replica
	standby.ReadyWALFiles = 99
	if _, matches := evaluateOnce(t, InstanceReadyWAL{Threshold: 32}, statusInput(primary, standby)); len(matches) != 1 || matches[0].Subject.Name != "orders-1" {
		t.Errorf("a standby's count matched: %+v", matches)
	}
	_, slots := evaluateOnce(t, InstanceSlotInactive{}, in)
	if len(slots) != 1 || !strings.Contains(slots[0].Summary, `"debezium"`) || slots[0].ID != "held/orders-1/debezium" {
		t.Errorf("slots = %+v (the operator's own slot must be left out)", slots)
	}
	_, drift := evaluateOnce(t, InstanceManagerDrift{}, in)
	if len(drift) != 1 || drift[0].Subject != clusterSubject || len(drift[0].Evidence) != 2 {
		t.Errorf("drift = %+v", drift)
	}
	replica.InstanceManagerVersion = "1.30.0"
	if check, _ := evaluateOnce(t, InstanceManagerDrift{}, statusInput(primary, replica)); check.Outcome != CheckClear {
		t.Errorf("same version everywhere: %v, want clear", check.Outcome)
	}
}

// TestMaskedReportsAreNotJudged (round 16, CNPG P2-1): a fenced or
// restarting instance answers with the error its manager masked and
// every PostgreSQL field at zero. Its readyWalFiles 0, its empty slot
// list and a pg_stat_archiver failure filled before the error are not
// readings: the rules on them do not clear, and say why.
func TestMaskedReportsAreNotJudged(t *testing.T) {
	t.Parallel()
	failed := now.Add(-time.Hour)
	masked := instancestatus.Reading{
		Instance: "orders-1", IsPrimary: true, MightBeUnavailable: true,
		MightBeUnavailableMaskedError: "FATAL: the database system is shutting down (SQLSTATE 57P03)",
		LastFailedWAL:                 "0006", LastFailedWALTime: &failed,
	}
	for name, rule := range map[string]interface {
		evaluate(string, Input) ([]conditionMatch, string)
	}{
		"archive failing": InstanceArchiveFailing{}, "ready WAL": InstanceReadyWAL{Threshold: 32}, "slots": InstanceSlotInactive{},
	} {
		matches, because := rule.evaluate("", statusInput(masked))
		if len(matches) != 0 || !strings.Contains(because, "without reading PostgreSQL") || !strings.Contains(because, "orders-1") {
			t.Errorf("%s: %d matches, because %q; want none and the masked instance named", name, len(matches), because)
		}
	}
	read := masked
	read.MightBeUnavailableMaskedError = ""
	if matches, _ := (InstanceArchiveFailing{}).evaluate("", statusInput(read)); len(matches) != 1 {
		t.Errorf("unmasked control: %d archive-failing matches, want 1", len(matches))
	}
}
