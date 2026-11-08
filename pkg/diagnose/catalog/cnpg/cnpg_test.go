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

package cnpg

import (
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

var now = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// inputOn is a cluster whose pods carry the given operator bootstrap
// image tag, which is what decides the catalog's applicability.
func inputOn(operatorVersion string) diagnose.Input {
	major := 17
	return diagnose.Input{
		Now:        now,
		HasCluster: true,
		Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{
			Present:              true,
			PostgresMajorVersion: &major,
		}},
		HasPods: true,
		Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
			Name: "orders-1",
			Containers: []observe.ContainerFacts{
				{
					Name: "bootstrap-controller", Init: true,
					Image: "ghcr.io/cloudnative-pg/cloudnative-pg:" + operatorVersion,
				},
				{Name: "postgres", Image: "ghcr.io/cloudnative-pg/postgresql:17.5"},
			},
		}}},
	}
}

// outcomeOf finds one named check in a result.
func outcomeOf(t *testing.T, result diagnose.Result, name string) diagnose.Check {
	t.Helper()
	for _, check := range result.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("check %q did not account for itself", name)
	return diagnose.Check{}
}

// findingByID finds one finding in a result.
func findingByID(t *testing.T, result diagnose.Result, id string) diagnose.Finding {
	t.Helper()
	for _, finding := range result.Findings {
		if finding.ID == id {
			return finding
		}
	}
	t.Fatalf("finding %q absent: %+v", id, result.Findings)
	return diagnose.Finding{}
}

// TestEveryRuleIsPinned is the catalog's versioning contract: a claim
// read out of specific operator releases must say so. A rule added
// without a CloudNativePG pin would silently assert itself against
// every release, which is exactly what the framework exists to prevent.
func TestEveryRuleIsPinned(t *testing.T) {
	t.Parallel()
	for _, rule := range Rules() {
		pinned := false
		for _, requirement := range rule.Requires {
			if requirement.Component == diagnose.ComponentCNPG {
				pinned = true
			}
		}
		if !pinned {
			t.Errorf("rule %q carries no CloudNativePG pin", rule.ID)
		}
	}
}

// TestSpansApplyPerRelease pins the applicability boundaries the
// verification established: the supported span covers the verified
// releases and nothing outside them. 1.29, below the support floor,
// answers "does not apply" for every rule, the 1.30 machinery included.
func TestSpansApplyPerRelease(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rule    string
		version string
		want    diagnose.CheckOutcome
	}{
		{"cnpg-unrecoverable", "1.30.0", diagnose.CheckClear},
		{"cnpg-unrecoverable", "1.30.1", diagnose.CheckClear},
		{"cnpg-unrecoverable", "1.29.2", diagnose.CheckNotApplicable},
		{"cnpg-service-account-missing", "1.29.3", diagnose.CheckNotApplicable},
		{"cnpg-unrecoverable", "1.28.4", diagnose.CheckNotApplicable},
		{"cnpg-service-account-missing", "1.28.4", diagnose.CheckNotApplicable},
		{"cnpg-invalid-definition", "1.29.2", diagnose.CheckNotApplicable},
		{"cnpg-lease-preempted", "1.29.2", diagnose.CheckNotApplicable},
		{"cnpg-unrecoverable", "1.27.5", diagnose.CheckNotApplicable},
		{"cnpg-unrecoverable", "1.31.0", diagnose.CheckNotApplicable},
	}
	for _, tc := range cases {
		in := inputOn(tc.version)
		if tc.want == diagnose.CheckClear {
			// A clear phase check needs no phase set; the events- and
			// log-backed checks would be unavailable instead, which is
			// why the representative rules are phase- or event-backed
			// with their input observed.
			in.HasEvents = true
		}
		result := diagnose.Run(in, Rules()...)
		if got := outcomeOf(t, result, tc.rule).Outcome; got != tc.want {
			t.Errorf("%s on %s = %v, want %v", tc.rule, tc.version, got, tc.want)
		}
	}
}

// TestArchivingFailureFiresAcrossVerifiedReleases walks the flagship
// rule end to end on each verified release: the finding fires, quoting
// both the operator's condition and the version fact the pin rests on.
func TestArchivingFailureFiresAcrossVerifiedReleases(t *testing.T) {
	t.Parallel()
	for _, version := range VerifiedReleases {
		in := inputOn(version)
		in.Cluster.Cluster.Conditions = []observe.Condition{{
			Type: "ContinuousArchiving", Status: "False", Reason: "ContinuousArchivingFailing",
			Message: "unexpected failure invoking barman-cloud-wal-archive: exit status 1",
		}}
		finding := findingByID(t, diagnose.Run(in, Rules()...), "cnpg-wal-archiving-failing")
		if !strings.Contains(finding.Evidence[0].Detail, "barman-cloud-wal-archive") {
			t.Errorf("%s: the archiver's own error is not quoted: %+v", version, finding.Evidence[0])
		}
		last := finding.Evidence[len(finding.Evidence)-1]
		if !strings.Contains(last.Detail, version) {
			t.Errorf("%s: the version fact the pin rests on is not quoted: %+v", version, finding.Evidence)
		}
	}
}

