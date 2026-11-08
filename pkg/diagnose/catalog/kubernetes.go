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

package catalog

import (
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// kubernetesRules are the claims about Kubernetes itself: the kubelet
// and scheduler failures that keep a member pod from running, in the
// gaps the hand-written detectors leave. Quota refusals, unschedulable
// pods, image pulls and unbound volumes already have detectors; what is
// here is the rest of the ways a pod sits dead while every CloudNativePG
// status looks fine.
//
// The event reasons and container-state reasons are Kubernetes API
// conventions, stable across the versions this console meets, so these
// rules are unpinned — matching the barman file's reasoning, not an
// oversight. The one pinned rule rides on the observed server version,
// polled from the API server's own /version endpoint.
//
// NetworkPolicy problems have no rule here on purpose: a policy that
// drops traffic produces no event, no condition, and no state — only
// timeouts elsewhere. Its observable symptoms in this console are
// CloudNativePG's: the status-unreachable phase check and the
// instance-isolation log check.
func kubernetesRules() []diagnose.Rule {
	// timelineNote is the standing qualifier on a count taken from the
	// object timeline.
	const timelineNote = "The timeline coalesces rapid repeats and evicts old revisions to " +
		"stay inside its bounds, so the count is a floor and an absence rules " +
		"nothing out."
	return []diagnose.Rule{
		{
			ID:        "k8s-volume-mount-failed",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityCritical,
			Describes: "a pod that cannot mount or attach one of its volumes",
			Summary:   "A member pod cannot mount or attach a volume, so it cannot start.",
			Detail: "The kubelet's message below names the piece that is missing — most " +
				"often a Secret or ConfigMap a projected volume references, or a " +
				"volume the storage driver cannot attach to the node.",
			When: diagnose.EventMatch{Reasons: []string{
				"FailedMount", "FailedAttachVolume", "FailedMapVolume",
			}},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "k8s-pod-evicted",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityWarning,
			Describes: "a member pod evicted from its node",
			Summary:   "A member pod was evicted from its node.",
			Detail: "Eviction is the kubelet reclaiming resources under pressure; the " +
				"message names which resource ran short. The operator replaces the " +
				"pod, but repeated evictions mean the node is undersized for what is " +
				"scheduled onto it.",
			When:      diagnose.EventMatch{Reasons: []string{"Evicted"}},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "k8s-container-crashloop",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityCritical,
			Describes: "a container in CrashLoopBackOff",
			Summary:   "A container is crash-looping: it keeps exiting and the kubelet is backing off restarting it.",
			Detail: "A crash-looping postgres shows its cause in the log checks; this " +
				"check also catches every other container — a plugin sidecar or a " +
				"pooler crash-looping breaks backups or connections while the " +
				"instance itself reads healthy.",
			When: diagnose.ContainerState{Reasons: []string{"CrashLoopBackOff"}},
			NextSteps: "Read that container's log tail: the last lines before each exit " +
				"name the reason the kubelet cannot.",
			// Every cause here is a fact about one pod — its log, its own
			// container states — so the relation holds only on that pod.
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-wal-disk-full", Scope: diagnose.ScopePod},
				{Cause: "cnpg-postgres-exited", Scope: diagnose.ScopePod},
				{Cause: "k8s-container-oom", Scope: diagnose.ScopePod},
			},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "k8s-container-oom",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityHigh,
			Describes: "a container the kernel killed for exceeding its memory limit",
			Summary:   "A container was killed for exceeding its memory limit.",
			Detail: "OOMKilled is visible only while the termination state is current, " +
				"so one sighting is one kill, not a count. For postgres it usually " +
				"means the memory limit and the configured shared buffers plus work " +
				"memory do not fit together.",
			When: diagnose.ContainerState{Reasons: []string{"OOMKilled"}},
			NextSteps: "Raise the memory limit, or shrink what PostgreSQL is configured " +
				"to use — shared buffers and per-connection work memory are the " +
				"usual sum that no longer fits.",
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "k8s-container-config-error",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityCritical,
			Describes: "a container the kubelet cannot construct",
			Summary:   "A container cannot be created, so its pod is stuck before starting.",
			Detail: "CreateContainerConfigError is almost always a reference that " +
				"resolves to nothing: a Secret or ConfigMap named in the container's " +
				"environment that does not exist.",
			When: diagnose.ContainerState{Reasons: []string{
				"CreateContainerConfigError", "CreateContainerError",
			}},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			// The two timeline rules. Both read the object history
			// rather than a current snapshot, because what they report
			// is repetition: no single observation of a replaced pod or
			// a rewritten definition is wrong, and the fault is only
			// visible in the count. They are Kubernetes claims because
			// object lifecycle is the API server's own vocabulary —
			// creation, deletion, and the field manager that wrote last
			// mean the same thing whichever operator owns the object.
			ID:        "k8s-pod-replaced-repeatedly",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityWarning,
			Describes: "a pod replaced several times inside an hour",
			Summary:   "A pod has been replaced several times in the last hour.",
			Detail: "Each replacement is a new object wearing the same name, so this " +
				"counts identities rather than edits. A rollout replaces each member " +
				"once; several replacements of one name in an hour is something " +
				"destroying it faster than it settles — a failing probe, an evicting " +
				"node, or a controller and an operator disagreeing about whether it " +
				"should exist. " + timelineNote,
			When: diagnose.HistoryIncarnations{Kind: "Pod", Identities: 3, Within: time.Hour},
			NextSteps: "Open the timeline for that name and read what ended each " +
				"incarnation. The pod's own events say who deleted it; a kubelet " +
				"eviction and an operator replacement look nothing alike there.",
			Link:      "/history",
			LinkLabel: "History",
		},
		{
			ID:        "k8s-definition-rewritten-repeatedly",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityWarning,
			Describes: "an object whose definition is rewritten again and again inside an hour",
			Summary:   "An object's definition is being rewritten again and again.",
			Detail: "Repeated writes to the definition, as opposed to the status, mean " +
				"something keeps changing what the object should be. Two writers " +
				"disagreeing is the usual cause — a deployment pipeline reapplying " +
				"what a mutating webhook or an autoscaler immediately changes back — " +
				"and each rewrite restarts whatever reconciliation the change implies. " +
				"The field managers below are the API server's own record of who " +
				"wrote last. " + timelineNote,
			When: diagnose.HistoryChanges{
				Changes: []history.Change{history.ChangeSpec}, Count: 5, Within: time.Hour,
			},
			NextSteps: "The named field managers are the writers. Where two of them " +
				"alternate, one is undoing the other, and the fix is upstream of this " +
				"cluster: reconcile what the pipeline applies with what the admitting " +
				"webhook or autoscaler expects.",
			Link:      "/history",
			LinkLabel: "History",
		},
		{
			// Like postgres-eol: the version is the finding, and where
			// support ends is the lifecycle table's data, not code.
			ID:        "k8s-eol",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityWarning,
			Describes: "a Kubernetes version past upstream end of life",
			Summary:   "The Kubernetes server version no longer receives upstream patches.",
			Detail: "Kubernetes patches each minor for about fourteen months, so bug and " +
				"security fixes are no longer published for this one. Where support ends " +
				"is the lifecycle table's claim, quoted below with the version it is applied to.",
			When:      diagnose.EndOfLife{Component: diagnose.ComponentKubernetes},
			Link:      "/",
			LinkLabel: "Overview",
		},
		{
			ID:        "k8s-eol-soon",
			Component: diagnose.ComponentKubernetes,
			Layer:     diagnose.LayerKubernetes,
			Severity:  diagnose.SeverityNote,
			Describes: "a Kubernetes version leaving upstream support within 90 days",
			Summary:   "The Kubernetes server version leaves upstream support soon.",
			Detail:    "The date below, from the lifecycle table, is when patches stop for this minor.",
			When:      diagnose.EndOfLife{Component: diagnose.ComponentKubernetes, Within: 90 * 24 * time.Hour},
			Link:      "/",
			LinkLabel: "Overview",
		},
	}
}
