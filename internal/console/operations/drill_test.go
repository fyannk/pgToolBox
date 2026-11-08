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

package operations

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

type fakeDrillState struct {
	stores   map[string]ops.StoreFacts // namespace/name
	bytes    *int64
	running  []*cnpgv1.Cluster
	listErr  error
	readable map[string]bool // namespaces whose ObjectStores are readable; nil: all with stores != nil
	// target is the evidence agent's newest window backup; targetWhy
	// why there is none.
	target    PgBackRestTarget
	targetWhy string
}

func (f *fakeDrillState) PgBackRestTarget(string, string) (PgBackRestTarget, string) {
	return f.target, f.targetWhy
}

func (f *fakeDrillState) DatabaseBytes(string, string, string) *int64 { return f.bytes }

func (f *fakeDrillState) ObjectStore(_ context.Context, ns, name string) (ops.StoreFacts, bool, bool) {
	if f.stores == nil || (f.readable != nil && !f.readable[ns]) {
		return ops.StoreFacts{}, false, false
	}
	facts, ok := f.stores[ns+"/"+name]
	return facts, ok, true
}

func (f *fakeDrillState) DrillClusters(context.Context, string) ([]*cnpgv1.Cluster, error) {
	return f.running, f.listErr
}

var sourceStore = ops.StoreFacts{DestinationPath: "s3://backups/team-a", EndpointURL: "https://s3.example"}

func runningDrill(source, name string) *cnpgv1.Cluster {
	return &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, Labels: map[string]string{
		ops.LabelRestoreDrillOf: source, ops.LabelRestoreDrillSourceNamespace: "team-a", ops.LabelDrillOwner: "team-a",
	}}}
}

func completed(name string, stopped time.Time) *cnpgv1.Backup {
	return &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, CreationTimestamp: metav1.NewTime(stopped.Add(-time.Hour))},
		Status:     cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseCompleted, StoppedAt: &metav1.Time{Time: stopped}},
	}
}

type drillFixture struct {
	svc     *Service
	w       *fakeWriter
	records *drills.Store
	asked   []string
	log     *bytes.Buffer
}

func newDrillFixture(t *testing.T, drillNS string, mutate func(*DrillOptions)) *drillFixture {
	t.Helper()
	f := &drillFixture{w: &fakeWriter{c: cluster()}, records: drills.NewStore(nil, nil, time.Now()), log: &bytes.Buffer{}}
	size := int64(50 << 30)
	d := &DrillOptions{
		Namespace: drillNS, Prefix: "drill-", TimeLimit: 2 * time.Hour, Owner: "team-a",
		Keys: [][]byte{bytes.Repeat([]byte{1}, 32)}, Selector: labels.Everything(),
		State:   &fakeDrillState{stores: map[string]ops.StoreFacts{"team-a/store": sourceStore}, bytes: &size},
		Records: f.records,
		CanCreate: func(_ context.Context, _ *auth.Principal, ns string) (bool, bool) {
			f.asked = append(f.asked, ns)
			return true, false
		},
	}
	if mutate != nil {
		mutate(d)
	}
	st := ClusterState{Cluster: cluster(), Pods: pods(), Backups: []*cnpgv1.Backup{
		completed("orders-old", now.Add(-30*time.Hour)), completed("orders-new", now.Add(-5*time.Hour)),
		{ObjectMeta: metav1.ObjectMeta{Name: "orders-failed"}, Status: cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseFailed}},
	}}
	f.svc = service(st, f.w, f.log)
	f.svc.opts.Drills = d
	return f
}

func drillOfferOf(t *testing.T, v viewmodel.Operations) viewmodel.OperationOffer {
	t.Helper()
	for _, o := range v.Offers {
		if o.Kind == viewmodel.OperationRestoreDrill {
			return o
		}
	}
	t.Fatal("no restoreDrill offer")
	return viewmodel.OperationOffer{}
}

func reasons(bl []viewmodel.OperationBlocker) []string {
	out := make([]string, 0, len(bl))
	for _, b := range bl {
		out = append(out, b.Reason)
	}
	return out
}

func TestDrillOffer(t *testing.T) {
	ctx := context.Background()
	f := newDrillFixture(t, "", nil)
	v, err := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	o := drillOfferOf(t, v)
	if !o.Allowed || len(o.Blockers) != 0 || o.Confirmation != "orders" || o.Drill == nil || o.Drill.Namespace != "team-a" ||
		o.Drill.Backup != "orders-new" || o.Drill.TimeLimitSeconds != 7200 || !strings.Contains(o.Drill.RecoverySource, "ObjectStore store") ||
		len(f.asked) != 1 || f.asked[0] != "team-a" {
		t.Errorf("offer = %+v, asked %v", o, f.asked)
	}

	// Disabled: no offer at all.
	off := newDrillFixture(t, "", nil)
	off.svc.opts.Drills = nil
	v, _ = off.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
	for _, o := range v.Offers {
		if o.Kind == viewmodel.OperationRestoreDrill {
			t.Error("offered while disabled")
		}
	}
}

func TestDrillOfferSaysWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		ns      string
		mutate  func(*DrillOptions)
		cluster func(*cnpgv1.Cluster)
		backups bool
		want    string
	}{
		{"no completed backup", "", nil, nil, false, "NoCompletedBackup"},
		{"ObjectStore missing in the drill namespace", "drills", nil, nil, true, "ObjectStoreMissing"},
		{"ObjectStores not readable", "", func(d *DrillOptions) { d.State.(*fakeDrillState).stores = nil }, nil, true, "ObjectStoreNotReadable"},
		{"volume snapshots", "", nil, func(c *cnpgv1.Cluster) {
			c.Spec.Plugins = nil
			c.Spec.Backup = &cnpgv1.BackupConfiguration{VolumeSnapshot: &cnpgv1.VolumeSnapshotConfiguration{}}
		}, true, "NoRecoverySource"},
		{"in-tree in another namespace", "drills", nil, func(c *cnpgv1.Cluster) {
			c.Spec.Plugins = nil
			c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{DestinationPath: "s3://b"}}
		}, true, "InTreeOtherNamespace"},
		{"a drill of a drill", "", nil, func(c *cnpgv1.Cluster) { c.Labels = map[string]string{ops.LabelRestoreDrillOf: "x"} }, true, "DrillOfDrill"},
		{"outside the selector", "", func(d *DrillOptions) { d.Selector = labels.SelectorFromSet(labels.Set{"tier": "db"}) }, nil, true, "OutsideClusterSelector"},
		{"the drill namespace's store points elsewhere", "drills", func(d *DrillOptions) {
			d.State.(*fakeDrillState).stores["drills/store"] = ops.StoreFacts{DestinationPath: "s3://other-bucket/team-a", EndpointURL: "https://s3.example"}
		}, nil, true, "ObjectStoreMismatch"},
		{"another endpoint", "drills", func(d *DrillOptions) {
			d.State.(*fakeDrillState).stores["drills/store"] = ops.StoreFacts{DestinationPath: "s3://backups/team-a", EndpointURL: "https://evil.example"}
		}, nil, true, "ObjectStoreMismatch"},
		{"another serverName", "drills", func(d *DrillOptions) {
			d.State.(*fakeDrillState).stores["drills/store"] = ops.StoreFacts{DestinationPath: "s3://backups/team-a", EndpointURL: "https://s3.example", ServerName: "x"}
		}, nil, true, "ObjectStoreMismatch"},
		{"the source's store unreadable", "drills", func(d *DrillOptions) {
			st := d.State.(*fakeDrillState)
			st.stores["drills/store"] = sourceStore
			st.readable = map[string]bool{"drills": true}
		}, nil, true, "SourceObjectStoreUnknown"},
		{"workload identity in the drill's store", "drills", func(d *DrillOptions) {
			d.State.(*fakeDrillState).stores["drills/store"] = ops.StoreFacts{DestinationPath: "s3://backups/team-a", EndpointURL: "https://s3.example", WorkloadIdentity: "inheritFromIAMRole"}
		}, nil, true, "WorkloadIdentity"},
		{"workload identity in-tree", "", nil, func(c *cnpgv1.Cluster) {
			c.Spec.Plugins = nil
			c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{
				DestinationPath: "s3://b", BarmanCredentials: cnpgv1.BarmanCredentials{AWS: &cnpgv1.S3Credentials{InheritFromIAMRole: true}},
			}}
		}, true, "WorkloadIdentity"},
		{"a drill of this source running on another replica", "", func(d *DrillOptions) {
			d.State.(*fakeDrillState).running = []*cnpgv1.Cluster{runningDrill("orders", "drill-orders-1")}
		}, nil, true, "DrillRunning"},
		{"the console's cap", "", func(d *DrillOptions) {
			d.MaxConcurrent = 2
			d.State.(*fakeDrillState).running = []*cnpgv1.Cluster{runningDrill("billing", "drill-billing-1"), runningDrill("audit", "drill-audit-1")}
		}, nil, true, "TooManyDrills"},
		{"the source's server certificate is user-provided", "", func(d *DrillOptions) {
			d.ServerCA = ops.DrillServerCA{Mode: ops.DrillServerCASource}
		}, func(c *cnpgv1.Cluster) {
			c.Spec.Certificates = &cnpgv1.CertificatesConfiguration{ServerCASecret: "issuer", ServerTLSSecret: "orders-tls"}
		}, true, "DrillNotVerifiable"},
		{"the source's CA in another namespace", "drills", func(d *DrillOptions) {
			d.ServerCA = ops.DrillServerCA{Mode: ops.DrillServerCASource}
			d.State.(*fakeDrillState).stores["drills/store"] = sourceStore
		}, nil, true, "DrillNotVerifiable"},
		{"the running drills cannot be listed", "", func(d *DrillOptions) {
			d.State.(*fakeDrillState).listErr = errors.New("cache not synced")
		}, nil, true, "DrillsUnknown"},
	}
	for _, tt := range tests {
		f := newDrillFixture(t, tt.ns, tt.mutate)
		st := f.svc.opts.State.(*fakeState).st
		if tt.cluster != nil {
			tt.cluster(st.Cluster)
		}
		if !tt.backups {
			st.Backups = nil
		}
		f.svc.opts.State = &fakeState{st: st}
		v, err := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
		if err != nil {
			t.Fatal(err)
		}
		if got := reasons(drillOfferOf(t, v).Blockers); !strings.Contains(strings.Join(got, ","), tt.want) {
			t.Errorf("%s: blockers %v, want %s", tt.name, got, tt.want)
		}
	}
	// The selector keys the source carries are copied, so the drill
	// stays inside it.
	f := newDrillFixture(t, "", func(d *DrillOptions) { d.Selector = labels.SelectorFromSet(labels.Set{"tier": "db"}) })
	st := f.svc.opts.State.(*fakeState).st
	st.Cluster.Labels = map[string]string{"tier": "db", "other": "x"}
	f.svc.opts.State = &fakeState{st: st}
	f.w.c = st.Cluster
	v, _ := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
	if bl := drillOfferOf(t, v).Blockers; len(bl) != 0 {
		t.Errorf("inside the selector: %v", bl)
	}
	if _, err := f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"}); err != nil {
		t.Fatal(err)
	}
	if l := f.w.drills[0].Labels; l["tier"] != "db" || l["other"] != "" {
		t.Errorf("drill labels = %v", l)
	}
}

