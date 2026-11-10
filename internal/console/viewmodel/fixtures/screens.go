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

	"github.com/fyannk/pgToolBox/internal/console/topology"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The screen scenarios are built on
// team-a/orders: three instances, plugin-based backups, an evidence
// agent. Each one carries at least one honesty case (a stale, an
// unknown, a not-reported value) so the UI has a fixture for it.

var ordersRef = viewmodel.ClusterRef{Namespace: "team-a", Name: "orders", UID: "0b6a2c1e-8f0d-4a55-9a3e-7c1f2b3d4e5f"}

func since(d time.Duration) viewmodel.SourceState {
	return viewmodel.SourceState{State: viewmodel.SourceLive, Since: ago(d)}
}

func src(name string, s viewmodel.SourceState) viewmodel.SourceState {
	s.Source = name
	return s
}

func consoleProv(source string) viewmodel.Provenance {
	return viewmodel.Provenance{Origin: viewmodel.OriginConsole, Source: source, ObservedAt: ago(2 * time.Second)}
}

func kubeProv(source string) viewmodel.Provenance {
	return viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: source, ObservedAt: ago(2 * time.Second)}
}

func evidenceProv(source string) viewmodel.Provenance {
	return viewmodel.Provenance{Origin: viewmodel.OriginEvidence, Source: source, ObservedAt: ago(40 * time.Second)}
}

// Replication is orders' replication: two streaming standbys, one
// quorum-synchronous; one replica's /pg/status could not be verified.
func Replication() viewmodel.Replication {
	return viewmodel.Replication{
		Ref: ordersRef, GeneratedAt: Now,
		Primary: viewmodel.PrimaryElection{
			PrimarySummary: viewmodel.PrimarySummary{Current: "orders-1", Target: "orders-1", State: viewmodel.PrimaryStable},
			LivePrimaries:  []string{"orders-1"}, LabelPrimaries: []string{"orders-1"},
			Lease:      &viewmodel.Lease{Holder: "orders-1", RenewedAt: ago(4 * time.Second), Provenance: kubeProv("Lease.spec")},
			Provenance: operatorProv("Cluster.status.currentPrimary"),
		},
		Synchronous: viewmodel.SyncConfig{
			Mode: "explicit", Method: "any", Number: new(1), DataDurability: "required", FailoverQuorum: true,
			Provenance: operatorProv("Cluster.spec.postgresql.synchronous"),
		},
		Quorum: &viewmodel.QuorumView{
			Health: viewmodel.HealthOK, Method: "ANY", Number: new(1), StandbyNames: []string{"orders-2", "orders-3"}, Primary: "orders-1",
			Text: "the primary applied ANY 1 (orders-2, orders-3)", Provenance: operatorProv("FailoverQuorum.status"),
		},
		Standbys: []viewmodel.StandbyRow{
			{
				Name: "orders-2", State: "streaming", SyncState: "quorum", SyncPriority: "1", Synchronous: true,
				SentLSN: "0/9000148", WriteLSN: "0/9000148", FlushLSN: "0/9000148", ReplayLSN: "0/9000148",
				WriteLag: "00:00:00.000412", FlushLag: "00:00:00.000903", ReplayLag: "00:00:00.001021",
				LagBytes: new(int64(0)), Health: viewmodel.HealthOK, Provenance: instanceProv(),
			},
			{
				Name: "orders-3", State: "catchup", SyncState: "potential", SyncPriority: "1",
				SentLSN: "0/8F00000", ReplayLSN: "0/8E00000", LagBytes: new(int64(18874696)),
				Health: viewmodel.HealthProgressing, Provenance: instanceProv(),
			},
		},
		Receivers: []viewmodel.ReceiverRow{
			{
				Name: "orders-2", Health: viewmodel.HealthOK, StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadOK, At: ago(8 * time.Second)},
				ReceivedLSN: "0/9000148", ReplayLSN: "0/9000148", LagBytes: new(int64(0)), WALReceiverActive: new(true), TimelineID: new(3),
				Provenance: instanceProv(),
			},
			{
				Name: "orders-3", Health: viewmodel.HealthUnknown,
				StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadNotVerifiable, Reason: "status port not verifiable: CA not in the console's bundle", Since: ago(20 * time.Minute)},
				Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status"},
			},
		},
		Failover: OrdersFailover(),
		Slots: []viewmodel.SlotRow{
			{Instance: "orders-1", Name: "_cnpg_orders_2", Type: "physical", WALStatus: "reserved", Active: true, Health: viewmodel.HealthOK, Provenance: instanceProv()},
			{Instance: "orders-1", Name: "debezium", Type: "logical", Database: "app", Plugin: "pgoutput", WALStatus: "extended", Active: false, Health: viewmodel.HealthWarning, Provenance: instanceProv()},
		},
		Sources:    []viewmodel.SourceState{src("clusters", since(3*time.Hour)), src("instanceStatus", since(3*time.Hour)), src("failoverQuorums", since(3*time.Hour)), src("leases", since(3*time.Hour))},
		BlindSpots: []viewmodel.BlindSpot{replicationBlindSpot, {ID: "replication.syncNamesNotReported", Panel: "replication", Text: "synchronous_standby_names is not reported by the instance manager; FailoverQuorum.status is the only account of the set PostgreSQL applied."}},
	}
}

