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
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	restartValue = "2026-10-30T10:00:00Z"
	laterValue   = "2026-10-30T10:05:00Z"
	earlierValue = "2026-10-30T09:00:00Z"
)

func opCluster(annotations map[string]string, phase, current, target string) *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Annotations: annotations},
		Status:     cnpgv1.ClusterStatus{Phase: phase, CurrentPrimary: current, TargetPrimary: target},
	}
}

func instancePod(name, restartedAt string) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{LabelCluster: "pg", LabelPodRole: PodRoleInstance},
	}}
	if restartedAt != "" {
		pod.Annotations = map[string]string{AnnotationRestartedAt: restartedAt}
	}
	return pod
}

func TestRestartProgress(t *testing.T) {
	restarted := map[string]string{AnnotationRestartedAt: restartValue}
	pooler := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pool", Labels: map[string]string{LabelCluster: "pg", LabelPodRole: PodRolePooler}}}
	tests := []struct {
		name    string
		cluster *cnpgv1.Cluster
		pods    []corev1.Pod
		phase   string
		blocked bool
	}{
		{"nil cluster", nil, nil, OpUnknown, false},
		{"annotation not visible", opCluster(nil, phaseHealthy, "pg-1", "pg-1"), []corev1.Pod{instancePod("pg-1", "")}, OpRequested, false},
		{"older annotation is not ours", opCluster(map[string]string{AnnotationRestartedAt: earlierValue}, phaseHealthy, "pg-1", "pg-1"), []corev1.Pod{instancePod("pg-1", "")}, OpRequested, false},
		{
			"accepted, rollout not started", opCluster(restarted, phaseHealthy, "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", ""), instancePod("pg-2", ""), pooler},
			OpAccepted, false,
		},
		{
			"replica restarted", opCluster(restarted, phaseUpgrade, "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", ""), instancePod("pg-2", restartValue)},
			OpActing, false,
		},
		{
			"supervised waits for the user", opCluster(restarted, phaseWaitingForUser, "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", ""), instancePod("pg-2", restartValue)},
			OpActing, true,
		},
		{
			"primary annotated before its in-place restart", opCluster(restarted, phaseInplacePrimaryRestart, "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", restartValue), instancePod("pg-2", restartValue)},
			OpActing, false,
		},
		{
			"settled", opCluster(restarted, phaseHealthy, "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", restartValue), instancePod("pg-2", restartValue), pooler},
			OpSettled, false,
		},
		{
			"all annotated, other phase", opCluster(restarted, "Waiting for the instances to become active", "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", restartValue)},
			OpActing, false,
		},
		{
			"a later restart covers ours", opCluster(map[string]string{AnnotationRestartedAt: laterValue}, phaseHealthy, "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", laterValue)},
			OpSettled, false,
		},
		{
			"fenced instance is skipped", opCluster(map[string]string{AnnotationRestartedAt: restartValue, AnnotationFencedInstances: `["pg-2"]`}, phaseHealthy, "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", restartValue), instancePod("pg-2", "")},
			OpSettled, false,
		},
		{
			"every instance fenced", opCluster(map[string]string{AnnotationRestartedAt: restartValue, AnnotationFencedInstances: `["*"]`}, phaseHealthy, "pg-1", "pg-1"),
			[]corev1.Pod{instancePod("pg-1", "")},
			OpAccepted, true,
		},
		{"no pods", opCluster(restarted, phaseHealthy, "pg-1", "pg-1"), nil, OpUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OperationProgress(OperationInput{Kind: OperationRestart, Minor: Minor130, Cluster: tt.cluster, Pods: tt.pods, Value: restartValue})
			if got.Phase != tt.phase || got.Blocked != tt.blocked || got.Source == "" {
				t.Errorf("restart = %+v, want %s blocked=%v", got, tt.phase, tt.blocked)
			}
		})
	}
	deleting := instancePod("pg-1", restartValue)
	deleting.DeletionTimestamp = &metav1.Time{}
	got := OperationProgress(OperationInput{Kind: OperationRestart, Cluster: opCluster(restarted, phaseUpgrade, "pg-1", "pg-1"), Pods: []corev1.Pod{deleting}, Value: restartValue})
	if got.Phase != OpActing {
		t.Errorf("a deleting Pod is not restarted yet: %+v", got)
	}
}

