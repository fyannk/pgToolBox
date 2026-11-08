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
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	cnpgevidence "github.com/fyannk/pgToolBox/pkg/cnpgstatus/evidence"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	pbFull = "20261107-081054F"
	pbIncr = "20261107-081054F_20261107-091108I"
)

// pgbackrestItems is a full and an incremental on it, oldest first as
// the evidence runner keeps them; the incremental carries Opera's
// cnpg-backup-name annotation.
func pgbackrestItems() []evidence.BackupItem {
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	full, opera := pbFull, "orders-manual"
	return []evidence.BackupItem{
		{ID: pbFull, State: evidencev1alpha1.Healthy, BeginAt: at(3 * time.Hour), EndAt: at(170 * time.Minute), PgBackRest: &evidence.PgBackRestItem{
			Stanza: "orders", Type: evidencev1alpha1.PGBackRestFull, Usable: true, Timeline: new(uint64(1)), SizeBytes: 100, RepositorySizeBytes: 40,
		}},
		{ID: pbIncr, State: evidencev1alpha1.Warning, BeginAt: at(2 * time.Hour), EndAt: at(110 * time.Minute), PgBackRest: &evidence.PgBackRestItem{
			Stanza: "orders", Type: evidencev1alpha1.PGBackRestIncremental, Prior: &full, References: []string{pbFull}, Usable: false,
			UnusableReasons: []string{evidencev1alpha1.UnusableWALToConsistency}, CNPGBackupName: &opera, SizeBytes: 10, RepositorySizeBytes: 4,
		}},
	}
}

func TestPgBackRestBackupKey(t *testing.T) {
	scanned := now.Add(-time.Minute)
	before := now.Add(-time.Hour)
	e := clusterEvidence{result: evidence.Result{
		Snapshot:  &evidencev1alpha1.RepositoryEvidenceSnapshot{CompletedAt: &scanned},
		BackupIDs: map[string]bool{pbFull: true, pbIncr: true}, BackupsComplete: true, Backups: pgbackrestItems(),
	}}
	tests := []struct {
		name      string
		object    string
		r         cnpgstatus.BackupObjectReading
		presence  viewmodel.RepositoryPresence
		wantLabel string
	}{
		{"Dalibo reads status.backupName", "orders-nightly", cnpgstatus.BackupObjectReading{PluginName: cnpgstatus.PgBackRestDaliboPlugin, BackupName: pbFull, BackupID: "ignored", StoppedAt: &before}, viewmodel.RepositoryPresent, pbFull},
		{"Dalibo without backupName is not checked", "orders-pending", cnpgstatus.BackupObjectReading{PluginName: cnpgstatus.PgBackRestDaliboPlugin, BackupID: pbFull}, viewmodel.RepositoryNotChecked, ""},
		{"Opera reads status.backupId", "orders-other", cnpgstatus.BackupObjectReading{PluginName: cnpgstatus.PgBackRestOperaPlugin, BackupID: pbIncr, StoppedAt: &before}, viewmodel.RepositoryPresent, pbIncr},
		{"Opera falls back to the cnpg-backup-name annotation", "orders-manual", cnpgstatus.BackupObjectReading{PluginName: cnpgstatus.PgBackRestOperaPlugin}, viewmodel.RepositoryNotChecked, pbIncr},
		{"a label absent from a complete scan", "orders-lost", cnpgstatus.BackupObjectReading{PluginName: cnpgstatus.PgBackRestOperaPlugin, BackupID: "20261101-000000F", StoppedAt: &before}, viewmodel.RepositoryAbsent, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.presence(tt.r); got != tt.presence {
				t.Errorf("presence %s, want %s", got, tt.presence)
			}
			ref := e.pgbackrestRef(tt.object, tt.r)
			switch {
			case tt.wantLabel == "" && ref != nil:
				t.Errorf("ref %+v, want none", ref)
			case tt.wantLabel != "" && (ref == nil || ref.Label != tt.wantLabel):
				t.Errorf("ref %+v, want %s", ref, tt.wantLabel)
			case ref != nil && ref.Full != pbFull:
				t.Errorf("chain head %q, want %q", ref.Full, pbFull)
			}
		})
	}
}

