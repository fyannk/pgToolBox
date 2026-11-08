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

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

func warning(kind, object, reason, message string) observe.EventFacts {
	return observe.EventFacts{
		Kind: kind, Object: object, Type: "Warning",
		Reason: reason, Message: message, Count: 1, LastSeen: now,
	}
}

func withEvents(events ...observe.EventFacts) Input {
	return Input{
		Now:       now,
		HasEvents: true,
		Events:    observe.EventsSnapshot{Events: events},
	}
}

// findingByID returns one finding, failing when it is absent.
func findingByID(t *testing.T, result Result, id string) Finding {
	t.Helper()
	for _, finding := range result.Findings {
		if finding.ID == id {
			return finding
		}
	}
	t.Fatalf("finding %q absent: %+v", id, result.Findings)
	return Finding{}
}

// TestQuotaQuotesTheRefusalIncludingItsNumbers is the reason this
// detector needs no ResourceQuota read: the admission message already
// carries used and limited, so quoting it verbatim gives the reader the
// headroom a separate grant would have fetched.
func TestQuotaQuotesTheRefusalIncludingItsNumbers(t *testing.T) {
	t.Parallel()
	const message = `pods "orders-3" is forbidden: exceeded quota: compute, ` +
		`requested: pods=1, used: pods=8, limited: pods=8`
	result := Run(withEvents(warning("Cluster", "orders", "FailedCreate", message)))

	finding := findingByID(t, result, "resource-quota")
	if finding.Severity != SeverityCritical {
		t.Errorf("severity = %v, want critical", finding.Severity)
	}
	if len(finding.Evidence) == 0 || finding.Evidence[0].Detail != message {
		t.Fatalf("refusal not quoted verbatim: %+v", finding.Evidence)
	}
	for _, want := range []string{"used: pods=8", "limited: pods=8"} {
		if !strings.Contains(finding.Evidence[0].Detail, want) {
			t.Errorf("evidence lost the quota numbers: missing %q", want)
		}
	}
	if finding.Evidence[0].Origin != "Kubernetes-observed" {
		t.Errorf("origin = %q", finding.Evidence[0].Origin)
	}
}

// TestQuotaAddsTheShortfallOnlyWhenBothCountsAreKnown proves the
// declared-versus-observed line is context on a refusal, and is omitted
// rather than guessed when either half is unknown.
func TestQuotaAddsTheShortfallOnlyWhenBothCountsAreKnown(t *testing.T) {
	t.Parallel()
	three := 3
	in := withEvents(warning("Cluster", "orders", "FailedCreate", "exceeded quota: compute"))
	in.HasCluster = true
	in.Cluster = observe.Snapshot{Cluster: observe.ClusterFacts{DesiredInstances: &three}}
	in.HasPods = true
	in.Pods = observe.PodsSnapshot{Pods: []observe.PodFacts{{Name: "orders-1"}, {Name: "orders-2"}}}

	finding := findingByID(t, Run(in), "resource-quota")
	var found bool
	for _, evidence := range finding.Evidence {
		if strings.Contains(evidence.Detail, "3 instances declared, 2 pods observed") {
			found = true
		}
	}
	if !found {
		t.Errorf("shortfall context missing: %+v", finding.Evidence)
	}

	// Without the pod roster there is no second number, so the line is
	// omitted rather than half-stated.
	bare := withEvents(warning("Cluster", "orders", "FailedCreate", "exceeded quota: compute"))
	for _, evidence := range findingByID(t, Run(bare), "resource-quota").Evidence {
		if strings.Contains(evidence.Detail, "pods observed") {
			t.Errorf("shortfall claimed without the counts: %+v", evidence)
		}
	}
}

// TestQuotaAndSchedulingDoNotBothClaimTheSameEvent proves the two
// detectors partition the FailedScheduling reason rather than each
// reporting it: a quota refusal is a quota finding, and reporting it
// twice would double-count one problem.
func TestQuotaAndSchedulingDoNotBothClaimTheSameEvent(t *testing.T) {
	t.Parallel()
	result := Run(withEvents(warning("Pod", "orders-3", "FailedScheduling",
		"0/3 nodes are available: exceeded quota: compute")))

	findingByID(t, result, "resource-quota")
	if outcomeOf(t, result, "pod-scheduling") != CheckClear {
		t.Error("the scheduling detector also claimed a quota refusal")
	}
}

