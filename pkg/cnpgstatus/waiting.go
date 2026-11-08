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

package cnpgstatus

import (
	"slices"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
)

// "Why is the operator waiting?"
//
// The fork (fyannk/cloudnative-pg origin/dev/6 and dev/12) states some
// causes itself, as Warning Event reasons it records on the object whose
// reconciliation failed (internal/controller/reconciliation_error.go
// dev/6:38-46: QuotaExceeded, AdmissionDenied, ReconciliationFailed;
// BackupDelayed on the ScheduledBackup, scheduledbackup_controller.go
// dev/6:152; VolumeSnapshotError on the Backup,
// pkg/reconciler/backup/volumesnapshot/reconciler.go dev/6:684). Those are
// reported causes: the operator said so.
//
// Upstream CloudNativePG (v1.30.0, main) has no such codes. Its
// phases are fixed sentences (byte-identical across both, 1.30 having added
// "Invalid cluster definition"), its phaseReason is free text, and the
// scheduler and kubelet state of its Pods is not something it repeats.
// WaitReasons therefore also derives causes from the phase, the
// phaseReason's fixed prefixes, the Cluster's PVC lists, Pod conditions
// and container states, and Events; every such line is a heuristic, says
// so (Heuristic true), and lists the evidence it rests on. Each rule
// below cites where the operator sets what it keys on, or where it waits.
//
// Citations are file:line at v1.30.0 unless stated, with main where the
// line differs; verified 2026-10-30 against ../cloudnative-pg (tag
// v1.30.0, upstream/main).

// WaitReason is one explanation of why the operator does not progress.
type WaitReason struct {
	// ID is stable API (kebab-case): the UI keys translations and tests
	// on it.
	ID string `json:"id"`
	// Text is the explanation in English.
	Text string `json:"text"`
	// Heuristic is true when the console derived the cause; false when
	// the operator itself reported it (a fork reason code, or the exact
	// phase and phaseReason the operator writes for that cause).
	Heuristic bool `json:"heuristic"`
	// Evidence lists what the reason rests on, one fact per line, each
	// naming its source ("Pod/orders-2 PodScheduled=False Unschedulable:
	// 0/3 nodes are available ...").
	Evidence []string `json:"evidence"`
}

// Wait-reason IDs.
const (
	WaitForkQuotaExceeded        = "fork-quota-exceeded"
	WaitForkAdmissionDenied      = "fork-admission-denied"
	WaitForkReconciliationFailed = "fork-reconciliation-failed"
	WaitForkBackupDelayed        = "fork-backup-delayed"
	WaitForkVolumeSnapshotError  = "fork-volume-snapshot-error"
	WaitInstancesNotActive       = "instances-not-active"
	WaitPodUnschedulable         = "pod-unschedulable"
	WaitImagePull                = "image-pull"
	WaitCrashLoop                = "crash-loop"
	WaitInstanceCreationFailed   = "instance-creation-failed"
	WaitPVCsLost                 = "pvcs-lost"
	WaitPVCDangling              = "pvc-dangling"
	WaitPVCUnusable              = "pvc-unusable"
	WaitPVCInitializing          = "pvc-initializing"
	WaitPVCResizing              = "pvc-resizing"
	WaitSupervisedSwitchover     = "supervised-switchover"
	WaitRolloutDelayed           = "rollout-delayed"
	WaitDefinitionInvalid        = "definition-invalid"
	WaitCannotCreateObjects      = "cannot-create-objects"
	WaitQuotaRefused             = "quota-refused"
	WaitAdmissionRefused         = "admission-refused"
	WaitSwitchoverPending        = "switchover-pending"
	WaitPrimaryStatusCheck       = "primary-status-check-failed"
	WaitBackupPending            = "backup-pending"
)

// Bounds of the evidence: lines per reason and bytes per line.
const (
	maxWaitEvidence     = 8
	maxWaitEvidenceText = 512
)

// Phase reason prefixes the operator writes verbatim (the rest of the
// text names jobs, pods or the API error).
const (
	// cluster_controller.go v1.30.0:860; main:854.
	reasonInstanceJobsFailed = "Instance creation failed for the following jobs:"
	// main:882 (PLANNED 1.31: bootstrap runs in an init container).
	reasonInstanceBootstrapFailed = "Instance bootstrap failed for the following Pods:"
	// cluster_create.go v1.30.0:1158 (main:1162).
	reasonPVCsLost = "One or more instances were previously created, but no PersistentVolumeClaims"
	// cluster_upgrade.go:135, identical at the three tags.
	reasonSupervisedSwitchover = "User must issue a supervised switchover"
)

