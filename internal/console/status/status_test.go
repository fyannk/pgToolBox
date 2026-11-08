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

package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// fakeCache is the informer layer over a fake client with the console's
// own field indexes and a fixed account of the sources.
type fakeCache struct {
	reader     client.Reader
	namespaces []string
	states     map[string]viewmodel.SourceState
}

func (f *fakeCache) Reader() client.Reader { return f.reader }
func (f *fakeCache) Namespaces() []string  { return f.namespaces }
func (f *fakeCache) Source(_, source string) viewmodel.SourceState {
	if st, ok := f.states[source]; ok {
		return st
	}
	return viewmodel.SourceState{Source: source, State: viewmodel.SourceLive, Since: new(now.Add(-3 * time.Hour))}
}

func (f *fakeCache) Sources(ns string) []viewmodel.SourceState {
	out := make([]viewmodel.SourceState, 0, 5)
	for _, s := range []string{cache.SourceClusters, cache.SourcePods, cache.SourceEvents, cache.SourceBackups, cache.SourceObjectStores} {
		out = append(out, f.Source(ns, s))
	}
	return out
}

func (f *fakeCache) Readable(ns, source string) bool {
	st := f.Source(ns, source).State
	return st == viewmodel.SourceLive || st == viewmodel.SourceStale
}

func (f *fakeCache) Served(source string) *bool {
	st := f.Source("", source).State
	served := st != viewmodel.SourceNotInstalled
	return &served
}

type fakeSweep struct{ snaps map[sweep.Key]sweep.Snapshot }

func (f fakeSweep) Snapshot(k sweep.Key) (sweep.Snapshot, bool) { s, ok := f.snaps[k]; return s, ok }

func (fakeSweep) Series(sweep.Key, string) []viewmodel.Series {
	return []viewmodel.Series{{Key: "readyWalFiles", Unit: "files", Points: []viewmodel.Point{{At: now.Add(-30 * time.Second), Value: new(0.0)}, {At: now, Value: new(1.0)}}}}
}
func (fakeSweep) Metrics(sweep.Key) *metrics.Store              { return nil }
func (fakeSweep) InstanceStatus(k sweep.Key) sweep.StatusSource { return sweep.StatusSource{} }
func (fakeSweep) Insight(sweep.Key) (insight.Snapshot, bool)    { return insight.Snapshot{}, false }
func (fakeSweep) RefreshInsight(context.Context, sweep.Key) (time.Time, error) {
	return time.Time{}, sweep.ErrNoPrimary
}

func (fakeSweep) State() viewmodel.SourceState {
	return viewmodel.SourceState{Source: "instanceStatus", State: viewmodel.SourceLive, Since: new(now.Add(-3 * time.Hour))}
}

const (
	ordersUID  = types.UID("0b6a2c1e-8f0d-4a55-9a3e-7c1f2b3d4e5f")
	billingUID = types.UID("4d2f1e0a-1b2c-4d3e-8f90-a1b2c3d4e5f6")
)