func TestReloadProgress(t *testing.T) {
	in := OperationInput{Kind: OperationReload, Value: restartValue}
	if got := OperationProgress(in); got.Phase != OpUnknown {
		t.Errorf("nil cluster: %+v", got)
	}
	in.Cluster = opCluster(nil, phaseHealthy, "pg-1", "pg-1")
	if got := OperationProgress(in); got.Phase != OpRequested {
		t.Errorf("not visible: %+v", got)
	}
	in.Cluster = opCluster(map[string]string{AnnotationReloadedAt: restartValue}, phaseHealthy, "pg-1", "pg-1")
	if got := OperationProgress(in); got.Phase != OpNotObservable || got.Source != srcClusterReloadAnnotation {
		t.Errorf("reload must be not observable once accepted: %+v", got)
	}
}

func TestPromoteProgress(t *testing.T) {
	marker := "2026-10-30T10:00:00.000000Z"
	tests := []struct {
		name    string
		cluster *cnpgv1.Cluster
		phase   string
	}{
		{"nil", nil, OpUnknown},
		{"not visible", opCluster(nil, phaseHealthy, "pg-1", "pg-1"), OpRequested},
		{"target set, not promoted", opCluster(nil, phaseSwitchover, "pg-1", "pg-2"), OpActing},
		{"promoted, finishing", opCluster(nil, phaseSwitchover, "pg-2", "pg-2"), OpActing},
		{"settled", opCluster(nil, phaseHealthy, "pg-2", "pg-2"), OpSettled},
		{"settled during a later rollout", opCluster(nil, phaseUpgrade, "pg-2", "pg-2"), OpSettled},
		{"failover pending", opCluster(nil, phaseSwitchover, "pg-1", PendingFailoverMarker), OpFailed},
		{"failing over", opCluster(nil, phaseFailOver, "pg-1", "pg-3"), OpFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OperationProgress(OperationInput{Kind: OperationPromote, Cluster: tt.cluster, Value: "pg-2", Marker: marker})
			if got.Phase != tt.phase {
				t.Errorf("promote = %+v, want %s", got, tt.phase)
			}
		})
	}
	overridden := opCluster(nil, phaseSwitchover, "pg-1", "pg-3")
	overridden.Status.TargetPrimaryTimestamp = "2026-10-30T10:00:05.000000Z"
	if got := OperationProgress(OperationInput{Kind: OperationPromote, Cluster: overridden, Value: "pg-2", Marker: marker}); got.Phase != OpFailed {
		t.Errorf("a later targetPrimary must fail the promote: %+v", got)
	}
	overridden.Status.TargetPrimaryTimestamp = "2026-10-30T09:00:00.000000Z"
	if got := OperationProgress(OperationInput{Kind: OperationPromote, Cluster: overridden, Value: "pg-2", Marker: marker}); got.Phase != OpRequested {
		t.Errorf("an older targetPrimary is a cache not caught up: %+v", got)
	}
}

func TestBackupProgress(t *testing.T) {
	backup := func(phase, errText string) *cnpgv1.Backup {
		return &cnpgv1.Backup{Status: cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhase(phase), Error: errText}}
	}
	tests := []struct {
		name  string
		minor Minor
		in    *cnpgv1.Backup
		phase string
	}{
		{"not visible", Minor130, nil, OpRequested},
		{"created", Minor130, backup("", ""), OpAccepted},
		{"queued", Minor130, backup(BackupPhasePending, ""), OpAccepted},
		{"started", Minor130, backup(BackupPhaseStarted, ""), OpActing},
		{"running", Minor130, backup(BackupPhaseRunning, ""), OpActing},
		{"finalizing", Minor130, backup(BackupPhaseFinalizing, ""), OpActing},
		{"completed", Minor130, backup(BackupPhaseCompleted, ""), OpSettled},
		{"failed", Minor130, backup(BackupPhaseFailed, "cannot backup a hibernated cluster"), OpFailed},
		{"wal archiving failing", Minor130, backup(BackupPhaseWalArchivingFailing, ""), OpFailed},
		{"invalid definition", Minor130, backup(BackupPhaseDefinitionInvalid, "bad"), OpFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OperationProgress(OperationInput{Kind: OperationBackup, Minor: tt.minor, Backup: tt.in})
			if got.Phase != tt.phase {
				t.Errorf("backup = %+v, want %s", got, tt.phase)
			}
		})
	}
	if got := OperationProgress(OperationInput{Kind: "vacuum"}); got.Phase != OpUnknown {
		t.Errorf("unknown kind: %+v", got)
	}
}

func reasons(blockers []Blocker) []string {
	out := make([]string, 0, len(blockers))
	for _, b := range blockers {
		out = append(out, b.Reason)
	}
	return out
}

