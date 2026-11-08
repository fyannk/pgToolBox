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
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// errTornDown is the failure the failing readers return.
var errTornDown = errors.New("cache torn down")

// failing decides, per object or list, whether a read fails.
type failing struct {
	list func(client.ObjectList) bool
	get  func(client.ObjectKey, client.Object) bool
}

// failingService is the read model over the fixture objects with reads
// that fail where f says, on sources the cache reports live.
func failingService(t *testing.T, f *failing) *Service {
	t.Helper()
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects()...).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if f.list != nil && f.list(list) {
				return errTornDown
			}
			return c.List(ctx, list, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if f.get != nil && f.get(key, obj) {
				return errTornDown
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	return New(Options{Cache: &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}}, Sweep: fakeSweep{snaps: sweepSnaps()}, Now: func() time.Time { return now }})
}

func sourceState(states []viewmodel.SourceState, source string) viewmodel.SourceStateKind {
	for _, s := range states {
		if s.Source == source {
			return s.State
		}
	}
	return ""
}

// A failed Cluster list keeps the namespace's last known Clusters in the
// roster (the sweep, diagnostics and timeline would otherwise see every
// Cluster deleted) and marks the namespace unread on the list.
func TestKeysListFailureKeepsLastKnown(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	svc := failingService(t, &failing{list: func(l client.ObjectList) bool {
		_, ok := l.(*cnpgv1.ClusterList)
		return ok && fail.Load()
	}})
	before := svc.Keys(ctx)
	if len(before) != 2 {
		t.Fatalf("keys before the failure: %v", before)
	}
	fail.Store(true)
	if after := svc.Keys(ctx); len(after) != len(before) {
		t.Fatalf("a failed list dropped Clusters from the roster: %v", after)
	}
	list, err := svc.ClusterList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceState(list.Namespaces[0].Sources, cache.SourceClusters); got != viewmodel.SourceUnavailable {
		t.Fatalf("namespace clusters source %q after a failed list, want unavailable", got)
	}
	if len(list.Items) != 2 {
		t.Fatalf("the last known Clusters are still listed, got %d rows", len(list.Items))
	}

	// A console that never listed the namespace has nothing to keep: the
	// list is empty, and says the namespace was not read.
	cold := failingService(t, &failing{list: func(l client.ObjectList) bool { _, ok := l.(*cnpgv1.ClusterList); return ok }})
	list, err = cold.ClusterList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 || sourceState(list.Namespaces[0].Sources, cache.SourceClusters) != viewmodel.SourceUnavailable {
		t.Fatalf("a namespace never listed reads as empty: %+v", list.Namespaces)
	}
}

// A Cluster the cache lists but cannot read keeps its row, unknown, and
// its namespace says it was not read; a Cluster deleted in between is
// simply gone.
func TestClusterListKeepsUnreadableRow(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{get: func(key client.ObjectKey, obj client.Object) bool {
		_, ok := obj.(*cnpgv1.Cluster)
		return ok && key.Name == "billing"
	}})
	list, err := svc.ClusterList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var billing *viewmodel.ClusterRow
	for i := range list.Items {
		if list.Items[i].Ref.Name == "billing" {
			billing = &list.Items[i]
		}
	}
	if billing == nil {
		t.Fatal("an unreadable Cluster vanished from the list")
	}
	if billing.Verdict.Health != viewmodel.HealthUnknown || !billing.Verdict.Stale || len(billing.Verdict.Contributors) != 1 {
		t.Fatalf("unreadable row verdict: %+v", billing.Verdict)
	}
	if got := sourceState(list.Namespaces[0].Sources, cache.SourceClusters); got != viewmodel.SourceUnavailable {
		t.Fatalf("namespace clusters source %q with a Cluster unread", got)
	}
	if len(list.Items) != 2 {
		t.Fatalf("rows: %d", len(list.Items))
	}
}

