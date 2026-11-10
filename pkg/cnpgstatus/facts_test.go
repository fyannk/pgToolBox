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
	"reflect"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func clusterWith(annotations map[string]string, conditions ...metav1.Condition) *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "team", Annotations: annotations},
		Status:     cnpgv1.ClusterStatus{Conditions: conditions},
	}
}

func cond(typ, status, reason string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: metav1.ConditionStatus(status), Reason: reason, Message: reason + " message"}
}

func TestFencing(t *testing.T) {
	tests := []struct {
		name    string
		cluster *cnpgv1.Cluster
		want    FencingReading
	}{
		{"nil cluster", nil, FencingReading{State: FencingUnreadable}},
		{"no annotation", clusterWith(nil), FencingReading{State: FencingNone}},
		{"empty annotation", clusterWith(map[string]string{AnnotationFencedInstances: ""}), FencingReading{State: FencingNone}},
		{"empty list", clusterWith(map[string]string{AnnotationFencedInstances: "[]"}), FencingReading{State: FencingNone}},
		{"null", clusterWith(map[string]string{AnnotationFencedInstances: "null"}), FencingReading{State: FencingNone}},
		{"some sorted dedup", clusterWith(map[string]string{AnnotationFencedInstances: `["pg-2","pg-1","pg-2"]`}), FencingReading{State: FencingSome, Instances: []string{"pg-1", "pg-2"}}},
		{"all", clusterWith(map[string]string{AnnotationFencedInstances: `["*"]`}), FencingReading{State: FencingAll, Instances: []string{"*"}}},
		{"star wins over names", clusterWith(map[string]string{AnnotationFencedInstances: `["pg-1","*"]`}), FencingReading{State: FencingAll, Instances: []string{"*"}}},
		{"unreadable", clusterWith(map[string]string{AnnotationFencedInstances: `pg-1`}), FencingReading{State: FencingUnreadable}},
		{"unreadable object", clusterWith(map[string]string{AnnotationFencedInstances: `{"a":1}`}), FencingReading{State: FencingUnreadable}},
		{"unreadable numbers", clusterWith(map[string]string{AnnotationFencedInstances: `[1,2]`}), FencingReading{State: FencingUnreadable}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Fencing(tt.cluster); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Fencing() = %+v, want %+v", got, tt.want)
			}
		})
	}
	// Bounded: many long names.
	names := make([]string, 0, 200)
	for i := range 200 {
		names = append(names, `"`+strings.Repeat("x", 300)+string(rune('a'+i%26))+strings.Repeat("y", i)+`"`)
	}
	got := Fencing(clusterWith(map[string]string{AnnotationFencedInstances: "[" + strings.Join(names, ",") + "]"}))
	if got.State != FencingSome || len(got.Instances) > maxFencedInstances {
		t.Fatalf("unbounded fencing: state %q, %d instances", got.State, len(got.Instances))
	}
	for _, name := range got.Instances {
		if len(name) > maxAnnotationValue {
			t.Fatalf("instance name of %d bytes", len(name))
		}
	}
}

func TestHibernation(t *testing.T) {
	on := map[string]string{AnnotationHibernation: "on"}
	off := map[string]string{AnnotationHibernation: "off"}
	tests := []struct {
		name       string
		cluster    *cnpgv1.Cluster
		wantState  string
		wantReason string
	}{
		{"nil", nil, HibernationUnreadable, ""},
		{"no annotation", clusterWith(nil), HibernationOff, ""},
		{"off", clusterWith(off), HibernationOff, ""},
		{"on, not reported yet", clusterWith(on), HibernationInProgress, "Requested"},
		{"on, waiting for healthy", clusterWith(on, cond(ConditionHibernation, "False", "WaitingForHealthy")), HibernationInProgress, "WaitingForHealthy"},
		{"on, deleting pods", clusterWith(on, cond(ConditionHibernation, "False", "DeletingPods")), HibernationInProgress, "DeletingPods"},
		{"on, waiting pods deletion", clusterWith(on, cond(ConditionHibernation, "False", "WaitingPodsDeletion")), HibernationInProgress, "WaitingPodsDeletion"},
		{"on, hibernated", clusterWith(on, cond(ConditionHibernation, "True", "Hibernated")), HibernationHibernated, "Hibernated"},
		{"on, wrong value reported", clusterWith(on, cond(ConditionHibernation, "False", "WrongAnnotationValue")), HibernationUnreadable, "WrongAnnotationValue"},
		{"off, condition left", clusterWith(off, cond(ConditionHibernation, "True", "Hibernated")), HibernationInProgress, "Resuming"},
		{"absent, condition left", clusterWith(nil, cond(ConditionHibernation, "True", "Hibernated")), HibernationInProgress, "Resuming"},
		{"invalid value", clusterWith(map[string]string{AnnotationHibernation: "yes"}), HibernationUnreadable, "InvalidAnnotationValue"},
		{"case matters", clusterWith(map[string]string{AnnotationHibernation: "On"}), HibernationUnreadable, "InvalidAnnotationValue"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Hibernation(tt.cluster)
			if got.State != tt.wantState || got.Reason != tt.wantReason {
				t.Errorf("Hibernation() = %+v, want state %q reason %q", got, tt.wantState, tt.wantReason)
			}
		})
	}
	long := Hibernation(clusterWith(map[string]string{AnnotationHibernation: strings.Repeat("é", 1000)}))
	if len(long.Message) > 2*maxAnnotationValue+200 {
		t.Errorf("unbounded hibernation message: %d bytes", len(long.Message))
	}
}