func instancePod(cluster string, uid types.UID, n string, ready bool, role string) *corev1.Pod {
	name := cluster + "-" + n
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: name, UID: types.UID(name + "-uid"),
			CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour)),
			Labels:            map[string]string{cache.ClusterLabel: cluster, "cnpg.io/instanceRole": role, "cnpg.io/reconcile": "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: cluster, UID: uid, Controller: new(true),
			}},
		},
		Spec: corev1.PodSpec{NodeName: "node-" + n, Containers: []corev1.Container{{
			Name: "postgres", Image: "ghcr.io/cloudnative-pg/postgresql:18.1",
			Command: []string{"/controller/manager", "instance", "run", "--status-port-tls"},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.0.0." + n,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func objects() []client.Object {
	orders := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: ordersUID, Labels: map[string]string{"app": "orders"}},
		Spec: cnpgv1.ClusterSpec{Instances: 3, Plugins: []cnpgv1.PluginConfiguration{{
			Name: "barman-cloud.cloudnative-pg.io", IsWALArchiver: new(true), Parameters: map[string]string{"barmanObjectName": "store"},
		}}},
		Status: cnpgv1.ClusterStatus{
			Phase: "Cluster in healthy state", Instances: 3, ReadyInstances: 3,
			CurrentPrimary: "orders-1", TargetPrimary: "orders-1", Image: "ghcr.io/cloudnative-pg/postgresql:18.1",
			InstanceNames: []string{"orders-1", "orders-2", "orders-3"},
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ClusterIsReady", Message: "Cluster is Ready", LastTransitionTime: metav1.NewTime(now.Add(-26 * time.Hour))},
				{Type: "ContinuousArchiving", Status: metav1.ConditionTrue, Reason: "ContinuousArchivingSuccess", LastTransitionTime: metav1.NewTime(now.Add(-26 * time.Hour))},
				{Type: "example.com/Custom", Status: metav1.ConditionTrue, Reason: "Whatever", LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))},
			},
		},
	}
	billing := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "billing", UID: billingUID, Annotations: map[string]string{"cnpg.io/skipWalArchiving": "enabled"}},
		Spec:       cnpgv1.ClusterSpec{Instances: 2},
		Status: cnpgv1.ClusterStatus{
			Phase: "Failing over", PhaseReason: "Failing over from billing-1 to billing-2", Instances: 2, ReadyInstances: 1,
			CurrentPrimary: "billing-1", TargetPrimary: "billing-2",
			TargetPrimaryTimestamp: now.Add(-95 * time.Second).Format(time.RFC3339Nano), CurrentPrimaryFailingSinceTimestamp: now.Add(-100 * time.Second).Format(time.RFC3339Nano),
			InstanceNames: []string{"billing-1", "billing-2", "billing-3"},
			Conditions:    []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ClusterIsNotReady", LastTransitionTime: metav1.NewTime(now.Add(-95 * time.Second))}},
		},
	}
	crash := instancePod("billing", billingUID, "1", false, "primary")
	crash.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "postgres", RestartCount: 4, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	noTLS := instancePod("billing", billingUID, "2", true, "replica")
	noTLS.Spec.Containers[0].Command = []string{"/controller/manager", "instance", "run"}
	stranger := instancePod("orders", "someone-else", "9", true, "replica")
	stranger.OwnerReferences[0].Name = "impostor"

	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(cache.ObjectStoreGVK)
	store.SetNamespace("team-a")
	store.SetName("store")
	_ = unstructured.SetNestedMap(store.Object, map[string]any{
		"firstRecoverabilityPoint": now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		"lastSuccessfulBackupTime": now.Add(-5 * time.Hour).Format(time.RFC3339),
	}, "status", "serverRecoveryWindow", "orders")

	return []client.Object{
		orders, billing,
		instancePod("orders", ordersUID, "1", true, "primary"),
		instancePod("orders", ordersUID, "2", true, "replica"),
		instancePod("orders", ordersUID, "3", true, "replica"),
		stranger, crash, noTLS,
		&cnpgv1.Backup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-nightly-1", CreationTimestamp: metav1.NewTime(now.Add(-6 * time.Hour))},
			Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}, Method: "plugin"},
			Status:     cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseCompleted, StoppedAt: new(metav1.NewTime(now.Add(-5 * time.Hour)))},
		},
		&cnpgv1.ScheduledBackup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-nightly"},
			Spec:       cnpgv1.ScheduledBackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}, Schedule: "0 0 0 * * *"},
			Status:     cnpgv1.ScheduledBackupStatus{NextScheduleTime: new(metav1.NewTime(now.Add(12 * time.Hour)))},
		},
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", OwnerReferences: []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "orders", UID: ordersUID, Controller: new(true)}}},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: new("orders-1"), RenewTime: new(metav1.NewMicroTime(now.Add(-3 * time.Second)))},
		},
		store,
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "team-a", Name: "billing.1", UID: "e1"},
			InvolvedObject: corev1.ObjectReference{Kind: "Cluster", Name: "billing", Namespace: "team-a"},
			Type:           corev1.EventTypeWarning, Reason: "FailingOver", Message: "Primary billing-1 is not healthy",
			Count: 1, FirstTimestamp: metav1.NewTime(now.Add(-95 * time.Second)), LastTimestamp: metav1.NewTime(now.Add(-95 * time.Second)),
			ReportingController: "cloudnative-pg",
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "team-a", Name: "billing-1.1", UID: "e2"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "billing-1", Namespace: "team-a"},
			Type:           corev1.EventTypeWarning, Reason: "BackOff", Message: "Back-off restarting failed container postgres",
			Count: 7, LastTimestamp: metav1.NewTime(now.Add(-30 * time.Second)), Source: corev1.EventSource{Component: "kubelet"},
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "team-a", Name: "old", UID: "e3"},
			InvolvedObject: corev1.ObjectReference{Kind: "Cluster", Name: "billing"},
			Type:           corev1.EventTypeNormal, Reason: "Old", LastTimestamp: metav1.NewTime(now.Add(-48 * time.Hour)),
		},
	}
}

