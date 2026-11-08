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
	"context"
	"errors"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/evidence"
	consolehistory "github.com/fyannk/pgToolBox/internal/console/history"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/internal/console/topology"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func screenObjects() []client.Object {
	pvc := func(name, instance, role, size string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, Labels: map[string]string{
				cache.ClusterLabel: "orders", "cnpg.io/instanceName": instance, "cnpg.io/pvcRole": role,
			}},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: new("fast"),
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
			},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
		}
	}
	service := func(name, ip string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, Labels: map[string]string{cache.ClusterLabel: "orders"}},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: ip, Ports: []corev1.ServicePort{{Port: 5432}}},
		}
	}
	applied, notApplied := true, false
	return []client.Object{
		service("orders-rw", "10.96.14.20"),
		service("orders-ro", "10.96.14.21"),
		pvc("orders-1", "orders-1", "PG_DATA", "10Gi"),
		pvc("orders-2", "orders-2", "PG_DATA", "10Gi"),
		pvc("orders-3", "orders-3", "PG_DATA", "20Gi"),
		&cnpgv1.Database{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-app", Generation: 2},
			Spec:       cnpgv1.DatabaseSpec{ClusterRef: corev1.LocalObjectReference{Name: "orders"}, Name: "app", Owner: "app"},
			Status:     cnpgv1.DatabaseStatus{Applied: &applied, ObservedGeneration: 2},
		},
		&cnpgv1.Database{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-reporting", Generation: 3},
			Spec:       cnpgv1.DatabaseSpec{ClusterRef: corev1.LocalObjectReference{Name: "orders"}, Name: "reporting", Owner: "reporter"},
			Status:     cnpgv1.DatabaseStatus{Applied: &notApplied, ObservedGeneration: 2, Message: `role "reporter" does not exist`},
		},
		&cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-pooler-rw"},
			Spec:       cnpgv1.PoolerSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}, Type: cnpgv1.PoolerTypeRW, Instances: new(int32(2))},
			Status:     cnpgv1.PoolerStatus{Instances: 2},
		},
		&pgtoolboxv1alpha1.PgAdmin{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "team-a", Generation: 1},
			Status: pgtoolboxv1alpha1.PgAdminStatus{
				ObservedGeneration: 1, URL: "https://pgadmin.example.com",
				Servers:    []pgtoolboxv1alpha1.PgAdminServerStatus{{Cluster: "orders", Endpoint: "rw", State: "Registered"}},
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AsExpected", LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))}},
			},
		},
		&pgtoolboxv1alpha1.EvidenceAgent{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-evidence", Generation: 2},
			Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
			Status:     pgtoolboxv1alpha1.EvidenceAgentStatus{ObservedGeneration: 1, Endpoint: "https://orders-evidence.team-a.svc:8443"},
		},
	}
}

// replicationSnaps gives orders' primary walsenders and slots.
func replicationSnaps() map[sweep.Key]sweep.Snapshot {
	snaps := sweepSnaps()
	key := sweep.Key{Namespace: "team-a", Name: "orders"}
	snap := snaps[key]
	primary := snap.Instances["orders-1"]
	r := *primary.Reading
	r.Standbys = []instancestatus.Standby{
		{ApplicationName: "orders-2", State: "streaming", SyncState: "quorum", SentLSN: "0/3000000", ReplayLSN: "0/3000000"},
		{ApplicationName: "orders-3", State: "catchup", SyncState: "async", SentLSN: "0/3000000", ReplayLSN: "0/2FFC000"},
	}
	r.Slots = []instancestatus.Slot{{Name: "debezium", Type: "logical", Database: "app", Plugin: "pgoutput", WALStatus: "lost"}}
	primary.Reading = &r
	instances := map[string]sweep.Instance{}
	for k, v := range snap.Instances {
		instances[k] = v
	}
	instances["orders-1"] = primary
	snap.Instances = instances
	snaps[key] = snap
	return snaps
}

type fakeEvidence struct{ results map[string]evidence.Result }

func (f fakeEvidence) ForCluster(_, cluster string) evidence.Result {
	if r, ok := f.results[cluster]; ok {
		return r
	}
	return evidence.Result{State: viewmodel.EvidenceNoAgent}
}

func (fakeEvidence) State() viewmodel.SourceState {
	return viewmodel.SourceState{Source: "evidence", State: viewmodel.SourceLive}
}