// Backups is orders' backups: plugin-based, one completed, one running,
// one failed; one schedule; the evidence agent live.
func Backups() viewmodel.Backups {
	return viewmodel.Backups{
		Ref: ordersRef, GeneratedAt: Now,
		Summary: viewmodel.BackupSummary{
			BackupRecency: viewmodel.BackupRecency{Health: viewmodel.HealthOK, LastCompletedAt: ago(5 * time.Hour), Configured: true, PluginBased: true, Provenance: operatorProv("Backup.status")},
			Archiving:     viewmodel.Archiving{Health: viewmodel.HealthOK, ConditionStatus: "True", Reason: "ContinuousArchivingSuccess", LiveArchiving: new(true), ReadyWALFiles: new(0), Provenance: operatorProv("Cluster.status.conditions[ContinuousArchiving]")},
			RecoveryWindow: &viewmodel.RecoveryWindow{
				First: ago(7 * 24 * time.Hour), Last: ago(30 * time.Second),
				Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "ObjectStore.status.serverRecoveryWindow[orders]", ObservedAt: ago(2 * time.Second)},
			},
			NextScheduledAt: new(Now.Add(19 * time.Hour)),
			NotReported:     []string{"status.firstRecoverabilityPoint", "status.lastSuccessfulBackup", "metric:cnpg_collector_last_available_backup_timestamp"},
		},
		Method: viewmodel.BackupMethod{Configured: true, PluginBased: true, PluginName: "barman-cloud.cloudnative-pg.io", ObjectStore: "orders-store", Provenance: operatorProv("Cluster.spec.plugins")},
		Items: []viewmodel.BackupRow{
			{
				Name: "ondemand-20261029115500-x7k2p", Phase: "started", Health: viewmodel.HealthProgressing, Known: true, Method: "plugin",
				PluginName: "barman-cloud.cloudnative-pg.io", CreatedAt: ago(5 * time.Minute),
				NotReportedWhileRunning: []string{"status.startedAt", "status.stoppedAt", "status.backupId"},
				Repository:              viewmodel.RepositoryUnknown, Provenance: operatorProv("Backup.status"),
			},
			{
				Name: "orders-daily-20261029070000", Phase: "completed", Health: viewmodel.HealthOK, Known: true, Terminal: true, Method: "plugin",
				PluginName: "barman-cloud.cloudnative-pg.io", Instance: "orders-2", Online: new(true), BackupID: "20261029T070001",
				CreatedAt: ago(5 * time.Hour), StartedAt: ago(5 * time.Hour), StoppedAt: ago(4*time.Hour + 52*time.Minute), Schedule: "orders-daily",
				Repository: viewmodel.RepositoryPresent, Provenance: operatorProv("Backup.status"),
			},
			{
				Name: "orders-daily-20261028070000", Phase: "failed", Health: viewmodel.HealthCritical, Known: true, Terminal: true, Method: "plugin",
				PluginName: "barman-cloud.cloudnative-pg.io", Error: "rpc error: code = Unknown desc = missing WAL file 000000030000000000000007", CreatedAt: ago(29 * time.Hour),
				Schedule: "orders-daily", Repository: viewmodel.RepositoryNotChecked, Provenance: operatorProv("Backup.status"),
			},
		},
		Schedules: []viewmodel.ScheduleRow{{
			Name: "orders-daily", Schedule: "0 0 7 * * *", State: "active", Health: viewmodel.HealthOK, Method: "plugin", Target: "prefer-standby",
			LastScheduledAt: ago(5 * time.Hour), NextScheduledAt: new(Now.Add(19 * time.Hour)), Provenance: operatorProv("ScheduledBackup.status"),
		}},
		Evidence: viewmodel.EvidenceCheck{
			State: viewmodel.EvidenceLive, Health: viewmodel.HealthOK, Agent: "orders-evidence",
			Repository: &viewmodel.RepositoryEvidence{
				Completeness: "complete",
				Overall:      viewmodel.EvidenceFact{State: "healthy", Health: viewmodel.HealthOK},
				WAL:          viewmodel.EvidenceFact{State: "healthy", Health: viewmodel.HealthOK},
				Timeline:     viewmodel.EvidenceFact{State: "healthy", Health: viewmodel.HealthOK},
				Coverage:     viewmodel.EvidenceFact{State: "warning", Health: viewmodel.HealthWarning, Code: "coverage_gap", Message: "one WAL gap on timeline 2 before the oldest usable backup"},
				Retention:    viewmodel.EvidenceFact{State: "unknown", Health: viewmodel.HealthUnknown, Code: "policy_not_configured", Message: "no retention policy is configured on the ObjectStore"},
				Backups:      new(int64(8)), StructurallyUsable: new(int64(7)), RepositoryOnly: new(6),
				OldestCompletionAt: ago(7 * 24 * time.Hour), NewestCompletionAt: ago(4*time.Hour + 52*time.Minute), LatestArchiveReceiptAt: ago(45 * time.Second),
				CompletedAt: ago(40 * time.Second),
			},
			Provenance: evidenceProv("EvidenceAgent orders-evidence /api/v1alpha1/snapshot"),
		},
		RecoveryTimeline: restoreTested(RecoveryTimeline()),
		Drills:           RestoreDrills(),
		Sources:          []viewmodel.SourceState{src("backups", since(3*time.Hour)), src("scheduledBackups", since(3*time.Hour)), src("objectStores", since(3*time.Hour)), src("evidence", since(time.Hour))},
		BlindSpots:       []viewmodel.BlindSpot{pluginBackupBlindSpot},
	}
}