// TestStuckPhaseProducesTheFinding covers the phase family with the
// unrecoverable case, whose reason text is the only place the specific
// cause is written.
func TestStuckPhaseProducesTheFinding(t *testing.T) {
	t.Parallel()
	in := inputOn("1.30.0")
	in.Cluster.Cluster.Phase = "Cluster is unrecoverable and needs manual intervention"
	in.Cluster.Cluster.PhaseReason = "Instance creation failed for the following jobs: orders-2-join"

	finding := findingByID(t, diagnose.Run(in, Rules()...), "cnpg-unrecoverable")
	if !strings.Contains(finding.Evidence[0].Detail, "orders-2-join") {
		t.Errorf("the phase reason is not quoted: %+v", finding.Evidence[0])
	}
}

// replicaWindow is a MetricsWindow with per-instance instant readings
// and a held run of series samples.
type replicaWindow struct {
	instants map[string]map[string]metrics.Instant
	series   map[string]map[string]float64
}

func (w replicaWindow) Interval() time.Duration { return metrics.DefaultInterval }

func (w replicaWindow) Instances() []string { return nil }

func (w replicaWindow) Range(key string, tier metrics.Tier) ([]int64, map[string][]*float64) {
	if tier != metrics.TierRaw {
		return nil, nil
	}
	times := []int64{now.Add(-10 * time.Minute).Unix(), now.Add(-5 * time.Minute).Unix(), now.Unix()}
	out := map[string][]*float64{}
	for instance, byKey := range w.series {
		value, ok := byKey[key]
		if !ok {
			continue
		}
		column := make([]*float64, len(times))
		for i := range column {
			held := value
			column[i] = &held
		}
		out[instance] = column
	}
	return times, out
}

func (w replicaWindow) InstantReadings() map[string]map[string]metrics.Instant { return w.instants }

// TestReplicaNotReceivingNeedsAllThreeReadings walks the catalog's
// corroborating rule: a replica in recovery is not a finding, a replica
// without a WAL receiver is not a finding, and a lag past the threshold
// is its own lesser finding — but one instance showing all three is the
// replica that stopped streaming, and the lesser finding then nests
// inside it rather than standing beside it.
func TestReplicaNotReceivingNeedsAllThreeReadings(t *testing.T) {
	t.Parallel()
	const rule = "cnpg-replica-not-receiving"
	run := func(instants map[string]map[string]metrics.Instant, series map[string]map[string]float64) diagnose.Result {
		in := inputOn("1.30.0")
		in.Metrics = replicaWindow{instants: instants, series: series}
		return diagnose.Run(in, Rules()...)
	}
	reading := func(value float64) metrics.Instant { return metrics.Instant{At: now.Unix(), Value: value} }

	// A healthy replica: in recovery, receiver up, no lag reported.
	healthy := run(map[string]map[string]metrics.Instant{
		"orders-2": {"in-recovery": reading(1), "wal-receiver-up": reading(1)},
	}, nil)
	if got := outcomeOf(t, healthy, rule).Outcome; got != diagnose.CheckClear {
		t.Errorf("outcome = %v on a streaming replica, want clear", got)
	}

	// Receiver down but the lag is not past the threshold: a replica
	// replaying the archive on its way up looks exactly like this.
	catchingUp := run(map[string]map[string]metrics.Instant{
		"orders-2": {"in-recovery": reading(1), "wal-receiver-up": reading(0)},
	},
		map[string]map[string]float64{"orders-2": {"replication-lag": 5}})
	if got := outcomeOf(t, catchingUp, rule).Outcome; got != diagnose.CheckClear {
		t.Errorf("outcome = %v on a replica catching up, want clear", got)
	}

	// All three: in recovery, no receiver, and the lag holding.
	stalled := run(map[string]map[string]metrics.Instant{
		"orders-2": {"in-recovery": reading(1), "wal-receiver-up": reading(0)},
	},
		map[string]map[string]float64{"orders-2": {"replication-lag": 900}})
	if got := outcomeOf(t, stalled, rule).Outcome; got != diagnose.CheckMatched {
		t.Fatalf("outcome = %v on a stalled replica, want matched", got)
	}
	finding := findingByID(t, stalled, rule+"/orders-2")
	if len(finding.Evidence) < 4 {
		t.Errorf("finding quotes %d claims, want all three readings and the version pin: %+v",
			len(finding.Evidence), finding.Evidence)
	}
	lag := findingByID(t, stalled, "cnpg-replication-lag-high/orders-2")
	if len(lag.ConsequenceOf) != 1 || lag.ConsequenceOf[0].Cause != rule ||
		lag.ConsequenceOf[0].Scope != diagnose.ScopePod {
		t.Errorf("the lag finding is not related to the stalled replica on the same pod: %+v", lag.ConsequenceOf)
	}
}