type fakeHistory struct {
	entries []history.Entry
	revs    map[uint64]history.Revision
	owners  map[string]consolehistory.Owner
}

func (f fakeHistory) Snapshot() (history.Snapshot, bool) {
	return history.Snapshot{Entries: f.entries}, true
}

func (f fakeHistory) Revision(seq uint64) (history.Revision, bool) {
	r, ok := f.revs[seq]
	return r, ok
}

func (f fakeHistory) Diff(seq uint64) (history.Diff, bool) {
	if seq != 2 {
		return history.Diff{Seq: seq}, true
	}
	return history.Diff{Seq: 2, HasBase: true, BaseSeq: 1, BaseObservedAt: now.Add(-time.Hour), Entries: []history.DiffEntry{{Path: ".spec.instances", Op: history.DiffChanged, Before: "2", After: "3"}}}, true
}

func (f fakeHistory) OwnerOf(uid, kind, ns, name string) (consolehistory.Owner, bool) {
	o, ok := f.owners[uid]
	if !ok && kind == "Cluster" {
		return consolehistory.Owner{Namespace: ns, Cluster: name}, true
	}
	return o, ok
}
func (fakeHistory) Persistent() bool { return false }

type staticClock time.Time

func (c staticClock) Now() time.Time { return time.Time(c) }

// screenJournal holds what the recorder would have written for orders:
// the console's start, a switchover, a restart, a failed backup, a
// finding that appeared and one that cleared, and a pods gap in team-a.
func screenJournal() *timeline.Journal {
	j := timeline.New(timeline.Limits{}, staticClock(now))
	base := timeline.Event{Namespace: "team-a", Cluster: "orders", Timing: timeline.TimingObserved}
	ev := func(key string, e timeline.Event) timeline.Event {
		e.Key = key
		if e.Namespace == "" && e.Kind != timeline.KindConsole && e.Kind != timeline.KindGap {
			e.Namespace, e.Cluster = base.Namespace, base.Cluster
		}
		if e.Timing == "" {
			e.Timing = base.Timing
		}
		return e
	}
	for _, e := range []timeline.Event{
		ev("console", timeline.Event{Kind: timeline.KindConsole, Action: timeline.ActionStarted, At: now.Add(-3 * time.Hour)}),
		ev("gap/pods/began", timeline.Event{Kind: timeline.KindGap, Action: timeline.ActionBegan, At: now.Add(-2 * time.Hour), Namespace: "team-a", Ref: "pods", Detail: "watch lost"}),
		ev("gap/pods/ended", timeline.Event{Kind: timeline.KindGap, Action: timeline.ActionEnded, At: now.Add(-110 * time.Minute), Namespace: "team-a", Ref: "pods", Since: now.Add(-2 * time.Hour)}),
		ev("primary/x", timeline.Event{Kind: timeline.KindPrimary, Action: timeline.ActionSwitchover, At: now.Add(-90 * time.Minute), Timing: timeline.TimingReported, Subject: "orders-2", SubjectKind: "Pod", From: "orders-1", To: "orders-2"}),
		ev("restart/p/1", timeline.Event{Kind: timeline.KindRestart, Action: timeline.ActionRestarted, At: now.Add(-80 * time.Minute), Timing: timeline.TimingReported, Subject: "orders-1", SubjectKind: "Pod", Detail: "OOMKilled (exit 137)"}),
		ev("backup/b/failed", timeline.Event{Kind: timeline.KindBackup, Action: timeline.ActionFailed, At: now.Add(-time.Hour), Timing: timeline.TimingReported, Subject: "orders-nightly-1", SubjectKind: "Backup", Detail: "AccessDenied"}),
		ev("finding/a", timeline.Event{Kind: timeline.KindFinding, Action: timeline.ActionAppeared, At: now.Add(-50 * time.Minute), Ref: "backup.failed", Check: "backup.failed", Severity: "warning", Detail: "The last backup failed"}),
		ev("finding/c", timeline.Event{Kind: timeline.KindFinding, Action: timeline.ActionCleared, At: now.Add(-40 * time.Minute), Ref: "wal.archiveLag", Check: "wal.archiveLag", Severity: "warning"}),
		ev("other", timeline.Event{Kind: timeline.KindPhase, Action: timeline.ActionChanged, At: now.Add(-30 * time.Minute), Namespace: "team-a", Cluster: "billing", From: "a", To: "b"}),
	} {
		j.Record(e)
	}
	return j
}

