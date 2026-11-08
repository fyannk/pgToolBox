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

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// TestStaleInputIsNotAClearResult is the staleness half of the honesty
// property: a snapshot the collector has lost contact with cannot clear
// a check, and cannot match one either. Each case builds an input in
// which every named check matches while fresh, marks one source stale,
// and expects every one of those checks to report that it could not
// run — naming staleness as the reason — with no finding left behind.
func TestStaleInputIsNotAClearResult(t *testing.T) {
	t.Parallel()
	old := now.Add(-time.Hour)
	rules := []Rule{
		{
			ID: "stale-event", Summary: "Event.", Severity: SeverityWarning,
			When: EventMatch{Reasons: []string{"FailedScheduling"}},
		},
		{
			ID: "stale-condition", Summary: "Condition.", Severity: SeverityWarning,
			When: ClusterCondition{Type: "ContinuousArchiving", Status: "False"},
		},
		{
			ID: "stale-phase", Summary: "Phase.", Severity: SeverityWarning,
			When: ClusterPhase{AnyOf: []string{"Not enough disk space"}},
		},
		{
			ID: "stale-primary", Summary: "Primary.", Severity: SeverityWarning,
			When: PrimaryMismatch{MinAge: 10 * time.Minute},
		},
		{
			ID: "stale-container", Summary: "Container.", Severity: SeverityWarning,
			When: ContainerState{Reasons: []string{"CrashLoopBackOff"}},
		},
	}
	crashing := observe.PodsSnapshot{Pods: []observe.PodFacts{{
		Name: "orders-1",
		Containers: []observe.ContainerFacts{
			{Name: "postgres", State: "waiting", Reason: "CrashLoopBackOff"},
			{Name: "plugin-barman-cloud", Image: "barman:0.5", State: "waiting", Reason: "ImagePullBackOff"},
		},
	}}}

	for _, tc := range []struct {
		name   string
		in     Input
		stale  func(*Input)
		checks []string
	}{
		{
			name: "events",
			in: withEvents(
				warning("Pod", "orders-1", "FailedCreate", "pods \"orders-1\" is forbidden: exceeded quota: tight"),
				warning("Pod", "orders-2", "FailedScheduling", "0/3 nodes are available: Insufficient cpu"),
			),
			stale:  func(in *Input) { in.Events.Stale = true },
			checks: []string{"stale-event", "resource-quota", "pod-scheduling"},
		},
		{
			name: "cluster",
			in: func() Input {
				in := clusterInput(17)
				in.Cluster.Cluster.Phase = "Not enough disk space"
				in.Cluster.Cluster.Conditions = []observe.Condition{
					{Type: "ContinuousArchiving", Status: "False", Reason: "Failing"},
				}
				in.Cluster.Cluster.CurrentPrimary = "orders-1"
				in.Cluster.Cluster.TargetPrimary = "orders-2"
				in.Cluster.Cluster.TargetPrimaryTimestamp = &old
				return in
			}(),
			stale:  func(in *Input) { in.Cluster.Stale = true },
			checks: []string{"stale-condition", "stale-phase", "stale-primary"},
		},
		{
			name:   "instance pods",
			in:     Input{Now: now, HasPods: true, Pods: crashing},
			stale:  func(in *Input) { in.Pods.Stale = true },
			checks: []string{"stale-container", "image-pull"},
		},
		{
			name: "pooler pods",
			in: Input{
				Now: now, HasPods: true, HasPoolerPods: true,
				Pods:       observe.PodsSnapshot{Pods: []observe.PodFacts{{Name: "orders-1"}}},
				PoolerPods: crashing,
			},
			stale:  func(in *Input) { in.PoolerPods.Stale = true },
			checks: []string{"stale-container"},
		},
		{
			name: "infrastructure",
			in: Input{
				Now: now, HasInfrastructure: true,
				Infrastructure: observe.InfrastructureSnapshot{Volumes: []observe.VolumeFacts{
					{Name: "orders-1", Phase: "Pending"},
				}},
			},
			stale:  func(in *Input) { in.Infrastructure.Stale = true },
			checks: []string{"volume-binding"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fresh := Run(tc.in, rules...)
			for _, name := range tc.checks {
				if got := outcomeOf(t, fresh, name); got != CheckMatched {
					t.Fatalf("%s = %v while fresh, want matched (the case does not exercise it)", name, got)
				}
			}

			in := tc.in
			tc.stale(&in)
			result := Run(in, rules...)
			for _, name := range tc.checks {
				if got := outcomeOf(t, result, name); got != CheckUnavailable {
					t.Errorf("%s = %v on a stale snapshot, want could-not-run", name, got)
				}
				if because := becauseOf(t, result, name); !strings.Contains(because, "stale") {
					t.Errorf("%s does not name staleness as its reason: %q", name, because)
				}
			}
			for _, finding := range result.Findings {
				for _, name := range tc.checks {
					if finding.Check == name {
						t.Errorf("%s reported a finding from a stale snapshot: %+v", name, finding)
					}
				}
			}
		})
	}
}