func TestEscapeHatches(t *testing.T) {
	if got := EscapeHatches(nil); got != nil {
		t.Errorf("nil cluster: %v", got)
	}
	if got := EscapeHatches(clusterWith(nil)); got != nil {
		t.Errorf("no annotations: %v", got)
	}
	inactive := clusterWith(map[string]string{
		AnnotationReconciliationLoop: "enabled",
		AnnotationValidation:         "enabled",
		AnnotationReconcilePodSpec:   "enabled",
		AnnotationSkipWalArchiving:   "disabled",
		AnnotationPodPatch:           "",
		AnnotationUnrecoverable:      "true", // a Pod annotation: no effect on the Cluster
	})
	if got := EscapeHatches(inactive); got != nil {
		t.Errorf("inactive values flagged: %v", got)
	}
	all := clusterWith(map[string]string{
		AnnotationPodPatch:                 `[{"op":"add","path":"/metadata/labels/x","value":"` + strings.Repeat("a", 1000) + `"}]`,
		AnnotationValidation:               "disabled",
		AnnotationSkipEmptyWalArchiveCheck: "enabled",
		AnnotationReconcilePodSpec:         "disabled",
		AnnotationSkipWalArchiving:         "enabled",
		AnnotationReconciliationLoop:       "disabled",
	})
	got := EscapeHatches(all)
	wantOrder := []string{
		AnnotationReconciliationLoop, AnnotationSkipWalArchiving, AnnotationValidation,
		AnnotationReconcilePodSpec, AnnotationPodPatch, AnnotationSkipEmptyWalArchiveCheck,
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d hatches, want %d: %+v", len(got), len(wantOrder), got)
	}
	for i, hatch := range got {
		if hatch.Annotation != wantOrder[i] {
			t.Errorf("position %d: %q, want %q", i, hatch.Annotation, wantOrder[i])
		}
		if len(hatch.Value) > maxAnnotationValue {
			t.Errorf("%q value of %d bytes", hatch.Annotation, len(hatch.Value))
		}
		if hatch.Effect == "" || !hatch.Class.Valid() {
			t.Errorf("%q: effect %q class %q", hatch.Annotation, hatch.Effect, hatch.Class)
		}
	}
	if got[0].Class != ClassCritical || got[1].Class != ClassCritical {
		t.Errorf("reconciliationLoop and skipWalArchiving must be critical: %+v", got[:2])
	}
	// Deterministic.
	if again := EscapeHatches(all); !reflect.DeepEqual(again, got) {
		t.Error("EscapeHatches is not deterministic")
	}
}

func TestPodEscapeHatches(t *testing.T) {
	pod := func(name, value string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if value != "-" {
			p.Annotations = map[string]string{AnnotationUnrecoverable: value}
		}
		return p
	}
	got := PodEscapeHatches([]*corev1.Pod{nil, pod("pg-3", "true"), pod("pg-1", "1"), pod("pg-2", "yes"), pod("pg-4", "-"), pod("pg-5", "false")})
	if len(got) != 2 || got[0].Pod != "pg-1" || got[1].Pod != "pg-3" {
		t.Fatalf("PodEscapeHatches = %+v", got)
	}
	if got[0].Class != ClassCritical || got[0].Annotation != AnnotationUnrecoverable {
		t.Errorf("hatch = %+v", got[0])
	}
	if PodEscapeHatches(nil) != nil {
		t.Error("nil pods")
	}
}