func reading(primary bool, replay string) *instancestatus.Reading {
	return &instancestatus.Reading{
		IsPrimary: primary, CurrentLSN: "0/3000000", ReplayLSN: replay, IsArchivingWAL: primary,
		InstanceManagerVersion: "1.30.0", TimelineID: 1, ObservedAt: now.Add(-8 * time.Second),
	}
}

func sweepSnaps() map[sweep.Key]sweep.Snapshot {
	at := now.Add(-8 * time.Second)
	ok := func(r *instancestatus.Reading) sweep.Instance {
		return sweep.Instance{State: viewmodel.StatusReadOK, At: at, Since: now.Add(-3 * time.Hour), Reading: r}
	}
	return map[sweep.Key]sweep.Snapshot{
		{Namespace: "team-a", Name: "orders"}: {SweptAt: at, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"orders-1": ok(reading(true, "")),
			"orders-2": ok(reading(false, "0/3000000")),
			"orders-3": ok(reading(false, "0/2FFC000")),
		}},
		{Namespace: "team-a", Name: "billing"}: {SweptAt: at, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"billing-1": {State: viewmodel.StatusReadUnreachable, Reason: sweep.ReasonNotReachable + ": timed out", At: at, Since: now.Add(-100 * time.Second), Reading: reading(true, "")},
			"billing-2": {State: viewmodel.StatusReadNoTLS, Reason: sweep.ReasonNoTLS, At: at, Since: now.Add(-3 * time.Hour)},
		}},
	}
}

func newService(ctx context.Context, t *testing.T, states map[string]viewmodel.SourceState) *Service {
	t.Helper()
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects()...)
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	fc := &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}, states: states}
	svc := New(Options{Cache: fc, Sweep: fakeSweep{snaps: sweepSnaps()}, Now: func() time.Time { return now }})
	runner := diag.New(svc, nil, diag.Options{Interval: time.Minute, Now: func() time.Time { return now }})
	svc.SetDiagnostics(runner)
	runner.RunAll(ctx)
	return svc
}

func golden(t *testing.T, name string, value any) {
	t.Helper()
	got, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from its golden file; run go test ./internal/console/status -update and review the diff", name)
	}
}

