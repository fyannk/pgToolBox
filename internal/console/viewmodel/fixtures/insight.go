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

package fixtures

import (
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fyannk/pgToolBox/internal/console/insightview"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
)

// The insight scenario of team-a/orders is scripted through the real
// tracker and the real conversion, so the fixture is what the console
// would draw after 26 hours of sweeps: transaction ids consumed at
// 1,500/s, app's age 1.7 times its anti-wraparound threshold with an
// anti-wraparound autovacuum running and an abandoned replication slot
// holding the xmin horizon just behind it, a table autovacuum is not keeping
// up with, a session idle in a transaction blocking a chain of two,
// 172 of 200 connections in use, a statement whose mean time tripled
// since yesterday, and an unused, an invalid and a duplicate index.
// team-a/billing has not opted in.

const sweepInterval = 30 * time.Second

func f(v float64) *float64 { return &v }

func insightCluster(name string, uid types.UID, optedIn bool) *cnpgv1.Cluster {
	c := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, UID: uid}}
	if optedIn {
		c.Spec.Monitoring = &cnpgv1.MonitoringConfiguration{CustomQueriesConfigMap: []machineryapi.ConfigMapKeySelector{
			{LocalObjectReference: machineryapi.LocalObjectReference{Name: insight.ConfigMapName}, Key: insight.ConfigMapKey},
		}}
	}
	return c
}

var ordersSettings = map[string]float64{
	"max_connections": 200, "superuser_reserved_connections": 3, "reserved_connections": 0,
	"autovacuum": 1, "track_counts": 1, "autovacuum_max_workers": 3, "autovacuum_naptime": 60,
	"autovacuum_freeze_max_age": 2e8, "autovacuum_multixact_freeze_max_age": 4e8,
	"vacuum_failsafe_age": 1.6e9, "vacuum_multixact_failsafe_age": 1.6e9,
	"autovacuum_vacuum_threshold": 50, "autovacuum_vacuum_scale_factor": 0.2,
	"idle_in_transaction_session_timeout": 0,
}

