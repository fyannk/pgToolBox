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
	"strconv"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/topology"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

func TestDisplayURLDropsCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"s3://orders-backups/orders", "s3://orders-backups/orders"},
		{"s3://AKIA:secret@bucket/path?X-Amz-Signature=abc#frag", "s3://bucket/path"},
		{"https://user@s3.example.com:9000", "https://s3.example.com:9000"},
		{"not a url at all", ""},
		{"://broken", ""},
	} {
		if got := displayURL(tc.in); got != tc.want {
			t.Errorf("displayURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPodCondition(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		row  viewmodel.InstanceRow
		want string
	}{
		{viewmodel.InstanceRow{}, "no pod"},
		{viewmodel.InstanceRow{Pod: &viewmodel.PodView{Phase: "Running", Ready: true}}, "Running · ready"},
		{viewmodel.InstanceRow{Pod: &viewmodel.PodView{Phase: "Running"}, Fenced: true}, "Running · not ready · fenced"},
		{viewmodel.InstanceRow{Pod: &viewmodel.PodView{Phase: "Running", Waiting: "CrashLoopBackOff", Restarts: 4}}, "CrashLoopBackOff · 4 restarts"},
		{viewmodel.InstanceRow{Pod: &viewmodel.PodView{Deleting: true, Ready: true}}, "phase not reported · deleting"},
		{viewmodel.InstanceRow{Pod: &viewmodel.PodView{Phase: "Running", Ready: true, Restarts: 1}}, "Running · ready · 1 restart"},
	} {
		if got := podCondition(tc.row); got != tc.want {
			t.Errorf("podCondition(%+v) = %q, want %q", tc.row.Pod, got, tc.want)
		}
	}
}

// The base-backup wire leaves the instance the newest Backup names, into
// the box its method writes to; an unattributed newest Backup, or a
// method whose box is not drawn, draws no wire rather than an older or
// invented one.
func TestBaseFlow(t *testing.T) {
	t.Parallel()
	backup := func(name string, age time.Duration, method, plugin, pod string) *cnpgv1.Backup {
		b := &cnpgv1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(now.Add(-age))},
			Spec:       cnpgv1.BackupSpec{Method: cnpgv1.BackupMethod(method)},
		}
		if plugin != "" {
			b.Spec.PluginConfiguration = &cnpgv1.BackupPluginConfiguration{Name: plugin}
		}
		if pod != "" {
			b.Status.InstanceID = &cnpgv1.InstanceID{PodName: pod}
		}
		return b
	}
	for _, tc := range []struct {
		name            string
		backups         []*cnpgv1.Backup
		store, snapshot bool
		want            *topology.Base
	}{
		{"none", nil, true, true, nil},
		{"plugin from a replica", []*cnpgv1.Backup{
			backup("old", 2*time.Hour, "plugin", "barman-cloud.cloudnative-pg.io", "orders-1"),
			backup("new", time.Hour, "plugin", "barman-cloud.cloudnative-pg.io", "orders-2"),
		}, true, false, &topology.Base{Instance: "orders-2", Target: topology.TargetStore, Label: "base backup · barman-cloud"}},
		{"unattributed newest skipped for the newest attributed", []*cnpgv1.Backup{
			backup("running", time.Minute, "plugin", "barman-cloud.cloudnative-pg.io", ""),
			backup("done", time.Hour, "barmanObjectStore", "", "orders-3"),
		}, true, false, &topology.Base{Instance: "orders-3", Target: topology.TargetStore, Label: "base backup · barman-cloud"}},
		{
			"snapshot",
			[]*cnpgv1.Backup{backup("snap", time.Hour, "volumeSnapshot", "", "orders-2")},
			true, true,
			&topology.Base{Instance: "orders-2", Target: topology.TargetSnapshot, Label: "base backup · volume snapshot"},
		},
		{"snapshot box not drawn", []*cnpgv1.Backup{
			backup("snap", time.Hour, "volumeSnapshot", "", "orders-2"),
			backup("older", 2*time.Hour, "plugin", "barman-cloud.cloudnative-pg.io", "orders-1"),
		}, true, false, nil},
	} {
		g := &gathered{backups: tc.backups}
		got := baseFlow(g, tc.store, tc.snapshot)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: baseFlow = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// The in-tree object store shows its destination without the
// credentials a URL can carry, and its endpoint rides the frame note.
func TestStoreBoxKeepsSecretsOut(t *testing.T) {
	t.Parallel()
	c := &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Backup: &cnpgv1.BackupConfiguration{
		RetentionPolicy: "30d",
		BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{
			DestinationPath: "s3://AKIA:hunter2@bucket/orders?token=hunter2",
			EndpointURL:     "https://key:hunter2@minio.example.com:9000",
		},
	}}}
	g := &gathered{cluster: c}
	var facts topoFacts
	b, note := (&Service{}).storeBox(g, cnpgstatus.Backups(c), true, &facts)
	text := make([]string, 0, len(b.Rows)+1)
	for _, r := range b.Rows {
		text = append(text, r.Text)
	}
	all := strings.Join(append(text, note), "|")
	if strings.Contains(all, "hunter2") || strings.Contains(all, "AKIA") {
		t.Errorf("credentials reached the drawing: %s", all)
	}
	if !strings.Contains(all, "s3://bucket/orders") || note != "https://minio.example.com:9000" || !strings.Contains(all, "retention 30d") {
		t.Errorf("store rows %q note %q", text, note)
	}
}

