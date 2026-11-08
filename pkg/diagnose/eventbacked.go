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

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// The event-backed detectors all work the same way: a symptom visible in
// one snapshot, correlated with the event in which Kubernetes already
// said why. The value is entirely in putting the two next to each other
// — the cause is never inferred, it is quoted.
//
// That is also why these detectors prefer events over their own
// reasoning. An event message states the refusal in the API server's own
// words, including the numbers a reader needs; a detector that
// paraphrased would be adding a claim to a source that already made one.

// warningEvents returns the Warning events whose reason is in the set,
// newest first, bounded to keep one flapping object from filling a
// finding with the same line.
func warningEvents(events []observe.EventFacts, reasons ...string) []observe.EventFacts {
	const maxQuoted = 3
	var matched []observe.EventFacts
	for _, event := range events {
		if event.Type != "Warning" {
			continue
		}
		for _, reason := range reasons {
			if event.Reason == reason {
				matched = append(matched, event)
				break
			}
		}
		if len(matched) >= maxQuoted {
			break
		}
	}
	return matched
}

// eventEvidence quotes one event as the API server wrote it.
func eventEvidence(event observe.EventFacts) Evidence {
	object := event.Kind + "/" + event.Object
	if event.Kind == "" {
		object = event.Object
	}
	detail := event.Message
	if event.Count > 1 {
		detail = fmt.Sprintf("%s (×%d)", detail, event.Count)
	}
	return Evidence{Origin: "Kubernetes-observed", Object: object, Detail: detail}
}

// quotaDetector reports a create the API server refused against a
// ResourceQuota.
//
// No ResourceQuota read is needed, and none is taken. The admission
// message already carries the numbers — "used: pods=8, limited: pods=8"
// — so reading the quota object would add a grant to restate what the
// evidence says. Quoting the refusal is both cheaper and more honest.
//
// It implements Detector.
type quotaDetector struct{}

func (quotaDetector) Name() string { return "resource-quota" }

func (quotaDetector) Layer() Layer { return LayerKubernetes }

func (quotaDetector) Describes() string {
	return "an object the API server refused to create against a namespace quota"
}

func (d quotaDetector) Detect(in Input) ([]Finding, string) {
	if reason := eventsUnavailable(in); reason != "" {
		return nil, reason
	}
	matched := warningEvents(in.Events.Events, "FailedCreate", "FailedScheduling")
	var quota []observe.EventFacts
	for _, event := range matched {
		if strings.Contains(event.Message, "exceeded quota") ||
			strings.Contains(event.Message, "forbidden: quota") {
			quota = append(quota, event)
		}
	}
	if len(quota) == 0 {
		return nil, ""
	}

	finding := Finding{
		ID:       "resource-quota",
		Subject:  EntityRef{Kind: quota[0].Kind, Name: quota[0].Object},
		At:       quota[0].LastSeen,
		Severity: SeverityCritical,
		Summary:  "The namespace quota is refusing objects this cluster needs.",
		Detail: "The API server rejected a create, so the object does not exist and " +
			"nothing will retry it into existence. The refusal below carries the " +
			"quota's own numbers.",
		NextSteps: "Raise the quota's ceiling or lower what the cluster asks for; the " +
			"operator retries refused objects on its own once room exists.",
		ConsequenceOf: []Relation{{Cause: "quota-exhausted"}},
		Link:          "/cluster/pods",
		LinkLabel:     "Pods",
	}
	for _, event := range quota {
		finding.Evidence = append(finding.Evidence, eventEvidence(event))
	}
	if shortfall, ok := instanceShortfall(in); ok {
		finding.Evidence = append(finding.Evidence, shortfall)
	}
	return []Finding{finding}, ""
}

// instanceShortfall states the declared and observed instance counts when
// both are known. It is context on a refusal, never a finding of its own:
// a cluster can be mid-scale for entirely ordinary reasons.
func instanceShortfall(in Input) (Evidence, bool) {
	// Both counts must be current: a stale roster or a stale declared
	// count would state a shortfall the cluster may no longer have.
	if !in.HasCluster || in.Cluster.Stale || in.Cluster.Cluster.DesiredInstances == nil ||
		podsUnavailable(in) != "" {
		return Evidence{}, false
	}
	desired := *in.Cluster.Cluster.DesiredInstances
	observed := len(in.Pods.Pods)
	if observed >= desired {
		return Evidence{}, false
	}
	return Evidence{
		Origin: "operator-reported and Kubernetes-observed",
		Object: "Cluster instances",
		Detail: fmt.Sprintf("%d instances declared, %d pods observed", desired, observed),
	}, true
}

// schedulingDetector reports a pod the scheduler cannot place. The event
// names the actual constraint — insufficient cpu, no matching node, an
// unsatisfied affinity — which is the part an operator cannot guess.
//
// It implements Detector.
type schedulingDetector struct{}

func (schedulingDetector) Name() string { return "pod-scheduling" }

func (schedulingDetector) Layer() Layer { return LayerKubernetes }

func (schedulingDetector) Describes() string {
	return "a pod the scheduler cannot place on any node"
}

