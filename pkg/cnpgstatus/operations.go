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
	"strconv"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
)

// Operation annotations (pkg/utils/labels_annotations.go
// ClusterReloadAnnotationName v1.30.0:213, main:213;
// ClusterRestartAnnotationName v1.30.0:274, main:281).
const (
	AnnotationRestartedAt = "kubectl.kubernetes.io/restartedAt"
	AnnotationReloadedAt  = "cnpg.io/reloadedAt"
)

// OperationKind is a console-issued operation.
type OperationKind string

// The operations.
const (
	OperationRestart OperationKind = "restart"
	OperationReload  OperationKind = "reload"
	OperationPromote OperationKind = "promote"
	OperationBackup  OperationKind = "backup"
)

// Operation phases, the OperationTracker's vocabulary. TimedOut is the
// tracker's own verdict (it owns the clock); this package never returns
// it. NotObservable means accepted and nothing further can be seen.
const (
	OpRequested     = "requested"
	OpAccepted      = "accepted"
	OpActing        = "acting"
	OpSettled       = "settled"
	OpFailed        = "failed"
	OpTimedOut      = "timedOut"
	OpNotObservable = "notObservable"
	OpUnknown       = "unknown"
)

// Source fields the readings attribute each step to.
const (
	srcClusterRestartAnnotation = "Cluster.metadata.annotations[kubectl.kubernetes.io/restartedAt]"
	srcPodRestartAnnotation     = "Pod.metadata.annotations[kubectl.kubernetes.io/restartedAt]"
	srcClusterReloadAnnotation  = "Cluster.metadata.annotations[cnpg.io/reloadedAt]"
	srcClusterPhase             = "Cluster.status.phase"
	srcTargetPrimary            = "Cluster.status.targetPrimary"
	srcCurrentPrimary           = "Cluster.status.currentPrimary"
	srcBackupObject             = "Backup"
	srcBackupPhase              = "Backup.status.phase"
	srcPods                     = "Pod list"
)

// Cluster phases the trackers read (api/v1/cluster_types.go, same
// strings at v1.30.0 and main; see phases.go).
const (
	phaseHealthy                = "Cluster in healthy state"
	phaseUpgrade                = "Upgrading cluster"
	phaseWaitingForUser         = "Waiting for user action"
	phaseInplacePrimaryRestart  = "Primary instance is being restarted in-place"
	phaseInplaceDeletePrimaryRs = "Primary instance is being restarted without a switchover"
)

// OperationInput is what the tracker observed after its write, from the
// informer cache.
type OperationInput struct {
	Kind  OperationKind
	Minor Minor
	// Cluster is the current Cluster (nil when not readable).
	Cluster *cnpgv1.Cluster
	// Pods are the Cluster's Pods; instance Pods are selected by
	// cnpg.io/cluster and cnpg.io/podRole=instance.
	Pods []corev1.Pod
	// Backup is the Backup the console created (nil until the cache has it).
	Backup *cnpgv1.Backup
	// Value is what the console wrote: the restartedAt or reloadedAt
	// annotation value (RFC3339), or the promote target Pod name.
	Value string
	// Marker is the targetPrimaryTimestamp the promote patch wrote.
	Marker string
}

// OperationReading is the tracker step and the field that shows it.
type OperationReading struct {
	Phase  string `json:"phase"`
	Source string `json:"source"`
	Text   string `json:"text,omitempty"`
	// Blocked is true when the operator is waiting for a human (a
	// supervised primary update, every instance fenced).
	Blocked bool `json:"blocked,omitempty"`
}

// OperationProgress maps what is observable to a tracker step. It is
// pure: the tracker calls it on every watch event and applies its own
// timeout.
func OperationProgress(in OperationInput) OperationReading {
	switch in.Kind {
	case OperationRestart:
		return restartProgress(in)
	case OperationReload:
		return reloadProgress(in)
	case OperationPromote:
		return promoteProgress(in)
	case OperationBackup:
		return backupProgress(in)
	default:
		return OperationReading{Phase: OpUnknown, Text: "unknown operation"}
	}
}

// effectiveAnnotation returns the annotation value the operator acts on
// and whether it covers the console's write: equal, or a later RFC3339
// value (a newer restart or reload includes ours).
func effectiveAnnotation(c *cnpgv1.Cluster, key, written string) (string, bool) {
	value := c.Annotations[key]
	if value == "" || written == "" {
		return value, false
	}
	if value == written {
		return value, true
	}
	got, want := parseOperatorTimestamp(value), parseOperatorTimestamp(written)
	if got != nil && want != nil && got.After(*want) {
		return value, true
	}
	return value, false
}