// restoreTested marks a timeline whose Cluster passed a drill recently,
// as the Backups screen does from its drill record.
func restoreTested(t viewmodel.RecoveryTimeline) viewmodel.RecoveryTimeline {
	t.RestoreTested = true
	return t
}

// RestoreDrills is orders' drill record: one passed yesterday (14 min,
// 182 GiB) and one that failed its size check the week before.
func RestoreDrills() viewmodel.RestoreDrills {
	drillRef := func(name string) viewmodel.ClusterRef { return viewmodel.ClusterRef{Namespace: "team-a", Name: name} }
	check := func(n viewmodel.DrillCheckName, o viewmodel.DrillCheckOutcome, text, source string) viewmodel.DrillCheck {
		return viewmodel.DrillCheck{Name: n, Outcome: o, Text: text, Source: source}
	}
	const (
		phaseSrc = "Cluster.status.phase, Cluster.status.readyInstances"
		pgSrc    = "/pg/status (isPrimary, timeLineID)"
		sizeSrc  = "exporter cnpg_pg_database_size_bytes, sum over databases"
	)
	return viewmodel.RestoreDrills{
		Enabled: true, Namespace: "team-a", TimeLimitSeconds: 7200, SizeTolerancePercent: 10,
		LastPassedAt: ago(23*time.Hour + 46*time.Minute), RestoreTested: true, WindowDays: 90,
		Items: []viewmodel.RestoreDrill{
			{
				Drill: drillRef("drill-orders-2610281200"), Source: ordersRef, Backup: "orders-daily-20261028070000", BackupStoppedAt: ago(28*time.Hour + 52*time.Minute),
				StartedAt: Now.Add(-24 * time.Hour), Deadline: Now.Add(-22 * time.Hour), HealthyAt: ago(23*time.Hour + 46*time.Minute),
				FinishedAt: ago(23*time.Hour + 46*time.Minute), TornDownAt: ago(23*time.Hour + 45*time.Minute), DurationSeconds: new(int64(14 * 60)),
				RestoredBytes: new(int64(182 << 30)), SourceBytes: new(int64(185 << 30)), Timeline: new(4),
				Result: viewmodel.DrillPassed, Health: viewmodel.HealthOK,
				Checks: []viewmodel.DrillCheck{
					check(viewmodel.DrillCheckHealthy, viewmodel.DrillCheckPassed, "healthy after 14m", phaseSrc),
					check(viewmodel.DrillCheckPromoted, viewmodel.DrillCheckPassed, "recovery ended; primary on timeline 4", pgSrc),
					check(viewmodel.DrillCheckSize, viewmodel.DrillCheckPassed, "restored 182.0 GiB, source 185.0 GiB at the start (tolerance ±10%)", sizeSrc),
				},
			},
			{
				Drill: drillRef("drill-orders-2610211200"), Source: ordersRef, Backup: "orders-daily-20261021070000",
				StartedAt: Now.Add(-8 * 24 * time.Hour), Deadline: Now.Add(-8*24*time.Hour + 2*time.Hour), HealthyAt: ago(8*24*time.Hour - 16*time.Minute),
				FinishedAt: ago(8*24*time.Hour - 17*time.Minute), TornDownAt: ago(8*24*time.Hour - 17*time.Minute), DurationSeconds: new(int64(16 * 60)),
				RestoredBytes: new(int64(120 << 30)), SourceBytes: new(int64(178 << 30)), Timeline: new(3),
				Result: viewmodel.DrillFailed, Health: viewmodel.HealthCritical,
				Reason: "size: restored 120.0 GiB, source 178.0 GiB at the start (tolerance ±10%)",
				Checks: []viewmodel.DrillCheck{
					check(viewmodel.DrillCheckHealthy, viewmodel.DrillCheckPassed, "healthy after 16m", phaseSrc),
					check(viewmodel.DrillCheckPromoted, viewmodel.DrillCheckPassed, "recovery ended; primary on timeline 3", pgSrc),
					check(viewmodel.DrillCheckSize, viewmodel.DrillCheckFailed, "restored 120.0 GiB, source 178.0 GiB at the start (tolerance ±10%)", sizeSrc),
				},
			},
		},
		Scope: "kept in this console's persistent history (bounded to the 50 newest drills per Cluster)",
		NotChecked: []string{
			"the content of the data: no query is run (the console runs no SQL)",
			"each database separately: the size check compares the sum over all databases",
			"a recovery to a point in time other than the end of the archive",
			"WAL written after the last archived segment",
			"the replicas, poolers and applications of the source",
		},
	}
}