// ordersPrimary is the primary's reading at t, minutes before Now.
func ordersPrimary(t time.Time) insight.Reading {
	elapsed := t.Sub(Now).Seconds() // negative before Now
	xid := 9.6e9 + 1500*elapsed
	exporter := t.Add(-12 * time.Second)
	// events gains 40,000 dead rows a minute with no autovacuum
	// completing: notStarted.
	dead := 2.4e6 + 40000*elapsed/60
	statsReset := float64(Now.Add(-40 * 24 * time.Hour).Unix())
	r := insight.Reading{
		Version: "1", Settings: ordersSettings, NextXID: f(xid),
		Databases: []insight.Database{
			{Name: "app", XIDAge: f(3.4e8 + 1500*elapsed), MXIDAge: f(1.2e7), MinMXID: f(1), StatsResetUnix: &statsReset, AllowConnections: f(1)},
			{Name: "postgres", XIDAge: f(4.1e7), MXIDAge: f(1.2e7), MinMXID: f(1), AllowConnections: f(1)},
			{Name: "template0", XIDAge: f(1.9e8), MXIDAge: f(1.2e7), MinMXID: f(1), AllowConnections: f(0)},
		},
		Tables: []insight.Table{
			{
				Database: "app", Schema: "public", Name: "orders_archive", Kind: "table", XIDAge: f(3.4e8), MXIDAge: f(1.2e7), FreezeMaxAge: f(2e8),
				LiveTuples: f(8.1e7), Reltuples: f(8.1e7), DeadTuples: f(1200), AutovacuumThreshold: f(1.62e7 + 50), SinceVacuum: f(86400 * 9), SinceAutovacuum: f(86400 * 9),
				SinceAnalyze: f(86400 * 2), AutovacuumCount: f(14), AutovacuumEnabled: f(1),
			},
			{
				Database: "app", Schema: "public", Name: "events", Kind: "table", XIDAge: f(1.1e8), MXIDAge: f(1.2e7), FreezeMaxAge: f(2e8),
				LiveTuples: f(9.5e6), Reltuples: f(9.5e6), DeadTuples: f(dead), AutovacuumThreshold: f(1.9e6 + 50), SinceVacuum: f(7200), SinceAutovacuum: f(7200),
				SinceAnalyze: f(3600), AutovacuumCount: f(311), AutovacuumEnabled: f(1),
			},
			{
				Database: "app", Schema: "audit", Name: "trail", Kind: "table", XIDAge: f(2.9e8), MXIDAge: f(1.2e7), FreezeMaxAge: f(1e8),
				LiveTuples: f(2.2e7), Reltuples: f(2.2e7), DeadTuples: f(5.3e6), AutovacuumThreshold: f(4.4e6 + 50), AutovacuumEnabled: f(0),
			},
			{
				Database: "app", Schema: "pg_toast", Name: "pg_toast_16412", Kind: "toast", XIDAge: f(3.3e8), MXIDAge: f(1.2e7), FreezeMaxAge: f(2e8),
				LiveTuples: f(4.4e5), Reltuples: f(4.4e5), DeadTuples: f(0), AutovacuumThreshold: f(88050), AutovacuumEnabled: f(1),
			},
		},
		Vacuums: []insight.VacuumProgress{{
			Database: "app", Schema: "public", Table: "orders_archive", Phase: "scanning heap",
			Autovacuum: f(1), Wraparound: f(1), RunningSeconds: f(1260), HeapBlksTotal: f(1.04e6), HeapBlksScanned: f(4.1e5),
			HeapBlksVacuumed: f(0), IndexVacuumCount: f(0),
		}},
		Sessions: []insight.Session{
			{
				PID: 4711, Database: "app", User: "billing", Application: "billing-api", State: "idle in transaction",
				WaitEventType: "Client", WaitEvent: "ClientRead", BackendType: "client backend", QueryID: "-6311483212785534117",
				XactAgeSeconds: f(2280), StateAgeSeconds: f(2275), BlockingCount: f(1),
			},
			{
				PID: 5120, BlockedBy: []int64{4711}, Database: "app", User: "migrator", Application: "flyway", State: "active",
				WaitEventType: "Lock", WaitEvent: "relation", BackendType: "client backend", QueryID: "3305271982216418801",
				WaitSeconds: f(610), XactAgeSeconds: f(611), StateAgeSeconds: f(610), BlockingCount: f(1),
			},
			{
				PID: 5133, BlockedBy: []int64{5120}, Database: "app", User: "orders", Application: "orders-api", State: "active",
				WaitEventType: "Lock", WaitEvent: "relation", BackendType: "client backend", QueryID: "812734598812374661",
				WaitSeconds: f(598), XactAgeSeconds: f(598), StateAgeSeconds: f(598), BlockingCount: f(0),
			},
		},
		Backends: &insight.Backends{
			Client: f(172), Active: f(19), IdleInTransaction: f(1), IdleInTransactionMaxSeconds: f(2275),
			LockWaiting: f(2), MaxXactAgeSeconds: f(2280),
		},
		// What holds the xmin horizon: a physical slot left behind by a
		// replica that no longer streams kept its xmin 2M transactions
		// after app's oldest frozen id, so the anti-wraparound autovacuum
		// cannot advance app's age past it; the idle-in-transaction
		// session (2,280 s at 1,500/s) holds 3.42M.
		Horizons: &insight.Horizons{
			SlotXminAge:    f(3.38e8 + 1500*elapsed),
			BackendXminAge: f(3.42e6),
			BackendXIDAge:  f(3.42e6),
		},
		Connections: []insight.ConnectionGroup{
			{Database: "app", Application: "orders-api", State: "idle", Sessions: f(96), MaxStateAgeSeconds: f(3400)},
			{Database: "app", Application: "billing-api", State: "idle", Sessions: f(48), MaxStateAgeSeconds: f(5100)},
			{Database: "app", Application: "orders-api", State: "active", Sessions: f(17), MaxStateAgeSeconds: f(598)},
			{Database: "app", Application: "reporting", State: "idle", Sessions: f(8), MaxStateAgeSeconds: f(86000)},
			{Database: "app", Application: "billing-api", State: "idle in transaction", Sessions: f(1), MaxStateAgeSeconds: f(2275)},
			{Database: "app", Application: "flyway", State: "active", Sessions: f(1), MaxStateAgeSeconds: f(610)},
			{Database: "postgres", Application: "psql", State: "active", Sessions: f(1), MaxStateAgeSeconds: f(2)},
		},
		StatementsInfo: &insight.StatementsInfo{Dealloc: f(0), StatsResetUnix: &statsReset, Max: f(5000)},
		Indexes: []insight.Index{
			{Database: "app", Schema: "public", Table: "events", Name: "events_payload_gin", Kind: "unused", SizeBytes: f(3.1e9), Scans: f(0), Unique: f(0)},
			{Database: "app", Schema: "public", Table: "orders", Name: "orders_note_trgm", Kind: "unused", SizeBytes: f(4.2e8), Scans: f(0), Unique: f(0)},
			{Database: "app", Schema: "public", Table: "orders", Name: "orders_customer_idx_ccnew", Kind: "invalid", SizeBytes: f(9.1e8), Scans: f(0), Unique: f(0)},
			{Database: "app", Schema: "public", Table: "orders", Name: "orders_customer_id_idx1", Kind: "duplicate", DuplicateOf: "orders_customer_id_idx", SizeBytes: f(9.0e8), Scans: f(12), Unique: f(0)},
		},
		ExporterUpdatedAt: &exporter,
	}
	// Statements, cumulative: lookup is steady at 1.2 ms; refresh_totals
	// went from 40 ms to 130 ms per call in the last hour; report is
	// slow and steady.
	hours := elapsed / 3600
	steady := func(id, role string, perHour, mean float64) insight.Statement {
		calls := 5e6 + perHour*hours
		return insight.Statement{
			QueryID: id, Database: "app", Role: role, Calls: f(calls), TotalMillis: f(calls * mean),
			MeanMillis: f(mean), Rows: f(calls), SharedBlksHit: f(calls * 40), SharedBlksRead: f(calls * 0.3),
		}
	}
	lookup := steady("812734598812374661", "orders", 3.6e5, 1.2)
	report := steady("-1840029318271940210", "reporting", 24, 41000)
	report.SharedBlksRead = f(*report.Calls * 9000)
	calls := 2e5 + 600*hours
	total := calls * 40
	if t.After(Now.Add(-time.Hour)) {
		total += (t.Sub(Now.Add(-time.Hour)).Hours() * 600) * 90
	}
	refresh := insight.Statement{
		QueryID: "7766023351299184043", Database: "app", Role: "billing", Calls: f(calls),
		TotalMillis: f(total), MeanMillis: f(total / calls), Rows: f(calls * 12), SharedBlksHit: f(calls * 900), SharedBlksRead: f(calls * 30),
	}
	r.Statements = []insight.Statement{report, lookup, refresh}
	return r
}