// instancePods are the Cluster's instance Pods, by label
// (cnpg.io/cluster, cnpg.io/podRole=instance).
func instancePods(c *cnpgv1.Cluster, pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, pod := range pods {
		if pod.Labels[LabelCluster] == c.Name && pod.Labels[LabelPodRole] == PodRoleInstance {
			out = append(out, pod)
		}
	}
	return out
}

// fenced reports whether a Pod is fenced according to the Cluster's
// annotation (the rollout skips fenced instances, cluster_upgrade.go
// v1.30.0:73, main:73).
func fenced(fencing FencingReading, name string) bool {
	return fencing.State == FencingAll || slices.Contains(fencing.Instances, name)
}

// restartProgress: the console merge-patches the Cluster annotation
// kubectl.kubernetes.io/restartedAt (as `kubectl cnpg restart`,
// internal/cmd/plugin/restart/restart.go v1.30.0:51). The operator then
// rolls out every instance whose Pod annotation differs
// (checkClusterHasDifferentRestartAnnotation, cluster_upgrade.go
// v1.30.0:607, main:607), replicas first with phase
// "Upgrading cluster" (:97), recreated Pods carrying the Cluster's value
// (cluster_create.go v1.30.0:1573, main:1655); fenced
// instances are skipped (:73). The primary is last: "Waiting for user
// action" under a supervised strategy (:135), or an in-place restart
// whose Pod annotation is patched BEFORE PostgreSQL restarts
// (updateRestartAnnotation :278), the instance manager then setting the
// phase back to healthy with reason "Primary instance restarted in-place"
// (instance_controller.go v1.30.0:410, main:425).
//
// Settled: every non-fenced instance Pod carries the value and the phase
// is "Cluster in healthy state". The PostgreSQL restart itself is not
// reported by /pg/status (no postmaster start time): Pod annotations and
// the phase are the only evidence.
func restartProgress(in OperationInput) OperationReading {
	c := in.Cluster
	if c == nil {
		return OperationReading{Phase: OpUnknown, Source: "Cluster", Text: "the Cluster could not be read"}
	}
	value, covered := effectiveAnnotation(c, AnnotationRestartedAt, in.Value)
	if !covered {
		return OperationReading{Phase: OpRequested, Source: srcClusterRestartAnnotation, Text: "the restart annotation is not visible on the Cluster yet"}
	}
	pods := instancePods(c, in.Pods)
	if len(pods) == 0 {
		return OperationReading{Phase: OpUnknown, Source: srcPods, Text: "no instance Pod is visible (hibernated, or Pods not readable): Pods created later carry the restart"}
	}
	fencing := Fencing(c)
	phase := c.Status.Phase
	var restarted, eligible int
	for _, pod := range pods {
		if fenced(fencing, pod.Name) {
			continue
		}
		eligible++
		if pod.DeletionTimestamp == nil && pod.Annotations[AnnotationRestartedAt] == value {
			restarted++
		}
	}
	if eligible == 0 {
		return OperationReading{Phase: OpAccepted, Source: AnnotationFencedInstances, Blocked: true, Text: "every instance is fenced: the operator restarts none"}
	}
	if restarted < eligible {
		switch {
		case phase == phaseWaitingForUser:
			return OperationReading{
				Phase: OpActing, Source: srcClusterPhase, Blocked: true,
				Text: "replicas restarted; the supervised primary update waits for a switchover",
			}
		case restarted > 0 || phase == phaseUpgrade || phase == phaseInplacePrimaryRestart || phase == phaseInplaceDeletePrimaryRs || phase == phaseSwitchover:
			return OperationReading{Phase: OpActing, Source: srcPodRestartAnnotation, Text: strconv.Itoa(restarted) + " of " + strconv.Itoa(eligible) + " instances restarted"}
		default:
			return OperationReading{Phase: OpAccepted, Source: srcClusterRestartAnnotation, Text: "the operator has not started the rollout"}
		}
	}
	switch phase {
	case phaseHealthy:
		return OperationReading{Phase: OpSettled, Source: srcClusterPhase, Text: "every instance Pod carries the restart and the cluster is healthy"}
	case phaseInplacePrimaryRestart, phaseInplaceDeletePrimaryRs:
		return OperationReading{Phase: OpActing, Source: srcClusterPhase, Text: "the primary is restarting"}
	default:
		return OperationReading{Phase: OpActing, Source: srcClusterPhase, Text: "every instance Pod carries the restart; phase: " + bound(phase, maxAnnotationValue)}
	}
}