// The fork's reconciliation-error reasons (reported causes), with the
// explanation each one carries.
var forkWaitEvents = []struct {
	reason, id, text string
}{
	{"QuotaExceeded", WaitForkQuotaExceeded, "A ResourceQuota refused an object the operator creates."},
	{"AdmissionDenied", WaitForkAdmissionDenied, "An admission webhook or Pod Security admission refused an object the operator creates."},
	{"ReconciliationFailed", WaitForkReconciliationFailed, "The operator's last reconciliation failed."},
	{"BackupDelayed", WaitForkBackupDelayed, "A scheduled backup is delayed."},
	{"VolumeSnapshotError", WaitForkVolumeSnapshotError, "A volume-snapshot backup failed to take its snapshot."},
}

// WaitInput is what WaitReasons reads.
type WaitInput struct {
	Minor   Minor
	Cluster *cnpgv1.Cluster
	// Pods are the Cluster's instance Pods.
	Pods []*corev1.Pod
	// Events are the Events about the Cluster and its children (Pods,
	// Backups, ScheduledBackups, Poolers), any order.
	Events  []*corev1.Event
	Backups []*cnpgv1.Backup
}

// WaitReasons explains why the operator waits. It returns nil for a
// healthy phase: an operator that reports Healthy is not waiting, and a
// stale PVC list must not contradict it.
func WaitReasons(in WaitInput) []WaitReason {
	c := in.Cluster
	if c == nil || ClassifyPhase(in.Minor, c.Status.Phase).Class == ClassOK {
		return nil
	}
	pods := slices.Clone(in.Pods)
	slices.SortFunc(pods, func(a, b *corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	var out []WaitReason
	add := func(r WaitReason) {
		if len(r.Evidence) > maxWaitEvidence {
			r.Evidence = append(r.Evidence[:maxWaitEvidence], "(more not shown)")
		}
		for i, e := range r.Evidence {
			r.Evidence[i] = bound(e, maxWaitEvidenceText)
		}
		out = append(out, r)
	}
	phase, reason := c.Status.Phase, c.Status.PhaseReason
	phaseLine := "Cluster.status.phase = " + phase
	reasonLine := "Cluster.status.phaseReason = " + reason

	for _, f := range forkWaitEvents {
		if e := newestEvent(in.Events, corev1.EventTypeWarning, f.reason); e != nil {
			add(WaitReason{ID: f.id, Text: f.text, Evidence: []string{eventLine(e)}})
		}
	}

	switch {
	case phase == cnpgv1.PhaseWaitingForInstancesToBeActive:
		// cluster_controller.go v1.30.0:910 (main:978): the
		// operator stops until every instance Pod is active
		// (pkg/utils/pod_conditions.go:72 IsPodActive: not Pending,
		// Failed or Succeeded, and not being deleted). Which Pod holds it
		// up is the console's reading; the fork's phaseReason names it.
		r := WaitReason{
			ID: WaitInstancesNotActive, Heuristic: true,
			Text:     "The operator does nothing more until every instance Pod is running; these Pods are not.",
			Evidence: []string{phaseLine},
		}
		for _, p := range pods {
			if line, inactive := podInactive(p); inactive {
				r.Evidence = append(r.Evidence, line)
			}
		}
		add(r)
	case phase == cnpgv1.PhaseUnrecoverable && strings.HasPrefix(reason, reasonPVCsLost):
		r := WaitReason{
			ID: WaitPVCsLost,
			Text: "The operator created instances before but finds none of their PersistentVolumeClaims; " +
				"it will not act on its own. Restore from a backup, or, if this operator runs the fork's filtered cache " +
				"(FILTERED_CACHE_MODE strict or reload), let it label this Cluster's objects first.",
			Evidence: []string{phaseLine, reasonLine},
		}
		if mixedReconcileLabels(pods) {
			r.Evidence = append(r.Evidence, "some instance Pods carry cnpg.io/reconcile and others do not: "+
				"a filtered-cache operator labelled only part of this Cluster")
		}
		add(r)
	case phase == cnpgv1.PhaseUnrecoverable && (strings.HasPrefix(reason, reasonInstanceJobsFailed) || strings.HasPrefix(reason, reasonInstanceBootstrapFailed)):
		add(WaitReason{
			ID:       WaitInstanceCreationFailed,
			Text:     "Creating an instance failed for good; there is no automatic recovery. Read the failed job's or bootstrap container's logs.",
			Evidence: []string{phaseLine, reasonLine},
		})
	case phase == cnpgv1.PhaseWaitingForUser && reason == reasonSupervisedSwitchover:
		// cluster_upgrade.go:135 (all three tags).
		add(WaitReason{
			ID: WaitSupervisedSwitchover,
			Text: "primaryUpdateStrategy is supervised: the primary needs a restart or an update, and the operator waits for " +
				"someone to switch over (kubectl cnpg promote) or restart it.",
			Evidence: []string{phaseLine, reasonLine},
		})
	case phase == cnpgv1.PhaseUpgradeDelayed:
		// cluster_controller.go v1.30.0:1217 (main:1277);
		// cluster_upgrade.go:87,149; the delays are the operator's
		// CLUSTERS_ROLLOUT_DELAY / INSTANCES_ROLLOUT_DELAY
		// (internal/configuration/configuration.go:156,162). The reason
		// text changed on main: key on the phase and the event only.
		r := WaitReason{
			ID: WaitRolloutDelayed,
			Text: "The operator's own rollout delay (CLUSTERS_ROLLOUT_DELAY / INSTANCES_ROLLOUT_DELAY in its configuration) " +
				"holds the rollout back; it resumes by itself.",
			Evidence: []string{phaseLine},
		}
		if e := newestEvent(in.Events, corev1.EventTypeNormal, "RolloutDelayed"); e != nil {
			r.Evidence = append(r.Evidence, eventLine(e))
		}
		add(r)
	case phase == cnpgv1.PhaseDefinitionInvalid && in.Minor.atLeast(Minor130):
		// api/v1/cluster_funcs.go v1.30.0:1654 (main:1673), set by
		// internal/webhook/guard/guard.go:178.
		add(WaitReason{
			ID:       WaitDefinitionInvalid,
			Text:     "The Cluster's spec fails the operator's validation; it stops until the spec is fixed.",
			Evidence: []string{phaseLine, reasonLine},
		})
	case phase == cnpgv1.PhaseCannotCreateClusterObjects:
		// cluster_controller.go v1.30.0:377 (main:376):
		// phaseReason is the raw API error. The quota and admission
		// readings mirror the fork's classification
		// (reconciliation_error.go dev/6) on the message text, because the
		// API server gives quota no structured reason.
		add(cannotCreate(phaseLine, reasonLine, reason))
	}

	if c.Status.TargetPrimary != "" && c.Status.CurrentPrimary != "" && c.Status.TargetPrimary != c.Status.CurrentPrimary {
		// cluster_controller.go v1.30.0:756-772; replicas.go:189,322
		// (main:211,344).
		r := WaitReason{
			ID:   WaitSwitchoverPending,
			Text: "The operator asked " + c.Status.TargetPrimary + " to become primary and waits for it.",
			Evidence: []string{
				"Cluster.status.currentPrimary = " + c.Status.CurrentPrimary,
				"Cluster.status.targetPrimary = " + c.Status.TargetPrimary,
			},
		}
		if t := parseOperatorTimestamp(c.Status.TargetPrimaryTimestamp); t != nil {
			r.Evidence = append(r.Evidence, "Cluster.status.targetPrimaryTimestamp = "+t.UTC().Format(time.RFC3339))
		}
		add(r)
	}
	if e := newestEvent(in.Events, corev1.EventTypeWarning, "PrimaryStatusCheckFailed"); e != nil {
		// cluster_controller.go v1.30.0:676 (main:680): the
		// failover is deferred until Kubernetes marks the primary not
		// Ready.
		add(WaitReason{
			ID: WaitPrimaryStatusCheck, Heuristic: true,
			Text:     "The operator could not read the primary's status; it defers a failover until Kubernetes marks the primary not Ready.",
			Evidence: []string{eventLine(e)},
		})
	}

	podReasons(pods, phase, reason, add)
	pvcReasons(c, add)
	backupReasons(in, add)
	return out
}

func cannotCreate(phaseLine, reasonLine, reason string) WaitReason {
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "exceeded quota") || strings.Contains(lower, "failed quota"):
		return WaitReason{
			ID: WaitQuotaRefused, Heuristic: true,
			Text:     "A ResourceQuota refused an object the operator creates; raise the quota or lower the Cluster's requests.",
			Evidence: []string{phaseLine, reasonLine},
		}
	case strings.Contains(lower, "denied the request") || strings.Contains(lower, "violates podsecurity"):
		return WaitReason{
			ID: WaitAdmissionRefused, Heuristic: true,
			Text:     "An admission webhook or Pod Security admission refused an object the operator creates.",
			Evidence: []string{phaseLine, reasonLine},
		}
	default:
		return WaitReason{
			ID:       WaitCannotCreateObjects,
			Text:     "Kubernetes refused an object the operator creates; the phase reason carries the API server's answer.",
			Evidence: []string{phaseLine, reasonLine},
		}
	}
}