// Disabled default Services are not drawn; the any-instance Service is
// drawn only when a Pooler fronts it, and then the r Pooler wires to it.
func TestTopologyServices(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orders := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: ordersUID},
		Spec: cnpgv1.ClusterSpec{Instances: 1, Managed: &cnpgv1.ManagedConfiguration{Services: &cnpgv1.ManagedServices{
			DisabledDefaultServices: []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeRO},
		}}},
		Status: cnpgv1.ClusterStatus{CurrentPrimary: "orders-1", InstanceNames: []string{"orders-1"}, TimelineID: 2},
	}
	pooler := &cnpgv1.Pooler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-any"},
		Spec:       cnpgv1.PoolerSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}, Type: cnpgv1.PoolerTypeR},
	}
	rw := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-rw", Labels: map[string]string{cache.ClusterLabel: "orders"}},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.12", Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	svc := topologyService(t, orders, instancePod("orders", ordersUID, "1", true, "primary"), pooler, rw)
	tp, err := svc.Topology(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range topology.Check(tp) {
		t.Error(v)
	}
	ids := map[string]viewmodel.TopoNode{}
	for _, n := range tp.Nodes {
		ids[n.ID] = n
	}
	if _, ok := ids["svc-ro"]; ok {
		t.Error("the disabled ro Service is drawn")
	}
	if _, ok := ids["svc-r"]; !ok {
		t.Error("the r Service fronted by a Pooler is not drawn")
	}
	if p := ids["inst-orders-1"]; len(p.Lines) < 2 || p.Lines[1].Text != "timeline 2" {
		t.Errorf("primary lines %+v, want the timeline second", p.Lines)
	}
	reads := 0
	for _, e := range tp.Edges {
		if e.Kind == topology.EdgeRead {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("%d read wires, want the r Pooler into its Service only", reads)
	}
	blind := map[string]bool{}
	for _, b := range tp.BlindSpots {
		blind[b.ID] = true
	}
	if blind["topology.servicesNotRead"] || blind["topology.noPrimary"] {
		t.Errorf("blind spots %v", blind)
	}
	// The address is the observed one; a Service the cache lacks says so.
	for id, want := range map[string]string{"svc-rw": "ClusterIP 10.96.0.12:5432", "svc-r": "Service not found"} {
		if l := ids[id].Lines; len(l) == 0 || l[len(l)-1].Text != want {
			t.Errorf("%s lines %+v, want %q last", id, l, want)
		}
	}
}

// The ro Service of a Cluster that declares one instance routes to no
// replica: the box says so and is inactive, not a neutral note.
func TestTopologyROWithoutReplica(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		instances   int
		wantRouting string
		wantState   viewmodel.Health
	}{
		{1, "routes to the replicas: none", viewmodel.HealthInactive},
		{2, "routes to the replicas", viewmodel.HealthInfo},
	} {
		orders := &cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: ordersUID},
			Spec:       cnpgv1.ClusterSpec{Instances: tc.instances},
			Status:     cnpgv1.ClusterStatus{CurrentPrimary: "orders-1", InstanceNames: []string{"orders-1"}},
		}
		svc := topologyService(t, orders, instancePod("orders", ordersUID, "1", true, "primary"))
		tp, err := svc.Topology(context.Background(), "team-a", "orders")
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range topology.Check(tp) {
			t.Error(v)
		}
		var ro *viewmodel.TopoNode
		for i := range tp.Nodes {
			if tp.Nodes[i].ID == "svc-ro" {
				ro = &tp.Nodes[i]
			}
		}
		if ro == nil {
			t.Fatalf("%d instances: the ro Service is not drawn", tc.instances)
		}
		if ro.State != tc.wantState || len(ro.Lines) < 2 || ro.Lines[1].Text != tc.wantRouting {
			t.Errorf("%d instances: ro state %s lines %+v, want %s and %q", tc.instances, ro.State, ro.Lines, tc.wantState, tc.wantRouting)
		}
	}
}