func TestDrillRequest(t *testing.T) {
	ctx := context.Background()
	f := newDrillFixture(t, "", nil)
	if _, err := f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "nope"}); err == nil {
		t.Fatal("wrong confirmation accepted")
	}
	acc, err := f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.w.drills) != 1 {
		t.Fatalf("created %d drills", len(f.w.drills))
	}
	d := f.w.drills[0]
	if d.Name != "drill-orders-2610291200" || d.Namespace != "team-a" || acc.Created != d.Name || acc.CreatedNamespace != "team-a" {
		t.Errorf("drill %s/%s, accepted %+v", d.Namespace, d.Name, acc)
	}
	if err := ops.CheckDrillCluster(d); err != nil {
		t.Errorf("invariant: %v", err)
	}
	d.CreationTimestamp = metav1.NewTime(now.Add(time.Second)) // the API server's
	meta, err := ops.VerifyDrill(d, "team-a", f.svc.opts.Drills.Keys)
	if err != nil || meta.Backup != "orders-new" || meta.SourceBytes == nil || !meta.Deadline.Equal(now.Add(2*time.Hour)) {
		t.Errorf("sealed record = %+v, %v", meta, err)
	}
	rec, ok := f.records.Get(drills.Key{Namespace: "team-a", Name: d.Name})
	if !ok || rec.UID != "drill-uid" || rec.Result != viewmodel.DrillRunning {
		t.Errorf("record = %+v", rec)
	}
	if !strings.Contains(f.log.String(), "operation=restoreDrill") || !strings.Contains(f.log.String(), "createdNamespace=team-a") {
		t.Errorf("audit line: %s", f.log.String())
	}
	if strings.Contains(f.log.String(), "secret-token-value") {
		t.Error("token in the audit log")
	}

	// One running drill per source.
	_, err = f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"})
	var rej *Rejected
	if !errors.As(err, &rej) || rej.Status != http.StatusConflict || !strings.Contains(rej.Text, "still running") {
		t.Errorf("second drill: %v", err)
	}

	// The tracker follows the record.
	id, _ := ParseID(acc.OperationID)
	op, err := f.svc.Lookup(ctx, id)
	if err != nil || op.Phase != viewmodel.OpPhaseActing || op.CreatedNamespace != "team-a" || !op.Deadline.After(now.Add(2*time.Hour)) {
		t.Errorf("tracked = %+v, %v", op, err)
	}
	rec = drills.Judge(rec, drills.Observation{Phase: "Setting up primary"}, now.Add(time.Minute))
	f.records.Put(rec)
	if op, _ = f.svc.Lookup(ctx, id); op.Phase != viewmodel.OpPhaseActing || !strings.HasPrefix(op.Steps[len(op.Steps)-1].Text, "healthy: phase") {
		t.Errorf("acting = %+v", op)
	}
	rec.Result, rec.FinishedAt, rec.TornDownAt = viewmodel.DrillPassed, now.Add(15*time.Minute), now.Add(16*time.Minute)
	f.records.Put(rec)
	if op, _ = f.svc.Lookup(ctx, id); op.Phase != viewmodel.OpPhaseSettled || op.Health != viewmodel.HealthOK ||
		op.Steps[len(op.Steps)-1].Text != "check passed; drill Cluster deleted" {
		t.Errorf("settled = %+v", op)
	}
}