// Storage is orders' storage: data and WAL volumes, one resizing, no
// usage reported (upstream CNPG).
func Storage() viewmodel.Storage {
	pvc := func(name, instance, role, group string, h viewmodel.Health, resizing bool) viewmodel.VolumeRow {
		return viewmodel.VolumeRow{
			Name: name, Instance: instance, Role: role, Group: group, Health: h, Phase: "Bound", Capacity: "20Gi", Requested: "20Gi",
			StorageClass: "fast-ssd", Resizing: resizing, Provenance: kubeProv("PersistentVolumeClaim"),
		}
	}
	resizing := pvc("orders-3", "orders-3", "PG_DATA", "resizing", viewmodel.HealthProgressing, true)
	resizing.Requested, resizing.GroupText = "40Gi", "being resized"
	return viewmodel.Storage{
		Ref: ordersRef, GeneratedAt: Now,
		Requests: []viewmodel.VolumeSpec{
			{Role: "PG_DATA", Size: "40Gi", StorageClass: "fast-ssd", ResizeInUseVolumes: new(true), Provenance: operatorProv("Cluster.spec.storage")},
			{Role: "PG_WAL", Size: "5Gi", StorageClass: "fast-ssd", Provenance: operatorProv("Cluster.spec.walStorage")},
		},
		Volumes: []viewmodel.VolumeRow{
			pvc("orders-1", "orders-1", "PG_DATA", "healthy", viewmodel.HealthOK, false),
			pvc("orders-1-wal", "orders-1", "PG_WAL", "healthy", viewmodel.HealthOK, false),
			pvc("orders-2", "orders-2", "PG_DATA", "healthy", viewmodel.HealthOK, false),
			pvc("orders-2-wal", "orders-2", "PG_WAL", "healthy", viewmodel.HealthOK, false),
			resizing,
			pvc("orders-3-wal", "orders-3", "PG_WAL", "healthy", viewmodel.HealthOK, false),
		},
		Sources: []viewmodel.SourceState{src("clusters", since(3*time.Hour)), src("pvcs", since(3*time.Hour))},
		BlindSpots: []viewmodel.BlindSpot{{
			ID: "storage.usageNotReported", Panel: "storage",
			Text: "Upstream CloudNativePG does not report volume usage in /pg/status; disk usage is the exporter's or your monitoring's to show.",
		}},
		Forecasts: StorageUnavailable(),
	}
}

// Objects is orders' declarative objects.
func Objects() viewmodel.Objects {
	gen := func(g, og int64) (*int64, *int64) { return new(g), new(og) }
	g1, og1 := gen(3, 3)
	g2, og2 := gen(2, 1)
	return viewmodel.Objects{
		Ref: ordersRef, GeneratedAt: Now,
		Databases: []viewmodel.DeclaredObject{
			{Kind: "Database", Name: "orders-app", PGName: "app", Ensure: "present", State: "applied", Health: viewmodel.HealthOK, Generation: g1, ObservedGeneration: og1, Provenance: operatorProv("Database.status")},
			{Kind: "Database", Name: "orders-reporting", PGName: "reporting", Ensure: "present", State: "failed", Health: viewmodel.HealthWarning, Message: `ERROR: role "reporter" does not exist (SQLSTATE 42704)`, Generation: g2, ObservedGeneration: og2, Provenance: operatorProv("Database.status")},
		},
		Roles: []viewmodel.DeclaredObject{
			{Kind: "DatabaseRole", Name: "orders-reader", PGName: "reader", Ensure: "present", State: "applied", Health: viewmodel.HealthOK, Generation: new(int64(1)), ObservedGeneration: new(int64(1)), Provenance: operatorProv("DatabaseRole.status")},
		},
		ManagedRoles: []viewmodel.ManagedRole{
			{Name: "app_owner", Status: "reconciled", Health: viewmodel.HealthOK, Declared: true, Provenance: operatorProv("Cluster.status.managedRolesStatus")},
			{Name: "etl", Status: "unreported", Health: viewmodel.HealthUnknown, Declared: true, Provenance: operatorProv("Cluster.status.managedRolesStatus")},
		},
		Tablespaces:   []viewmodel.TablespaceView{},
		Publications:  []viewmodel.DeclaredObject{{Kind: "Publication", Name: "orders-pub", PGName: "orders_pub", Database: "app", State: "applied", Health: viewmodel.HealthOK, Generation: new(int64(1)), ObservedGeneration: new(int64(1)), Provenance: operatorProv("Publication.status")}},
		Subscriptions: []viewmodel.DeclaredObject{},
		Poolers: []viewmodel.PoolerRow{{
			Name: "orders-pooler-rw", Type: "rw", PoolMode: "transaction", DesiredInstances: new(2), Instances: new(2), PodsReady: new(2),
			Phase: "active", Image: "ghcr.io/cloudnative-pg/pgbouncer:1.24.1", Health: viewmodel.HealthOK,
			Text: "status.instances is the Deployment's replica count, not ready pods", Provenance: operatorProv("Pooler.status"),
		}},
		ImageCatalog: &viewmodel.CatalogView{
			Kind: "ClusterImageCatalog", Name: "postgresql", Major: new(18), State: "notReadable", Health: viewmodel.HealthUnknown,
			Reason:     "catalog not readable: the console is not granted the opt-in ClusterImageCatalog read",
			Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "ClusterImageCatalog"},
		},
		Sources: []viewmodel.SourceState{
			src("databases", since(3*time.Hour)), src("databaseRoles", since(3*time.Hour)), src("publications", since(3*time.Hour)),
			src("subscriptions", since(3*time.Hour)), src("poolers", since(3*time.Hour)),
			{Source: "clusterImageCatalogs", State: viewmodel.SourceDisabled, Reason: "ALLOW_CLUSTER_CATALOGS is false"},
		},
		BlindSpots: []viewmodel.BlindSpot{},
	}
}

