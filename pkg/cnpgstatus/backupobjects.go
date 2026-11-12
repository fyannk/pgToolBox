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

package cnpgstatus

import (
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// Backup.status.phase values. They are untyped constants in
// api/v1/backup_types.go (v1.30.0:32-58, main:32-58);
// "invalid backup definition" is new in 1.30.
const (
	BackupPhasePending             = "pending"
	BackupPhaseStarted             = "started"
	BackupPhaseRunning             = "running"
	BackupPhaseFinalizing          = "finalizing"
	BackupPhaseCompleted           = "completed"
	BackupPhaseFailed              = "failed"
	BackupPhaseWalArchivingFailing = "walArchivingFailing"
	BackupPhaseDefinitionInvalid   = "invalid backup definition"
)

// Backup methods (api/v1/backup_types.go BackupMethod
// v1.30.0:114-122, main:114-122). spec.method defaults to
// barmanObjectStore in the CRD (+kubebuilder:default, v1.30.0:143-146).
const (
	BackupMethodBarmanObjectStore = "barmanObjectStore"
	BackupMethodVolumeSnapshot    = "volumeSnapshot"
	BackupMethodPlugin            = "plugin"
)

// BackupPhaseEntry is one row of a minor's Backup phase table.
type BackupPhaseEntry struct {
	Phase string `json:"phase"`
	Class Class  `json:"class"`
	// Terminal mirrors BackupStatus.IsDone (api/v1/backup_funcs.go
	// v1.30.0:100-102, identical at main): only
	// completed and failed. The backup controller returns early on those
	// two and never touches the object again (backup_controller.go
	// v1.30.0:154-157, main:157-160).
	Terminal bool `json:"terminal"`
	// Failed is true when the backup will not produce a usable backup
	// without a human: failed, and the two non-terminal dead ends.
	Failed bool   `json:"failed"`
	Since  Minor  `json:"since"`
	Source string `json:"source"`
}

// backupPhaseRows: line numbers are api/v1/backup_types.go at
// v1.30.0 and main (21e24f6).
var backupPhaseRows = []BackupPhaseEntry{
	{
		// "waiting to be started": queued behind another backup of the
		// cluster or a target Pod that is not ready (Warning event
		// BackupPending, backup_controller.go v1.30.0:325).
		Phase: BackupPhasePending, Class: ClassProgressing, Since: Minor130,
		Source: "api/v1/backup_types.go BackupPhasePending (v1.30.0:32, main:32)",
	},
	{
		// SetAsStarted (backup_funcs.go v1.30.0:74-84): the operator chose
		// the instance (status.instanceID) and handed the backup over.
		Phase: BackupPhaseStarted, Class: ClassProgressing, Since: Minor130,
		Source: "api/v1/backup_types.go BackupPhaseStarted (v1.30.0:35, main:35)",
	},
	{
		// Set by the instance's barman-cloud-backup path only
		// (pkg/management/postgres/backup.go v1.30.0:112-113, :318);
		// plugin backups never reach it (backup_controller.go
		// v1.30.0:276, main:273).
		Phase: BackupPhaseRunning, Class: ClassProgressing, Since: Minor130,
		Source: "api/v1/backup_types.go BackupPhaseRunning (v1.30.0:38, main:38)",
	},
	{
		// VolumeSnapshot backups: snapshot taken, waiting for readyToUse.
		Phase: BackupPhaseFinalizing, Class: ClassProgressing, Since: Minor130,
		Source: "api/v1/backup_types.go BackupPhaseFinalizing (v1.30.0:46, main:46)",
	},
	{
		Phase: BackupPhaseCompleted, Class: ClassOK, Terminal: true, Since: Minor130,
		Source: "api/v1/backup_types.go BackupPhaseCompleted (v1.30.0:49, main:49)",
	},
	{
		Phase: BackupPhaseFailed, Class: ClassCritical, Terminal: true, Failed: true, Since: Minor130,
		Source: "api/v1/backup_types.go BackupPhaseFailed (v1.30.0:52, main:52)",
	},
	{
		// Set by the instance when the WAL archive check fails before a
		// Barman backup (pkg/management/postgres/backup.go v1.30.0:106-109).
		// IsDone is false, but nothing moves the backup forward again:
		// the controller only starts a backup whose phase is empty or
		// pending (backup_controller.go v1.30.0:1115).
		Phase: BackupPhaseWalArchivingFailing, Class: ClassCritical, Failed: true, Since: Minor130,
		Source: "api/v1/backup_types.go BackupPhaseWalArchivingFailing (v1.30.0:55, main:55)",
	},
	{
		// SetAdmissionError (backup_funcs.go v1.30.0:328-343): the
		// definition failed validation; status.error carries why. Fixing
		// the spec clears the phase, so it is not terminal.
		Phase: BackupPhaseDefinitionInvalid, Class: ClassCritical, Failed: true, Since: Minor130,
		Source: "api/v1/backup_types.go BackupPhaseDefinitionInvalid (v1.30.0:58, main:58)",
	},
}

// BackupPhaseTable returns minor's Backup phase rows in source order; nil
// for an unsupported minor.
func BackupPhaseTable(minor Minor) []BackupPhaseEntry {
	if !minor.supported() {
		return nil
	}
	rows := make([]BackupPhaseEntry, 0, len(backupPhaseRows))
	for _, row := range backupPhaseRows {
		if minor.atLeast(row.Since) {
			rows = append(rows, row)
		}
	}
	return rows
}

// BackupPhaseReading is how the console renders one Backup phase.
type BackupPhaseReading struct {
	Class    Class `json:"class"`
	Known    bool  `json:"known"`
	Terminal bool  `json:"terminal"`
	Failed   bool  `json:"failed"`
}

// ClassifyBackupPhase classifies Backup.status.phase for minor. An empty
// phase is unknown (the operator has not reconciled the Backup yet, or
// is down). A phase outside the minor's table is a warning, shown
// verbatim. An unsupported minor is read against 1.30.
func ClassifyBackupPhase(minor Minor, phase string) BackupPhaseReading {
	if phase == "" {
		return BackupPhaseReading{Class: ClassUnknown}
	}
	if !minor.supported() {
		minor = newestReleased
	}
	for _, row := range BackupPhaseTable(minor) {
		if row.Phase == phase {
			return BackupPhaseReading{Class: row.Class, Known: true, Terminal: row.Terminal, Failed: row.Failed}
		}
	}
	return BackupPhaseReading{Class: ClassWarning}
}

// BackupObjectReading is one Backup object as the backups screen shows
// it (operator claim; the instance writes most of the status).
type BackupObjectReading struct {
	Phase   string             `json:"phase,omitempty"`
	Reading BackupPhaseReading `json:"reading"`
	// Method is status.method (written by SetAsStarted,
	// backup_funcs.go v1.30.0:82), falling back to spec.method.
	Method string `json:"method,omitempty"`
	// PluginName is spec.pluginConfiguration.name for a plugin backup.
	PluginName string `json:"pluginName,omitempty"`
	// PodName is status.instanceID.podName: the instance the operator
	// elected (backup_types.go v1.30.0:298, :321).
	PodName string `json:"podName,omitempty"`
	// Online is status.online; nil when not reported.
	Online   *bool  `json:"online,omitempty"`
	BackupID string `json:"backupId,omitempty"`
	// BackupName is status.backupName: Dalibo's pgBackRest plugin writes
	// the backup label there.
	BackupName string `json:"backupName,omitempty"`
	// Error is status.error (bounded): the failure, or the admission
	// error of an invalid definition.
	Error string `json:"error,omitempty"`
	// CreatedAt is the object's creation; ReconciliationStartedAt is
	// when the operator handed it to an instance (SetAsStarted);
	// StartedAt/StoppedAt are the tool's own times;
	// ReconciliationTerminatedAt is CloudNativePG's completion stamp, set
	// by SetAsCompleted/SetAsFailed (backup_funcs.go v1.30.0:46-71): the
	// instance manager's for an in-tree barman or plugin Backup it
	// completed or failed, else the operator's (a volume snapshot, or a
	// Backup the operator failed). Queue time is
	// ReconciliationStartedAt - CreatedAt.
	CreatedAt                  *time.Time `json:"createdAt,omitempty"`
	ReconciliationStartedAt    *time.Time `json:"reconciliationStartedAt,omitempty"`
	StartedAt                  *time.Time `json:"startedAt,omitempty"`
	StoppedAt                  *time.Time `json:"stoppedAt,omitempty"`
	ReconciliationTerminatedAt *time.Time `json:"reconciliationTerminatedAt,omitempty"`
	// NotReportedWhileRunning lists the status fields a plugin backup
	// fills only on completion (plugin_backup.go v1.30.0:129-149): the UI
	// renders them as "reported at completion", never as missing.
	NotReportedWhileRunning []string `json:"notReportedWhileRunning,omitempty"`
}

// pluginFieldsAtCompletion are filled by PluginBackupCommand only once
// the plugin returns (pkg/management/postgres/webserver/plugin_backup.go
// v1.30.0:129-149; same flow at main). Between "started" and
// "completed" a plugin backup shows none of them.
var pluginFieldsAtCompletion = []string{
	"status.startedAt", "status.stoppedAt", "status.backupId", "status.backupName",
	"status.beginWal", "status.endWal", "status.beginLSN", "status.endLSN", "status.online",
}

// BackupTimesOf are b's plausible times at now (observe.SaneBackupTimes):
// the one reading every console consumer of a Backup's times shares.
func BackupTimesOf(b *cnpgv1.Backup, now time.Time) observe.BackupTimes {
	if b == nil {
		return observe.BackupTimes{}
	}
	return observe.SaneBackupTimes(now, observe.BackupStamps{
		Created: b.CreationTimestamp.Time,
		Started: metaTime(b.Status.StartedAt), Stopped: metaTime(b.Status.StoppedAt),
		ReconciliationStarted:    metaTime(b.Status.ReconciliationStartedAt),
		ReconciliationTerminated: metaTime(b.Status.ReconciliationTerminatedAt),
	})
}

// BackupObject reads one Backup at now (its times through BackupTimesOf:
// an impossible one, 1970 or in the future, is not reported; a zero now
// checks no future). A nil Backup is unknown.
func BackupObject(minor Minor, b *cnpgv1.Backup, now time.Time) BackupObjectReading {
	if b == nil {
		return BackupObjectReading{Reading: BackupPhaseReading{Class: ClassUnknown}}
	}
	status := b.Status
	reading := BackupObjectReading{
		Phase:      bound(string(status.Phase), maxAnnotationValue),
		Reading:    ClassifyBackupPhase(minor, string(status.Phase)),
		Method:     bound(string(status.Method), maxAnnotationValue),
		Online:     status.Online,
		BackupID:   bound(status.BackupID, maxAnnotationValue),
		BackupName: bound(status.BackupName, maxAnnotationValue),
		Error:      bound(status.Error, maxMessage),
		CreatedAt:  metaTime(&b.CreationTimestamp),
	}
	times := BackupTimesOf(b, now)
	reading.StartedAt, reading.StoppedAt = times.Start, times.Stop
	reading.ReconciliationStartedAt, reading.ReconciliationTerminatedAt = times.OperatorStarted, times.OperatorTerminated
	if reading.Method == "" {
		reading.Method = bound(string(b.Spec.Method), maxAnnotationValue)
	}
	if b.Spec.PluginConfiguration != nil {
		reading.PluginName = bound(b.Spec.PluginConfiguration.Name, maxAnnotationValue)
	}
	if status.InstanceID != nil {
		reading.PodName = bound(status.InstanceID.PodName, maxAnnotationValue)
	}
	if reading.Method == BackupMethodPlugin && !reading.Reading.Terminal {
		reading.NotReportedWhileRunning = append([]string(nil), pluginFieldsAtCompletion...)
	}
	return reading
}

// metaTime converts an API time to UTC; nil or zero is nil.
func metaTime(t *metav1.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// Schedule states.
const (
	ScheduleActive          = "active"
	ScheduleSuspended       = "suspended"
	ScheduleAwaitingFirst   = "awaitingFirstCheck"
	ScheduleOverdue         = "overdue"
	ScheduleInvalid         = "invalidSchedule"
	ScheduleAdmissionFailed = "admissionError"
	ScheduleUnknown         = "unknown"
)

// scheduleOverdueGrace is how long past status.nextScheduleTime the
// console waits before calling a schedule overdue. The controller requeues
// at nextScheduleTime and, while a Backup of the same ScheduledBackup is
// not done, every minute (scheduledbackup_controller.go v1.30.0:124-131):
// an inference, not an operator fact.
const scheduleOverdueGrace = 10 * time.Minute

// ScheduleReading is one ScheduledBackup.
type ScheduleReading struct {
	Class Class  `json:"class"`
	State string `json:"state"`
	// Schedule is spec.schedule verbatim (bounded); Fields its field
	// count. CNPG parses it with robfig/cron v1 cron.Parse
	// (scheduledbackup_controller.go v1.30.0:146), whose parser is
	// Second|Minute|Hour|Dom|Month|DowOptional|Descriptor
	// (robfig/cron v1.2.0 parser.go:174-176): six fields, seconds
	// first, or five read as "sec min hour dom month" with day of week *.
	Schedule string `json:"schedule,omitempty"`
	Fields   int    `json:"fields,omitempty"`
	// ScheduleNote explains a schedule that does not read as written.
	ScheduleNote string `json:"scheduleNote,omitempty"`
	Suspended    bool   `json:"suspended"`
	Immediate    bool   `json:"immediate"`
	Method       string `json:"method,omitempty"`
	Target       string `json:"target,omitempty"`
	Cluster      string `json:"cluster,omitempty"`
	// LastCheckTime, LastScheduleTime, NextScheduleTime are the status
	// fields (scheduledbackup_types.go v1.30.0:91-99).
	LastCheckTime    *time.Time `json:"lastCheckTime,omitempty"`
	LastScheduleTime *time.Time `json:"lastScheduleTime,omitempty"`
	NextScheduleTime *time.Time `json:"nextScheduleTime,omitempty"`
	// Error is status.error, the latest admission error (1.30+,
	// scheduledbackup_types.go v1.30.0:103).
	Error string `json:"error,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Schedule reads a ScheduledBackup at now (UTC, injected).
//
//   - spec.suspend: inactive; the controller returns before scheduling
//     (scheduledbackup_controller.go v1.30.0:111, main:111).
//   - status.error (1.30+): warning, the admission guard refused it.
//   - a schedule that robfig/cron would not parse (field count outside
//     5-6 and not a "@" descriptor): warning, nothing is scheduled.
//   - no lastCheckTime: progressing (not reconciled yet).
//   - nextScheduleTime more than 10 minutes in the past: warning
//     (console inference: operator down or a Backup of this schedule
//     still running).
//   - otherwise ok; a five-field schedule is ok with a ScheduleNote,
//     because the webhook only warns about it
//     (internal/webhook/v1/scheduledbackup_webhook.go
//     v1.30.0:143, main:143).
//
// A nil ScheduledBackup is unknown.
func Schedule(sb *cnpgv1.ScheduledBackup, now time.Time) ScheduleReading {
	if sb == nil {
		return ScheduleReading{Class: ClassUnknown, State: ScheduleUnknown}
	}
	reading := ScheduleReading{
		Schedule:         bound(sb.Spec.Schedule, maxAnnotationValue),
		Suspended:        sb.Spec.Suspend != nil && *sb.Spec.Suspend,
		Immediate:        sb.Spec.Immediate != nil && *sb.Spec.Immediate,
		Method:           bound(string(sb.Spec.Method), maxAnnotationValue),
		Target:           bound(string(sb.Spec.Target), maxAnnotationValue),
		Cluster:          bound(sb.Spec.Cluster.Name, maxAnnotationValue),
		LastCheckTime:    metaTime(sb.Status.LastCheckTime),
		LastScheduleTime: metaTime(sb.Status.LastScheduleTime),
		NextScheduleTime: metaTime(sb.Status.NextScheduleTime),
		Error:            bound(sb.Status.Error, maxMessage),
	}
	if reading.Method == "" {
		reading.Method = BackupMethodBarmanObjectStore
	}
	valid := true
	if strings.HasPrefix(strings.TrimSpace(sb.Spec.Schedule), "@") {
		reading.Fields = 1
	} else {
		reading.Fields = len(strings.Fields(sb.Spec.Schedule))
		switch reading.Fields {
		case 6:
		case 5:
			reading.ScheduleNote = "five fields are read seconds first (second minute hour day-of-month month, any day of week): " +
				"\"0 2 * * *\" runs at minute 2 of every hour, not at 02:00"
		default:
			valid = false
			reading.ScheduleNote = "CloudNativePG schedules have six fields, seconds first"
		}
	}
	switch {
	case reading.Suspended:
		reading.Class, reading.State = ClassInactive, ScheduleSuspended
		reading.Text = "suspended: no backup is scheduled"
	case reading.Error != "":
		reading.Class, reading.State = ClassWarning, ScheduleAdmissionFailed
		reading.Text = "the operator refused this schedule: " + reading.Error
	case !valid:
		reading.Class, reading.State = ClassWarning, ScheduleInvalid
		reading.Text = "the schedule does not parse: no backup is scheduled"
	case reading.LastCheckTime == nil:
		reading.Class, reading.State = ClassProgressing, ScheduleAwaitingFirst
		reading.Text = "not checked by the operator yet"
	case reading.NextScheduleTime != nil && now.UTC().After(reading.NextScheduleTime.Add(scheduleOverdueGrace)):
		reading.Class, reading.State = ClassWarning, ScheduleOverdue
		reading.Text = "the next scheduled backup time has passed: the operator may be down, or a backup of this schedule is still running"
	case reading.NextScheduleTime == nil:
		reading.Class, reading.State = ClassUnknown, ScheduleUnknown
		reading.Text = "checked but no next schedule time is reported"
	default:
		reading.Class, reading.State = ClassOK, ScheduleActive
	}
	return reading
}