func (d schedulingDetector) Detect(in Input) ([]Finding, string) {
	if reason := eventsUnavailable(in); reason != "" {
		return nil, reason
	}
	var unplaceable []observe.EventFacts
	for _, event := range warningEvents(in.Events.Events, "FailedScheduling") {
		// A quota refusal is the quota detector's finding, not this one.
		if strings.Contains(event.Message, "exceeded quota") {
			continue
		}
		unplaceable = append(unplaceable, event)
	}
	if len(unplaceable) == 0 {
		return nil, ""
	}
	finding := Finding{
		ID:       "pod-scheduling",
		Subject:  EntityRef{Kind: unplaceable[0].Kind, Name: unplaceable[0].Object},
		At:       unplaceable[0].LastSeen,
		Severity: SeverityWarning,
		Summary:  "A pod cannot be scheduled onto any node.",
		Detail:   "The pod exists and will stay Pending until the constraint below is satisfied.",
		NextSteps: "The scheduler's message names the missing piece. A missing " +
			"persistentvolumeclaim usually means the claim's creation was refused — " +
			"an exhausted storage quota is the common cause — while insufficient " +
			"cpu or memory needs node capacity or smaller requests, and an " +
			"unsatisfied affinity needs a node that matches.",
		// Plausible, not established: the scheduler's message is the only
		// thing that ties an unplaceable pod to a refused claim.
		ConsequenceOf: []Relation{
			{Cause: "quota-exhausted", Strength: StrengthPlausible},
			{Cause: "resource-quota", Strength: StrengthPlausible},
		},
		Link:      "/cluster/pods",
		LinkLabel: "Pods",
	}
	for _, event := range unplaceable {
		finding.Evidence = append(finding.Evidence, eventEvidence(event))
	}
	return []Finding{finding}, ""
}

// imagePullDetector reports a container whose image cannot be pulled.
//
// This one reads the container states rather than events, because since
// the pod model became container-aware the kubelet's reason is on the
// container itself — and it names which container, which an event on the
// pod does not. In a plugin world that distinction is the whole point:
// a sidecar failing to pull is a very different incident from postgres
// failing to.
//
// It implements Detector.
type imagePullDetector struct{}

func (imagePullDetector) Name() string { return "image-pull" }

func (imagePullDetector) Layer() Layer { return LayerKubernetes }

func (imagePullDetector) Describes() string {
	return "a container whose image the kubelet cannot pull"
}

func (d imagePullDetector) Detect(in Input) ([]Finding, string) {
	if reason := podsUnavailable(in); reason != "" {
		return nil, reason
	}
	var findings []Finding
	for _, pod := range in.Pods.Pods {
		for _, container := range pod.Containers {
			if !isImagePullReason(container.Reason) {
				continue
			}
			findings = append(findings, Finding{
				ID:       "image-pull/" + pod.Name + "/" + container.Name,
				Subject:  EntityRef{Kind: "Pod", Name: pod.Name},
				Severity: SeverityCritical,
				Summary: fmt.Sprintf("Container %s in pod %s cannot pull its image.",
					container.Name, pod.Name),
				Detail: "The container cannot start, so whatever it provides is absent " +
					"until the image is reachable.",
				Evidence: []Evidence{{
					Origin: "Kubernetes-observed",
					Object: "Pod/" + pod.Name,
					Detail: fmt.Sprintf("container %q: %s, image %q",
						container.Name, container.Reason, container.Image),
				}},
				Link:      "/cluster/pods/" + pod.Name,
				LinkLabel: "Pod detail",
			})
		}
	}
	return findings, ""
}

// isImagePullReason names the kubelet's pull failures. The set is closed
// rather than a substring match, so a reason the console does not know
// is reported by no detector instead of by the wrong one.
func isImagePullReason(reason string) bool {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull":
		return true
	default:
		return false
	}
}

// volumeDetector reports a claim that never bound. It is the classic
// first-install failure: the pod is Pending forever, and the reason is
// on the claim rather than on the pod.
//
// It implements Detector.
type volumeDetector struct{}

func (volumeDetector) Name() string { return "volume-binding" }

func (volumeDetector) Layer() Layer { return LayerKubernetes }

func (volumeDetector) Describes() string {
	return "a persistent volume claim that has not bound"
}

func (d volumeDetector) Detect(in Input) ([]Finding, string) {
	if reason := infrastructureUnavailable(in); reason != "" {
		return nil, reason
	}
	var unbound []observe.VolumeFacts
	for _, volume := range in.Infrastructure.Volumes {
		// An empty phase is unreported, not unbound. Treating unknown as
		// a fault is exactly the inversion rule 4 forbids.
		if volume.Phase != "" && volume.Phase != "Bound" {
			unbound = append(unbound, volume)
		}
	}
	if len(unbound) == 0 {
		return nil, ""
	}

	finding := Finding{
		ID:        "volume-binding",
		Severity:  SeverityCritical,
		Summary:   fmt.Sprintf("%d persistent volume claims have not bound.", len(unbound)),
		Detail:    "An instance cannot start without its data volume, so the pod stays Pending.",
		Link:      "/objects",
		LinkLabel: "Objects",
	}
	// One unbound claim is the finding's subject; several are the
	// cluster's problem, and the finding names no single one.
	if len(unbound) == 1 {
		finding.Subject = EntityRef{Kind: "PersistentVolumeClaim", Name: unbound[0].Name}
	}
	for _, volume := range unbound {
		finding.Evidence = append(finding.Evidence, Evidence{
			Origin: "Kubernetes-observed",
			Object: "PersistentVolumeClaim/" + volume.Name,
			Detail: "phase " + volume.Phase,
		})
	}
	// The claim states that it is not bound; the event states why.
	if eventsUnavailable(in) == "" {
		for _, event := range warningEvents(in.Events.Events,
			"ProvisioningFailed", "FailedBinding", "VolumeBindingFailed") {
			finding.Evidence = append(finding.Evidence, eventEvidence(event))
		}
	}
	return []Finding{finding}, ""
}
