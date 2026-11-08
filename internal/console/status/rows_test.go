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
	"sync"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// genCache is fakeCache plus the change stamps of cache.Manager.
type genCache struct {
	*fakeCache
	mu   sync.Mutex
	gens map[cache.ClusterKey]uint64
}

func (g *genCache) Generation(key cache.ClusterKey) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gens[key]
}

func (g *genCache) bump(key cache.ClusterKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gens[key]++
}

func TestListRowIsReusedUntilItsClusterChanges(t *testing.T) {
	ctx := context.Background()
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects()...).WithStatusSubresource(&cnpgv1.Cluster{})
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	c := builder.Build()
	gc := &genCache{fakeCache: &fakeCache{reader: c, namespaces: []string{"team-a"}}, gens: map[cache.ClusterKey]uint64{}}
	clock := now
	svc := New(Options{Cache: gc, Sweep: fakeSweep{snaps: sweepSnaps()}, Now: func() time.Time { return clock }})

	phaseOf := func(name string) string {
		t.Helper()
		list, err := svc.ClusterList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range list.Items {
			if row.Ref.Name == name {
				return row.Phase.Text
			}
		}
		t.Fatalf("%s not listed", name)
		return ""
	}
	setPhase := func(phase string) {
		t.Helper()
		cluster := &cnpgv1.Cluster{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "orders"}, cluster); err != nil {
			t.Fatal(err)
		}
		cluster.Status.Phase = phase
		if err := c.Status().Update(ctx, cluster, &client.SubResourceUpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	before := phaseOf("orders")
	setPhase("Switchover in progress")
	if got := phaseOf("orders"); got != before {
		t.Fatalf("phase = %q with no change announced; want the memoized %q", got, before)
	}
	gc.bump(cache.ClusterKey{Namespace: "team-a", Name: "orders"})
	if got := phaseOf("orders"); got != "Switchover in progress" {
		t.Fatalf("phase = %q after the change was announced", got)
	}

	setPhase("Cluster in healthy state")
	clock = clock.Add(rowMaxAge)
	if got := phaseOf("orders"); got != "Cluster in healthy state" {
		t.Fatalf("phase = %q after %s; want a rebuilt row", got, rowMaxAge)
	}
}

// TestListRowCarriesTheEncodingOfItsContent: a memoized row carries its
// JSON, which is always the encoding of the row it travels with (built,
// reused, rebuilt after a change), and it leaves the memo with its
// Cluster. A cache without change stamps memoizes nothing and attaches
// nothing.
func TestListRowCarriesTheEncodingOfItsContent(t *testing.T) {
	ctx := context.Background()
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects()...).WithStatusSubresource(&cnpgv1.Cluster{})
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	c := builder.Build()
	fc := &fakeCache{reader: c, namespaces: []string{"team-a"}}
	gc := &genCache{fakeCache: fc, gens: map[cache.ClusterKey]uint64{}}
	orders := cache.ClusterKey{Namespace: "team-a", Name: "orders"}

	list := func(svc *Service) []viewmodel.ClusterRow {
		t.Helper()
		l, err := svc.ClusterList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return l.Items
	}
	check := func(rows []viewmodel.ClusterRow, wantEncoded bool) {
		t.Helper()
		if len(rows) == 0 {
			t.Fatal("no rows")
		}
		for _, row := range rows {
			got := row.EncodedJSON()
			if !wantEncoded {
				if got != nil {
					t.Errorf("%s carries an encoding without a memo", row.Ref.Name)
				}
				continue
			}
			var want bytes.Buffer
			if err := viewmodel.AppendJSON(&want, row); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Errorf("%s carries\n%s\nwant the encoding of its content\n%s", row.Ref.Name, got, want.Bytes())
			}
		}
	}

	svc := New(Options{Cache: gc, Sweep: fakeSweep{snaps: sweepSnaps()}, Now: func() time.Time { return now }})
	check(list(svc), true) // built
	check(list(svc), true) // reused
	cluster := &cnpgv1.Cluster{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "orders"}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Status.Phase = "Switchover in progress <now> & \u2028"
	if err := c.Status().Update(ctx, cluster, &client.SubResourceUpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	gc.bump(orders)
	rows := list(svc) // rebuilt
	check(rows, true)
	for _, row := range rows {
		if row.Ref.Name == "orders" && !bytes.Contains(row.EncodedJSON(), []byte("Switchover in progress <now> &")) {
			t.Fatalf("the rebuilt row's encoding is the old one: %s", row.EncodedJSON())
		}
	}

	if err := c.Delete(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	check(list(svc), true)
	svc.rows.mu.Lock()
	_, kept := svc.rows.rows[orders]
	n := len(svc.rows.rows)
	svc.rows.mu.Unlock()
	if kept || n != 1 {
		t.Fatalf("memo after the deletion: orders kept = %v, %d entries; want only billing", kept, n)
	}

	plain := New(Options{Cache: fc, Sweep: fakeSweep{snaps: sweepSnaps()}, Now: func() time.Time { return now }})
	check(list(plain), false)
}