func TestPgBackRestEvidenceJoinsBackupObjects(t *testing.T) {
	end := now.Add(-time.Minute)
	summary := evidencev1alpha1.PGBackRestSummary{
		Stanza: "orders", ArchiveStanza: "orders-archive", Repository: 2, Encrypted: new(true),
		Catalog: evidencev1alpha1.StateReason{State: evidencev1alpha1.Healthy},
		RecoveryWindow: &evidencev1alpha1.PGBackRestRecoveryWindow{
			OldestBackup: pbFull, StartAt: now.Add(-170 * time.Minute), NewestBackup: pbFull, NewestStopAt: now.Add(-170 * time.Minute),
			EndTimeline: 1, EndWAL: "000000010000000000000009", EndReceiptAt: &end, UsableBackups: 1,
		},
		Findings: []evidencev1alpha1.PGBackRestFinding{
			{Code: "StanzaSplit", Severity: evidencev1alpha1.FindingWarning, Subject: "orders-archive"},
			{Code: "ChecksumMismatch", Severity: evidencev1alpha1.FindingError, Subject: "backup/orders/" + pbFull + "/backup.manifest"},
		},
		Retention: pgbackrestRetention(),
	}
	dalibo := &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-nightly"},
		Spec:       cnpgv1.BackupSpec{PluginConfiguration: &cnpgv1.BackupPluginConfiguration{Name: cnpgstatus.PgBackRestDaliboPlugin}},
		Status:     cnpgv1.BackupStatus{BackupName: pbFull},
	}
	opera := &cnpgv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "orders-manual"}}
	got := pgbackrestEvidence(summary, pgbackrestItems(), []*cnpgv1.Backup{dalibo, opera})
	if len(got.Backups) != 2 || got.Backups[0].Label != pbIncr || got.Backups[1].Label != pbFull {
		t.Fatalf("backups %+v, want the incremental first", got.Backups)
	}
	incr, full := got.Backups[0], got.Backups[1]
	if incr.Full != pbFull || incr.Prior != pbFull || incr.References != 1 || incr.Usable || incr.BackupName != "orders-manual" {
		t.Errorf("incremental row %+v", incr)
	}
	if full.Full != pbFull || full.BackupName != "orders-nightly" || !full.Usable {
		t.Errorf("full row %+v", full)
	}
	if got.RecoveryWindow == nil || got.RecoveryWindow.NewestBackup != pbFull || got.RecoveryWindow.EndWAL != "000000010000000000000009" {
		t.Errorf("window %+v", got.RecoveryWindow)
	}
	if len(got.Findings) != 2 || got.Findings[0].Health != viewmodel.HealthWarning || got.Findings[1].Health != viewmodel.HealthCritical {
		t.Errorf("findings %+v", got.Findings)
	}
	if got.Repository != 2 || got.Encrypted == nil || !*got.Encrypted || got.ArchiveStanza != "orders-archive" {
		t.Errorf("identity %+v", got)
	}
	r := got.Retention
	if r == nil || !r.Configured || r.Full == nil || *r.Full != 2 || r.FullType != "count" || r.Diff != nil || r.EffectiveArchive == nil || *r.EffectiveArchive != 2 ||
		r.FullBackups != 1 || r.IncrementalBackups != 1 || r.EvaluatedAt == nil || r.EvaluatedAt.Location() != time.UTC ||
		r.Result.State != "warning" || r.Result.Health != viewmodel.HealthWarning || r.Result.Code != "retention-warning" {
		t.Errorf("retention %+v", r)
	}
	summary.Retention = nil
	if got := pgbackrestEvidence(summary, nil, nil); got.Retention != nil {
		t.Errorf("retention without the agent's block: %+v", got.Retention)
	}
}

// pgbackrestRetention is a stanza that keeps two fulls and still holds WAL
// expire would have removed.
func pgbackrestRetention() *evidencev1alpha1.PGBackRestRetention {
	evaluated := now.Add(-110 * time.Minute).In(time.FixedZone("CET", 3600))
	return &evidencev1alpha1.PGBackRestRetention{
		Configured: true, Full: new(uint64(2)), FullType: evidencev1alpha1.RetentionFullCount, ArchiveType: "full", EffectiveArchive: new(uint64(2)),
		FullBackups: 1, IncrementalBackups: 1, EvaluatedAt: &evaluated,
		State: evidencev1alpha1.Warning, Reason: evidencev1alpha1.Reason{Code: "retention-warning", Message: "the stanza holds backups or WAL the retention expires: expire is not applying it"},
	}
}