// reloadProgress: the console merge-patches cnpg.io/reloadedAt (as
// `kubectl cnpg reload`, internal/cmd/plugin/reload/reload.go
// v1.30.0:35-49, main:35-49: "marks the cluster as
// needing to have a reconciliation loop"). Nothing reads the annotation
// (it appears only in reload.go, pkg/specs/pods.go and
// labels_annotations.go at all three tags); the instance managers
// reconcile and reload PostgreSQL only when a configuration file changed
// (instance_controller.go v1.30.0:277, main:292), with a
// log line and no status field, event or Pod annotation. Once the
// annotation is visible the operation is NOT OBSERVABLE.
func reloadProgress(in OperationInput) OperationReading {
	c := in.Cluster
	if c == nil {
		return OperationReading{Phase: OpUnknown, Source: "Cluster", Text: "the Cluster could not be read"}
	}
	if _, covered := effectiveAnnotation(c, AnnotationReloadedAt, in.Value); !covered {
		return OperationReading{Phase: OpRequested, Source: srcClusterReloadAnnotation, Text: "the reload annotation is not visible on the Cluster yet"}
	}
	return OperationReading{
		Phase: OpNotObservable, Source: srcClusterReloadAnnotation,
		Text: "accepted; CloudNativePG reports no completion for a reload (pendingRestart and loadedConfigurationHash per instance show the configuration state)",
	}
}

// promoteProgress: the console patches the status subresource as
// `kubectl cnpg promote` does (internal/cmd/plugin/promote/promote.go
// v1.30.0:63-66, main:66-69): targetPrimary,
// targetPrimaryTimestamp, phase "Switchover in progress". The target's
// instance manager promotes itself and sets currentPrimary
// (instance_controller.go v1.30.0:1263, main:1358); the
// operator registers "Cluster in healthy state" at the end of a clean
// reconcile (internal/controller/plugins.go v1.30.0:132,
// main:136).
//
// Settled: currentPrimary == targetPrimary == target and the phase is
// neither "Switchover in progress" nor "Failing over". Failed:
// targetPrimary becomes "pending" (failover delay running) or, after
// the console's write (targetPrimaryTimestamp later than Marker),
// another name. CloudNativePG emits no Warning event for a failed
// switchover (only Normal SwitchingOver / FailingOver,
// internal/controller/replicas.go v1.30.0:133-378): timeouts are the
// tracker's.
func promoteProgress(in OperationInput) OperationReading {
	c := in.Cluster
	if c == nil {
		return OperationReading{Phase: OpUnknown, Source: "Cluster", Text: "the Cluster could not be read"}
	}
	st := c.Status
	target := in.Value
	if target != "" && st.TargetPrimary == target {
		if st.CurrentPrimary != target {
			return OperationReading{Phase: OpActing, Source: srcCurrentPrimary, Text: "waiting for " + bound(target, maxAnnotationValue) + " to promote"}
		}
		if st.Phase == phaseSwitchover || st.Phase == phaseFailOver {
			return OperationReading{Phase: OpActing, Source: srcClusterPhase, Text: "promoted; the operator is finishing the switchover"}
		}
		return OperationReading{Phase: OpSettled, Source: srcCurrentPrimary, Text: bound(target, maxAnnotationValue) + " is the primary"}
	}
	if st.TargetPrimary == PendingFailoverMarker {
		return OperationReading{Phase: OpFailed, Source: srcTargetPrimary, Text: "the primary is failing: a failover is pending instead of the switchover"}
	}
	if st.Phase == phaseFailOver {
		return OperationReading{Phase: OpFailed, Source: srcClusterPhase, Text: "the operator is failing over instead of switching over"}
	}
	written, seen := parseOperatorTimestamp(in.Marker), parseOperatorTimestamp(st.TargetPrimaryTimestamp)
	if written != nil && seen != nil && seen.After(*written) && st.TargetPrimary != "" {
		return OperationReading{
			Phase: OpFailed, Source: srcTargetPrimary,
			Text: "targetPrimary was changed to " + bound(st.TargetPrimary, maxAnnotationValue) + " after the request",
		}
	}
	return OperationReading{Phase: OpRequested, Source: srcTargetPrimary, Text: "the new targetPrimary is not visible yet"}
}