// With RESTORE_DRILL_SERVER_CA=source the drill names the source's server
// CA Secret, which an admin-filled bundle already holds; with secret, the
// admin's; with own (the operator's mirror), none.
func TestDrillRequestServerCA(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		ca   ops.DrillServerCA
		want string
	}{
		{ops.DrillServerCA{}, ""},
		{ops.DrillServerCA{Mode: ops.DrillServerCASource}, "orders-ca"},
		{ops.DrillServerCA{Mode: ops.DrillServerCASecret, Secret: "drill-ca"}, "drill-ca"},
	} {
		f := newDrillFixture(t, "", func(d *DrillOptions) { d.ServerCA = tt.ca })
		v, err := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
		if err != nil {
			t.Fatal(err)
		}
		if bl := drillOfferOf(t, v).Blockers; len(bl) != 0 {
			t.Errorf("%+v: blockers %v", tt.ca, bl)
		}
		if _, err := f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"}); err != nil {
			t.Fatal(err)
		}
		got := ""
		if cs := f.w.drills[0].Spec.Certificates; cs != nil {
			got = cs.ServerCASecret
		}
		if got != tt.want {
			t.Errorf("%+v: serverCASecret %q, want %q", tt.ca, got, tt.want)
		}
	}
}

func TestDrillRequestRechecksLive(t *testing.T) {
	ctx := context.Background()
	f := newDrillFixture(t, "", nil)
	live := cluster()
	live.Spec.Plugins = nil // the live Cluster no longer backs up
	f.w.c = live
	_, err := f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"})
	var rej *Rejected
	if !errors.As(err, &rej) || rej.Status != http.StatusConflict || len(f.w.drills) != 0 {
		t.Errorf("err = %v, created %d", err, len(f.w.drills))
	}
	off := newDrillFixture(t, "", nil)
	off.svc.opts.Drills = nil
	if _, err := off.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"}); !errors.As(err, &rej) || rej.Status != http.StatusUnprocessableEntity {
		t.Errorf("disabled: %v", err)
	}
}

// A same-namespace drill and a matching store in another namespace pass;
// the cap counts only live drills.
func TestDrillOfferStoreMatchAndCap(t *testing.T) {
	ctx := context.Background()
	f := newDrillFixture(t, "drills", func(d *DrillOptions) {
		d.MaxConcurrent = 2
		st := d.State.(*fakeDrillState)
		st.stores["drills/store"] = ops.StoreFacts{DestinationPath: "s3://backups/team-a", EndpointURL: "https://s3.example"}
		gone := runningDrill("billing", "drill-billing-1")
		gone.DeletionTimestamp = &metav1.Time{Time: now}
		st.running = []*cnpgv1.Cluster{gone, runningDrill("audit", "drill-audit-1")}
	})
	v, err := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if bl := drillOfferOf(t, v).Blockers; len(bl) != 0 {
		t.Errorf("blockers = %v", reasons(bl))
	}
}

// Regression: the operation handle is not signed;
// a handle naming another Cluster than the drill's source must not reveal
// the drill's record.
func TestTrackDrillAnswersOnlyForTheHandlesCluster(t *testing.T) {
	ctx := context.Background()
	f := newDrillFixture(t, "", nil)
	acc, err := f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ParseID(acc.OperationID)
	rec, _ := f.records.Get(drills.Key{Namespace: id.CreatedNamespace, Name: id.Created})
	rec.Result, rec.FinishedAt, rec.Reason = viewmodel.DrillFailed, now.Add(time.Minute), "size: restored 1 GiB of the secret table"
	f.records.Put(rec)

	forged := id
	forged.Cluster = "billing" // a Cluster the reader may see, not the drill's source
	op := f.svc.trackDrill(forged, viewmodel.Operation{})
	for _, step := range op.Steps {
		if strings.Contains(step.Text, "secret table") {
			t.Fatalf("forged handle read the drill record: %+v", op.Steps)
		}
	}
	if op.Phase != viewmodel.OpPhaseAccepted {
		t.Errorf("forged handle phase = %s, want accepted (no record)", op.Phase)
	}
	if real := f.svc.trackDrill(id, viewmodel.Operation{}); real.Phase != viewmodel.OpPhaseFailed {
		t.Errorf("the real handle = %s", real.Phase)
	}
}