func TestGoldenViews(t *testing.T) {
	ctx := context.Background()
	svc := newService(ctx, t, map[string]viewmodel.SourceState{
		cache.SourceObjectStores: {Source: cache.SourceObjectStores, State: viewmodel.SourceLive, Since: new(now.Add(-3 * time.Hour))},
		cache.SourcePoolers:      {Source: cache.SourcePoolers, State: viewmodel.SourceForbidden, Since: new(now.Add(-3 * time.Hour)), Reason: "the console ServiceAccount is not allowed to list Pooler.postgresql.cnpg.io in team-a"},
	})
	list, err := svc.ClusterList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "cluster-list", list)
	for _, name := range []string{"orders", "billing"} {
		ov, err := svc.Overview(ctx, "team-a", name)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "overview-"+name, ov)
		inst, err := svc.Instances(ctx, "team-a", name)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "instances-"+name, inst)
	}
	events, err := svc.Events(ctx, "team-a", "billing")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "events-billing", events)
	d, err := svc.Diagnostics(ctx, "team-a", "billing")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "diagnostics-billing", d)
	tr, err := svc.Triage(ctx, "team-a", "billing")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "triage-billing", tr)
}

// TestHonesty asserts the invariants the goldens could hide in a diff.
func TestHonesty(t *testing.T) {
	ctx := context.Background()

	t.Run("healthy cluster rolls up ok", func(t *testing.T) {
		svc := newService(ctx, t, nil)
		ov, err := svc.Overview(ctx, "team-a", "orders")
		if err != nil {
			t.Fatal(err)
		}
		if ov.Verdict.Health != viewmodel.HealthOK {
			for _, c := range ov.Verdict.Contributors {
				t.Logf("%s %s %s", c.ID, c.Health, c.Detail)
			}
			t.Errorf("verdict = %s", ov.Verdict.Health)
		}
		if len(ov.Cards) != 3 {
			t.Errorf("cards = %d: a pod controlled by another Cluster was counted", len(ov.Cards))
		}
		if ov.Backups.RecoveryWindow == nil || ov.Backups.RecoveryWindow.First == nil {
			t.Error("plugin recovery window not read from the ObjectStore")
		}
		if len(ov.Backups.NotReported) == 0 {
			t.Error("plugin-based backups do not list the fields they never report")
		}
	})

	t.Run("a stale pod source never reads healthy", func(t *testing.T) {
		svc := newService(ctx, t, map[string]viewmodel.SourceState{
			cache.SourcePods: {Source: cache.SourcePods, State: viewmodel.SourceStale, Since: new(now.Add(-4 * time.Minute)), Reason: "watch lost: connection refused"},
		})
		ov, err := svc.Overview(ctx, "team-a", "orders")
		if err != nil {
			t.Fatal(err)
		}
		if ov.Verdict.Health == viewmodel.HealthOK || !ov.Verdict.Stale {
			t.Errorf("verdict = %s stale=%v", ov.Verdict.Health, ov.Verdict.Stale)
		}
	})

	t.Run("forbidden clusters are not found, not empty healthy", func(t *testing.T) {
		svc := newService(ctx, t, map[string]viewmodel.SourceState{
			cache.SourceClusters: {Source: cache.SourceClusters, State: viewmodel.SourceForbidden, Reason: "no"},
		})
		if _, err := svc.Overview(ctx, "team-a", "orders"); !errors.Is(err, ErrNotFound) {
			t.Errorf("overview = %v", err)
		}
		list, err := svc.ClusterList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 || list.Namespaces[0].Sources[0].State != viewmodel.SourceForbidden {
			t.Errorf("list = %+v", list)
		}
	})

	t.Run("unknown cluster is not found", func(t *testing.T) {
		svc := newService(ctx, t, nil)
		for _, call := range []func() error{
			func() error { _, err := svc.Overview(ctx, "team-a", "ghost"); return err },
			func() error { _, err := svc.Instances(ctx, "team-a", "ghost"); return err },
			func() error { _, err := svc.Events(ctx, "team-a", "ghost"); return err },
			func() error { _, err := svc.Diagnostics(ctx, "team-a", "ghost"); return err },
			func() error { _, err := svc.Triage(ctx, "team-a", "ghost"); return err },
		} {
			if err := call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v", err)
			}
		}
	})

	t.Run("instances: error rows are present, never omitted", func(t *testing.T) {
		svc := newService(ctx, t, nil)
		list, err := svc.Instances(ctx, "team-a", "billing")
		if err != nil {
			t.Fatal(err)
		}
		states := map[string]viewmodel.StatusReadState{}
		for _, row := range list.Items {
			states[row.Name] = row.StatusRead.State
		}
		want := map[string]viewmodel.StatusReadState{
			"billing-1": viewmodel.StatusReadUnreachable, "billing-2": viewmodel.StatusReadNoTLS, "billing-3": viewmodel.StatusReadNotRunning,
		}
		for name, state := range want {
			if states[name] != state {
				t.Errorf("%s = %q, want %q", name, states[name], state)
			}
		}
		if list.Items[0].Name != "billing-1" {
			t.Errorf("primary not first: %s", list.Items[0].Name)
		}
		if live := list.Items[0].Live; live == nil || !live.Provenance.Stale {
			t.Error("the last good reading of a failing instance is not marked stale")
		}
	})

	t.Run("old events are outside the window", func(t *testing.T) {
		svc := newService(ctx, t, nil)
		events, err := svc.Events(ctx, "team-a", "billing")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events.Items {
			if e.Reason == "Old" {
				t.Error("an event older than the window was served")
			}
		}
		if len(events.Items) != 2 || events.Items[0].Reason != "BackOff" {
			t.Errorf("events = %+v", events.Items)
		}
	})

	t.Run("sweep roster", func(t *testing.T) {
		svc := newService(ctx, t, nil)
		targets := svc.Targets(ctx)
		if len(targets) != 2 {
			t.Fatalf("targets = %+v", targets)
		}
		for _, target := range targets {
			if target.Key.Name == "billing" {
				for _, pod := range target.Pods {
					if pod.Name == "billing-2" && pod.StatusPortTLS {
						t.Error("a pod without --status-port-tls is marked TLS")
					}
				}
			}
			if target.Key.Name == "orders" && !strings.HasPrefix(target.ServerName, "orders-rw.team-a") {
				t.Errorf("server name = %q", target.ServerName)
			}
		}
	})

	t.Run("diagnostics input claims only readable sources", func(t *testing.T) {
		svc := newService(ctx, t, map[string]viewmodel.SourceState{
			cache.SourceEvents:           {Source: cache.SourceEvents, State: viewmodel.SourceForbidden},
			cache.SourceScheduledBackups: {Source: cache.SourceScheduledBackups, State: viewmodel.SourceNotInstalled},
		})
		in, ok := svc.Input(ctx, cache.ClusterKey{Namespace: "team-a", Name: "billing"})
		if !ok {
			t.Fatal("no input")
		}
		if in.HasEvents || in.HasBackups {
			t.Errorf("HasEvents=%v HasBackups=%v with forbidden/absent sources", in.HasEvents, in.HasBackups)
		}
		if !in.HasCluster || !in.HasPods || len(in.Pods.Pods) != 2 {
			t.Errorf("cluster/pods: %v %v %d", in.HasCluster, in.HasPods, len(in.Pods.Pods))
		}
	})
}

func TestLinkMapping(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "/": "", "/cluster": "", "/cluster/overview": "",
		"/cluster/pods": "instances", "/cluster/pods/": "instances", "/cluster/pods/x-1": "instances/x-1",
		"/pods": "instances", "/cluster/metrics": "metrics", "/poolers/metrics": "metrics",
		"/poolers": "objects", "/databases": "objects", "/objects": "objects",
		"/backups": "backups", "/backups/evidence": "backups", "backups": "backups",
		"/history": "history", "/history/42": "history/42",
		"diagnostics": "diagnostics", "instances": "instances", "/somewhere/else": "",
		"/insight": "insight", "/insight/vacuum": "insight/vacuum", "/insight/connections": "insight/connections",
		"/insight/nope": "insight",
	} {
		if got := link(in); got != want {
			t.Errorf("link(%q) = %q, want %q", in, got, want)
		}
	}
}