// screenTrends is eight days of orders' volumes as the fork reports
// them: orders-1's data volume grows 1 GiB a day, its WAL volume is
// flat; billing reports nothing (stock CloudNativePG).
func screenTrends() *volumetrend.Store {
	const gib = 1 << 30
	store := volumetrend.NewStore(volumetrend.Limits{})
	start := now.Add(-8 * 24 * time.Hour)
	for at := start; !at.After(now); at = at.Add(15 * time.Minute) {
		days := at.Sub(start).Hours() / 24
		used := uint64(4*gib + days*gib)
		store.Observe(volumetrend.Key{Namespace: "team-a", Cluster: "orders", Instance: "orders-1", Volume: "pgdata"}, volumetrend.Reading{
			At: at, TotalBytes: 20 * gib, AvailableBytes: 20*gib - used, UsedBytes: used,
			Inodes: 1_310_720, InodesFree: 1_300_000, HasInodes: true, Source: volumetrend.SourceStatus,
		})
		store.Observe(volumetrend.Key{Namespace: "team-a", Cluster: "orders", Instance: "orders-1", Volume: "wal"}, volumetrend.Reading{
			At: at, TotalBytes: 5 * gib, AvailableBytes: 4 * gib, UsedBytes: gib, Source: volumetrend.SourceStatus,
		})
	}
	return store
}

// screenRecovery is orders' observed coverage: the older backup's path
// stops at a confirmed gap, the newer one reaches the frontier.
func screenRecovery() evidence.Recovery {
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	assumptions := []string{"segment-name-presence-only", "wal-bytes-and-restore-not-verified"}
	return evidence.Recovery{State: evidence.RecoveryAvailable, Complete: true, Paths: []evidence.RecoveryPath{
		{
			BackupID: "20261028T060000", TargetTimeline: 1, State: evidencev1alpha1.Unhealthy, Stop: evidencev1alpha1.CoverageGapLimited,
			Reason:       evidencev1alpha1.Reason{Code: "wal-gap", Message: "a confirmed WAL gap stops this path"},
			LowerBoundAt: at(30 * time.Hour), FrontierReceiptAt: at(20 * time.Hour), Assumptions: assumptions,
			StartTimeline: new(uint64(1)), FrontierTimeline: new(uint64(1)), FrontierPosition: new(uint64(120)),
		},
		{
			BackupID: "20261029T060000", TargetTimeline: 1, State: evidencev1alpha1.Healthy, Stop: evidencev1alpha1.CoverageFrontier,
			Reason:       evidencev1alpha1.Reason{Code: "evidence-evaluated", Message: "structural evidence evaluated"},
			LowerBoundAt: at(6 * time.Hour), FrontierReceiptAt: at(2 * time.Minute), Assumptions: assumptions,
			StartTimeline: new(uint64(1)), FrontierTimeline: new(uint64(1)), FrontierPosition: new(uint64(300)),
		},
	}}
}

func screenRepositoryBackups() []evidence.BackupItem {
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	done, failed := "DONE", "FAILED"
	return []evidence.BackupItem{
		{ID: "20261027T060000", Status: &failed, State: evidencev1alpha1.Unhealthy, Reason: evidencev1alpha1.Reason{Code: "backup-failed", Message: "barman reports FAILED"}, BeginAt: at(54 * time.Hour)},
		{ID: "20261028T060000", Status: &done, State: evidencev1alpha1.Healthy, BeginAt: at(31 * time.Hour), EndAt: at(30 * time.Hour), EndTimeline: new(uint64(1)), EndPosition: new(uint64(100))},
		{ID: "20261029T060000", Status: &done, State: evidencev1alpha1.Healthy, BeginAt: at(7 * time.Hour), EndAt: at(6 * time.Hour), EndTimeline: new(uint64(1)), EndPosition: new(uint64(250))},
	}
}