// backupProgress: the console creates a Backup (generateName). Pending
// is queued (another backup of the cluster, or a target Pod not ready);
// started/running/finalizing is acting; completed settles; failed,
// walArchivingFailing and "invalid backup definition" fail, with
// status.error as the text.
func backupProgress(in OperationInput) OperationReading {
	b := in.Backup
	if b == nil {
		return OperationReading{Phase: OpRequested, Source: srcBackupObject, Text: "the Backup is not visible yet"}
	}
	phase := string(b.Status.Phase)
	reading := ClassifyBackupPhase(in.Minor, phase)
	switch {
	case phase == "":
		return OperationReading{Phase: OpAccepted, Source: srcBackupObject, Text: "created; the operator has not reconciled it"}
	case phase == BackupPhasePending:
		return OperationReading{Phase: OpAccepted, Source: srcBackupPhase, Text: "queued: another backup is running or the target instance is not ready"}
	case reading.Failed:
		text := phase
		if b.Status.Error != "" {
			text += ": " + bound(b.Status.Error, maxMessage)
		}
		return OperationReading{Phase: OpFailed, Source: srcBackupPhase, Text: text}
	case reading.Terminal:
		return OperationReading{Phase: OpSettled, Source: srcBackupPhase, Text: "completed"}
	case reading.Known:
		return OperationReading{Phase: OpActing, Source: srcBackupPhase, Text: phase}
	default:
		return OperationReading{Phase: OpUnknown, Source: srcBackupPhase, Text: "unknown phase: " + bound(phase, maxAnnotationValue)}
	}
}