// A failed PgAdmin or EvidenceAgent list marks its source unavailable on
// the admin page: "no pgAdmin here" is not what the console read.
func TestAdminListFailureIsNotEmpty(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{list: func(l client.ObjectList) bool {
		switch l.(type) {
		case *pgtoolboxv1alpha1.PgAdminList, *pgtoolboxv1alpha1.EvidenceAgentList:
			return true
		}
		return false
	}})
	admin, err := svc.Admin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(admin.Namespaces) != 1 {
		t.Fatalf("namespaces: %+v", admin.Namespaces)
	}
	for _, source := range []string{cache.SourcePgAdmins, cache.SourceEvidenceAgents} {
		if got := sourceState(admin.Namespaces[0].Sources, source); got != viewmodel.SourceUnavailable {
			t.Errorf("%s source %q after a failed list, want unavailable", source, got)
		}
	}
}

// A Lease or FailoverQuorum Get that fails (not NotFound) is a failed
// read, not "no lease" / "no quorum": the diagnostics input leaves the
// snapshot unsupplied, the overview and replication sources say so, and
// version detection does not take it for an absent Lease.
func TestLeaseAndQuorumGetFailureIsNotAbsence(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{get: func(_ client.ObjectKey, obj client.Object) bool {
		switch obj.(type) {
		case *coordinationv1.Lease, *cnpgv1.FailoverQuorum:
			return true
		}
		return false
	}})
	key := cache.ClusterKey{Namespace: "team-a", Name: "orders"}
	in, ok := svc.Input(ctx, key)
	if !ok {
		t.Fatal("no input")
	}
	if in.HasPrimaryLease || in.HasFailoverQuorum {
		t.Fatalf("a failed read supplied as a snapshot: lease %v quorum %v", in.HasPrimaryLease, in.HasFailoverQuorum)
	}
	ov, err := svc.Overview(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Replication(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{cache.SourceLeases, cache.SourceFailoverQuorums} {
		if got := sourceState(ov.Sources, source); got != viewmodel.SourceUnavailable {
			t.Errorf("overview %s source %q, want unavailable", source, got)
		}
		if got := sourceState(rep.Sources, source); got != viewmodel.SourceUnavailable {
			t.Errorf("replication %s source %q, want unavailable", source, got)
		}
	}
	g, err := svc.gather(ctx, key, false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseObserved(g) != nil {
		t.Error("a failed Lease read counted as observed")
	}

	// NotFound stays an answer.
	absent := failingService(t, &failing{})
	in, _ = absent.Input(ctx, cache.ClusterKey{Namespace: "team-a", Name: "billing"})
	if !in.HasPrimaryLease || !in.HasFailoverQuorum {
		t.Fatal("an absent Lease or quorum is an answer, not a failed read")
	}
}

// A failed event list marks the events source on the events screen and
// leaves the diagnostics' event snapshot unsupplied: "no events" is not
// what the console read.
func TestEventsListFailureIsNotEmpty(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{list: func(l client.ObjectList) bool {
		_, ok := l.(*corev1.EventList)
		return ok
	}})
	ev, err := svc.Events(ctx, "team-a", "billing")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Source.State != viewmodel.SourceUnavailable {
		t.Fatalf("events source %q after a failed list", ev.Source.State)
	}
	in, ok := svc.Input(ctx, cache.ClusterKey{Namespace: "team-a", Name: "billing"})
	if !ok || in.HasEvents {
		t.Fatalf("a failed event list supplied as a snapshot (ok %v)", ok)
	}
	ov, err := svc.Overview(ctx, "team-a", "billing")
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceState(ov.Sources, cache.SourceEvents); got != viewmodel.SourceUnavailable {
		t.Fatalf("overview events source %q", got)
	}
}

// A failed pod list is a required source lost: the verdict says unknown
// for it, and no pod count is claimed.
func TestPodsListFailureMakesTheVerdictUnknown(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{list: func(l client.ObjectList) bool {
		_, ok := l.(*corev1.PodList)
		return ok
	}})
	ov, err := svc.Overview(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range ov.Verdict.Contributors {
		if c.ID == "source:"+cache.SourcePods && c.Health == viewmodel.HealthUnknown {
			found = true
		}
	}
	if !found {
		t.Fatalf("no unknown contributor for the failed pod list: %+v", ov.Verdict.Contributors)
	}
	if ov.Instances.PodsReady != nil {
		t.Fatalf("pods ready claimed from a failed list: %d", *ov.Instances.PodsReady)
	}
}

// A failed read of the raw Cluster view (the fork's autoGrow) marks that
// source unavailable on the storage screen, rather than reading as "no
// autoGrow".
func TestStorageAutoGrowReadFailure(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{get: func(_ client.ObjectKey, obj client.Object) bool {
		u, ok := obj.(*unstructured.Unstructured)
		return ok && u.GroupVersionKind().Kind == "Cluster"
	}})
	st, err := svc.Storage(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceState(st.Sources, cache.SourceClusterStorage); got != viewmodel.SourceUnavailable {
		t.Fatalf("clusterStorage source %q after a failed read", got)
	}
	ok, err := failingService(t, &failing{}).Storage(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceState(ok.Sources, cache.SourceClusterStorage); got == viewmodel.SourceUnavailable {
		t.Fatal("a successful read marked unavailable")
	}
}

// A failed ObjectStore read is "cannot tell", never "does not exist":
// the drill would otherwise be refused with a false ObjectStoreMissing.
func TestObjectStoreReadFailureIsNotMissing(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{get: func(_ client.ObjectKey, obj client.Object) bool {
		u, ok := obj.(*unstructured.Unstructured)
		return ok && u.GroupVersionKind() == cache.ObjectStoreGVK
	}})
	if _, exists, readable := svc.ObjectStore(ctx, "team-a", "store"); exists || readable {
		t.Fatalf("a failed read answered exists=%v readable=%v, want unreadable", exists, readable)
	}
	if _, exists, readable := failingService(t, &failing{}).ObjectStore(ctx, "team-a", "absent"); exists || !readable {
		t.Fatalf("NotFound answered exists=%v readable=%v, want a readable absence", exists, readable)
	}
}

// A failed Cluster read on the diagnostics and triage screens is an
// error, not the 404 of a Cluster that does not exist.
func TestDiagnosticsClusterReadFailureIsNotNotFound(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{get: func(_ client.ObjectKey, obj client.Object) bool {
		_, ok := obj.(*cnpgv1.Cluster)
		return ok
	}})
	for name, call := range map[string]func() error{
		"diagnostics": func() error { _, err := svc.Diagnostics(ctx, "team-a", "orders"); return err },
		"triage":      func() error { _, err := svc.Triage(ctx, "team-a", "orders"); return err },
	} {
		err := call()
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: a failed read answered %v, want an error other than not found", name, err)
		}
	}
	if _, err := failingService(t, &failing{}).Diagnostics(ctx, "team-a", "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing Cluster: %v", err)
	}
}

