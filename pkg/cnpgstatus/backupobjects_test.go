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
	"slices"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClassifyBackupPhase(t *testing.T) {
	tests := []struct {
		name  string
		minor Minor
		phase string
		want  BackupPhaseReading
	}{
		{"empty is unknown", Minor130, "", BackupPhaseReading{Class: ClassUnknown}},
		{"pending", Minor130, BackupPhasePending, BackupPhaseReading{Class: ClassProgressing, Known: true}},
		{"started", Minor130, BackupPhaseStarted, BackupPhaseReading{Class: ClassProgressing, Known: true}},
		{"running", Minor131, BackupPhaseRunning, BackupPhaseReading{Class: ClassProgressing, Known: true}},
		{"finalizing", Minor130, BackupPhaseFinalizing, BackupPhaseReading{Class: ClassProgressing, Known: true}},
		{"completed", Minor130, BackupPhaseCompleted, BackupPhaseReading{Class: ClassOK, Known: true, Terminal: true}},
		{"failed", Minor130, BackupPhaseFailed, BackupPhaseReading{Class: ClassCritical, Known: true, Terminal: true, Failed: true}},
		{"wal archiving failing is a dead end, not terminal", Minor130, BackupPhaseWalArchivingFailing, BackupPhaseReading{Class: ClassCritical, Known: true, Failed: true}},
		{"invalid definition on 1.30", Minor130, BackupPhaseDefinitionInvalid, BackupPhaseReading{Class: ClassCritical, Known: true, Failed: true}},
		{"foreign phase is a warning", Minor130, "Completed", BackupPhaseReading{Class: ClassWarning}},
		{"unsupported minor reads 1.30", "1.28", BackupPhaseDefinitionInvalid, BackupPhaseReading{Class: ClassCritical, Known: true, Failed: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyBackupPhase(tt.minor, tt.phase); got != tt.want {
				t.Errorf("ClassifyBackupPhase(%s, %q) = %+v, want %+v", tt.minor, tt.phase, got, tt.want)
			}
		})
	}
	if BackupPhaseTable("1.28") != nil {
		t.Error("unsupported minor has a table")
	}
	for _, minor := range SupportedMinors() {
		for _, row := range BackupPhaseTable(minor) {
			if row.Class == ClassOK && !row.Terminal {
				t.Errorf("%s: %q is ok but not terminal", minor, row.Phase)
			}
			if !row.Class.Valid() || row.Source == "" {
				t.Errorf("%s: %q has class %q / source %q", minor, row.Phase, row.Class, row.Source)
			}
		}
	}
}

func TestBackupObject(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC))
	started := metav1.NewTime(created.Add(time.Minute))
	now := created.Add(2 * time.Hour)
	plugin := &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", CreationTimestamp: created},
		Spec: cnpgv1.BackupSpec{
			Method:              cnpgv1.BackupMethodPlugin,
			PluginConfiguration: &cnpgv1.BackupPluginConfiguration{Name: BarmanCloudPlugin},
		},
		Status: cnpgv1.BackupStatus{
			Phase:                   BackupPhaseStarted,
			InstanceID:              &cnpgv1.InstanceID{PodName: "pg-2"},
			ReconciliationStartedAt: &started,
		},
	}
	got := BackupObject(Minor130, plugin, now)
	if got.Method != BackupMethodPlugin || got.PluginName != BarmanCloudPlugin || got.PodName != "pg-2" {
		t.Errorf("plugin backup fields: %+v", got)
	}
	if !slices.Contains(got.NotReportedWhileRunning, "status.startedAt") {
		t.Errorf("plugin backup in progress must list fields reported at completion: %v", got.NotReportedWhileRunning)
	}
	if got.CreatedAt == nil || got.ReconciliationStartedAt == nil || got.ReconciliationStartedAt.Sub(*got.CreatedAt) != time.Minute {
		t.Errorf("queue time: %v -> %v", got.CreatedAt, got.ReconciliationStartedAt)
	}
	plugin.Status.Phase = BackupPhaseCompleted
	if got := BackupObject(Minor130, plugin, now); got.NotReportedWhileRunning != nil {
		t.Errorf("completed plugin backup still lists missing fields: %v", got.NotReportedWhileRunning)
	}
	// A plugin that leaves stopped_at unset: CloudNativePG stamps
	// 1970-01-01 (round 18, CNPG P3); the reading has no stop time.
	epoch, stopped := metav1.NewTime(time.Unix(0, 0)), metav1.NewTime(created.Add(time.Hour))
	plugin.Status.StartedAt, plugin.Status.StoppedAt = &started, &epoch
	if got := BackupObject(Minor130, plugin, now); got.StoppedAt != nil || got.StartedAt == nil {
		t.Errorf("epoch stoppedAt: started %v stopped %v, want a start and no stop", got.StartedAt, got.StoppedAt)
	}
	plugin.Status.StoppedAt = &stopped
	if got := BackupObject(Minor130, plugin, now); got.StoppedAt == nil || !got.StoppedAt.Equal(stopped.Time) {
		t.Errorf("plausible stoppedAt lost: %v", got.StoppedAt)
	}
	// A plugin that sends milliseconds for seconds stamps the far
	// future (round 19, CNPG P3-1): no stop time, never a fresh one.
	future := metav1.NewTime(created.Add(1000 * 24 * time.Hour))
	plugin.Status.StoppedAt = &future
	if got := BackupObject(Minor130, plugin, now); got.StoppedAt != nil {
		t.Errorf("future stoppedAt read as true: %v", got.StoppedAt)
	}
	// A zero now (topology.go's callers read only the method and the
	// pod) sets no ceiling: the times are kept, not all dropped (round
	// 20, Go P3-2).
	plugin.Status.StoppedAt = &stopped
	if got := BackupObject(Minor130, plugin, time.Time{}); got.StoppedAt == nil || got.StartedAt == nil || got.ReconciliationStartedAt == nil {
		t.Errorf("zero now dropped the times: started %v stopped %v reconciliation %v", got.StartedAt, got.StoppedAt, got.ReconciliationStartedAt)
	}
	plugin.Status.StartedAt, plugin.Status.StoppedAt = nil, nil

	inTree := &cnpgv1.Backup{
		Spec:   cnpgv1.BackupSpec{Method: cnpgv1.BackupMethodBarmanObjectStore},
		Status: cnpgv1.BackupStatus{Phase: BackupPhaseFailed, Error: strings.Repeat("x", 5000), Online: new(true)},
	}
	got = BackupObject(Minor130, inTree, now)
	if got.Method != BackupMethodBarmanObjectStore || len(got.Error) != maxMessage || got.Online == nil || !*got.Online {
		t.Errorf("in-tree failed backup: method %q error %d online %v", got.Method, len(got.Error), got.Online)
	}
	if got.NotReportedWhileRunning != nil || got.Reading.Class != ClassCritical {
		t.Errorf("in-tree failed backup reading: %+v", got)
	}
	if got := BackupObject(Minor130, nil, now); got.Reading.Class != ClassUnknown {
		t.Errorf("nil backup: %+v", got)
	}
}