func ordersReplica(at time.Time) insight.Reading {
	exporter := at.Add(-20 * time.Second)
	return insight.Reading{
		Version: "1", Settings: ordersSettings,
		Backends: &insight.Backends{Client: f(31), Active: f(4), IdleInTransaction: f(0), LockWaiting: f(0), MaxXactAgeSeconds: f(12)},
		Connections: []insight.ConnectionGroup{
			{Database: "app", Application: "reporting", State: "idle", Sessions: f(27), MaxStateAgeSeconds: f(9000)},
			{Database: "app", Application: "reporting", State: "active", Sessions: f(4), MaxStateAgeSeconds: f(12)},
		},
		ExporterUpdatedAt: &exporter,
	}
}

func ordersSnapshot() insight.Snapshot {
	tr := insight.NewTracker()
	// Hourly sweeps for the first day, then every minute for the last
	// two hours: what the tracker keeps is the same as from sweeps every
	// 30 seconds.
	for at := Now.Add(-26 * time.Hour); !at.After(Now); {
		tr.Observe("orders-1", at, ordersPrimary(at), true)
		if at.Before(Now.Add(-2 * time.Hour)) {
			at = at.Add(time.Hour)
		} else {
			at = at.Add(time.Minute)
		}
	}
	tr.Observe("orders-2", Now.Add(-4*time.Second), ordersReplica(Now.Add(-4*time.Second)), false)
	tr.Observe("orders-3", Now.Add(-4*time.Second), ordersReplica(Now.Add(-4*time.Second)), false)
	return tr.Snapshot(Now)
}

func billingSnapshot() insight.Snapshot {
	tr := insight.NewTracker()
	tr.Observe("billing-1", Now.Add(-3*time.Second), insight.Reading{}, true)
	tr.Observe("billing-2", Now.Add(-3*time.Second), insight.Reading{}, false)
	return tr.Snapshot(Now)
}

// Insight is orders' insight screens.
func Insight() viewmodel.Insight {
	v := insightview.Build(insightCluster("orders", types.UID(ordersRef.UID), true), ordersSnapshot(), true, sweepInterval, Now)
	v.Sources = []viewmodel.SourceState{src("insight", since(3*time.Hour))}
	return v
}

// InsightNotEnabled is billing's: the Cluster has not opted in.
func InsightNotEnabled() viewmodel.Insight {
	v := insightview.Build(insightCluster("billing", "4d2f1e0a-1b2c-4d3e-8f90-a1b2c3d4e5f6", false), billingSnapshot(), true, sweepInterval, Now)
	v.Sources = []viewmodel.SourceState{src("insight", since(3*time.Hour))}
	return v
}

// InsightBlocking is orders' lock graph after an on-demand read.
func InsightBlocking() viewmodel.InsightBlocking {
	v := Insight().Blocking
	v.RefreshedAt = ago(0)
	return v
}

// FleetStatementsOf is the team view over those of orders and billing
// that visible keeps.
func FleetStatementsOf(visible func([]viewmodel.ClusterRef) []viewmodel.ClusterRef) viewmodel.FleetStatements {
	billingRef := viewmodel.ClusterRef{Namespace: "team-a", Name: "billing", UID: "4d2f1e0a-1b2c-4d3e-8f90-a1b2c3d4e5f6"}
	snaps := map[string]insight.Snapshot{ordersRef.Name: ordersSnapshot(), billingRef.Name: billingSnapshot()}
	kept := visible([]viewmodel.ClusterRef{ordersRef, billingRef})
	clusters := make([]insightview.FleetCluster, 0, len(kept))
	for _, ref := range kept {
		clusters = append(clusters, insightview.FleetCluster{Ref: ref, Snapshot: snaps[ref.Name], Has: true, Interval: sweepInterval})
	}
	return insightview.Fleet(clusters, Now)
}