// History is orders' history: a spec change, a status transition and a
// first observation.
func History() viewmodel.History {
	return viewmodel.History{
		Ref: ordersRef, GeneratedAt: Now,
		Source: viewmodel.SourceState{Source: "history", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
		Items: []viewmodel.HistoryEntry{
			{Seq: 42, Kind: "Cluster", Name: "orders", Change: "spec-changed", Manager: "kubectl-client-side-apply", Operation: "Update", ObservedAt: Now.Add(-10 * time.Minute), PrevObservedAt: ago(2 * time.Hour), HasManifest: true},
			{Seq: 41, Kind: "Backup", Name: "orders-daily-20261029070000", Change: "status-transition", Manager: "cloudnative-pg", Operation: "Update", ObservedAt: Now.Add(-4*time.Hour - 52*time.Minute), PrevObservedAt: ago(5 * time.Hour), Coalesced: 3, HasManifest: true},
			{Seq: 7, Kind: "Cluster", Name: "orders", Change: "first-observed", ObservedAt: Now.Add(-3 * time.Hour), HasManifest: true},
		},
	}
}

// Revision is revision 42 of orders: the instances went from 2 to 3.
func Revision() viewmodel.Revision {
	h := History()
	return viewmodel.Revision{
		Ref: ordersRef, Entry: h.Items[0],
		Manifest:     "{\n  \"apiVersion\": \"postgresql.cnpg.io/v1\",\n  \"kind\": \"Cluster\",\n  \"spec\": {\n    \"instances\": 3,\n    \"postgresql\": {\n      \"parameters\": {\n        \"max_connections\": \"300\",\n        \"work_mem\": \"16MB\"\n      }\n    }\n  }\n}",
		BaseManifest: "{\n  \"apiVersion\": \"postgresql.cnpg.io/v1\",\n  \"kind\": \"Cluster\",\n  \"spec\": {\n    \"instances\": 2,\n    \"postgresql\": {\n      \"parameters\": {\n        \"max_connections\": \"200\",\n        \"work_mem\": \"4MB\"\n      }\n    }\n  }\n}",
		Diff: viewmodel.RevisionDiff{
			HasBase: true, BaseSeq: new(int64(7)), BaseObservedAt: ago(3 * time.Hour),
			Entries: []viewmodel.DiffEntry{
				{Path: ".spec.instances", Op: "changed", Before: "2", After: "3", Manager: "kubectl-client-side-apply"},
				{Path: ".spec.postgresql.parameters.max_connections", Op: "changed", Before: "200", After: "300", Manager: "kubectl-client-side-apply"},
				{Path: ".spec.postgresql.parameters.work_mem", Op: "changed", Before: "4MB", After: "16MB", Manager: "kubectl-client-side-apply"},
			},
		},
		Preview: RevisionPreview(),
	}
}

// Metrics is orders' metrics: tiles for two instances, one chart with a
// gap, monitoring not configured.
func Metrics() viewmodel.Metrics {
	at := ago(8 * time.Second)
	prov := viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: ":9187/metrics", ObservedAt: at}
	return viewmodel.Metrics{
		Ref: ordersRef, GeneratedAt: Now,
		Source:          viewmodel.SourceState{Source: "metrics", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
		IntervalSeconds: 30, RetentionSeconds: 3600, ChartSource: viewmodel.ChartSourceRing,
		Tiles: []viewmodel.MetricTile{
			{Key: "backends", Label: "Backends", Group: "Connections", Unit: "count", Instance: "orders-1", Value: new(42.0), At: at, Provenance: prov},
			{Key: "backends", Label: "Backends", Group: "Connections", Unit: "count", Instance: "orders-2", Value: new(3.0), At: at, Provenance: prov},
			{Key: "backends", Label: "Backends", Group: "Connections", Unit: "count", Instance: "orders-3", Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: ":9187/metrics", Stale: true, StaleReason: "the exporter did not answer"}},
		},
		Charts: []viewmodel.MetricChart{{
			Key: "replication_lag", Label: "Replication lag", Group: "Replication", Unit: "seconds",
			Times: []time.Time{Now.Add(-90 * time.Second), Now.Add(-60 * time.Second), Now.Add(-30 * time.Second)},
			Lines: []viewmodel.MetricLine{
				{Instance: "orders-2", Values: []*float64{new(0.0), new(0.4), new(0.0)}},
				{Instance: "orders-3", Values: []*float64{new(1.2), nil, new(3.5)}},
			},
			Note: "grows on an idle primary: never alert on it alone",
		}},
		Monitoring: viewmodel.Monitoring{State: "notConfigured"},
		BlindSpots: []viewmodel.BlindSpot{{ID: "metrics.notAMonitoringSystem", Panel: "metrics", Text: "The console keeps one hour of the values its checks read; it is not a monitoring system."}},
	}
}

