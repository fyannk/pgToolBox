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
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func waitCluster(phase, reason string) *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "team"},
		Status:     cnpgv1.ClusterStatus{Phase: phase, PhaseReason: reason, CurrentPrimary: "pg-1", TargetPrimary: "pg-1"},
	}
}

func waitPod(name string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func waitEvent(typ, reason, kind, name, message string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name + "." + reason, Namespace: "team"},
		Type:           typ,
		Reason:         reason,
		Message:        message,
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name},
		LastTimestamp:  metav1.NewTime(at),
	}
}

type reasonWant struct {
	id        string
	heuristic bool
	// evidence must each be a substring of some evidence line.
	evidence []string
}

func TestWaitReasons(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	unschedulable := func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/3 nodes are available: 3 Insufficient memory.",
		}}
	}
	tests := []struct {
		name string
		in   WaitInput
		want []reasonWant
	}{
		{
			name: "healthy phase waits for nothing even with a stale PVC list",
			in: WaitInput{Minor: Minor130, Cluster: func() *cnpgv1.Cluster {
				c := waitCluster(cnpgv1.PhaseHealthy, "")
				c.Status.DanglingPVC = []string{"pg-3"}
				return c
			}()},
		},
		{name: "nil cluster", in: WaitInput{Minor: Minor130}},
		{
			name: "fork reason codes are reported, not heuristic",
			in: WaitInput{Minor: Minor131, Cluster: waitCluster(cnpgv1.PhaseCannotCreateClusterObjects, "pods \"pg-2\" is forbidden: exceeded quota: team-quota"), Events: []*corev1.Event{
				waitEvent(corev1.EventTypeWarning, "QuotaExceeded", "Cluster", "pg", "old", t0.Add(-time.Hour)),
				waitEvent(corev1.EventTypeWarning, "QuotaExceeded", "Cluster", "pg", "exceeded quota: team-quota", t0),
				waitEvent(corev1.EventTypeWarning, "VolumeSnapshotError", "Backup", "pg-snap", "snapshot class missing", t0),
			}},
			want: []reasonWant{
				{id: WaitForkQuotaExceeded, evidence: []string{"Event Warning QuotaExceeded on Cluster/pg: exceeded quota: team-quota"}},
				{id: WaitForkVolumeSnapshotError, evidence: []string{"Backup/pg-snap"}},
				{id: WaitQuotaRefused, heuristic: true, evidence: []string{"exceeded quota"}},
			},
		},
		{
			name: "a Normal event with a fork reason name is not a reported cause",
			in: WaitInput{Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseCannotCreateClusterObjects, "the server could not find the requested resource"), Events: []*corev1.Event{
				waitEvent(corev1.EventTypeNormal, "ReconciliationFailed", "Cluster", "pg", "x", t0),
			}},
			want: []reasonWant{{id: WaitCannotCreateObjects, evidence: []string{"could not find"}}},
		},
		{
			name: "admission refusal read from the phase reason",
			in:   WaitInput{Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseCannotCreateClusterObjects, `admission webhook "x" denied the request: no`)},
			want: []reasonWant{{id: WaitAdmissionRefused, heuristic: true}},
		},
		{
			name: "instances not active names the pending pod and why",
			in: WaitInput{
				Minor:   Minor130,
				Cluster: waitCluster(cnpgv1.PhaseWaitingForInstancesToBeActive, "Some instances are not yet active. Please wait."),
				Pods: []*corev1.Pod{
					waitPod("pg-2", unschedulable),
					waitPod("pg-1", nil),
					waitPod("pg-3", func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: t0} }),
				},
			},
			want: []reasonWant{
				{id: WaitInstancesNotActive, heuristic: true, evidence: []string{"Pod/pg-2 status.phase = Pending", "Pod/pg-3 is being deleted"}},
				{id: WaitPodUnschedulable, heuristic: true, evidence: []string{"Pod/pg-2 PodScheduled=False Unschedulable: 0/3 nodes"}},
			},
		},
		{
			name: "image pull in an init container",
			in: WaitInput{Minor: Minor131, Cluster: waitCluster(cnpgv1.PhaseCreatingReplica, ""), Pods: []*corev1.Pod{
				waitPod("pg-2", func(p *corev1.Pod) {
					p.Status.Phase = corev1.PodPending
					p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
						Name: "bootstrap-controller", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
					}}
				}),
			}},
			want: []reasonWant{{id: WaitImagePull, heuristic: true, evidence: []string{"container bootstrap-controller waiting: ImagePullBackOff: Back-off"}}},
		},
		{
			name: "no live instance: crash loop with the unrecoverable wording",
			in: WaitInput{Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseUnrecoverable, "No pods are active, the cluster needs manual intervention "), Pods: []*corev1.Pod{
				waitPod("pg-1", func(p *corev1.Pod) {
					p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "postgres", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
				}),
			}},
			want: []reasonWant{{id: WaitCrashLoop, heuristic: true, evidence: []string{"Pod/pg-1 container postgres waiting: CrashLoopBackOff"}}},
		},
		{
			name: "instance creation failed (jobs, 1.30)",
			in:   WaitInput{Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseUnrecoverable, "Instance creation failed for the following jobs: [pg-2-join]")},
			want: []reasonWant{{id: WaitInstanceCreationFailed, evidence: []string{"pg-2-join"}}},
		},
		{
			name: "instance bootstrap failed (pods, 1.31)",
			in:   WaitInput{Minor: Minor131, Cluster: waitCluster(cnpgv1.PhaseUnrecoverable, "Instance bootstrap failed for the following Pods: [pg-2]")},
			want: []reasonWant{{id: WaitInstanceCreationFailed}},
		},
		{
			name: "PVCs lost on an upstream build: no trap evidence",
			in: WaitInput{Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseUnrecoverable, "One or more instances were previously created, but no PersistentVolumeClaims (PVCs) exist"), Pods: []*corev1.Pod{
				waitPod("pg-1", nil),
			}},
			want: []reasonWant{{id: WaitPVCsLost}},
		},
		{
			name: "PVCs lost with mixed reconcile labels names the filtered cache",
			in: WaitInput{Minor: Minor131, Cluster: waitCluster(cnpgv1.PhaseUnrecoverable, "One or more instances were previously created, but no PersistentVolumeClaims (PVCs) exist"), Pods: []*corev1.Pod{
				waitPod("pg-1", func(p *corev1.Pod) { p.Labels = map[string]string{"cnpg.io/reconcile": "true"} }),
				waitPod("pg-2", nil),
			}},
			want: []reasonWant{{id: WaitPVCsLost, evidence: []string{"filtered-cache operator labelled only part"}}},
		},
		{
			name: "supervised switchover",
			in:   WaitInput{Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseWaitingForUser, "User must issue a supervised switchover")},
			want: []reasonWant{{id: WaitSupervisedSwitchover}},
		},
		{
			name: "waiting for user for another reason adds nothing of its own",
			in:   WaitInput{Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseWaitingForUser, "something else")},
		},
		{
			name: "rollout delayed keys on the phase and the event",
			in: WaitInput{Minor: Minor131, Cluster: waitCluster(cnpgv1.PhaseUpgradeDelayed, "The cluster needs to be updated, but it's being delayed"), Events: []*corev1.Event{
				waitEvent(corev1.EventTypeNormal, "RolloutDelayed", "Cluster", "pg", "Rollout of pod pg-1 have been delayed for 30s", t0),
			}},
			want: []reasonWant{{id: WaitRolloutDelayed, evidence: []string{"Event Normal RolloutDelayed on Cluster/pg"}}},
		},
		{
			name: "invalid definition exists from 1.30",
			in:   WaitInput{Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseDefinitionInvalid, "spec.instances: Invalid value")},
			want: []reasonWant{{id: WaitDefinitionInvalid, evidence: []string{"spec.instances"}}},
		},
		{
			name: "switchover pending with its timestamp, and a deferred failover",
			in: WaitInput{Minor: Minor130, Cluster: func() *cnpgv1.Cluster {
				c := waitCluster(cnpgv1.PhaseFailOver, "Failing over from pg-1 to pg-2")
				c.Status.TargetPrimary = "pg-2"
				c.Status.TargetPrimaryTimestamp = "2026-10-30T09:59:00.000000Z"
				return c
			}(), Events: []*corev1.Event{
				waitEvent(corev1.EventTypeWarning, "PrimaryStatusCheckFailed", "Cluster", "pg", "cannot read pg-1", t0),
			}},
			want: []reasonWant{
				{id: WaitSwitchoverPending, evidence: []string{"targetPrimary = pg-2", "targetPrimaryTimestamp = 2026-10-30T09:59:00Z"}},
				{id: WaitPrimaryStatusCheck, heuristic: true, evidence: []string{"cannot read pg-1"}},
			},
		},
		{
			name: "PVC classes, names sorted",
			in: WaitInput{Minor: Minor130, Cluster: func() *cnpgv1.Cluster {
				c := waitCluster(cnpgv1.PhaseCreatingReplica, "")
				c.Status.UnusablePVC = []string{"pg-3-wal", "pg-3"}
				c.Status.ResizingPVC = []string{"pg-1"}
				return c
			}()},
			want: []reasonWant{
				{id: WaitPVCUnusable, heuristic: true, evidence: []string{"Cluster.status.unusablePVC = pg-3, pg-3-wal"}},
				{id: WaitPVCResizing, heuristic: true},
			},
		},
		{
			name: "pending backup with its event, and a schedule waiting for health",
			in: WaitInput{
				Minor: Minor130, Cluster: waitCluster(cnpgv1.PhaseSwitchover, ""),
				Backups: []*cnpgv1.Backup{
					{ObjectMeta: metav1.ObjectMeta{Name: "b1"}, Status: cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhasePending}},
					{ObjectMeta: metav1.ObjectMeta{Name: "b0"}, Status: cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseCompleted}},
				},
				Events: []*corev1.Event{
					waitEvent(corev1.EventTypeWarning, "BackupPending", "Backup", "b1", "Backup target pod not ready", t0),
					waitEvent(corev1.EventTypeWarning, "ClusterNotHealthy", "ScheduledBackup", "nightly", "cluster not healthy", t0),
				},
			},
			want: []reasonWant{{id: WaitBackupPending, heuristic: true, evidence: []string{
				"Backup/b1 status.phase = pending; Event Warning BackupPending on Backup/b1: Backup target pod not ready",
				"ScheduledBackup/nightly",
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := WaitReasons(tt.in)
			ids := make([]string, 0, len(got))
			for _, r := range got {
				ids = append(ids, r.ID)
			}
			wantIDs := make([]string, 0, len(tt.want))
			for _, w := range tt.want {
				wantIDs = append(wantIDs, w.id)
			}
			if !slices.Equal(ids, wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, wantIDs)
			}
			for i, w := range tt.want {
				r := got[i]
				if r.Heuristic != w.heuristic {
					t.Errorf("%s: heuristic = %v, want %v", r.ID, r.Heuristic, w.heuristic)
				}
				if r.Text == "" || len(r.Evidence) == 0 {
					t.Errorf("%s: a reason needs text and evidence: %+v", r.ID, r)
				}
				for _, e := range w.evidence {
					if !slices.ContainsFunc(r.Evidence, func(line string) bool { return strings.Contains(line, e) }) {
						t.Errorf("%s: no evidence line contains %q: %q", r.ID, e, r.Evidence)
					}
				}
			}
		})
	}
}