// TestSchedulingReportsAnUnplaceablePod proves the ordinary case is
// still reported, with the scheduler's own constraint quoted.
func TestSchedulingReportsAnUnplaceablePod(t *testing.T) {
	t.Parallel()
	const message = "0/3 nodes are available: 3 Insufficient cpu"
	finding := findingByID(t,
		Run(withEvents(warning("Pod", "orders-3", "FailedScheduling", message))),
		"pod-scheduling")
	if finding.Severity != SeverityWarning {
		t.Errorf("severity = %v, want warning", finding.Severity)
	}
	if finding.Evidence[0].Detail != message {
		t.Errorf("constraint not quoted verbatim: %q", finding.Evidence[0].Detail)
	}
}

// TestImagePullNamesTheContainer proves the finding says which container
// failed. On a plugin pod that is the whole point: a sidecar failing to
// pull is a different incident from postgres failing to.
func TestImagePullNamesTheContainer(t *testing.T) {
	t.Parallel()
	in := Input{Now: now, HasPods: true, Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
		Name: "orders-1",
		Containers: []observe.ContainerFacts{
			{Name: "postgres", Image: "pg:16", State: "running"},
			{Name: "plugin-barman-cloud", Image: "barman:0.5", State: "waiting", Reason: "ImagePullBackOff"},
		},
	}}}}

	finding := findingByID(t, Run(in), "image-pull/orders-1/plugin-barman-cloud")
	if !strings.Contains(finding.Summary, "plugin-barman-cloud") {
		t.Errorf("summary does not name the container: %q", finding.Summary)
	}
	if !strings.Contains(finding.Evidence[0].Detail, "ImagePullBackOff") ||
		!strings.Contains(finding.Evidence[0].Detail, "barman:0.5") {
		t.Errorf("evidence lost the reason or image: %q", finding.Evidence[0].Detail)
	}
	// The healthy container produces nothing.
	if len(Run(in).Findings) != 1 {
		t.Errorf("a running container was also reported: %+v", Run(in).Findings)
	}
}

// TestImagePullIgnoresAnUnknownReason proves the closed set. A reason
// the console does not know is reported by no detector rather than by
// the wrong one.
func TestImagePullIgnoresAnUnknownReason(t *testing.T) {
	t.Parallel()
	in := Input{Now: now, HasPods: true, Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
		Name:       "orders-1",
		Containers: []observe.ContainerFacts{{Name: "postgres", State: "waiting", Reason: "CrashLoopBackOff"}},
	}}}}
	if outcomeOf(t, Run(in), "image-pull") != CheckClear {
		t.Error("a crash loop was reported as an image pull failure")
	}
}

// TestVolumeReportsAnUnboundClaimButNotAnUnreportedOne proves the
// inversion rule 4 forbids: an empty phase is unreported, not unbound,
// and must never be read as a fault.
func TestVolumeReportsAnUnboundClaimButNotAnUnreportedOne(t *testing.T) {
	t.Parallel()
	in := Input{
		Now: now, HasInfrastructure: true,
		Infrastructure: observe.InfrastructureSnapshot{Volumes: []observe.VolumeFacts{
			{Name: "orders-1", Phase: "Pending"},
			{Name: "orders-2", Phase: "Bound"},
		}},
	}
	finding := findingByID(t, Run(in), "volume-binding")
	if !strings.Contains(finding.Summary, "1 persistent volume") {
		t.Errorf("summary = %q, want it to count only the unbound claim", finding.Summary)
	}

	unreported := Input{
		Now: now, HasInfrastructure: true,
		Infrastructure: observe.InfrastructureSnapshot{Volumes: []observe.VolumeFacts{
			{Name: "orders-1", Phase: ""},
		}},
	}
	if outcomeOf(t, Run(unreported), "volume-binding") != CheckClear {
		t.Error("an unreported phase was treated as unbound")
	}
}

// TestVolumeAddsTheProvisionerReasonWhenThereIsOne proves the claim
// states that it did not bind and the event states why.
func TestVolumeAddsTheProvisionerReasonWhenThereIsOne(t *testing.T) {
	t.Parallel()
	const message = `storageclass.storage.k8s.io "fast" not found`
	in := withEvents(warning("PersistentVolumeClaim", "orders-1", "ProvisioningFailed", message))
	in.HasInfrastructure = true
	in.Infrastructure = observe.InfrastructureSnapshot{
		Volumes: []observe.VolumeFacts{{Name: "orders-1", Phase: "Pending"}},
	}

	var quoted bool
	for _, evidence := range findingByID(t, Run(in), "volume-binding").Evidence {
		if evidence.Detail == message {
			quoted = true
		}
	}
	if !quoted {
		t.Error("the provisioner's reason was not quoted alongside the unbound claim")
	}
}