// Topology is orders' wiring drawing, laid out by the read model's own
// layout (internal/console/topology) from the facts the other orders
// fixtures state: two poolers, the rw and ro Services, the
// primary and two replicas with the primary's walsender report, data and
// WAL volumes per instance, the daily schedule, the ObjectStore, and the
// newest attributed base backup served by orders-2.
func Topology() viewmodel.Topology {
	row := func(class, text string) topology.Row { return topology.Row{Class: class, Text: text} }
	box := func(id, kind, label string, state viewmodel.Health, link string, rows ...topology.Row) topology.Box {
		return topology.Box{ID: id, Kind: kind, Label: label, State: state, Link: link, Rows: append([]topology.Row{row("label", label)}, rows...)}
	}
	inst := func(name, role, kind, title string, state viewmodel.Health, rows ...topology.Row) topology.Instance {
		b := box("inst-"+name, kind, name, state, "instances/"+name, rows...)
		b.Rows[0].Text = name + " — " + title
		return topology.Instance{Box: b, Name: name, Role: role}
	}
	claim := func(name, instance, role string, state viewmodel.Health) topology.Claim {
		return topology.Claim{Instance: instance, Box: box("pvc-"+name, topology.KindVolume, name, state, "storage", row("sub", role+" · Bound"), row("disk", "20Gi · fast-ssd"))}
	}
	service := func(typ, routing, addr string) topology.Service {
		return topology.Service{Type: typ, Box: box("svc-"+typ, topology.KindService, "orders-"+typ, viewmodel.HealthInfo, "", row("sub", routing), row("disk", "ClusterIP "+addr))}
	}
	store := box("store", topology.KindStore, "ObjectStore orders-store", viewmodel.HealthOK, "backups",
		row("sub", "WAL archive · base backups"), row("disk", "s3://orders-backups/orders"), row("note", "plugin barman-cloud"))
	d := topology.Layout(topology.Input{
		Poolers: []topology.Pooler{
			{Type: "rw", Box: box("pooler-orders-pooler-rw", topology.KindPooler, "orders-pooler-rw", viewmodel.HealthOK, "objects", row("sub", "pgbouncer rw · transaction"), row("disk", "2 of 2 pods created"))},
			{Type: "ro", Box: box("pooler-orders-pooler-ro", topology.KindPooler, "orders-pooler-ro", viewmodel.HealthProgressing, "objects", row("sub", "pgbouncer ro · session"), row("disk", "1 of 2 pods created"))},
		},
		Services: []topology.Service{service("rw", "routes to the primary", "10.96.14.20:5432"), service("ro", "routes to the replicas", "10.96.14.21:5432")},
		Instances: []topology.Instance{
			inst("orders-1", topology.RolePrimary, topology.KindPrimary, "primary", viewmodel.HealthOK,
				row("sub", "timeline 3"), row("sub", "Running · ready"), row("disk", "node node-a"), row("disk", "postgresql:18.1-system-trixie")),
			inst("orders-2", topology.RoleReplica, topology.KindReplica, "replica", viewmodel.HealthOK,
				row("sub", "streaming · quorum"), row("sub", "Running · ready"), row("disk", "node node-b"), row("disk", "postgresql:18.1-system-trixie")),
			inst("orders-3", topology.RoleReplica, topology.KindReplica, "replica", viewmodel.HealthUnknown,
				row("sub", "catchup · potential"), row("sub", "Running · ready · fenced"), row("disk", "node node-c"), row("disk", "postgresql:18.1-system-trixie")),
		},
		Schedules: []topology.Box{box("sched-orders-daily", topology.KindSchedule, "orders-daily", viewmodel.HealthOK, "backups", row("disk", "0 0 7 * * *"), row("sub", "next Oct 30 07:00 UTC"))},
		Store:     &store,
		StoreNote: "https://s3.eu-west-1.example.com",
		Claims: []topology.Claim{
			claim("orders-1", "orders-1", "PG_DATA", viewmodel.HealthOK), claim("orders-1-wal", "orders-1", "PG_WAL", viewmodel.HealthOK),
			claim("orders-2", "orders-2", "PG_DATA", viewmodel.HealthOK), claim("orders-2-wal", "orders-2", "PG_WAL", viewmodel.HealthOK),
			claim("orders-3", "orders-3", "PG_DATA", viewmodel.HealthProgressing), claim("orders-3-wal", "orders-3", "PG_WAL", viewmodel.HealthOK),
		},
		WAL:  true,
		Base: &topology.Base{Instance: "orders-2", Target: topology.TargetStore, Label: "base backup · barman-cloud"},
	})
	return viewmodel.Topology{
		Ref: ordersRef, GeneratedAt: Now, Title: "How orders is wired",
		Aria: "The wiring of orders; 2 pooler(s) front its Services; orders-rw leads to the primary orders-1; orders-ro leads to the replicas orders-2, orders-3, which stream from the primary; " +
			"6 volume(s) are drawn beside their instances; 1 backup schedule(s) target the cluster; WAL is archived to the object store; the newest attributed base backup left orders-2.",
		Width: d.Width, Height: d.Height, Frames: d.Frames, Edges: d.Edges, Nodes: d.Nodes, Dots: d.Dots, Legend: d.Legend,
		Caption:    topology.Caption,
		BlindSpots: []viewmodel.BlindSpot{},
	}
}