// TestPgBackRestRetentionFacts: the agent's retention block reaches the
// format-neutral facts the diagnostics read and the repository's
// retention fact; without it both say the comparison was not made.
func TestPgBackRestRetentionFacts(t *testing.T) {
	usable := uint64(1)
	summary := evidencev1alpha1.PGBackRestSummary{Stanza: "orders", UsableBackups: &usable, Findings: []evidencev1alpha1.PGBackRestFinding{
		{Code: "RetentionNotApplied", Severity: evidencev1alpha1.FindingWarning, Subject: "archive/orders/17-1/000000010000000000000001"},
	}}
	if f := pgbackrestFacts(summary); f.Retention != nil {
		t.Fatalf("facts without a retention block: %+v", f.Retention)
	}
	summary.Retention = pgbackrestRetention()
	f := pgbackrestFacts(summary)
	if f.Retention == nil || f.Retention.Result.State != "warning" || f.Retention.Result.Code != "retention-warning" || !f.Retention.MinimumConfigured ||
		f.Retention.VisibleBackups == nil || *f.Retention.VisibleBackups != 2 || f.Retention.StructurallyUsableBackups != &usable ||
		f.Retention.PGBackRest == nil || *f.Retention.PGBackRest.Full != 2 || f.Retention.PGBackRest.FullType != "count" {
		t.Fatalf("facts %+v", f.Retention)
	}
	if !f.HasFinding("RetentionNotApplied") {
		t.Errorf("findings %+v", f.Findings)
	}
	for _, tt := range []struct {
		name      string
		retention *evidencev1alpha1.PGBackRestRetention
		state     string
		code      string
	}{
		{"no retention given", nil, "unknown", retentionNotEvaluated},
		{"the agent's comparison", pgbackrestRetention(), "warning", "retention-warning"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			summary.Retention = tt.retention
			snap := &evidencev1alpha1.RepositoryEvidenceSnapshot{State: evidencev1alpha1.Healthy, Details: evidencev1alpha1.Details{PGBackRest: &summary}}
			svc := New(Options{Evidence: fakeEvidence{results: map[string]evidence.Result{"orders": {Agent: "orders-evidence", State: viewmodel.EvidenceLive, Snapshot: snap}}}, Now: func() time.Time { return now }})
			g := &gathered{}
			g.key.Namespace, g.key.Name = "team-a", "orders"
			got := svc.evidenceFor(t.Context(), g).view.Repository
			if got == nil || got.Retention.State != tt.state || got.Retention.Code != tt.code {
				t.Fatalf("repository retention %+v", got)
			}
		})
	}
}

func TestPgBackRestMethod(t *testing.T) {
	cfg := &cnpgstatus.PgBackRestConfig{
		Plugin: cnpgstatus.PgBackRestDaliboPlugin, Flavor: cnpgstatus.PgBackRestFlavorDalibo, Display: "pgBackRest (Dalibo)",
		ObjectKind: cnpgstatus.PgBackRestKindStanza, ObjectName: "orders-stanza",
	}
	if got := pgbackrestMethod(nil, nil); got != nil {
		t.Errorf("no plugin: %+v", got)
	}
	bare := pgbackrestMethod(cfg, nil)
	if bare.Stanza != "" || bare.Repository != nil || bare.ObjectName != "orders-stanza" {
		t.Errorf("without evidence the Dalibo stanza is unknown: %+v", bare)
	}
	snap := &evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{PGBackRest: &evidencev1alpha1.PGBackRestSummary{
		Stanza: "orders", ArchiveStanza: "orders", Repository: 1,
	}}}
	fed := pgbackrestMethod(cfg, snap)
	if fed.Stanza != "orders" || fed.ArchiveStanza != "" || fed.Repository == nil || *fed.Repository != 1 {
		t.Errorf("with evidence: %+v", fed)
	}
}