func newScreenService(t *testing.T) *Service {
	t.Helper()
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects(), screenObjects()...)...)
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	fc := &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}, states: map[string]viewmodel.SourceState{
		cache.SourceDatabaseRoles: {Source: cache.SourceDatabaseRoles, State: viewmodel.SourceNotInstalled, Reason: "databaseroles.postgresql.cnpg.io is not served"},
	}}
	snap := evidencev1alpha1.RepositoryEvidenceSnapshot{
		State: evidencev1alpha1.Healthy, Completeness: "complete",
		Details: evidencev1alpha1.Details{BarmanCloud: &evidencev1alpha1.BarmanCloudSummary{
			BackupItems: new(uint64(3)), StructurallyUsableBackups: new(uint64(2)),
			WAL: evidencev1alpha1.StateReason{State: evidencev1alpha1.Healthy}, Timeline: evidencev1alpha1.StateReason{State: evidencev1alpha1.Healthy},
			Coverage:  evidencev1alpha1.StateReason{State: evidencev1alpha1.Warning, Reason: evidencev1alpha1.Reason{Code: "gap", Message: "one WAL gap"}},
			Retention: evidencev1alpha1.BarmanRetentionSummary{State: evidencev1alpha1.Unknown},
		}},
	}
	observed := now.Add(-40 * time.Second)
	ev := fakeEvidence{results: map[string]evidence.Result{"orders": {
		Agent: "orders-evidence", State: viewmodel.EvidenceLive, ObservedAt: &observed, Snapshot: &snap,
		BackupIDs: map[string]bool{"20261029T060000": true, "20261028T060000": true}, BackupsComplete: true,
		Backups:  screenRepositoryBackups(),
		Recovery: screenRecovery(),
	}}}
	manifest := []byte(`{"kind":"Cluster","spec":{"instances":3,"postgresql":{"parameters":{"max_connections":"200","work_mem":"4MB"}}}}`)
	changed := []byte(`{"kind":"Cluster","spec":{"instances":3,"postgresql":{"parameters":{"max_connections":"300","work_mem":"16MB"}}}}`)
	hist := fakeHistory{
		entries: []history.Entry{
			{Seq: 3, Kind: "Backup", Namespace: "team-a", Name: "orders-nightly-1", UID: "b1", Change: history.ChangeStatus, ObservedAt: now.Add(-5 * time.Hour), HasManifest: true},
			{Seq: 2, Kind: "Cluster", Namespace: "team-a", Name: "orders", UID: string(ordersUID), Change: history.ChangeSpec, Actor: history.Actor{Manager: "kubectl", Operation: "Update"}, ObservedAt: now.Add(-10 * time.Minute), PrevObservedAt: now.Add(-time.Hour), HasManifest: true},
			{Seq: 1, Kind: "Cluster", Namespace: "team-a", Name: "billing", UID: string(billingUID), Change: history.ChangeFirstObserved, ObservedAt: now.Add(-time.Hour), HasManifest: true},
		},
		revs: map[uint64]history.Revision{
			1: {Seq: 1, Group: "postgresql.cnpg.io", Kind: "Cluster", Namespace: "team-a", Name: "billing", UID: string(billingUID), Change: history.ChangeFirstObserved, Manifest: manifest},
			2: {Seq: 2, Group: "postgresql.cnpg.io", Kind: "Cluster", Namespace: "team-a", Name: "orders", UID: string(ordersUID), Change: history.ChangeSpec, ObservedAt: now.Add(-10 * time.Minute), Manifest: changed},
		},
		owners: map[string]consolehistory.Owner{"b1": {Namespace: "team-a", Cluster: "orders"}},
	}
	svc := New(Options{
		Cache: fc, Sweep: fakeSweep{snaps: replicationSnaps()}, Evidence: ev, History: hist, Timeline: screenJournal(),
		Console: ConsoleInfo{Version: "test", Namespace: "team-a"}, Now: func() time.Time { return now },
		Trends: screenTrends(),
	})
	svc.SetRunnerStates(func() []viewmodel.RunnerState {
		return []viewmodel.RunnerState{{Name: "informer-cache", Core: true, Running: true}}
	})
	return svc
}

func TestScreenGoldenViews(t *testing.T) {
	ctx := context.Background()
	svc := newScreenService(t)
	for _, name := range []string{"orders", "billing"} {
		views := map[string]func() (any, error){
			"replication": func() (any, error) { return svc.Replication(ctx, "team-a", name) },
			"backups":     func() (any, error) { return svc.Backups(ctx, "team-a", name) },
			"storage":     func() (any, error) { return svc.Storage(ctx, "team-a", name) },
			"objects":     func() (any, error) { return svc.Objects(ctx, "team-a", name) },
			"history":     func() (any, error) { return svc.History(ctx, "team-a", name) },
			"metrics":     func() (any, error) { return svc.Metrics(ctx, "team-a", name) },
			"topology":    func() (any, error) { return svc.Topology(ctx, "team-a", name) },
			"pgadmins":    func() (any, error) { return svc.PgAdminLinks(ctx, "team-a", name) },
		}
		for view, read := range views {
			v, err := read()
			if err != nil {
				t.Fatalf("%s %s: %v", view, name, err)
			}
			golden(t, view+"-"+name, v)
		}
	}
	rev, err := svc.Revision(ctx, "team-a", "orders", 2)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "revision-orders", rev)
	tl, err := svc.Timeline(ctx, "team-a", "orders", TimelineQuery{})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "timeline-orders", tl)
	admin, err := svc.Admin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "admin", admin)
}