func TestSchedule(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time { v := metav1.NewTime(now.Add(d)); return &v }
	sb := func(schedule string, mutate func(*cnpgv1.ScheduledBackup)) *cnpgv1.ScheduledBackup {
		s := &cnpgv1.ScheduledBackup{
			Spec: cnpgv1.ScheduledBackupSpec{Schedule: schedule, Cluster: cnpgv1.LocalObjectReference{Name: "pg"}},
			Status: cnpgv1.ScheduledBackupStatus{
				LastCheckTime: at(-time.Hour), LastScheduleTime: at(-time.Hour), NextScheduleTime: at(time.Hour),
			},
		}
		if mutate != nil {
			mutate(s)
		}
		return s
	}
	tests := []struct {
		name      string
		in        *cnpgv1.ScheduledBackup
		state     string
		class     Class
		wantNote  bool
		wantField int
	}{
		{"six fields active", sb("0 0 2 * * *", nil), ScheduleActive, ClassOK, false, 6},
		{"descriptor", sb("@daily", nil), ScheduleActive, ClassOK, false, 1},
		{"five fields read seconds first", sb("0 2 * * *", nil), ScheduleActive, ClassOK, true, 5},
		{"seven fields invalid", sb("0 0 2 * * * *", nil), ScheduleInvalid, ClassWarning, true, 7},
		{"suspended is inactive", sb("0 0 2 * * *", func(s *cnpgv1.ScheduledBackup) { s.Spec.Suspend = new(true) }), ScheduleSuspended, ClassInactive, false, 6},
		{"admission error", sb("0 0 2 * * *", func(s *cnpgv1.ScheduledBackup) { s.Status.Error = "bad" }), ScheduleAdmissionFailed, ClassWarning, false, 6},
		{"never checked", sb("0 0 2 * * *", func(s *cnpgv1.ScheduledBackup) { s.Status = cnpgv1.ScheduledBackupStatus{} }), ScheduleAwaitingFirst, ClassProgressing, false, 6},
		{"overdue", sb("0 0 2 * * *", func(s *cnpgv1.ScheduledBackup) { s.Status.NextScheduleTime = at(-11 * time.Minute) }), ScheduleOverdue, ClassWarning, false, 6},
		{"within grace", sb("0 0 2 * * *", func(s *cnpgv1.ScheduledBackup) { s.Status.NextScheduleTime = at(-9 * time.Minute) }), ScheduleActive, ClassOK, false, 6},
		{"checked without next", sb("0 0 2 * * *", func(s *cnpgv1.ScheduledBackup) { s.Status.NextScheduleTime = nil }), ScheduleUnknown, ClassUnknown, false, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Schedule(tt.in, now)
			if got.State != tt.state || got.Class != tt.class || (got.ScheduleNote != "") != tt.wantNote || got.Fields != tt.wantField {
				t.Errorf("Schedule = %+v, want state %s class %s note %v fields %d", got, tt.state, tt.class, tt.wantNote, tt.wantField)
			}
			if got.Method != BackupMethodBarmanObjectStore {
				t.Errorf("default method = %q", got.Method)
			}
		})
	}
	if got := Schedule(nil, now); got.Class != ClassUnknown {
		t.Errorf("nil schedule: %+v", got)
	}
}

// backupPhaseGolden pins each minor's Backup phase table as a reviewed
// file.
type backupPhaseGolden struct {
	Minor  Minor              `json:"minor"`
	Phases []BackupPhaseEntry `json:"phases"`
}

func TestBackupPhaseTablesGolden(t *testing.T) {
	for _, minor := range SupportedMinors() {
		assertGolden(t, "backupphases-"+string(minor), backupPhaseGolden{Minor: minor, Phases: BackupPhaseTable(minor)})
	}
}