func TestPgBackRestRecoveryWindow(t *testing.T) {
	start, stop := now.Add(-3*time.Hour), now.Add(-2*time.Hour)
	receipt := now.Add(-time.Minute)
	observed := now.Add(-30 * time.Second)
	snap := &evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{PGBackRest: &evidencev1alpha1.PGBackRestSummary{
		Stanza: "orders", RecoveryWindow: &evidencev1alpha1.PGBackRestRecoveryWindow{StartAt: start, NewestStopAt: stop, EndReceiptAt: &receipt},
	}}}
	for _, tt := range []struct {
		name      string
		r         evidence.Result
		wantLast  *time.Time
		wantStale bool
	}{
		{"live window ends at the last receipt", evidence.Result{Agent: "orders-evidence", State: viewmodel.EvidenceLive, ObservedAt: &observed, Snapshot: snap}, &receipt, false},
		{"a stale snapshot is marked", evidence.Result{Agent: "orders-evidence", State: viewmodel.EvidenceStale, Reason: "agent unreachable", Snapshot: snap}, &receipt, true},
		{"no snapshot, no window", evidence.Result{State: viewmodel.EvidenceUnavailable}, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := New(Options{Evidence: fakeEvidence{results: map[string]evidence.Result{"orders": tt.r}}, Now: func() time.Time { return now }})
			g := &gathered{}
			g.key.Namespace, g.key.Name = "team-a", "orders"
			got := svc.pgbackrestWindow(g)
			if tt.wantLast == nil {
				if got != nil {
					t.Fatalf("window %+v, want none", got)
				}
				return
			}
			if got == nil || !got.First.Equal(start) || !got.Last.Equal(*tt.wantLast) || got.Provenance.Stale != tt.wantStale {
				t.Fatalf("window %+v", got)
			}
		})
	}
}

func TestPgBackRestDrillTarget(t *testing.T) {
	stop := now.Add(-2 * time.Hour)
	snap := &evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{PGBackRest: &evidencev1alpha1.PGBackRestSummary{
		Stanza: "orders", RecoveryWindow: &evidencev1alpha1.PGBackRestRecoveryWindow{NewestBackup: pbIncr, NewestStopAt: stop},
	}}}
	noWindow := &evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{PGBackRest: &evidencev1alpha1.PGBackRestSummary{Stanza: "orders"}}}
	agentStale := *snap
	agentStale.Stale = true
	for _, tt := range []struct {
		name      string
		r         evidence.Result
		wantLabel string
	}{
		{"live window", evidence.Result{State: viewmodel.EvidenceLive, Snapshot: snap, PgBackRestPlugin: "p", PgBackRestObject: "o"}, pbIncr},
		{"a stale snapshot is not trusted", evidence.Result{State: viewmodel.EvidenceStale, Snapshot: snap}, ""},
		{"a live read of a snapshot the agent marks stale is not trusted", evidence.Result{State: viewmodel.EvidenceLive, Snapshot: &agentStale}, ""},
		{"no window", evidence.Result{State: viewmodel.EvidenceLive, Snapshot: noWindow}, ""},
		{"a Barman snapshot", evidence.Result{State: viewmodel.EvidenceLive, Snapshot: &evidencev1alpha1.RepositoryEvidenceSnapshot{}}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := New(Options{Evidence: fakeEvidence{results: map[string]evidence.Result{"orders": tt.r}}, Now: func() time.Time { return now }})
			got, why := svc.PgBackRestTarget("team-a", "orders")
			if got.Label != tt.wantLabel || (tt.wantLabel == "" && why == "") {
				t.Fatalf("target %+v, why %q", got, why)
			}
			if tt.wantLabel != "" && (got.Stanza != "orders" || got.StoppedAt == nil || !got.StoppedAt.Equal(stop) || got.Plugin != "p" || got.Object != "o") {
				t.Errorf("target %+v", got)
			}
		})
	}
}

func TestWALCountFactsCopiesEveryField(t *testing.T) {
	if walCountFacts[evidencev1alpha1.PGBackRestWALCounts](nil) != nil {
		t.Fatal("no counts must project to nil")
	}
	in := evidencev1alpha1.PGBackRestWALCounts{Segment: 1, Partial: 2, History: 3, BackupHistory: 4, Unknown: 5, Duplicate: 6}
	got := walCountFacts(&in)
	want := cnpgevidence.WALCountFacts{Segment: 1, Partial: 2, History: 3, BackupHistory: 4, Unknown: 5, Duplicate: 6}
	if got == nil || *got != want {
		t.Fatalf("pgBackRest counts = %+v, want %+v", got, want)
	}
	barman := evidencev1alpha1.BarmanWALCounts(in)
	if got := walCountFacts(&barman); got == nil || *got != want {
		t.Fatalf("barman counts = %+v, want %+v", got, want)
	}
}

func TestU64VClampsLikeU64(t *testing.T) {
	for _, v := range []uint64{0, 7, 1 << 62, 1<<63 + 1, ^uint64(0)} {
		if got, want := u64v(v), *u64(&v); got != want || got < 0 {
			t.Fatalf("u64v(%d) = %d, u64 = %d", v, got, want)
		}
	}
}
