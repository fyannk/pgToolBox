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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

type fakeLogs struct{}

func (fakeLogs) Observations() []logstream.Observation { return nil }
func (fakeLogs) Unread() []logstream.Unread            { return nil }

// TestInputFeedsEveryReadableSource is the table of what the console
// feeds the diagnostics catalog: each input is claimed exactly when its
// source is readable, and the ones outside the console's Role never.
func TestInputFeedsEveryReadableSource(t *testing.T) {
	ctx := context.Background()
	type flags struct {
		cluster, pods, events, backups, poolers, quorum, lease, declared, catalogs, kube, logs, history, evidence, infra, quotas, poolerPods bool
	}
	read := func(in diagnose.Input) flags {
		return flags{
			cluster: in.HasCluster, pods: in.HasPods, events: in.HasEvents, backups: in.HasBackups, poolers: in.HasPoolers,
			quorum: in.HasFailoverQuorum, lease: in.HasPrimaryLease, declared: in.HasDatabaseObjects, catalogs: in.HasImageCatalogs,
			kube: in.HasKubeVersion, logs: in.Logs != nil, history: in.HasHistory, evidence: in.HasEvidence,
			infra: in.HasInfrastructure, quotas: in.HasQuotas, poolerPods: in.HasPoolerPods,
		}
	}
	tests := []struct {
		name    string
		cluster string
		states  map[string]viewmodel.SourceState
		want    flags
	}{
		{
			"everything readable, an evidence agent", "orders", nil,
			flags{cluster: true, pods: true, events: true, backups: true, poolers: true, quorum: true, lease: true, declared: true, catalogs: true, kube: true, logs: true, history: true, evidence: true},
		},
		{
			"no evidence agent for this Cluster", "billing", nil,
			flags{cluster: true, pods: true, events: true, backups: true, poolers: true, quorum: true, lease: true, declared: true, catalogs: true, kube: true, logs: true, history: true},
		},
		{
			"DatabaseRole not served (CRDs without it) still feeds declared objects", "orders",
			map[string]viewmodel.SourceState{cache.SourceDatabaseRoles: {Source: cache.SourceDatabaseRoles, State: viewmodel.SourceNotInstalled}},
			flags{cluster: true, pods: true, events: true, backups: true, poolers: true, quorum: true, lease: true, declared: true, catalogs: true, kube: true, logs: true, history: true, evidence: true},
		},
		{
			"a forbidden declared kind withholds the input", "orders",
			map[string]viewmodel.SourceState{cache.SourcePublications: {Source: cache.SourcePublications, State: viewmodel.SourceForbidden}},
			flags{cluster: true, pods: true, events: true, backups: true, poolers: true, quorum: true, lease: true, catalogs: true, kube: true, logs: true, history: true, evidence: true},
		},
		{
			"catalogs not readable", "orders",
			map[string]viewmodel.SourceState{cache.SourceImageCatalogs: {Source: cache.SourceImageCatalogs, State: viewmodel.SourceForbidden}},
			flags{cluster: true, pods: true, events: true, backups: true, poolers: true, quorum: true, lease: true, declared: true, kube: true, logs: true, history: true, evidence: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newScreenService(t)
			fc := svc.cache.(*fakeCache)
			for k, v := range tt.states {
				fc.states[k] = v
			}
			svc.opts.KubeVersion = func() (observe.KubeVersionSnapshot, bool) {
				return observe.KubeVersionSnapshot{ObservedAt: now, GitVersion: "v1.34.1"}, true
			}
			svc.opts.Logs = func(string, string) diagnose.LogObservations { return fakeLogs{} }
			in, ok := svc.Input(ctx, cache.ClusterKey{Namespace: "team-a", Name: tt.cluster})
			if !ok {
				t.Fatal("no input")
			}
			if got := read(in); got != tt.want {
				t.Errorf("fed %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestInputHistoryIsTheClustersOwn(t *testing.T) {
	svc := newScreenService(t)
	in, _ := svc.Input(context.Background(), cache.ClusterKey{Namespace: "team-a", Name: "orders"})
	if len(in.History.Entries) != 2 {
		t.Fatalf("history entries %d, want orders' Cluster and Backup", len(in.History.Entries))
	}
	for _, e := range in.History.Entries {
		if e.Name == "billing" {
			t.Error("billing's revision fed to orders' diagnostics")
		}
	}
}

func TestInputDeclaredAndCatalogFacts(t *testing.T) {
	svc := newScreenService(t)
	in, _ := svc.Input(context.Background(), cache.ClusterKey{Namespace: "team-a", Name: "orders"})
	if len(in.DatabaseObjects.Databases) != 2 {
		t.Errorf("databases %d", len(in.DatabaseObjects.Databases))
	}
	if in.ImageCatalogs.ClusterCatalogState != observe.ClusterCatalogDisabled {
		t.Errorf("cluster catalog state %s without the opt-in", in.ImageCatalogs.ClusterCatalogState)
	}
	if !in.Evidence.HasReport || in.Evidence.Snapshot.Report.Facts == nil || in.Evidence.Snapshot.Report.Facts.Format != "barman-cloud" {
		t.Errorf("evidence %+v", in.Evidence)
	}
}

// TestDiagnosticsWithEveryInput runs the catalog over orders with every
// input the console can feed and pins the checks that still cannot run:
// only those whose source is outside the console's Role may say so.
func TestDiagnosticsWithEveryInput(t *testing.T) {
	ctx := context.Background()
	svc := newScreenService(t)
	svc.opts.KubeVersion = func() (observe.KubeVersionSnapshot, bool) {
		return observe.KubeVersionSnapshot{ObservedAt: now, GitVersion: "v1.34.1"}, true
	}
	svc.opts.Logs = func(string, string) diagnose.LogObservations { return fakeLogs{} }
	// Real instance pods carry the operator's bootstrap init container,
	// whose image tag is the CNPG version the catalog's pins read.
	c := svc.cache.(*fakeCache).reader.(client.Client)
	for _, name := range []string{"orders-1", "orders-2", "orders-3"} {
		pod := &corev1.Pod{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: name}, pod); err != nil {
			t.Fatal(err)
		}
		pod.Spec.InitContainers = []corev1.Container{{Name: "bootstrap-controller", Image: "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0"}}
		if err := c.Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	// Restore drills on, with a persistent record that reaches back past
	// the 90-day window and holds no drill of orders.
	svc.SetDrills(&DrillsView{Records: drills.NewStore(longDrillRecord{}, nil, now), TimeLimit: time.Hour})
	runner := diag.New(svc, nil, diag.Options{Interval: time.Minute, Now: func() time.Time { return now }})
	svc.SetDiagnostics(runner)
	runner.RunAll(ctx)
	d, err := svc.Diagnostics(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "diagnostics-orders-fed", d)
	// Only inputs outside the console's Role may withhold a check, plus
	// what this fixture does not carry and production feeds: the sweep's
	// instance status and metrics, the PostgreSQL major the Cluster
	// status reports, the lease renewal time, the cluster timeline and
	// the plugin sidecar; and the release lines the built-in lifecycle
	// table leaves to the deployment (CloudNativePG, the plugin).
	allowed := []string{
		"volumes", "resource quotas", "pooler pods", "pooler metrics",
		"instance metrics are not scraped", "status reports have not been swept yet",
		"PostgreSQL version is not observed", "lease carries no renewal time", "no cluster timeline",
		"Barman Cloud plugin version is not observed", "the lifecycle table lists no",
	}
	for _, c := range d.Checks {
		if c.Outcome != viewmodel.CheckUnavailable {
			continue
		}
		ok := false
		for _, a := range allowed {
			if strings.Contains(c.Because, a) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("check %s could not run: %s", c.Name, c.Because)
		}
	}
}

// longDrillRecord is a persistent drill record, empty, kept for 200 days.
type longDrillRecord struct{}

func (longDrillRecord) LoadDrills() ([][]byte, error) { return nil, nil }
func (longDrillRecord) PutDrill(string, []byte) error { return nil }
func (longDrillRecord) DeleteDrill(string) error      { return nil }
func (longDrillRecord) DrillsSince(now time.Time) (time.Time, error) {
	return now.Add(-200 * 24 * time.Hour), nil
}