// TestStaleSnapshotsDoNotLendEvidence covers the two places a snapshot
// is read for supporting evidence rather than for the match itself. A
// stale supporting snapshot is left out, and the finding it would have
// decorated is otherwise unchanged.
func TestStaleSnapshotsDoNotLendEvidence(t *testing.T) {
	t.Parallel()

	t.Run("instance shortfall", func(t *testing.T) {
		t.Parallel()
		desired := 3
		in := quotaInput(observe.QuotaResourceFacts{
			Resource: "requests.storage", Hard: "1Gi", Used: "1Gi", Exhausted: true,
		})
		in.HasCluster = true
		in.Cluster = observe.Snapshot{Cluster: observe.ClusterFacts{Present: true, DesiredInstances: &desired}}
		in.HasPods = true
		in.Pods = observe.PodsSnapshot{Stale: true, Pods: []observe.PodFacts{{Name: "quota-1"}}}

		findings, unavailable := quotaExhaustedDetector{}.Detect(in)
		if unavailable != "" || len(findings) != 1 {
			t.Fatalf("findings = %+v (%q), want the quota finding on its own", findings, unavailable)
		}
		if findings[0].Severity != SeverityWarning {
			t.Errorf("severity = %v, want warning: a stale pod count cannot prove a shortfall", findings[0].Severity)
		}
		for _, evidence := range findings[0].Evidence {
			if strings.Contains(evidence.Detail, "pods observed") {
				t.Errorf("a stale pod count was quoted as the shortfall: %+v", evidence)
			}
		}
	})

	t.Run("provisioner reason", func(t *testing.T) {
		t.Parallel()
		const message = `storageclass.storage.k8s.io "fast" not found`
		in := withEvents(warning("PersistentVolumeClaim", "orders-1", "ProvisioningFailed", message))
		in.Events.Stale = true
		in.HasInfrastructure = true
		in.Infrastructure = observe.InfrastructureSnapshot{
			Volumes: []observe.VolumeFacts{{Name: "orders-1", Phase: "Pending"}},
		}

		finding := findingByID(t, Run(in), "volume-binding")
		for _, evidence := range finding.Evidence {
			if evidence.Detail == message {
				t.Errorf("a stale event was quoted as the reason: %+v", finding.Evidence)
			}
		}
	})
}

// becauseOf returns one check's stated reason.
func becauseOf(t *testing.T, result Result, name string) string {
	t.Helper()
	for _, check := range result.Checks {
		if check.Name == name {
			return check.Because
		}
	}
	t.Fatalf("check %q did not account for itself: %+v", name, result.Checks)
	return ""
}

// TestSwitchedOffSourcesAreNamedAsSuch pins the classification the
// screen groups by. Producer and classifier share one constant, so this
// holds the pair together: a helper that stops returning its constant,
// or a constant sourceOff stops recognising, fails here.
func TestSwitchedOffSourcesAreNamedAsSuch(t *testing.T) {
	t.Parallel()
	for name, reason := range map[string]string{
		"logs":           logsUnavailable(Input{}),
		"history":        historyUnavailable(Input{}),
		"metrics":        metricsUnavailable(Input{}),
		"pooler metrics": poolerMetricsUnavailable(Input{}),
		"evidence":       evidenceUnavailable(Input{}),
	} {
		if reason == "" {
			t.Fatalf("%s: an unconfigured source gave no reason at all", name)
		}
		if !sourceOff(reason) {
			t.Errorf("%s: %q reads as a fault, but the source is only switched off", name, reason)
		}
	}
}

// TestAFailingSourceIsNotMistakenForASwitchedOffOne guards the direction
// that matters. Classifying a fault as a settled choice would file it
// under the group a reader is meant to stop looking at, which is how a
// lost collector becomes invisible.
func TestAFailingSourceIsNotMistakenForASwitchedOffOne(t *testing.T) {
	t.Parallel()
	stale := Input{
		HasEvents: true, Events: observe.EventsSnapshot{Stale: true},
		HasPods: true, Pods: observe.PodsSnapshot{Stale: true},
		HasCluster: true, Cluster: observe.Snapshot{Stale: true},
	}
	for name, reason := range map[string]string{
		"events unobserved": eventsUnavailable(Input{}),
		"events stale":      eventsUnavailable(stale),
		"pods stale":        podsUnavailable(stale),
		"cluster stale":     clusterUnavailable(stale),
	} {
		if reason == "" {
			t.Fatalf("%s: gave no reason at all", name)
		}
		if sourceOff(reason) {
			t.Errorf("%s: %q reads as a settled choice, but the source is on and not answering", name, reason)
		}
	}
}

// TestAnUnknownReasonReadsAsAFault holds the safe direction of the
// default. A reason nobody declared is a fault, not a choice: an unread
// notice costs a reader a moment, a hidden failure costs them the
// outage.
func TestAnUnknownReasonReadsAsAFault(t *testing.T) {
	t.Parallel()
	if sourceOff("some reason nobody has classified") {
		t.Error("an unclassified reason reads as a switched-off source, hiding whatever it was")
	}
}