func TestBackups(t *testing.T) {
	disabled := false
	tests := []struct {
		name    string
		cluster *cnpgv1.Cluster
		want    BackupConfig
	}{
		{"nil", nil, BackupConfig{}},
		{"nothing", clusterWith(nil), BackupConfig{}},
		{
			"in-tree barman", &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Backup: &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}}},
			BackupConfig{Configured: true},
		},
		{
			"volume snapshots only", &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Backup: &cnpgv1.BackupConfiguration{VolumeSnapshot: &cnpgv1.VolumeSnapshotConfiguration{}}}},
			BackupConfig{Configured: true},
		},
		{"empty backup section", &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Backup: &cnpgv1.BackupConfiguration{}}}, BackupConfig{}},
		{
			"barman cloud plugin by name", &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: BarmanCloudPlugin}}}},
			BackupConfig{Configured: true, PluginBased: true, PluginName: BarmanCloudPlugin, NotReported: pluginNotReported},
		},
		{
			"other archiver plugin", &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: "metrics.example"}, {Name: "pgbackrest.example", IsWALArchiver: new(true)}}}},
			BackupConfig{Configured: true, PluginBased: true, PluginName: "pgbackrest.example", NotReported: pluginNotReported},
		},
		{
			"disabled plugin", &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: BarmanCloudPlugin, Enabled: &disabled}}}},
			BackupConfig{},
		},
		{
			"non-archiver plugin", &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: "metrics.example", IsWALArchiver: new(false)}}}},
			BackupConfig{},
		},
		{
			// isWALArchiver is not required: CNPG archives through every
			// enabled plugin that can.
			"pgBackRest Dalibo without isWALArchiver",
			&cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "pg"}, Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: "metrics.example"}, {Name: PgBackRestDaliboPlugin, Parameters: map[string]string{"stanzaRef": "main"}}}}},
			BackupConfig{Configured: true, PluginBased: true, PluginName: PgBackRestDaliboPlugin, NotReported: pluginNotReported, PgBackRest: &PgBackRestConfig{
				Plugin: PgBackRestDaliboPlugin, Flavor: "dalibo", Display: "pgBackRest (Dalibo)", ObjectKind: "Stanza", ObjectName: "main",
			}},
		},
		{
			"pgBackRest Opera, stanza defaults to the Cluster name",
			&cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "pg"}, Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: PgBackRestOperaPlugin, Parameters: map[string]string{"pgbackrestObjectName": "store"}}}}},
			BackupConfig{Configured: true, PluginBased: true, PluginName: PgBackRestOperaPlugin, NotReported: pluginNotReported, PgBackRest: &PgBackRestConfig{
				Plugin: PgBackRestOperaPlugin, Flavor: "opera", Display: "pgBackRest (Opera)", ObjectKind: "Archive", ObjectName: "store", Stanza: "pg",
			}},
		},
		{
			"pgBackRest Opera with a stanza",
			&cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "pg"}, Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: PgBackRestOperaPlugin, Parameters: map[string]string{"pgbackrestObjectName": "store", "stanza": "prod"}}}}},
			BackupConfig{Configured: true, PluginBased: true, PluginName: PgBackRestOperaPlugin, NotReported: pluginNotReported, PgBackRest: &PgBackRestConfig{
				Plugin: PgBackRestOperaPlugin, Flavor: "opera", Display: "pgBackRest (Opera)", ObjectKind: "Archive", ObjectName: "store", Stanza: "prod",
			}},
		},
		{
			"disabled pgBackRest plugin",
			&cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: PgBackRestDaliboPlugin, Enabled: &disabled}}}},
			BackupConfig{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Backups(tt.cluster); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Backups() = %+v, want %+v", got, tt.want)
			}
		})
	}
	// NotReported is a copy: mutating it does not change the next answer.
	b := Backups(&cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{Name: BarmanCloudPlugin}}}})
	b.NotReported[0] = "mutated"
	if pluginNotReported[0] == "mutated" {
		t.Error("NotReported aliases the package table")
	}
}