// podReasons reads the scheduler's and the kubelet's state of the
// instance Pods. CloudNativePG never repeats these reasons; the link to
// the wait is the console's (heuristic).
func podReasons(pods []*corev1.Pod, phase, reason string, add func(WaitReason)) {
	unsched := WaitReason{
		ID: WaitPodUnschedulable, Heuristic: true,
		// pkg/utils/pod_conditions.go:80 IsPodUnschedulable.
		Text: "Kubernetes cannot place these instance Pods; the scheduler's message says which constraint fails.",
	}
	pull := WaitReason{
		ID: WaitImagePull, Heuristic: true,
		Text: "These instance Pods cannot pull their image, so the Cluster waits for its instances.",
	}
	crash := WaitReason{
		ID: WaitCrashLoop, Heuristic: true,
		Text: "These instance containers keep crashing; read the postgres container's logs.",
	}
	if phase == cnpgv1.PhaseUnrecoverable && strings.HasPrefix(reason, "No pods are active") {
		// cluster_controller.go v1.30.0:937 (main:1005):
		// no Pod is alive (pod_conditions.go:96 IsPodAlive).
		crash.Text = "No instance is alive and the operator will not act on its own; read the postgres container's logs."
	}
	for _, p := range pods {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse &&
				(cond.Reason == corev1.PodReasonUnschedulable || cond.Reason == corev1.PodReasonSchedulingGated) {
				unsched.Evidence = append(unsched.Evidence, "Pod/"+p.Name+" PodScheduled=False "+cond.Reason+": "+cond.Message)
			}
		}
		statuses := append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...)
		for _, cs := range statuses {
			w := cs.State.Waiting
			if w == nil {
				continue
			}
			line := "Pod/" + p.Name + " container " + cs.Name + " waiting: " + w.Reason
			if w.Message != "" {
				line += ": " + w.Message
			}
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				pull.Evidence = append(pull.Evidence, line)
			case "CrashLoopBackOff":
				crash.Evidence = append(crash.Evidence, line)
			}
		}
	}
	for _, r := range []WaitReason{unsched, pull, crash} {
		if len(r.Evidence) > 0 {
			add(r)
		}
	}
}