// A failed PgAdmin list is an error from PgAdminLinks, so the API can
// say the links were not read rather than offer none.
func TestPgAdminLinksListFailure(t *testing.T) {
	ctx := context.Background()
	svc := failingService(t, &failing{list: func(l client.ObjectList) bool {
		_, ok := l.(*pgtoolboxv1alpha1.PgAdminList)
		return ok
	}})
	if _, err := svc.PgAdminLinks(ctx, "team-a", "orders"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a failed list answered %v", err)
	}
	links, err := failingService(t, &failing{}).PgAdminLinks(ctx, "team-a", "orders")
	if err != nil || links == nil {
		t.Fatalf("a read list: %v %v", links, err)
	}
}

// A namespace whose Cluster list keeps failing is logged once when the
// failure starts and once when it ends, not on every read of the roster.
func TestKeysListFailureLogsTransitionsOnly(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	svc := failingService(t, &failing{list: func(l client.ObjectList) bool {
		_, ok := l.(*cnpgv1.ClusterList)
		return ok && fail.Load()
	}})
	var logs bytes.Buffer
	svc.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	count := func(msg string) int { return strings.Count(logs.String(), msg) }

	svc.Keys(ctx)
	fail.Store(true)
	for range 5 {
		svc.Keys(ctx)
	}
	if got := count("listing Clusters failed"); got != 1 {
		t.Fatalf("failure logged %d times over five failing reads, want once:\n%s", got, logs.String())
	}
	fail.Store(false)
	for range 3 {
		svc.Keys(ctx)
	}
	if got := count("listing Clusters works again"); got != 1 {
		t.Fatalf("recovery logged %d times, want once:\n%s", got, logs.String())
	}
	fail.Store(true)
	svc.Keys(ctx)
	if got := count("listing Clusters failed"); got != 2 {
		t.Fatalf("a second failure logged %d failures in all, want 2:\n%s", got, logs.String())
	}
}