func TestArchiving(t *testing.T) {
	plugin := func(annotations map[string]string, conditions ...metav1.Condition) *cnpgv1.Cluster {
		c := clusterWith(annotations, conditions...)
		c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: BarmanCloudPlugin}}
		return c
	}
	ok := cond(ConditionContinuousArchiving, "True", "ContinuousArchivingSuccess")
	failing := cond(ConditionContinuousArchiving, "False", "ContinuousArchivingFailing")
	tests := []struct {
		name       string
		cluster    *cnpgv1.Cluster
		live       *ArchiverLive
		wantClass  Class
		wantReason string
	}{
		{"nil cluster", nil, nil, ClassUnknown, "NoCluster"},
		{"not configured", clusterWith(nil), nil, ClassInfo, "NotConfigured"},
		{"configured, no condition", plugin(nil), nil, ClassUnknown, "NoCondition"},
		{"configured, no condition, live archiving", plugin(nil), &ArchiverLive{IsArchivingWAL: true}, ClassOK, "LiveArchiving"},
		{"ok", plugin(nil, ok), nil, ClassOK, "ContinuousArchivingSuccess"},
		{"idle primary with one old failure stays ok", plugin(nil, ok), &ArchiverLive{IsArchivingWAL: false, LastFailedWAL: "000000010000000000000003"}, ClassOK, "ContinuousArchivingSuccess"},
		{"never archived stays ok", plugin(nil, ok), &ArchiverLive{}, ClassOK, "ContinuousArchivingSuccess"},
		{"failure with backlog", plugin(nil, ok), &ArchiverLive{LastFailedWAL: "000000010000000000000003", ReadyWALFiles: 12}, ClassWarning, "FailureWithBacklog"},
		{"failing", plugin(nil, failing), nil, ClassCritical, "ContinuousArchivingFailing"},
		{"two segments waiting stays ok", plugin(nil, ok), &ArchiverLive{IsArchivingWAL: true, ReadyWALFiles: 2}, ClassOK, "ContinuousArchivingSuccess"},
		{"backlog without a failure", plugin(nil, ok), &ArchiverLive{IsArchivingWAL: true, ReadyWALFiles: 3}, ClassWarning, ArchivingReasonBacklog},
		{"hung archiver: backlog, no attempt finished", plugin(nil, ok), &ArchiverLive{ReadyWALFiles: 40}, ClassWarning, ArchivingReasonBacklog},
		{"no condition, live archiving with a backlog", plugin(nil), &ArchiverLive{IsArchivingWAL: true, ReadyWALFiles: 5}, ClassWarning, ArchivingReasonBacklog},
		{"backlog while failing stays critical", plugin(nil, failing), &ArchiverLive{ReadyWALFiles: 40}, ClassCritical, "ContinuousArchivingFailing"},
		{"failing, live recovered", plugin(nil, failing), &ArchiverLive{IsArchivingWAL: true}, ClassWarning, "RecoveredSinceFailure"},
		{"condition unknown", plugin(nil, cond(ConditionContinuousArchiving, "Unknown", "")), nil, ClassUnknown, ""},
		{"skip annotation", plugin(map[string]string{AnnotationSkipWalArchiving: "enabled"}, ok), nil, ClassWarning, "ArchivingSkipped"},
		{"condition without config (in-tree removed)", clusterWith(nil, failing), nil, ClassCritical, "ContinuousArchivingFailing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Archiving(tt.cluster, tt.live)
			if got.Class != tt.wantClass || got.Reason != tt.wantReason {
				t.Errorf("Archiving() = %+v, want class %q reason %q", got, tt.wantClass, tt.wantReason)
			}
		})
	}
}