// TestScreenHonesty asserts what a golden diff could hide.
func TestScreenHonesty(t *testing.T) {
	ctx := context.Background()
	svc := newScreenService(t)

	t.Run("a revision of another Cluster is not found", func(t *testing.T) {
		if _, err := svc.Revision(ctx, "team-a", "orders", 1); !errors.Is(err, ErrNotFound) {
			t.Errorf("revision of billing through orders: %v", err)
		}
		h, _ := svc.History(ctx, "team-a", "orders")
		for _, e := range h.Items {
			if e.Name == "billing" {
				t.Error("orders' history lists billing")
			}
		}
		if len(h.Items) != 2 {
			t.Errorf("orders history has %d items, want its Cluster and its Backup", len(h.Items))
		}
	})

	t.Run("a failed apply is never ok", func(t *testing.T) {
		o, _ := svc.Objects(ctx, "team-a", "orders")
		for _, d := range o.Databases {
			if d.Name == "orders-reporting" && d.Health == viewmodel.HealthOK {
				t.Errorf("failed Database is ok: %+v", d)
			}
		}
		for _, s := range o.Sources {
			if s.Source == cache.SourceDatabaseRoles && s.State != viewmodel.SourceNotInstalled {
				t.Errorf("databaseRoles source %s, want notInstalled", s.State)
			}
		}
	})

	t.Run("an unreachable primary leaves standbys unknown", func(t *testing.T) {
		r, _ := svc.Replication(ctx, "team-a", "billing")
		for _, sb := range r.Standbys {
			if sb.Health != viewmodel.HealthUnknown {
				t.Errorf("standby %s of an unreachable primary is %s", sb.Name, sb.Health)
			}
		}
		for _, rc := range r.Receivers {
			if rc.Health == viewmodel.HealthOK && rc.StatusRead.State != viewmodel.StatusReadOK {
				t.Errorf("receiver %s ok without a read", rc.Name)
			}
		}
	})

	t.Run("a lost slot is critical", func(t *testing.T) {
		r, _ := svc.Replication(ctx, "team-a", "orders")
		if len(r.Slots) != 1 || r.Slots[0].Health != viewmodel.HealthCritical {
			t.Errorf("slots %+v", r.Slots)
		}
	})

	t.Run("no agent is info, not a fault", func(t *testing.T) {
		b, _ := svc.Backups(ctx, "team-a", "billing")
		if b.Evidence.State == viewmodel.EvidenceLive || b.Evidence.Health == viewmodel.HealthOK {
			t.Errorf("billing evidence %+v", b.Evidence)
		}
	})

	t.Run("an unlisted PVC is unknown", func(t *testing.T) {
		s, _ := svc.Storage(ctx, "team-a", "orders")
		for _, v := range s.Volumes {
			if v.Group == "unlisted" && v.Health != viewmodel.HealthUnknown {
				t.Errorf("unlisted PVC %s is %s", v.Name, v.Health)
			}
		}
		if len(s.Volumes) != 3 {
			t.Errorf("%d volumes", len(s.Volumes))
		}
	})

	t.Run("topology geometry is inside the drawing", func(t *testing.T) {
		tp, _ := svc.Topology(ctx, "team-a", "orders")
		for _, n := range tp.Nodes {
			if !n.State.Valid() {
				t.Errorf("node %s state %q", n.ID, n.State)
			}
		}
		for _, v := range topology.Check(tp) {
			t.Error(v)
		}
	})

	t.Run("an EvidenceAgent behind its generation is not ok", func(t *testing.T) {
		a, _ := svc.Admin(ctx)
		for _, ns := range a.Namespaces {
			for _, ag := range ns.EvidenceAgents {
				if !ag.Stale || ag.Health != viewmodel.HealthUnknown {
					t.Errorf("agent %+v", ag)
				}
			}
		}
	})
}