// pvcReasons reads Cluster.status's PVC classes
// (pkg/reconciler/persistentvolumeclaim/status.go v1.30.0: unusable 262,
// resizing 268, dangling 287/296, initializing 292; main initializing
// 274/301). No class sets a phase; the operator waits on them in
// cluster_create.go v1.30.0:1530-1534 and 1559.
func pvcReasons(c *cnpgv1.Cluster, add func(WaitReason)) {
	rows := []struct {
		id, field, text string
		names           []string
	}{
		{WaitPVCUnusable, "unusablePVC", "These PersistentVolumeClaims belong to an incomplete instance volume group; the operator cannot start that instance from them.", c.Status.UnusablePVC},
		{WaitPVCDangling, "danglingPVC", "These PersistentVolumeClaims have no Pod; the operator re-attaches one only once every reporting instance is Ready.", c.Status.DanglingPVC},
		{WaitPVCInitializing, "initializingPVC", "These PersistentVolumeClaims are still being initialized (the init job, or on 1.31 the bootstrap init container, has not finished).", c.Status.InitializingPVC},
		{WaitPVCResizing, "resizingPVC", "These PersistentVolumeClaims are being resized by the storage provider.", c.Status.ResizingPVC},
	}
	for _, r := range rows {
		if len(r.names) == 0 {
			continue
		}
		names := slices.Sorted(slices.Values(r.names))
		add(WaitReason{
			ID: r.id, Heuristic: true, Text: r.text,
			Evidence: []string{"Cluster.status." + r.field + " = " + strings.Join(names, ", ")},
		})
	}
}

