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

// BackupClockSkew is backupClockSkew for the readers that compare an
// instance-written time with another clock (the console's purge band:
// an instance up to this far behind the agent is not ClockBehind, and
// its stamps are read as true; round 22, PG P3-A).
const BackupClockSkew = backupClockSkew

// backupTimeFloor is the earliest backup tool time read as true.
// CloudNativePG converts a plugin's start and stop times with
// time.Unix(seconds, 0) and keeps them unless IsZero
// (internal/cnpi/plugin/client/backup.go v1.30.1:156-157,
// webserver/plugin_backup.go:144-148), so a plugin that leaves one
// unset stamps the Backup 1970-01-01 (round 18, CNPG P3).
var backupTimeFloor = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// BackupStamps are the times a Backup object carries: its creation, the
// backup tool's status.startedAt and status.stoppedAt (a plugin's, or
// barman's begin and end), and CloudNativePG's own stamps:
// status.reconciliationStartedAt (SetAsStarted, called by the operator)
// and status.reconciliationTerminatedAt (SetAsCompleted and SetAsFailed,
// pkg/api/v1/backup_funcs.go v1.30.1:46-71 and 83): the instance
// manager's for an in-tree barman or plugin Backup it completed or failed
// once the tool returned (pkg/management/postgres/backup.go:202,
// webserver/plugin_backup.go:129), else the operator's (a volume
// snapshot, or a Backup the operator failed: internal/controller/
// backup_controller.go, for example a pod gone or a session changed).
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
	// OperatorTerminated is CloudNativePG's completion stamp
	// (status.reconciliationTerminatedAt: the instance manager's for a
	// barman or plugin Backup it completed or failed, else the
	// operator's), written when the Backup was
	// marked completed or failed: an upper bound on its end, seconds after
	// it, and after pgBackRest's expire at that end (round 19, CNPG P3-1;
	// round 20, PG P3-A).
	OperatorTerminated *time.Time
	// ClockBehind: outside Imported, on a Backup the operator started
	// after its creation, a tool time or the completion stamp is more
	// than backupClockSkew before the object's creation or the
	// operator's start stamp. The instance's clock and the operator's
	// (or the API server's) disagree: either the instance runs behind or
	// the operator runs ahead, and the console cannot tell which. Every
	// time the instance wrote (Start, Stop, OperatorTerminated) is
	// dropped, and OperatorStarted may itself be late: such a Backup has
	// no plausible end, never an early one, and a reader that needs one
	// takes a time on the API server's clock (its creation, before the
	// end; round 21, PG P3-B, CNPG P3-1; round 22, CNPG P3-1).
	ClockBehind bool
}

// SaneBackupTimes reads a Backup's times with every impossible value as
// unknown (nil), at now:
//
//   - a time before backupTimeFloor (the plugin epoch above);
//   - a time more than backupClockSkew after now: a plugin that sends
//     milliseconds for seconds stamps the far future, which would read as
//     a backup that never ages (round 19, CNPG P3-1); a future time is
//     dropped, never taken as fresh. A zero now sets no ceiling: a
//     caller that reads only the method or the pod gets the times
//     unchecked against a clock rather than none (round 20, Go P3-2);
//   - a tool time more than backupClockSkew before the object's creation
//     (created zero: not checked), unless start and stop are both
//     reported and coherent and CloudNativePG's stamps, when present, are
//     also that early (Imported). A Backup restored with its status
//     (Velero) carries its old stamps; a live one's are at or after its
//     creation, so tool times that early are an instance clock running
//     behind (round 20, PG P3-B, CNPG P3-2);
//   - outside Imported, on a Backup whose operator start stamp
//     (SetAsStarted, written by the operator before the instance runs
//     the tool) is not older than the object, a tool time or the
//     completion stamp more than backupClockSkew before that stamp or
//     the object's creation: the instance's clock and the operator's
//     disagree (the instance behind or the operator ahead). The
//     completion stamp of a barman or plugin Backup is the instance
//     manager's, on the same clock as the tool times, so every time the
//     instance wrote is dropped (ClockBehind; round 21, PG P3-B, CNPG
//     P3-1). Without such a start stamp the status was restored or
//     migrated: only the early tool times are dropped;
//   - a stop before the start.
//
// CloudNativePG's stamps are written on the instance's or the operator's
// clock as the Backup's status is patched: past the floor and the upper
// bound, only the clock check above applies. A Completed Backup whose
// stop time is unknown is then bounded by OperatorTerminated, else
// counted as one that could have ended at any time, never as one that
// ended in 1970 or before it ran.
func SaneBackupTimes(now time.Time, s BackupStamps) BackupTimes {
	ceiling := now.Add(backupClockSkew)
	plausible := func(at *time.Time) *time.Time {
		if at == nil || at.Before(backupTimeFloor) || (!now.IsZero() && at.After(ceiling)) {
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
	opStarted, opTerminated := plausible(s.ReconciliationStarted), plausible(s.ReconciliationTerminated)
	stampsEarly := (opStarted == nil || early(opStarted)) && (opTerminated == nil || early(opTerminated))
	if start != nil && stop != nil && early(stop) && stampsEarly {
		out.Imported = true
	} else {
		// beforeStart: at is earlier than the operator's start stamp, which
		// the operator writes before the instance runs the tool.
		beforeStart := func(at *time.Time) bool {
			return at != nil && at.Before(opStarted.Add(-backupClockSkew))
		}
		switch {
		case opStarted != nil && !early(opStarted) &&
			(early(start) || early(stop) || early(opTerminated) ||
				beforeStart(start) || beforeStart(stop) || beforeStart(opTerminated)):
			// The operator started this Backup here, and the instance
			// wrote times before that: the instance runs behind, or the
			// operator ahead.
			start, stop, opTerminated = nil, nil, nil
			out.ClockBehind = true
		default:
			// No live start stamp: a restored or migrated status, whose
			// stamps are kept; an early tool time alone is dropped.
			if early(start) {
				start = nil
			}
			if early(stop) {
				stop = nil
			}
		}
	}
	out.Start, out.Stop = start, stop
	out.OperatorStarted, out.OperatorTerminated = opStarted, opTerminated
	return out
}