func TestBackupPreconditions(t *testing.T) {
	pluginOnly := opCluster(nil, phaseHealthy, "pg-1", "pg-1")
	pluginOnly.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: BarmanCloudPlugin}}
	inTree := opCluster(nil, phaseHealthy, "pg-1", "pg-1")
	inTree.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}
	hibernated := opCluster(map[string]string{AnnotationHibernation: "on"}, phaseHealthy, "pg-1", "pg-1")
	hibernated.Spec.Plugins = pluginOnly.Spec.Plugins
	tests := []struct {
		name    string
		cluster *cnpgv1.Cluster
		method  string
		want    []string
	}{
		{"nil", nil, "", []string{BlockerClusterNotRead}},
		{"plugin ok", pluginOnly, BackupMethodPlugin, nil},
		{"default method on a plugin-only cluster", pluginOnly, "", []string{"ClusterHasBackupConfigured"}},
		{"plugin method without plugins", inTree, BackupMethodPlugin, []string{"ClusterHasNoBackupExecutorPlugin"}},
		{"in-tree ok", inTree, "", nil},
		{"snapshot without section", inTree, BackupMethodVolumeSnapshot, []string{"ClusterHasNoVolumeSnapshotSection"}},
		{"hibernated", hibernated, BackupMethodPlugin, []string{BlockerHibernated}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reasons(BackupPreconditions(tt.cluster, tt.method)); !slices.Equal(got, tt.want) {
				t.Errorf("BackupPreconditions = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRestartAndPromotePreconditions(t *testing.T) {
	stable := opCluster(nil, phaseHealthy, "pg-1", "pg-1")
	switching := opCluster(nil, phaseSwitchover, "pg-1", "pg-2")
	disabled := opCluster(map[string]string{AnnotationReconciliationLoop: valueDisabled}, phaseHealthy, "pg-1", "pg-1")
	if got := reasons(RestartPreconditions(stable)); len(got) != 0 {
		t.Errorf("stable restart blocked: %v", got)
	}
	if got := reasons(RestartPreconditions(switching)); !slices.Equal(got, []string{BlockerSwitchoverInProgress}) {
		t.Errorf("restart during switchover: %v", got)
	}
	if got := reasons(RestartPreconditions(disabled)); !slices.Equal(got, []string{BlockerReconciliationDisabled}) {
		t.Errorf("restart with reconciliation disabled: %v", got)
	}
	if got := reasons(RestartPreconditions(nil)); !slices.Equal(got, []string{BlockerClusterNotRead}) {
		t.Errorf("nil: %v", got)
	}
	for _, phase := range []string{cnpgv1.PhaseUpgrade, cnpgv1.PhaseMajorUpgrade, cnpgv1.PhaseInplacePrimaryRestart, cnpgv1.PhaseInplaceDeletePrimaryRestart} {
		if got := reasons(RestartPreconditions(opCluster(nil, phase, "pg-1", "pg-1"))); !slices.Equal(got, []string{BlockerRolloutInProgress}) {
			t.Errorf("restart during %q: %v", phase, got)
		}
	}
	if got := reasons(RestartPreconditions(opCluster(nil, cnpgv1.PhaseCreatingReplica, "pg-1", "pg-1"))); len(got) != 0 {
		t.Errorf("restart while a replica is created: %v", got)
	}

	pods := []corev1.Pod{instancePod("pg-1", ""), instancePod("pg-2", ""), instancePod("pg-3", "")}
	pods[2].DeletionTimestamp = &metav1.Time{}
	fencedCluster := opCluster(map[string]string{AnnotationFencedInstances: `["pg-2"]`}, phaseHealthy, "pg-1", "pg-1")
	tests := []struct {
		name    string
		cluster *cnpgv1.Cluster
		target  string
		want    []string
	}{
		{"ok", stable, "pg-2", nil},
		{"already primary", stable, "pg-1", []string{BlockerAlreadyTarget}},
		{"not an instance", stable, "pool-1", []string{BlockerTargetNotInstance}},
		{"deleting", stable, "pg-3", []string{BlockerTargetDeleting}},
		{"fenced", fencedCluster, "pg-2", []string{BlockerTargetFenced}},
		{"switchover in flight", switching, "pg-3", []string{BlockerSwitchoverInProgress, BlockerTargetDeleting}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reasons(PromotePreconditions(tt.cluster, pods, tt.target)); !slices.Equal(got, tt.want) {
				t.Errorf("PromotePreconditions = %v, want %v", got, tt.want)
			}
		})
	}
}