func TestPrimary(t *testing.T) {
	ts := "2026-10-29T10:11:12.345678Z"
	when := time.Date(2026, 10, 29, 10, 11, 12, 345678000, time.UTC)
	other := "2026-10-29T09:00:00Z"
	status := func(s cnpgv1.ClusterStatus) *cnpgv1.Cluster { return &cnpgv1.Cluster{Status: s} }
	tests := []struct {
		name      string
		cluster   *cnpgv1.Cluster
		wantState string
		wantSince *time.Time
	}{
		{"nil", nil, PrimaryUnknown, nil},
		{"none", status(cnpgv1.ClusterStatus{}), PrimaryNone, nil},
		{"bootstrap target only", status(cnpgv1.ClusterStatus{TargetPrimary: "pg-1", Phase: cnpgv1.PhaseFirstPrimary}), PrimaryNone, nil},
		{"stable", status(cnpgv1.ClusterStatus{CurrentPrimary: "pg-1", TargetPrimary: "pg-1", CurrentPrimaryTimestamp: ts, TargetPrimaryTimestamp: other}), PrimaryStable, &when},
		{"switchover by names", status(cnpgv1.ClusterStatus{CurrentPrimary: "pg-1", TargetPrimary: "pg-2", TargetPrimaryTimestamp: ts}), PrimarySwitchover, &when},
		{"switchover by phase", status(cnpgv1.ClusterStatus{CurrentPrimary: "pg-1", TargetPrimary: "pg-1", Phase: cnpgv1.PhaseSwitchover, TargetPrimaryTimestamp: ts}), PrimarySwitchover, &when},
		{"failover pending", status(cnpgv1.ClusterStatus{CurrentPrimary: "pg-1", TargetPrimary: "pending", CurrentPrimaryFailingSinceTimestamp: ts, TargetPrimaryTimestamp: other}), PrimaryFailoverPending, &when},
		{"failover pending, no failing-since", status(cnpgv1.ClusterStatus{CurrentPrimary: "pg-1", TargetPrimary: "pending", TargetPrimaryTimestamp: ts}), PrimaryFailoverPending, &when},
		{"failing over", status(cnpgv1.ClusterStatus{CurrentPrimary: "pg-1", TargetPrimary: "pg-2", Phase: cnpgv1.PhaseFailOver, TargetPrimaryTimestamp: ts}), PrimaryFailover, &when},
		{"bad timestamp", status(cnpgv1.ClusterStatus{CurrentPrimary: "pg-1", TargetPrimary: "pg-1", CurrentPrimaryTimestamp: "yesterday"}), PrimaryStable, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Primary(tt.cluster)
			if got.State != tt.wantState {
				t.Errorf("state = %q, want %q", got.State, tt.wantState)
			}
			switch {
			case tt.wantSince == nil && got.Since != nil:
				t.Errorf("since = %v, want nil", got.Since)
			case tt.wantSince != nil && (got.Since == nil || !got.Since.Equal(*tt.wantSince)):
				t.Errorf("since = %v, want %v", got.Since, tt.wantSince)
			}
		})
	}
}

func TestStatusPortTLS(t *testing.T) {
	pod := func(containers ...corev1.Container) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{Containers: containers}}
	}
	withFlag := corev1.Container{Name: "postgres", Command: []string{"/controller/manager", "instance", "run", "--status-port-tls"}}
	without := corev1.Container{Name: "postgres", Command: []string{"/controller/manager", "instance", "run"}}
	sidecar := corev1.Container{Name: "sidecar", Command: []string{"--status-port-tls"}}
	tests := []struct {
		name   string
		pod    *corev1.Pod
		want   bool
		want31 bool
	}{
		{"nil", nil, false, false},
		{"flag", pod(withFlag), true, true},
		{"pre-1.24 pod without flag", pod(without), false, true},
		{"flag on another container only", pod(sidecar, without), false, true},
		{"sidecar first, postgres with flag", pod(sidecar, withFlag), true, true},
		{"first postgres container decides", pod(without, withFlag), false, true},
		{"no postgres container", pod(sidecar), false, false},
		{"flag in args, not command", pod(corev1.Container{Name: "postgres", Args: []string{"--status-port-tls"}}), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusPortTLS(tt.pod); got != tt.want {
				t.Errorf("StatusPortTLS = %v, want %v", got, tt.want)
			}
			for _, minor := range []Minor{Minor130, "1.29", "", "1.28"} {
				if got := StatusPortTLSForMinor(minor, tt.pod); got != tt.want {
					t.Errorf("StatusPortTLSForMinor(%q) = %v, want %v", minor, got, tt.want)
				}
			}
			if got := StatusPortTLSForMinor(Minor131, tt.pod); got != tt.want31 {
				t.Errorf("StatusPortTLSForMinor(1.31) = %v, want %v", got, tt.want31)
			}
		})
	}
}

func TestStatusServerName(t *testing.T) {
	withDisabled := func(disabled ...cnpgv1.ServiceSelectorType) *cnpgv1.Cluster {
		c := clusterWith(nil)
		c.Spec.Managed = &cnpgv1.ManagedConfiguration{Services: &cnpgv1.ManagedServices{DisabledDefaultServices: disabled}}
		return c
	}
	tests := []struct {
		name    string
		cluster *cnpgv1.Cluster
		want    string
	}{
		{"nil", nil, ""},
		{"unnamed", &cnpgv1.Cluster{}, ""},
		{"no namespace", &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "pg"}}, ""},
		{"default", clusterWith(nil), "pg-rw.team.svc"},
		{"managed without services", func() *cnpgv1.Cluster {
			c := clusterWith(nil)
			c.Spec.Managed = &cnpgv1.ManagedConfiguration{}
			return c
		}(), "pg-rw.team.svc"},
		{"r and ro disabled", withDisabled("r", "ro"), "pg-rw.team.svc"},
		{"rw disabled", withDisabled("rw"), "pg-r.team.svc"},
		{"rw and r disabled", withDisabled("rw", "r"), "pg-ro.team.svc"},
		{"all disabled", withDisabled("rw", "r", "ro"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusServerName(tt.cluster); got != tt.want {
				t.Errorf("StatusServerName = %q, want %q", got, tt.want)
			}
		})
	}
}