// Blocker is a precondition that refuses an operation.
type Blocker struct {
	// Reason is a stable code; for a backup it is the Warning event
	// reason the operator would emit while failing the Backup.
	Reason string `json:"reason"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

// Blocker reasons the console originates.
const (
	BlockerClusterNotRead         = "ClusterNotRead"
	BlockerReconciliationDisabled = "ReconciliationDisabled"
	BlockerHibernated             = "ClusterIsHibernated"
	BlockerSwitchoverInProgress   = "SwitchoverInProgress"
	BlockerTargetNotInstance      = "TargetNotAnInstance"
	BlockerTargetDeleting         = "TargetDeleting"
	BlockerTargetFenced           = "TargetFenced"
	BlockerAlreadyTarget          = "AlreadyTarget"
	BlockerRolloutInProgress      = "RolloutInProgress"
)

// rolloutPhases are the Cluster phases of a rollout under way: a rolling
// update or restart (PhaseUpgrade, which a restart's own annotation
// starts), a major upgrade, an in-place primary restart
// (api/v1/cluster_types.go v1.30.0:711-727). A second restart then would
// pile a rollout on the one under way.
var rolloutPhases = []string{
	cnpgv1.PhaseUpgrade, cnpgv1.PhaseMajorUpgrade, cnpgv1.PhaseInplacePrimaryRestart, cnpgv1.PhaseInplaceDeletePrimaryRestart,
}

// BackupPreconditions mirrors BackupReconciler.checkPrerequisites
// (internal/controller/backup_controller.go
// v1.30.0:408-441, main:420-453): each refusal there fails the Backup
// with a Warning event of the same reason. An empty method is the CRD
// default barmanObjectStore, which fails on a plugin-only Cluster: the
// console must send method plugin with pluginConfiguration.name there.
// The VolumeSnapshot CRD check (ClusterHasNoVolumeSnapshotCRD) needs
// discovery and is the caller's.
func BackupPreconditions(c *cnpgv1.Cluster, method string) []Blocker {
	if c == nil {
		return []Blocker{{Reason: BlockerClusterNotRead, Text: "the Cluster could not be read", Source: "Cluster"}}
	}
	if method == "" {
		method = BackupMethodBarmanObjectStore
	}
	var blockers []Blocker
	if c.Annotations[AnnotationHibernation] == "on" {
		blockers = append(blockers, Blocker{Reason: BlockerHibernated, Text: "cannot backup a hibernated cluster", Source: "Cluster.metadata.annotations[cnpg.io/hibernation]"})
	}
	switch {
	case method == BackupMethodPlugin:
		if len(c.Spec.Plugins) == 0 {
			blockers = append(blockers, Blocker{Reason: "ClusterHasNoBackupExecutorPlugin", Text: "the cluster has no plugin configured", Source: "Cluster.spec.plugins"})
		}
	case c.Spec.Backup == nil:
		blockers = append(blockers, Blocker{Reason: "ClusterHasBackupConfigured", Text: "the cluster has no backup section", Source: "Cluster.spec.backup"})
	case method == BackupMethodVolumeSnapshot && c.Spec.Backup.VolumeSnapshot == nil:
		blockers = append(blockers, Blocker{Reason: "ClusterHasNoVolumeSnapshotSection", Text: "no volumeSnapshot section on the cluster", Source: "Cluster.spec.backup.volumeSnapshot"})
	case method == BackupMethodBarmanObjectStore && c.Spec.Backup.BarmanObjectStore == nil:
		blockers = append(blockers, Blocker{Reason: "ClusterHasNoBarmanSection", Text: "no barmanObjectStore section on the cluster", Source: "Cluster.spec.backup.barmanObjectStore"})
	}
	return blockers
}

// RestartPreconditions refuses a restart while a switchover or failover
// is in flight (the instance manager refuses an in-place primary restart
// then, instance_controller.go v1.30.0:396, main:411), on a
// hibernated cluster (no Pod to restart) and when the Cluster's
// reconciliation loop is disabled (cluster_controller.go
// v1.30.0:313, main:312: nothing would roll out); and, the console's own
// rule, while a rollout is under way (rolloutPhases). A reload drops the
// switchover and rollout blockers: it restarts nothing.
func RestartPreconditions(c *cnpgv1.Cluster) []Blocker {
	if c == nil {
		return []Blocker{{Reason: BlockerClusterNotRead, Text: "the Cluster could not be read", Source: "Cluster"}}
	}
	blockers := commonBlockers(c)
	if state := Primary(c).State; state == PrimarySwitchover || state == PrimaryFailover || state == PrimaryFailoverPending {
		blockers = append(blockers, Blocker{Reason: BlockerSwitchoverInProgress, Text: "a switchover or failover is in progress", Source: srcTargetPrimary})
	}
	if slices.Contains(rolloutPhases, c.Status.Phase) {
		blockers = append(blockers, Blocker{
			Reason: BlockerRolloutInProgress, Source: srcClusterPhase,
			Text: "a rollout is in progress (phase \"" + c.Status.Phase + "\"): wait for it to finish",
		})
	}
	return blockers
}

// ReloadPreconditions are RestartPreconditions without the switchover and
// rollout blockers: a reload restarts nothing.
func ReloadPreconditions(c *cnpgv1.Cluster) []Blocker {
	if c == nil {
		return []Blocker{{Reason: BlockerClusterNotRead, Text: "the Cluster could not be read", Source: "Cluster"}}
	}
	return commonBlockers(c)
}

// PromotePreconditions are what a promotion requires: target is an instance Pod of this
// Cluster, not deleting, not fenced, not already the target, no
// switchover in flight; plus a hibernated cluster and a disabled
// reconciliation loop.
func PromotePreconditions(c *cnpgv1.Cluster, pods []corev1.Pod, target string) []Blocker {
	if c == nil {
		return []Blocker{{Reason: BlockerClusterNotRead, Text: "the Cluster could not be read", Source: "Cluster"}}
	}
	blockers := commonBlockers(c)
	if state := Primary(c).State; state == PrimarySwitchover || state == PrimaryFailover || state == PrimaryFailoverPending {
		blockers = append(blockers, Blocker{Reason: BlockerSwitchoverInProgress, Text: "a switchover or failover is in progress", Source: srcTargetPrimary})
	}
	if target == c.Status.TargetPrimary || target == c.Status.CurrentPrimary {
		blockers = append(blockers, Blocker{Reason: BlockerAlreadyTarget, Text: "the instance is already the (target) primary", Source: srcTargetPrimary})
	}
	var pod *corev1.Pod
	candidates := instancePods(c, pods)
	for i := range candidates {
		if candidates[i].Name == target {
			pod = &candidates[i]
			break
		}
	}
	switch {
	case pod == nil:
		blockers = append(blockers, Blocker{Reason: BlockerTargetNotInstance, Text: "the target is not an instance Pod of this cluster", Source: srcPods})
	case pod.DeletionTimestamp != nil:
		blockers = append(blockers, Blocker{Reason: BlockerTargetDeleting, Text: "the target Pod is being deleted", Source: "Pod.metadata.deletionTimestamp"})
	}
	if fenced(Fencing(c), target) {
		blockers = append(blockers, Blocker{Reason: BlockerTargetFenced, Text: "the target instance is fenced", Source: AnnotationFencedInstances})
	}
	return blockers
}

func commonBlockers(c *cnpgv1.Cluster) []Blocker {
	var blockers []Blocker
	if c.Annotations[AnnotationReconciliationLoop] == valueDisabled {
		blockers = append(blockers, Blocker{Reason: BlockerReconciliationDisabled, Text: "cnpg.io/reconciliationLoop is disabled: the operator would not act", Source: AnnotationReconciliationLoop})
	}
	if h := Hibernation(c).State; h != HibernationOff {
		blockers = append(blockers, Blocker{Reason: BlockerHibernated, Text: "the cluster is hibernated or hibernating", Source: AnnotationHibernation})
	}
	return blockers
}