// LogTail is the tail of orders-1's postgres container.
func LogTail() viewmodel.LogTail {
	at := ago
	return viewmodel.LogTail{
		Ref: ordersRef, Pod: "orders-1", Container: "postgres", Containers: []string{"postgres"}, ReadAt: Now,
		Lines: []viewmodel.LogLine{
			{At: at(70 * time.Second), Text: `{"level":"info","ts":"2026-10-29T11:58:50Z","logger":"postgres","msg":"record","record":{"message":"checkpoint starting: time"}}`},
			{At: at(10 * time.Second), Text: `{"level":"info","ts":"2026-10-29T11:59:50Z","logger":"postgres","msg":"record","record":{"message":"checkpoint complete"}}`},
		},
	}
}

// Admin is the admin page: one PgAdmin, one EvidenceAgent, and a
// namespace whose PgAdmins the caller may not list.
func Admin() viewmodel.Admin {
	return viewmodel.Admin{
		GeneratedAt: Now,
		Namespaces: []viewmodel.AdminNamespace{
			{
				Name: "team-a", PgAdminsVisible: true, EvidenceAgentsVisible: true,
				PgAdmins: []viewmodel.PgAdminRow{{
					Name: "team-a", Health: viewmodel.HealthOK, URL: "https://pgadmin-team-a.apps.example.com", Version: "9.9", Image: "ghcr.io/fyannk/pgadmin@sha256:0123",
					Servers:    []viewmodel.PgAdminServer{{Cluster: "orders", Endpoint: "rw", State: "Registered"}, {Cluster: "billing", Endpoint: "rw", State: "Registered"}},
					Conditions: []viewmodel.Condition{{Type: "Ready", Status: "True", Reason: "Ready", Health: viewmodel.HealthOK, Known: true, LastTransition: ago(26 * time.Hour)}},
					Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "PgAdmin.status", ObservedAt: ago(2 * time.Second)},
				}},
				EvidenceAgents: []viewmodel.EvidenceAgentRow{{
					Name: "orders-evidence", Health: viewmodel.HealthOK, Endpoint: "https://orders-evidence.team-a.svc:8443", Fingerprint: "sha256:9f86d081884c7d65", ProducerEpoch: new(int64(3)),
					Conditions: []viewmodel.Condition{{Type: "Ready", Status: "True", Reason: "Ready", Health: viewmodel.HealthOK, Known: true, LastTransition: ago(26 * time.Hour)}},
					Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "EvidenceAgent.status", ObservedAt: ago(2 * time.Second)},
				}},
				Sources: []viewmodel.SourceState{src("pgAdmins", since(3*time.Hour)), src("evidenceAgents", since(3*time.Hour))},
			},
			{Name: "team-a-staging", PgAdmins: []viewmodel.PgAdminRow{}, EvidenceAgents: []viewmodel.EvidenceAgentRow{}, Sources: []viewmodel.SourceState{}},
		},
		Console: &viewmodel.ConsoleHealth{
			Version: "0.3.0", Namespace: "team-a", WatchNamespaces: []string{"team-a", "team-a-staging"},
			Runners: []viewmodel.RunnerState{
				{Name: "informers", Core: true, Running: true},
				{Name: "sweep", Core: true, Running: true},
				{Name: "diagnostics", Running: true, Restarts: 1, LastError: "diagnostics run panicked; restarted"},
			},
			Sources:    []viewmodel.SourceState{src("instanceStatus", since(3*time.Hour)), src("diagnostics", since(3*time.Hour)), src("history", since(3*time.Hour)), src("evidence", since(time.Hour))},
			Digest:     DigestStatus(),
			Provenance: consoleProv("console"),
		},
	}
}

