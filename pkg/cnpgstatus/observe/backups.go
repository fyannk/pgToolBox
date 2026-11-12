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

package observe

import (
	"time"
)

// ObjectStoreState is the outcome of the optional, metadata-only
// ObjectStore lookup.
type ObjectStoreState string

const (
	// ObjectStoreNotReferenced means the target Cluster has no enabled
	// Barman Cloud plugin ObjectStore reference.
	ObjectStoreNotReferenced ObjectStoreState = "not-referenced"
	// ObjectStorePresent means the referenced object was observed.
	ObjectStorePresent ObjectStoreState = "present"
	// ObjectStoreUnknown means the reference or object could not be
	// observed. Permission and CRD absence deliberately land here.
	ObjectStoreUnknown ObjectStoreState = "unknown"
)

// BackupFacts is the bounded operator-reported state of one Backup.
type BackupFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Phase is the phase reported in status.
	Phase string
	// Method is the status method, falling back to the spec method.
	Method string
	// BackupID is the operator-reported repository backup identity
	// (status.backupId) — the only cross-source correlation key the
	// evidence contract accepts. Empty when the operator has not
	// reported one.
	BackupID string
	// PluginName is the configured backup plugin name for the plugin
	// method, empty otherwise. Correlation eligibility requires the
	// accepted Barman Cloud plugin.
	PluginName string
	// SourceInstance is the pod the operator reports the backup was
	// taken from (status.instanceID.podName), empty when it reported
	// none. It is the only word on where a base backup came from, and
	// it is not the primary by default: CloudNativePG's backup target
	// prefers a standby, so a base backup and the WAL stream routinely
	// leave the cluster from different instances.
	SourceInstance string
	// ScheduledBackup names the ScheduledBackup that created this backup
	// (CloudNativePG's cnpg.io/scheduled-backup label, set whatever the
	// schedule's backupOwnerReference); empty for an on-demand backup.
	ScheduledBackup string
	// CreatedAt is the resource creation time.
	CreatedAt time.Time
	// StartedAt is the reported backup-tool start time.
	StartedAt *time.Time
	// StoppedAt is the reported backup-tool termination time.
	StoppedAt *time.Time
}

// ScheduledBackupFacts is the bounded operator-reported state of one
// ScheduledBackup. Schedule is a six-field cron expression reported by the
// resource; the console does not execute or reinterpret it.
type ScheduledBackupFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Method is the configured backup method.
	Method string
	// Schedule is the operator's six-field cron expression.
	Schedule string
	// Suspended is nil when the resource did not report the setting.
	Suspended *bool
	// CreatedAt is the resource creation time.
	CreatedAt time.Time
	// LastScheduleTime is the last successfully scheduled time.
	LastScheduleTime *time.Time
	// NextScheduleTime is the next time reported by the operator.
	NextScheduleTime *time.Time
}

// ObjectStoreReference carries only the safe metadata needed to identify the
// configured repository object. No ObjectStore spec, Secret reference, URL,
// or status content crosses the Kubernetes adapter boundary.
type ObjectStoreReference struct {
	// Name is the referenced ObjectStore name, if observable.
	Name string
	// State is the metadata-only lookup outcome.
	State ObjectStoreState
	// Destination is the reported destination path, such as an s3:// or
	// azure:// URL. Empty when the store reports none.
	Destination string
	// Endpoint is the reported endpoint URL, empty when the store uses
	// the provider default.
	Endpoint string
	// RetentionPolicy is the reported retention, empty when none is set.
	RetentionPolicy string
	// ServerName is the name this cluster's backups are filed under in
	// the store: the plugin's serverName parameter, or the cluster name.
	ServerName string
	// RecoveryWindow is the plugin's report for that server, nil when
	// the store reports none for it yet.
	RecoveryWindow *RecoveryWindow
}

// RecoveryWindow is the barman-cloud plugin's own summary of one
// server's backups in the store. Every instant is the plugin's; nil
// means the plugin reported none.
type RecoveryWindow struct {
	// FirstRecoverabilityPoint is the earliest instant a recovery could
	// target.
	FirstRecoverabilityPoint *time.Time
	// LastSuccessfulBackup is when the last successful backup was taken.
	LastSuccessfulBackup *time.Time
	// LastFailedBackup is when the last backup failed.
	LastFailedBackup *time.Time
}