// Without a primary and without readable PVCs the drawing says so.
func TestTopologyBlindSpots(t *testing.T) {
	t.Parallel()
	orders := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: ordersUID},
		Spec:       cnpgv1.ClusterSpec{Instances: 1},
		Status:     cnpgv1.ClusterStatus{InstanceNames: []string{"orders-1"}},
	}
	svc := topologyService(t, orders)
	svc.cache.(*fakeCache).states = map[string]viewmodel.SourceState{
		cache.SourcePVCs:     {Source: cache.SourcePVCs, State: viewmodel.SourceForbidden},
		cache.SourceServices: {Source: cache.SourceServices, State: viewmodel.SourceForbidden},
	}
	tp, err := svc.Topology(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	blind := map[string]bool{}
	for _, b := range tp.BlindSpots {
		blind[b.ID] = true
	}
	for _, id := range []string{"topology.noPrimary", "topology.pvcsNotRead", "topology.servicesNotRead"} {
		if !blind[id] {
			t.Errorf("blind spot %s missing from %v", id, blind)
		}
	}
	for _, v := range topology.Check(tp) {
		t.Error(v)
	}
}

func topologyService(t *testing.T, objs ...client.Object) *Service {
	t.Helper()
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	fc := &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}}
	return New(Options{Cache: fc, Sweep: fakeSweep{}, Now: func() time.Time { return now }})
}

// A crowded Cluster overflows every bound of the drawing: more instances,
// Poolers and ScheduledBackups than are drawn (each tail folds into a
// "+N more" box), a suspended schedule, and volume snapshots both
// configured and used. The whole view is pinned as a golden.
func TestTopologyGoldenCrowded(t *testing.T) {
	t.Parallel()
	const crowdUID = types.UID("9e8d7c6b-5a49-4382-9170-6f5e4d3c2b1a")
	names := make([]string, 0, topoMaxInst+2)
	for i := range topoMaxInst + 2 {
		names = append(names, "crowd-"+strconv.Itoa(i+1))
	}
	crowd := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "crowd", UID: crowdUID},
		Spec: cnpgv1.ClusterSpec{Instances: len(names), Backup: &cnpgv1.BackupConfiguration{
			VolumeSnapshot: &cnpgv1.VolumeSnapshotConfiguration{ClassName: "csi-snapclass"},
		}},
		Status: cnpgv1.ClusterStatus{CurrentPrimary: "crowd-1", InstanceNames: names, TimelineID: 3},
	}
	objs := make([]client.Object, 0, 1+len(names)+8+topoMaxSched+2+2)
	objs = append(objs, crowd)
	for i := range names {
		role := "replica"
		if i == 0 {
			role = "primary"
		}
		objs = append(objs, instancePod("crowd", crowdUID, strconv.Itoa(i+1), i%4 != 3, role))
	}
	for i, typ := range []cnpgv1.PoolerType{
		cnpgv1.PoolerTypeRW, cnpgv1.PoolerTypeRO, cnpgv1.PoolerTypeR, cnpgv1.PoolerTypeRW,
		cnpgv1.PoolerTypeRO, cnpgv1.PoolerTypeRW, cnpgv1.PoolerTypeR, cnpgv1.PoolerTypeRO,
	} {
		objs = append(objs, &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "crowd-pool-" + strconv.Itoa(i+1)},
			Spec:       cnpgv1.PoolerSpec{Cluster: cnpgv1.LocalObjectReference{Name: "crowd"}, Type: typ},
		})
	}
	for i := range topoMaxSched + 2 {
		sb := &cnpgv1.ScheduledBackup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "crowd-sched-" + strconv.Itoa(i+1)},
			Spec:       cnpgv1.ScheduledBackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "crowd"}, Schedule: "0 0 " + strconv.Itoa(i) + " * * *"},
			Status:     cnpgv1.ScheduledBackupStatus{NextScheduleTime: new(metav1.NewTime(now.Add(time.Duration(i+1) * time.Hour)))},
		}
		if i == 1 {
			sb.Spec.Suspend = new(true)
		}
		objs = append(objs, sb)
	}
	for i := range 2 {
		objs = append(objs, &cnpgv1.Backup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "crowd-snap-" + strconv.Itoa(i+1), CreationTimestamp: metav1.NewTime(now.Add(-time.Duration(i+1) * time.Hour))},
			Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "crowd"}, Method: cnpgv1.BackupMethodVolumeSnapshot},
			Status:     cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseCompleted, InstanceID: &cnpgv1.InstanceID{PodName: "crowd-2"}},
		})
	}
	tp, err := topologyService(t, objs...).Topology(context.Background(), "team-a", "crowd")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range topology.Check(tp) {
		t.Error(v)
	}
	golden(t, "topology-crowded", tp)
}
