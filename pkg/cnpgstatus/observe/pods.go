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

package observe

import (
	"time"
)

// PodFacts is the Kubernetes-observed state of one instance pod. Empty
// strings and nil numerics are unreported facts and render as unknown.
type PodFacts struct {
	// Name is the pod name.
	Name string
	// UID distinguishes incarnations sharing a name across restarts.
	UID string
	// Role is the instance role label value, such as "primary".
	Role string
	// Phase is the pod phase.
	Phase string
	// Ready reports the pod Ready condition; nil when unreported.
	Ready *bool
	// Restarts is the PostgreSQL container's restart count; nil when
	// unreported.
	//
	// Deliberately not the sum across containers, which is what kubectl
	// shows. On an instance row the number is read as "the database
	// restarted this many times", and once plugin sidecars exist a
	// summed count makes a crash-looping backup sidecar look like an
	// unstable instance. Per-container counts are in Containers.
	Restarts *int
	// Node is the assigned node name.
	Node string
	// IP is the pod IP, empty until assigned. The metrics scraper dials
	// it and the pod detail states it.
	IP string
	// Started is the reported start time; nil when unreported.
	Started *time.Time
	// Image is the PostgreSQL container image.
	Image string
	// MemoryLimit is the postgres container's memory limit in bytes, as
	// the Pod carries it (a LimitRange may have set it); nil when none.
	MemoryLimit *int64
	// OwnerUID is the UID of the Cluster that controls this pod, from
	// its owner reference; empty when the pod carries none.
	OwnerUID string
	// Deleting reports a set deletion timestamp.
	Deleting bool
	// StatusScheme is how the instance manager serves its status port,
	// "https" or "http", read from the pod spec the operator rendered
	// (see kube.statusScheme). Empty for a pod that is not an instance.
	StatusScheme string
	// Containers is every container the pod declares, init containers
	// first and each group in spec order. An instance pod is no longer
	// one container: CNPG-I ships capabilities as plugin sidecars, so
	// the backup half of the pod can be failing while postgres is
	// perfectly healthy. The pod-level fields above describe the
	// instance; this describes the pod.
	Containers []ContainerFacts
}

// ContainerFacts is the Kubernetes-observed state of one container in an
// instance pod. Empty strings and nil numerics are unreported facts and
// render as unknown.
type ContainerFacts struct {
	// Name is the container name, unique within the pod.
	Name string
	// Image is the container's declared image.
	Image string
	// Init reports an init container. Its failure blocks the pod from
	// starting at all, which reads very differently from a sidecar that
	// is merely unhealthy.
	Init bool
	// Ready reports the container's readiness; nil when the kubelet has
	// reported no status for it yet.
	Ready *bool
	// Restarts is this container's own restart count; nil when
	// unreported. Per container, never summed: a crash-looping sidecar
	// is not the instance restarting.
	Restarts *int
	// State is the reported state — "running", "waiting", or
	// "terminated" — and empty when the kubelet has reported none.
	State string
	// Reason is the kubelet's machine reason for a waiting or terminated
	// state: ImagePullBackOff, CrashLoopBackOff, CreateContainerConfigError,
	// OOMKilled, and so on. It is the field that names what is actually
	// wrong, and it is empty for a running container.
	Reason string
	// ExitCode is the last termination's exit code; nil unless the
	// container has terminated.
	ExitCode *int32
}

// PodsSnapshot is the rendered pod set. It is immutable and carries its
// own staleness, independent of the cluster snapshot: sources fail
// independently.
type PodsSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports lost contact; the pod set below is the retained
	// last-good observation.
	Stale bool
	// Truncated reports that more pods matched than the producer keeps and the set was
	// cut at the bound.
	Truncated bool
	// Pods is sorted by name; the producer bounds it.
	Pods []PodFacts
	// LastOperatorImage is the operator's own image as last seen on an
	// instance pod's bootstrap-controller init container, with the pod
	// and the observation it was seen in. It survives the pods' absence
	// on purpose: a hibernated or wholly failed cluster has no pod to
	// read the operator version from, and that is precisely when the
	// version-pinned checks matter. Empty until a pod has carried it.
	LastOperatorImage       string
	LastOperatorImagePod    string
	LastOperatorImageSeenAt time.Time
	// LastOperatorImageOwner is the UID of the Cluster that owned the
	// pod the image was seen on, so a recreated Cluster of the same
	// name never inherits it.
	LastOperatorImageOwner string
}

// BootstrapControllerContainer is the init container the operator
// injects into every instance pod, running the operator's own image.
const BootstrapControllerContainer = "bootstrap-controller"