func podWithInit(name, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/version": "17"}},
		Spec:       corev1.PodSpec{InitContainers: []corev1.Container{{Name: name, Image: image}}},
	}
}

func annotated(pod *corev1.Pod, version string) *corev1.Pod {
	pod.Annotations = map[string]string{"cnpg.io/operatorVersion": version}
	return pod
}

func TestUpgradeInFlight(t *testing.T) {
	pods := func(versions ...string) []*corev1.Pod {
		out := make([]*corev1.Pod, 0, len(versions))
		for _, v := range versions {
			out = append(out, annotated(podWithInit("other", ""), v))
		}
		return out
	}
	tests := []struct {
		name string
		e    VersionEvidence
		want string
	}{
		{"agree", VersionEvidence{InstanceManagerVersions: []string{"1.30.1", "1.30.1"}, InstancePods: pods("1.30.1", "1.30.1")}, ""},
		{"no annotation is no evidence", VersionEvidence{InstanceManagerVersions: []string{"1.30.0"}}, ""},
		{"no instance manager reading is no evidence", VersionEvidence{InstancePods: pods("1.30.1")}, ""},
		{
			"operator ahead of every instance",
			VersionEvidence{InstanceManagerVersions: []string{"1.30.0"}, InstancePods: pods("1.30.1")},
			"instanceManagerVersion 1.30.0; cnpg.io/operatorVersion 1.30.1",
		},
		{
			"half rolled",
			VersionEvidence{InstanceManagerVersions: []string{"1.30.1", "1.30.0", "garbage"}, InstancePods: pods("1.30.1", "1.30.1")},
			"instanceManagerVersion 1.30.0, 1.30.1; cnpg.io/operatorVersion 1.30.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UpgradeInFlight(tt.e); got != tt.want {
				t.Errorf("UpgradeInFlight() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectVersion(t *testing.T) {
	fingerprint := &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{OperatorCertificateFingerprint: "ab:cd"}}
	tests := []struct {
		name string
		e    VersionEvidence
		want Version
	}{
		{"nothing", VersionEvidence{}, Version{Confidence: ConfidenceUnknown}},
		{"labels are never evidence", VersionEvidence{
			Cluster:      &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/version": "1.30.0"}}},
			InstancePods: []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/version": "1.30.0"}}}},
		}, Version{Confidence: ConfidenceUnknown}},
		{
			"reported",
			VersionEvidence{InstanceManagerVersions: []string{"1.30.0"}},
			Version{"1.30.0", Minor130, ConfidenceReported, "instanceManagerVersion"},
		},
		{
			"reported mixed takes highest",
			VersionEvidence{InstanceManagerVersions: []string{"1.29.2", "garbage", "", "1.30.0", "1.29.2"}},
			Version{"1.30.0", Minor130, ConfidenceReported, "instanceManagerVersion (mixed: 1.29.2, 1.30.0)"},
		},
		{
			"reported unsupported",
			VersionEvidence{InstanceManagerVersions: []string{"1.28.1"}},
			Version{"1.28.1", "", ConfidenceReported, "instanceManagerVersion"},
		},
		{"garbage ignored, short version accepted, image not consulted", VersionEvidence{
			InstanceManagerVersions: []string{"latest", "1.30", "\x00", strings.Repeat("1", 100)},
			InstancePods:            []*corev1.Pod{podWithInit("bootstrap-controller", "ghcr.io/cloudnative-pg/cloudnative-pg:1.29.2")},
		}, Version{"1.30.0", Minor130, ConfidenceReported, "instanceManagerVersion"}},
		{"operatorVersion annotation ranks above the lagging init image", VersionEvidence{InstancePods: []*corev1.Pod{
			annotated(podWithInit("bootstrap-controller", "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0"), "1.30.1"),
		}}, Version{"1.30.1", Minor130, ConfidenceImage, "cnpg.io/operatorVersion annotation"}},
		{"operatorVersion annotation mixed takes highest, garbage ignored", VersionEvidence{InstancePods: []*corev1.Pod{
			annotated(podWithInit("other", ""), "1.30.0"), annotated(podWithInit("other", ""), "1.30.1"), annotated(podWithInit("other", ""), "main"),
		}}, Version{"1.30.1", Minor130, ConfidenceImage, "cnpg.io/operatorVersion annotation (mixed: 1.30.0, 1.30.1)"}},
		{"instanceManagerVersion ranks above the annotation", VersionEvidence{
			InstanceManagerVersions: []string{"1.30.0"},
			InstancePods:            []*corev1.Pod{annotated(podWithInit("other", ""), "1.30.1")},
		}, Version{"1.30.0", Minor130, ConfidenceReported, "instanceManagerVersion"}},
		{"image tag", VersionEvidence{InstancePods: []*corev1.Pod{
			nil,
			podWithInit("bootstrap-controller", "ghcr.io/cloudnative-pg/cloudnative-pg:1.29.2@sha256:0123"),
			podWithInit("bootstrap-controller", "registry.local:5000/mirror/cloudnative-pg:1.29.1"),
		}}, Version{"1.29.2", "", ConfidenceImage, "bootstrap-controller image (mixed: 1.29.1, 1.29.2)"}},
		{"digest-only image and other init containers ignored", VersionEvidence{InstancePods: []*corev1.Pod{
			podWithInit("bootstrap-controller", "ghcr.io/cloudnative-pg/cloudnative-pg@sha256:0123"),
			podWithInit("other", "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0"),
			podWithInit("bootstrap-controller", "registry.local:5000/cnpg:main"),
		}}, Version{Confidence: ConfidenceUnknown}},
		{
			"commit hash",
			VersionEvidence{Cluster: &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{CommitHash: "94ee311"}}},
			Version{"1.29.2", "", ConfidenceInferred, "status.cloudNativePGCommitHash"},
		},
		{
			"commit hash too short or not hex",
			VersionEvidence{Cluster: &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{CommitHash: "94ee"}}},
			Version{Confidence: ConfidenceUnknown},
		},
		{
			"commit hash unknown",
			VersionEvidence{Cluster: &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{CommitHash: "zzzzzzz"}}},
			Version{Confidence: ConfidenceUnknown},
		},
		{
			"feature floor 1.30",
			VersionEvidence{Cluster: fingerprint},
			Version{">=1.30", Minor130, ConfidenceInferred, "feature:status.operatorCertificateFingerprint"},
		},
		{
			"feature floor from discovery",
			VersionEvidence{LeaseServed: new(true), FailoverQuorumsServed: new(true)},
			Version{">=1.30", Minor130, ConfidenceInferred, "feature:primary Lease"},
		},
		{
			"false discovery is no evidence",
			VersionEvidence{LeaseServed: new(false), DatabaseRolesServed: new(false)},
			Version{Confidence: ConfidenceUnknown},
		},
		{
			"1.27 floor has no tables",
			VersionEvidence{Cluster: clusterWith(nil, cond(ConditionConsistentSystemID, "True", "Unique"))},
			Version{">=1.27", "", ConfidenceInferred, "feature:condition ConsistentSystemID"},
		},
		{
			"lagging instance manager overridden by operator floor",
			VersionEvidence{InstanceManagerVersions: []string{"1.29.2"}, Cluster: fingerprint},
			Version{">=1.30", Minor130, ConfidenceInferred, "feature:status.operatorCertificateFingerprint overrides instanceManagerVersion 1.29.2"},
		},
		{"main reports 1.30.1 but has bootstrap-instance", VersionEvidence{
			InstanceManagerVersions: []string{"1.30.1"},
			InstancePods:            []*corev1.Pod{podWithInit("bootstrap-instance", "ghcr.io/cloudnative-pg/postgresql:18")},
		}, Version{">=1.31", Minor131, ConfidenceInferred, "feature:bootstrap-instance init container (planned 1.31) overrides instanceManagerVersion 1.30.1"}},
		{
			"floor not above reported keeps reported",
			VersionEvidence{InstanceManagerVersions: []string{"1.30.0"}, Cluster: fingerprint},
			Version{"1.30.0", Minor130, ConfidenceReported, "instanceManagerVersion"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectVersion(tt.e); got != tt.want {
				t.Errorf("DetectVersion() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReleaseCommitsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range releaseCommits {
		if len(r.commit) != 40 || seen[r.commit[:7]] {
			t.Errorf("commit %q is not a unique full hash", r.commit)
		}
		seen[r.commit[:7]] = true
		v, ok := parseSemver(r.version)
		if !ok {
			t.Errorf("release %q does not parse", r.version)
			continue
		}
		// 1.29, below the support floor, is kept so that it is named
		// rather than guessed; every other release has a table.
		if _, ok := MinorOf(r.version); !ok && v.minorString() != "1.29" {
			t.Errorf("release %q has no table", r.version)
		}
	}
}

func TestBlindSpots(t *testing.T) {
	ids := func(spots []BlindSpot) []string {
		out := make([]string, 0, len(spots))
		for _, s := range spots {
			if s.Panel == "" || s.Text == "" {
				t.Errorf("blind spot %q has no panel or text", s.ID)
			}
			out = append(out, s.ID)
		}
		return out
	}
	contains := func(list []string, id string) bool {
		for _, x := range list {
			if x == id {
				return true
			}
		}
		return false
	}
	base := ids(BlindSpots(nil, BackupConfig{}))
	for _, id := range []string{BlindSpotStreamingReplicaOnly, BlindSpotIsFencedNotReported, BlindSpotNoObservedGeneration} {
		if !contains(base, id) {
			t.Errorf("unconditional blind spot %q missing", id)
		}
	}
	if contains(base, BlindSpotPluginNotReported) {
		t.Error("plugin blind spot without plugin backups")
	}
	plugin := BlindSpots(clusterWith(nil), BackupConfig{Configured: true, PluginBased: true, PluginName: BarmanCloudPlugin})
	if !contains(ids(plugin), BlindSpotPluginNotReported) {
		t.Error("plugin blind spot missing")
	}
	for _, s := range plugin {
		if s.ID == BlindSpotPluginNotReported && (!strings.Contains(s.Text, "not reported by plugin-based backups") || strings.Contains(strings.ToLower(s.Text), "no backup")) {
			t.Errorf("plugin blind spot text: %q", s.Text)
		}
	}
	if got := BlindSpotsFor("1.29", nil, BackupConfig{}); len(got) != len(base) {
		t.Errorf("1.29 is below the support floor but added blind spots: %v", ids(got))
	}
	if got := ids(BlindSpotsFor(Minor130, nil, BackupConfig{})); !contains(got, BlindSpotNoSyncTopologyCondition) {
		t.Errorf("1.30 blind spots: %v", got)
	}
	if got := ids(BlindSpotsFor(Minor131, nil, BackupConfig{})); contains(got, BlindSpotNoSyncTopologyCondition) || !contains(got, BlindSpotBootstrapJobsGone) {
		t.Errorf("1.31 blind spots: %v", got)
	}
	if got := BlindSpotsFor("garbage", nil, BackupConfig{}); len(got) != len(base) {
		t.Errorf("unsupported minor added blind spots: %v", ids(got))
	}
	// IDs are unique within one answer.
	seen := map[string]bool{}
	for _, id := range ids(BlindSpotsFor(Minor130, nil, BackupConfig{PluginBased: true})) {
		if seen[id] {
			t.Errorf("duplicate blind spot %q", id)
		}
		seen[id] = true
	}
}

func TestArchiveBacklogThresholdFollowsParallelArchiving(t *testing.T) {
	inTree := func(parallel int) *cnpgv1.Cluster {
		c := clusterWith(nil, cond(ConditionContinuousArchiving, "True", "ContinuousArchivingSuccess"))
		c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}
		if parallel > 0 {
			c.Spec.Backup.BarmanObjectStore.Wal = &barmanapi.WalBackupConfiguration{MaxParallel: parallel}
		}
		return c
	}
	tests := []struct {
		name      string
		cluster   *cnpgv1.Cluster
		ready     int
		wantClass Class
	}{
		{"nil cluster uses the floor", nil, ArchiveBacklogSegments, ClassUnknown},
		{"default parallelism: three is a backlog", inTree(0), 3, ClassWarning},
		{"maxParallel 8: eight in flight is not a backlog", inTree(8), 8, ClassOK},
		{"maxParallel 8: nine is", inTree(8), 9, ClassWarning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Archiving(tt.cluster, &ArchiverLive{IsArchivingWAL: true, ReadyWALFiles: tt.ready}).Class; got != tt.wantClass {
				t.Errorf("Archiving(ready %d) = %q, want %q", tt.ready, got, tt.wantClass)
			}
		})
	}
	if got := ArchiveBacklogThreshold(nil); got != ArchiveBacklogSegments {
		t.Errorf("ArchiveBacklogThreshold(nil) = %d", got)
	}
}