// Operations is orders' operations as an operator sees them: promote
// with one eligible and one ineligible replica, reload not observable,
// and one recent backup.
func Operations() viewmodel.Operations {
	return viewmodel.Operations{
		Ref: ordersRef, GeneratedAt: Now,
		Offers: []viewmodel.OperationOffer{
			{
				Kind: viewmodel.OperationBackup, Title: "Request an on-demand backup", Summary: "Creates one Backup referencing this cluster, through the plugin it archives with.",
				Consequence: "The backup reads from a standby when the cluster prefers one; it adds I/O load for its duration.", ExpectedDuration: "minutes to hours, depending on size",
				Allowed: true, Requires: []string{"create backups"}, Blockers: []viewmodel.OperationBlocker{}, Confirmation: "orders", Observable: true,
			},
			{
				Kind: viewmodel.OperationReload, Title: "Reload configuration", Summary: "Asks every instance to reload its configuration (annotation cnpg.io/reloadedAt).",
				Consequence: "PostgreSQL reloads only if a configuration file changed; CloudNativePG reports no outcome.", ExpectedDuration: "seconds",
				Allowed: true, Requires: []string{"patch clusters"}, Blockers: []viewmodel.OperationBlocker{}, Confirmation: "orders", Observable: false,
			},
			{
				Kind: viewmodel.OperationRestart, Title: "Restart the cluster", Summary: "Rolls every instance (annotation kubectl.kubernetes.io/restartedAt), replicas first, then a switchover.",
				Consequence: "Each instance restarts once; clients of the primary are disconnected during the switchover.", ExpectedDuration: "a few minutes per instance",
				Allowed: true, Requires: []string{"patch clusters"}, Blockers: []viewmodel.OperationBlocker{}, Confirmation: "orders", Observable: true,
			},
			{
				Kind: viewmodel.OperationPromote, Title: "Promote a replica (switchover)", Summary: "Moves the primary role to the chosen replica.",
				Consequence: "Clients of the primary are disconnected; the old primary becomes a replica.", ExpectedDuration: "under a minute",
				Allowed: true, Requires: []string{"patch clusters/status"}, Blockers: []viewmodel.OperationBlocker{}, Confirmation: "the target instance name", Observable: true,
				Targets: []viewmodel.PromoteTarget{
					{Name: "orders-2", Eligible: true, Health: viewmodel.HealthOK, LagBytes: new(int64(0))},
					{Name: "orders-3", Eligible: false, Health: viewmodel.HealthUnknown, Blockers: []viewmodel.OperationBlocker{{Reason: "TargetFenced", Text: "orders-3 is fenced", Source: "Cluster.metadata.annotations[cnpg.io/fencedInstances]"}}},
				},
			},
			{
				Kind: viewmodel.OperationRestoreDrill, Title: "Run a restore drill",
				Summary:          "Restores the latest backup into a throwaway Cluster in team-a, checks it without SQL, then deletes it.",
				Consequence:      "The drill Cluster uses storage and compute like one instance of this Cluster until it is deleted (after its check, at the latest after 2h). It has no WAL archiver and no backup configuration, and starts with logical replication workers, pg_cron jobs and TimescaleDB jobs off; other extensions that reach the network are not neutralised (see the drill NetworkPolicy).",
				ExpectedDuration: "about as long as a restore of this Cluster",
				Allowed:          true, Requires: []string{"create clusters in team-a"}, Blockers: []viewmodel.OperationBlocker{}, Confirmation: "orders", Observable: true,
				Drill: &viewmodel.DrillOffer{
					Namespace: "team-a", Backup: "orders-daily-20261029070000", BackupStoppedAt: ago(4*time.Hour + 52*time.Minute),
					RecoverySource: "ObjectStore orders-store (barman-cloud plugin), server orders", TimeLimitSeconds: 7200,
				},
			},
		},
		Recent:      []viewmodel.Operation{Operation()},
		RecentScope: "operations issued through this console replica since it started; the API server's audit log is the complete record",
	}
}

// Operation is a tracked on-demand backup that is running.
func Operation() viewmodel.Operation {
	return viewmodel.Operation{
		OperationID: "eyJ2IjoxLCJrIjoiYmFja3VwIiwibiI6InRlYW0tYSIsImMiOiJvcmRlcnMiLCJiIjoib25kZW1hbmQtMjAyNjEwMjkxMTU1MDAteDdrMnAiLCJ0IjoxNzkzMjc0OTAwfQ", Kind: viewmodel.OperationBackup, Target: ordersRef, Created: "ondemand-20261029115500-x7k2p",
		Phase: viewmodel.OpPhaseActing, Health: viewmodel.HealthProgressing, AcceptedAt: Now.Add(-5 * time.Minute), Deadline: Now.Add(25 * time.Minute),
		Steps: []viewmodel.OperationStep{
			{Phase: viewmodel.OpPhaseRequested, At: Now.Add(-5 * time.Minute), Source: "console", Text: "requested"},
			{Phase: viewmodel.OpPhaseAccepted, At: Now.Add(-5 * time.Minute), Source: "API server", Text: "Backup ondemand-20261029115500-x7k2p created"},
			{Phase: viewmodel.OpPhaseActing, At: Now.Add(-4 * time.Minute), Source: "Backup.status.phase", Text: "started"},
		},
	}
}

// PgAdminLinks are the pgAdmins orders is registered in.
func PgAdminLinks() []viewmodel.PgAdminLink {
	return []viewmodel.PgAdminLink{{Name: "team-a", URL: "https://pgadmin-team-a.apps.example.com", Health: viewmodel.HealthOK, ServerState: "Registered"}}
}