// BackupsSnapshot is an immutable, independently stale backup catalog.
type BackupsSnapshot struct {
	// Generation increases on every complete publication.
	Generation uint64
	// ObservedAt is the last successful source-contact time.
	ObservedAt time.Time
	// Stale reports lost contact while retaining the last-good catalog.
	Stale bool
	// BackupsTruncated reports a source or display safety ceiling.
	BackupsTruncated bool
	// SchedulesTruncated reports a source or display safety ceiling.
	SchedulesTruncated bool
	// Backups is newest-first; the producer bounds it.
	Backups []BackupFacts
	// ScheduledBackups is name-sorted; the producer bounds it.
	ScheduledBackups []ScheduledBackupFacts
	// ObjectStore is the optional metadata-only reference result.
	ObjectStore ObjectStoreReference
}

// backupClockSkew is how far before its Backup's creation, or after the
// reader's now, a backup time may fall and still be read as true: the
// instance's clock against the API server's and the console's.
const backupClockSkew = 5 * time.Minute

// backupTimeFloor is the earliest backup tool time read as true.
// CloudNativePG converts a plugin's start and stop times with
// time.Unix(seconds, 0) and keeps them unless IsZero
// (internal/cnpi/plugin/client/backup.go v1.30.1:156-157,
// webserver/plugin_backup.go:144-148), so a plugin that leaves one
// unset stamps the Backup 1970-01-01 (round 18, CNPG P3).
var backupTimeFloor = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// BackupStamps are the times a Backup object carries: its creation, the
// backup tool's status.startedAt and status.stoppedAt (a plugin's, or
// barman's begin and end), and the operator's own
// status.reconciliationStartedAt (SetAsStarted) and
// status.reconciliationTerminatedAt (SetAsCompleted and SetAsFailed,
// pkg/api/v1/backup_funcs.go v1.30.1:67-71 and 83).
type BackupStamps struct {
	Created                                         time.Time
	Started, Stopped                                *time.Time
	ReconciliationStarted, ReconciliationTerminated *time.Time
}

// BackupTimes are a Backup's plausible times (SaneBackupTimes), in UTC;
// nil where not known.
type BackupTimes struct {
	// Start and Stop are the backup tool's times.
	Start, Stop *time.Time
	// Imported: the tool's start and stop are coherent (start at or before
	// stop, both after the floor) but older than the object itself, as on
	// a Backup restored with its status (Velero) or migrated from another
	// cluster. They are kept: the backup ended then, not since the object
	// appeared (round 19, PG P3-B).
	Imported bool
	// OperatorStarted is when the operator began reconciling the Backup
	// (SetAsStarted): at or before the tool's start, and after the
	// object's creation while the Backup waited in the queue.
	OperatorStarted *time.Time
	// OperatorTerminated is when the operator marked the Backup completed
	// or failed: an upper bound on its end, seconds after it, read when
	// Stop is not known (round 19, CNPG P3-1).
	OperatorTerminated *time.Time
}

// SaneBackupTimes reads a Backup's times with every impossible value as
// unknown (nil), at now:
//
//   - a time before backupTimeFloor (the plugin epoch above);
//   - a time more than backupClockSkew after now: a plugin that sends
//     milliseconds for seconds stamps the far future, which would read as
//     a backup that never ages (round 19, CNPG P3-1); a future time is
//     dropped, never taken as fresh;
//   - a tool time more than backupClockSkew before the object's creation
//     (created zero: not checked), unless start and stop are both
//     reported and coherent (Imported);
//   - a stop before the start.
//
// The operator's times are its own clock's: only the floor and the
// upper bound apply. A Completed Backup whose stop time is unknown is
// then bounded by OperatorTerminated, else counted as one that could
// have ended at any time, never as one that ended in 1970.
func SaneBackupTimes(now time.Time, s BackupStamps) BackupTimes {
	ceiling := now.Add(backupClockSkew)
	plausible := func(at *time.Time) *time.Time {
		if at == nil || at.Before(backupTimeFloor) || at.After(ceiling) {
			return nil
		}
		t := at.UTC()
		return &t
	}
	var out BackupTimes
	start, stop := plausible(s.Started), plausible(s.Stopped)
	if start != nil && stop != nil && stop.Before(*start) {
		stop = nil
	}
	early := func(at *time.Time) bool {
		return at != nil && !s.Created.IsZero() && at.Before(s.Created.Add(-backupClockSkew))
	}
	switch {
	case start != nil && stop != nil && early(stop):
		out.Imported = true
	default:
		if early(start) {
			start = nil
		}
		if early(stop) {
			stop = nil
		}
	}
	out.Start, out.Stop = start, stop
	out.OperatorStarted, out.OperatorTerminated = plausible(s.ReconciliationStarted), plausible(s.ReconciliationTerminated)
	return out
}