func TestWaitReasonsBoundTheEvidence(t *testing.T) {
	t.Parallel()
	c := waitCluster(cnpgv1.PhaseWaitingForInstancesToBeActive, "")
	pods := make([]*corev1.Pod, 0, 10)
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		pods = append(pods, waitPod("pg-"+n, func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.Reason = strings.Repeat("x", 2*maxWaitEvidenceText)
		}))
	}
	got := WaitReasons(WaitInput{Minor: Minor130, Cluster: c, Pods: pods})
	if len(got) != 1 {
		t.Fatalf("got %d reasons", len(got))
	}
	ev := got[0].Evidence
	if len(ev) != maxWaitEvidence+1 || ev[maxWaitEvidence] != "(more not shown)" {
		t.Fatalf("evidence not capped: %d lines, last %q", len(ev), ev[len(ev)-1])
	}
	for _, line := range ev {
		if len(line) > maxWaitEvidenceText {
			t.Errorf("line of %d bytes exceeds the bound", len(line))
		}
	}
}

func TestFilteredCacheTrap(t *testing.T) {
	t.Parallel()
	lost := waitCluster(cnpgv1.PhaseUnrecoverable, "One or more instances were previously created, but no PersistentVolumeClaims (PVCs) exist")
	labelled := waitPod("pg-1", func(p *corev1.Pod) { p.Labels = map[string]string{"cnpg.io/reconcile": "true"} })
	plain := waitPod("pg-2", nil)
	tests := []struct {
		name string
		c    *cnpgv1.Cluster
		pods []*corev1.Pod
		want bool
	}{
		{"upstream: no pod is labelled", lost, []*corev1.Pod{plain}, false},
		{"fork, fully labelled", lost, []*corev1.Pod{labelled}, false},
		{"fork, partly labelled", lost, []*corev1.Pod{labelled, plain}, true},
		{"unrecoverable for another reason", waitCluster(cnpgv1.PhaseUnrecoverable, "No pods are active"), []*corev1.Pod{labelled, plain}, false},
		{"not unrecoverable", waitCluster(cnpgv1.PhaseHealthy, ""), []*corev1.Pod{labelled, plain}, false},
		{"nil cluster", nil, nil, false},
	}
	for _, tt := range tests {
		if got := FilteredCacheTrap(tt.c, tt.pods); got != tt.want {
			t.Errorf("%s: FilteredCacheTrap = %v, want %v", tt.name, got, tt.want)
		}
	}
}
