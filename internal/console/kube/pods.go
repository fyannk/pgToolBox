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

package kube

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// roleLabel is the CloudNativePG instance role label.
const roleLabel = "cnpg.io/instanceRole"

// postgresContainer is the CNPG instance container name whose image is
// reported.
const postgresContainer = "postgres"

// convertPod converts a raw pod into facts plus its membership verdict.
// Membership requires the controller owner reference to point at the
// configured CloudNativePG Cluster: the label already selected the pod,
// but labels are freely settable, so ownership is verified before the
// pod is presented as a cluster member.
func (c *Client) convertPod(content map[string]any) (observe.PodFacts, bool, error) {
	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &pod); err != nil {
		return observe.PodFacts{}, false, redact.NewError("pod convert", redact.CategoryInternal, err)
	}

	facts := observe.PodFacts{
		Name:     pod.Name,
		UID:      string(pod.UID),
		Role:     pod.Labels[roleLabel],
		OwnerUID: controllingClusterUID(pod.OwnerReferences),
		Phase:    string(pod.Status.Phase),
		Node:     pod.Spec.NodeName,
		IP:       pod.Status.PodIP,
		Deleting: pod.DeletionTimestamp != nil,
	}
	facts.StatusScheme = statusScheme(&pod)

	if pod.Status.StartTime != nil {
		started := pod.Status.StartTime.Time
		facts.Started = &started
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			ready := cond.Status == corev1.ConditionTrue
			facts.Ready = &ready
			break
		}
	}
	for i := range pod.Spec.Containers {
		if c := &pod.Spec.Containers[i]; c.Name == postgresContainer {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				v := q.Value()
				facts.MemoryLimit = &v
			}
		}
	}
	facts.Containers = convertContainers(&pod)
	for _, container := range facts.Containers {
		if container.Name == postgresContainer && !container.Init {
			facts.Restarts = container.Restarts
			facts.Image = container.Image
			break
		}
	}
	// A pod with no container named postgres is not one this console
	// expects, but membership has already been proven, so it is shown
	// rather than hidden. The first regular container's image is the
	// least misleading stand-in available.
	if facts.Image == "" {
		for _, container := range facts.Containers {
			if !container.Init {
				facts.Image = container.Image
				break
			}
		}
	}

	return facts, c.podIsMember(&pod), nil
}

// statusScheme is the scheme the instance manager serves its status port
// with, decided from the pod spec the operator rendered, the way
// CloudNativePG does: HTTPS when the postgres container's HTTP probes
// use HTTPS — every pod from 1.31, where TLS is the only mode, and
// TLS-enabled pods before — or when its command carries
// --status-port-tls (1.30's GetStatusSchemeFromPod). Anything else is a
// pre-TLS instance manager serving plain HTTP. The answer comes from the
// API server, not from the network, so it cannot be downgraded by
// anything on the path to the pod.
func statusScheme(pod *corev1.Pod) string {
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if container.Name != postgresContainer {
			continue
		}
		for _, probe := range []*corev1.Probe{container.StartupProbe, container.LivenessProbe, container.ReadinessProbe} {
			if probe != nil && probe.HTTPGet != nil && probe.HTTPGet.Scheme == corev1.URISchemeHTTPS {
				return "https"
			}
		}
		if slices.Contains(container.Command, "--status-port-tls") {
			return "https"
		}
		return "http"
	}
	return ""
}

// convertContainers pairs each declared container with the kubelet's
// status for it, init containers first and each group in spec order.
//
// The spec is the source of the list, not the status: a container that
// has never started has no status at all, and dropping it would hide
// exactly the pod that cannot start. Such a container reports its name
// and image with an empty state, which renders as unknown rather than as
// healthy.
func convertContainers(pod *corev1.Pod) []observe.ContainerFacts {
	total := len(pod.Spec.InitContainers) + len(pod.Spec.Containers)
	if total == 0 {
		return nil
	}
	status := make(map[string]corev1.ContainerStatus, total)
	for _, cs := range pod.Status.InitContainerStatuses {
		status["init/"+cs.Name] = cs
	}
	for _, cs := range pod.Status.ContainerStatuses {
		status["run/"+cs.Name] = cs
	}

	facts := make([]observe.ContainerFacts, 0, total)
	add := func(spec corev1.Container, init bool) {
		key := "run/"
		if init {
			key = "init/"
		}
		container := observe.ContainerFacts{Name: spec.Name, Image: spec.Image, Init: init}
		if cs, ok := status[key+spec.Name]; ok {
			ready := cs.Ready
			restarts := int(cs.RestartCount)
			container.Ready = &ready
			container.Restarts = &restarts
			container.State, container.Reason, container.ExitCode = containerState(cs.State)
		}
		facts = append(facts, container)
	}
	for _, spec := range pod.Spec.InitContainers {
		add(spec, true)
	}
	for _, spec := range pod.Spec.Containers {
		add(spec, false)
	}
	return facts
}

// containerState reduces the kubelet's three-way state union to a state
// name, its machine reason, and an exit code where one exists. A running
// container has no reason: the reason field exists to name a fault, and
// filling it for a healthy container would make "has a reason" useless
// as a signal.
func containerState(state corev1.ContainerState) (name, reason string, exitCode *int32) {
	switch {
	case state.Running != nil:
		return "running", "", nil
	case state.Waiting != nil:
		return "waiting", state.Waiting.Reason, nil
	case state.Terminated != nil:
		code := state.Terminated.ExitCode
		return "terminated", state.Terminated.Reason, &code
	default:
		return "", "", nil
	}
}

// podIsMember verifies the controller owner reference names the
// configured Cluster in the CloudNativePG group.
func (c *Client) podIsMember(pod *corev1.Pod) bool {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return false
	}
	group, _, ok := splitAPIVersion(ref.APIVersion)
	if !ok || group != clusterGVR.Group {
		return false
	}
	return ref.Kind == "Cluster" && ref.Name == c.opts.ClusterName
}

// splitAPIVersion splits "group/version"; a core "version" has no group.
func splitAPIVersion(apiVersion string) (group, version string, ok bool) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return "", "", false
	}
	return gv.Group, gv.Version, true
}

// controllingClusterUID is the UID of the Cluster that controls the
// object, from its owner references; empty when none does.
func controllingClusterUID(refs []metav1.OwnerReference) string {
	for i := range refs {
		ref := &refs[i]
		if ref.Controller != nil && *ref.Controller && ref.Kind == "Cluster" {
			return string(ref.UID)
		}
	}
	return ""
}