// backupReasons reads pending Backups and their events
// (backup_controller.go v1.30.0:325 BackupPending "Backup target pod not
// ready", :301 FindingPod; main:329,305) and the
// ScheduledBackup's ClusterNotHealthy (scheduledbackup_controller.go:178).
func backupReasons(in WaitInput, add func(WaitReason)) {
	r := WaitReason{
		ID: WaitBackupPending, Heuristic: true,
		Text: "A backup waits for its target Pod, for the Cluster to be healthy, or for another backup to finish.",
	}
	for _, b := range in.Backups {
		if b.Status.Phase != cnpgv1.BackupPhasePending {
			continue
		}
		line := "Backup/" + b.Name + " status.phase = pending"
		if e := newestEventFor(in.Events, "Backup", b.Name, "BackupPending", "FindingPod"); e != nil {
			line += "; " + eventLine(e)
		}
		r.Evidence = append(r.Evidence, line)
	}
	if e := newestEvent(in.Events, corev1.EventTypeWarning, "ClusterNotHealthy"); e != nil && e.InvolvedObject.Kind == "ScheduledBackup" {
		r.Evidence = append(r.Evidence, eventLine(e))
	}
	if len(r.Evidence) > 0 {
		add(r)
	}
}

// podInactive is pkg/utils/pod_conditions.go:72 IsPodActive negated, with
// the line that says why.
func podInactive(p *corev1.Pod) (string, bool) {
	switch {
	case p.DeletionTimestamp != nil:
		return "Pod/" + p.Name + " is being deleted", true
	case p.Status.Phase == corev1.PodPending, p.Status.Phase == corev1.PodFailed, p.Status.Phase == corev1.PodSucceeded:
		line := "Pod/" + p.Name + " status.phase = " + string(p.Status.Phase)
		if p.Status.Reason != "" {
			line += " (" + p.Status.Reason + ")"
		}
		return line, true
	default:
		return "", false
	}
}

// mixedReconcileLabels reports whether some instance Pods carry the
// fork's cnpg.io/reconcile label (origin/dev/19
// pkg/utils/labels_annotations.go:128) and others do not: proof that a
// filtered-cache fork labelled part of the Cluster. Upstream never sets
// the label, so its absence on every Pod proves nothing either way.
func mixedReconcileLabels(pods []*corev1.Pod) bool {
	with, without := 0, 0
	for _, p := range pods {
		if _, ok := p.Labels["cnpg.io/reconcile"]; ok {
			with++
		} else {
			without++
		}
	}
	return with > 0 && without > 0
}

// FilteredCacheTrap reports the fork's filtered-cache trap: the Cluster
// is Unrecoverable because its PVCs are invisible to an operator that only
// watches objects labelled cnpg.io/reconcile (the "no PVCs" reason,
// cluster_create.go v1.30.0:1158), and some of its Pods carry the label
// while others do not. Upstream builds never label, so they never match;
// a fork that labelled none of the Cluster's Pods is indistinguishable
// from upstream and is covered by the pvcs-lost reason's text.
func FilteredCacheTrap(c *cnpgv1.Cluster, pods []*corev1.Pod) bool {
	if c == nil || c.Status.Phase != cnpgv1.PhaseUnrecoverable || !strings.HasPrefix(c.Status.PhaseReason, reasonPVCsLost) {
		return false
	}
	return mixedReconcileLabels(pods)
}

func newestEvent(events []*corev1.Event, typ, reason string) *corev1.Event {
	var best *corev1.Event
	for _, e := range events {
		if e.Type == typ && e.Reason == reason && (best == nil || eventInstant(e).After(eventInstant(best))) {
			best = e
		}
	}
	return best
}

func newestEventFor(events []*corev1.Event, kind, name string, reasons ...string) *corev1.Event {
	var best *corev1.Event
	for _, e := range events {
		if e.InvolvedObject.Kind == kind && e.InvolvedObject.Name == name && slices.Contains(reasons, e.Reason) &&
			(best == nil || eventInstant(e).After(eventInstant(best))) {
			best = e
		}
	}
	return best
}

func eventInstant(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}

func eventLine(e *corev1.Event) string {
	line := "Event " + e.Type + " " + e.Reason + " on " + e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
	if e.Message != "" {
		line += ": " + e.Message
	}
	return line
}
