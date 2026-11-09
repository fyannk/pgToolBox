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

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The pgBackRest labels of billing's stanza: a weekly full, a
// differential on it, an incremental on the differential, and a newer
// incremental pgBackRest recorded with an error.
const (
	pgbFull  = "20261022-030004F"
	pgbDiff  = "20261022-030004F_20261026-030002D"
	pgbIncr  = "20261022-030004F_20261028-030003I"
	pgbError = "20261022-030004F_20261029-030001I"
)

// BackupsPgBackRest is billing's Backups screen: the Dalibo pgBackRest
// plugin, its Stanza object, a chain of full, differential and
// incremental backups, the evidence agent's recovery window and one
// finding. Neither plugin reports recoverability on the Cluster: the
// window comes from the agent.
func BackupsPgBackRest() viewmodel.Backups {
	at := ago
	day := 24 * time.Hour
	repo := int64(1)
	stopFull, stopDiff, stopIncr := ago(7*day-9*time.Hour+18*time.Minute), ago(3*day-9*time.Hour+11*time.Minute), ago(day-9*time.Hour+6*time.Minute)
	windowEnd := ago(50 * time.Second)
	ref := func(label, typ, prior string) *viewmodel.PgBackRestBackupRef {
		return &viewmodel.PgBackRestBackupRef{Label: label, Type: typ, Prior: prior, Full: pgbFull, Usable: new(true), Stanza: "billing"}
	}
	row := func(name string, stopped *time.Time) viewmodel.BackupRow {
		return viewmodel.BackupRow{
			Name: name, Phase: "completed", Health: viewmodel.HealthOK, Known: true, Terminal: true, Method: "plugin",
			PluginName: "pgbackrest.dalibo.com", Online: new(true), CreatedAt: stopped, StartedAt: stopped, StoppedAt: stopped,
			Schedule: "billing-nightly", Repository: viewmodel.RepositoryPresent, Provenance: operatorProv("Backup.status"),
		}
	}
	full, diff, incr := row("billing-nightly-20261022030000", stopFull), row("billing-nightly-20261026030000", stopDiff), row("billing-nightly-20261028030000", stopIncr)
	full.PgBackRest, diff.PgBackRest, incr.PgBackRest = ref(pgbFull, "full", ""), ref(pgbDiff, "diff", pgbFull), ref(pgbIncr, "incr", pgbDiff)
	failed := viewmodel.BackupRow{
		Name: "billing-nightly-20261029030000", Phase: "failed", Health: viewmodel.HealthCritical, Known: true, Terminal: true, Method: "plugin",
		PluginName: "pgbackrest.dalibo.com", Error: "pgbackrest backup exited with code 82", CreatedAt: at(9 * time.Hour),
		Schedule: "billing-nightly", Repository: viewmodel.RepositoryNotChecked, Provenance: operatorProv("Backup.status"),
	}
	repoBackup := func(label, typ, prior string, refs int, started, stopped *time.Time, size, stored int64, name string) viewmodel.PgBackRestRepoBackup {
		return viewmodel.PgBackRestRepoBackup{
			Label: label, Type: typ, Prior: prior, Full: pgbFull, References: refs, Usable: true, UnusableReasons: []string{},
			State: "healthy", Health: viewmodel.HealthOK, StartedAt: started, StoppedAt: stopped, Timeline: new(int64(2)),
			SizeBytes: size, RepositorySizeBytes: stored, BackupName: name,
		}
	}
	// The Stanza keeps four weekly fulls (repo1-retention-full=4); the
	// archive retention follows it. Retention is applied at the end of each
	// backup, so it is evaluated as of the newest one.
	retention := &viewmodel.PgBackRestRetention{
		Configured: true, Full: new(int64(4)), FullType: "count", ArchiveType: "full", EffectiveArchive: new(int64(4)),
		FullBackups: 1, DifferentialBackups: 1, IncrementalBackups: 2, EvaluatedAt: at(9*time.Hour - 4*time.Minute),
		Result: viewmodel.EvidenceFact{State: "healthy", Health: viewmodel.HealthOK, Code: "retention-healthy", Message: "the backups and WAL match the retention of 4 full backups"},
	}
	errored := repoBackup(pgbError, "incr", pgbIncr, 3, at(9*time.Hour), at(9*time.Hour-4*time.Minute), 2<<30, 310<<20, "")
	errored.Usable, errored.UnusableReasons, errored.State, errored.Health, errored.Failed = false, []string{"backup-error"}, "unhealthy", viewmodel.HealthCritical, true
	return viewmodel.Backups{
		Ref: billingRef, GeneratedAt: Now,
		Summary: viewmodel.BackupSummary{
			BackupRecency: viewmodel.BackupRecency{Health: viewmodel.HealthOK, LastCompletedAt: stopIncr, Configured: true, PluginBased: true, Provenance: operatorProv("Backup.status")},
			Archiving:     viewmodel.Archiving{Health: viewmodel.HealthOK, ConditionStatus: "True", Reason: "ContinuousArchivingSuccess", LiveArchiving: new(true), ReadyWALFiles: new(0), Provenance: operatorProv("Cluster.status.conditions[ContinuousArchiving]")},
			RecoveryWindow: &viewmodel.RecoveryWindow{
				First: stopFull, Last: windowEnd,
				Provenance: evidenceProv("EvidenceAgent billing-evidence /api/v1alpha1/snapshot recovery_window"),
			},
			NextScheduledAt: new(Now.Add(15 * time.Hour)),
			NotReported:     []string{"status.firstRecoverabilityPoint", "status.lastSuccessfulBackup", "metric:cnpg_collector_last_available_backup_timestamp"},
		},
		Method: viewmodel.BackupMethod{
			Configured: true, PluginBased: true, PluginName: "pgbackrest.dalibo.com",
			PgBackRest: &viewmodel.PgBackRestMethod{
				Plugin: "pgbackrest.dalibo.com", Flavor: "dalibo", Display: "pgBackRest (Dalibo)", ObjectKind: "Stanza", ObjectName: "billing-stanza",
				Stanza: "billing", Repository: &repo, Encrypted: new(true),
			},
			Provenance: operatorProv("Cluster.spec.plugins"),
		},
		Items: []viewmodel.BackupRow{failed, incr, diff, full},
		Schedules: []viewmodel.ScheduleRow{{
			Name: "billing-nightly", Schedule: "0 0 3 * * *", State: "active", Health: viewmodel.HealthOK, Method: "plugin", Target: "prefer-standby",
			LastScheduledAt: at(9 * time.Hour), NextScheduledAt: new(Now.Add(15 * time.Hour)), Provenance: operatorProv("ScheduledBackup.status"),
		}},
		Evidence: viewmodel.EvidenceCheck{
			State: viewmodel.EvidenceLive, Health: viewmodel.HealthWarning, Agent: "billing-evidence",
			Repository: &viewmodel.RepositoryEvidence{
				Completeness: "complete",
				Overall:      viewmodel.EvidenceFact{State: "warning", Health: viewmodel.HealthWarning, Code: "findings", Message: "the reader reported one finding"},
				WAL:          viewmodel.EvidenceFact{State: "healthy", Health: viewmodel.HealthOK},
				Timeline:     viewmodel.EvidenceFact{State: "healthy", Health: viewmodel.HealthOK},
				Coverage:     viewmodel.EvidenceFact{State: "healthy", Health: viewmodel.HealthOK},
				Retention:    retention.Result,
				Backups:      new(int64(4)), StructurallyUsable: new(int64(3)), RepositoryOnly: new(0),
				OldestCompletionAt: stopFull, NewestCompletionAt: stopIncr, LatestArchiveReceiptAt: windowEnd,
				CompletedAt: ago(40 * time.Second),
				PgBackRest: &viewmodel.PgBackRestEvidence{
					Stanza: "billing", ArchiveStanza: "billing", Repository: 1, RepositoryFormat: new(int64(5)), Encrypted: new(true),
					PgBackRestVersion: "2.57.0",
					Catalog:           viewmodel.EvidenceFact{State: "healthy", Health: viewmodel.HealthOK},
					Retention:         retention,
					RecoveryWindow: &viewmodel.PgBackRestRecoveryWindow{
						OldestBackup: pgbFull, StartAt: *stopFull, NewestBackup: pgbIncr, NewestStopAt: *stopIncr,
						EndTimeline: 2, EndWAL: "00000002000000140000003A", EndReceiptAt: windowEnd, UsableBackups: 3,
						Assumptions: []string{"segment-name-presence-only", "wal-bytes-and-restore-not-verified"},
					},
					Findings: []viewmodel.PgBackRestFinding{{
						Code: "WALUnknown", Severity: "warning", Health: viewmodel.HealthWarning, Subject: "archive/billing/16-1/0000000200000014",
						Message: "a WAL directory could not be listed completely",
					}},
					Backups: []viewmodel.PgBackRestRepoBackup{
						errored,
						repoBackup(pgbIncr, "incr", pgbDiff, 2, at(day-9*time.Hour+time.Minute), stopIncr, 2<<30, 280<<20, incr.Name),
						repoBackup(pgbDiff, "diff", pgbFull, 1, at(3*day-9*time.Hour+3*time.Minute), stopDiff, 9<<30, 1<<30, diff.Name),
						repoBackup(pgbFull, "full", "", 0, at(7*day-9*time.Hour+4*time.Minute), stopFull, 48<<30, 11<<30, full.Name),
					},
				},
			},
			Provenance: evidenceProv("EvidenceAgent billing-evidence /api/v1alpha1/snapshot"),
		},
		RecoveryTimeline: viewmodel.RecoveryTimeline{
			Basis: "evidence", State: "healthy", Earliest: stopFull, Latest: windowEnd, TargetTimeline: 2,
			Segments: []viewmodel.RecoverySegment{
				{From: *stopFull, To: *stopDiff, Kind: "covered", BackupID: pgbFull, Text: "Structurally covered: base backup " + pgbFull + " and contiguous archived WAL."},
				{From: *stopDiff, To: *stopIncr, Kind: "covered", BackupID: pgbDiff, Text: "Structurally covered: base backup " + pgbDiff + " and contiguous archived WAL."},
				{From: *stopIncr, To: *windowEnd, Kind: "covered", BackupID: pgbIncr, Text: "Structurally covered: base backup " + pgbIncr + " and contiguous archived WAL."},
			},
			Markers: []viewmodel.RecoveryMarker{
				{At: *stopFull, BackupID: pgbFull, Kind: "completed", Source: "repository"},
				{At: *stopDiff, BackupID: pgbDiff, Kind: "completed", Source: "repository"},
				{At: *stopIncr, BackupID: pgbIncr, Kind: "completed", Source: "repository"},
				{At: *at(9*time.Hour - 4*time.Minute), BackupID: pgbError, Kind: "failed", Source: "repository"},
			},
			Assumptions: []string{"segment-name-presence-only", "wal-bytes-and-restore-not-verified"},
			Complete:    true,
			Provenance:  evidenceProv("EvidenceAgent billing-evidence /api/v1alpha1/recovery-paths"),
		},
		Drills:  viewmodel.RestoreDrills{Items: []viewmodel.RestoreDrill{}, Scope: RestoreDrills().Scope, NotChecked: RestoreDrills().NotChecked, WindowDays: 90},
		Sources: []viewmodel.SourceState{src("backups", since(3*time.Hour)), src("scheduledBackups", since(3*time.Hour)), src("evidence", since(time.Hour))},
		BlindSpots: []viewmodel.BlindSpot{{
			ID: "backups.pluginNotReported", Panel: "backups",
			Text: "Backups go through pgBackRest (Dalibo): the Cluster's first recoverability point and last backup fields (and their cnpg_collector metrics) are not reported by plugin-based backups. Recoverability comes from the repository evidence of an EvidenceAgent and the Backup objects only.",
		}},
	}
}
