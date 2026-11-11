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

package evidence

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func TestRunnerReadsRecoveryCoverage(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	run := func(t *testing.T, script func(*fakeAgent, *evidencev1alpha1.RepositoryEvidenceSnapshot)) (Result, *fakeAgent) {
		t.Helper()
		fake := newFakeAgent(t, ca)
		snapshot := fixtureSnapshot(t)
		fake.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
		fake.recovery = fixtureRecovery(t, snapshot)
		script(fake, &snapshot)
		fake.snapshot = snapshot
		object := testEvidenceAgent(testAgent, ca, snapshot, testNow)
		runner := newTestRunner(t, newFakeLister(t, object), fake, writeToken(t, testToken))
		runner.refresh(context.Background())
		return runner.ForCluster(testNamespace, testCluster), fake
	}

	t.Run("the publication's paths and backup items are kept", func(t *testing.T) {
		got, _ := run(t, func(*fakeAgent, *evidencev1alpha1.RepositoryEvidenceSnapshot) {})
		if got.State != viewmodel.EvidenceLive || got.Recovery.State != RecoveryAvailable || !got.Recovery.Complete || len(got.Recovery.Paths) != 1 {
			t.Fatalf("%+v", got.Recovery)
		}
		p := got.Recovery.Paths[0]
		if p.Stop != evidencev1alpha1.CoverageFrontier || p.LowerBoundAt == nil || p.FrontierReceiptAt == nil || len(p.Assumptions) == 0 {
			t.Errorf("path = %+v", p)
		}
		if len(got.Backups) != 1 || got.Backups[0].ID != "b1" {
			t.Errorf("backups = %+v", got.Backups)
		}
		if p.StartTimeline == nil || p.FrontierTimeline == nil || p.FrontierPosition == nil || *p.FrontierPosition != 2 {
			t.Errorf("path positions = %+v", p)
		}
		b := got.Backups[0]
		if b.EndTimeline == nil || *b.EndTimeline != 1 || b.EndPosition == nil || *b.EndPosition != 2 {
			t.Errorf("backup end = %v %v", b.EndTimeline, b.EndPosition)
		}
		if got.RefreshInterval != DefaultAgentRefreshInterval {
			t.Errorf("refresh interval = %s", got.RefreshInterval)
		}
	})

	t.Run("a transient failure keeps the last good paths, stale", func(t *testing.T) {
		fake := newFakeAgent(t, ca)
		snapshot := fixtureSnapshot(t)
		fake.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
		fake.recovery = fixtureRecovery(t, snapshot)
		fake.snapshot = snapshot
		object := testEvidenceAgent(testAgent, ca, snapshot, testNow)
		runner := newTestRunner(t, newFakeLister(t, object), fake, writeToken(t, testToken))
		runner.refresh(context.Background())
		first := runner.ForCluster(testNamespace, testCluster).Recovery.ScannedAt
		if first == nil || snapshot.CompletedAt == nil || !first.Equal(*snapshot.CompletedAt) {
			t.Fatalf("fresh paths scanned at %v, want the snapshot's completedAt %v", first, snapshot.CompletedAt)
		}
		fake.recovery = nil
		later := snapshot.CompletedAt.Add(time.Hour)
		fake.snapshot.CompletedAt = &later
		runner.refresh(context.Background())
		result := runner.ForCluster(testNamespace, testCluster)
		got := result.Recovery
		if got.State != RecoveryAvailable || !got.Stale || got.StaleReason == "" || len(got.Paths) != 1 {
			t.Errorf("%+v", got)
		}
		// Round 10, PG P2: the kept paths keep their own scan time beside
		// the fresher snapshot, so the timeline's purge band is timed from
		// it.
		if got.ScannedAt == nil || !got.ScannedAt.Equal(*first) || result.Snapshot == nil || result.Snapshot.CompletedAt == nil || !result.Snapshot.CompletedAt.Equal(later) {
			t.Errorf("kept paths scanned at %v beside snapshot %v, want %v beside %v", got.ScannedAt, result.Snapshot.CompletedAt, first, later)
		}
	})

	t.Run("an agent without the collection keeps the rest of its evidence", func(t *testing.T) {
		got, _ := run(t, func(a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) { a.recovery = nil })
		if got.State != viewmodel.EvidenceLive || got.Recovery.State != RecoveryUnavailable || got.Recovery.Reason == "" || got.Snapshot == nil {
			t.Errorf("%+v", got)
		}
	})

	t.Run("an unsupported capability is not asked for", func(t *testing.T) {
		got, fake := run(t, func(_ *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			for i := range s.Capabilities {
				if s.Capabilities[i].ID == evidencev1alpha1.RecoveryCoverage {
					s.Capabilities[i].Support, s.Capabilities[i].State = evidencev1alpha1.Unsupported, evidencev1alpha1.Unknown
				}
			}
		})
		if got.Recovery.State != RecoveryUnsupported || fake.recoveryRequests != 0 {
			t.Errorf("%+v after %d requests", got.Recovery, fake.recoveryRequests)
		}
	})

	t.Run("a publication change is retried from the snapshot", func(t *testing.T) {
		got, fake := run(t, func(a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) { a.recoveryConflictOnce = true })
		if got.Recovery.State != RecoveryAvailable || fake.recoveryRequests != 2 || fake.snapshots != 2 {
			t.Errorf("%+v after %d recovery and %d snapshot requests", got.Recovery, fake.recoveryRequests, fake.snapshots)
		}
	})

	t.Run("a page of another publication is refused", func(t *testing.T) {
		got, _ := run(t, func(a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) { a.recovery.EvidenceGeneration++ })
		if got.Recovery.State != RecoveryUnavailable || len(got.Recovery.Paths) != 0 {
			t.Errorf("%+v", got.Recovery)
		}
	})
}

func TestKeepRecovery(t *testing.T) {
	snapshot := fixtureSnapshot(t)
	scanned := testNow
	good := Recovery{State: RecoveryAvailable, Complete: true, Paths: []RecoveryPath{{BackupID: "b1", State: evidencev1alpha1.Healthy}}, ScannedAt: &scanned}
	previous := Result{Snapshot: &snapshot, Recovery: good}
	failed := func(after time.Duration) Recovery {
		at := scanned.Add(after)
		return Recovery{State: RecoveryUnavailable, Reason: "timeout", ScannedAt: &at}
	}
	moved := snapshot
	moved.Identity.Repository.DestinationFingerprint = "sha256:another"
	cases := []struct {
		name     string
		previous Result
		current  Recovery
		snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot
		stale    bool
		state    string
	}{
		{"a failure keeps the last good paths", previous, failed(time.Hour), &snapshot, true, RecoveryAvailable},
		{"a fresh read replaces them", previous, Recovery{State: RecoveryAvailable, Complete: true}, &snapshot, false, RecoveryAvailable},
		{"unsupported is definitive", previous, Recovery{State: RecoveryUnsupported}, &snapshot, false, RecoveryUnsupported},
		{"nothing to keep", Result{Snapshot: &snapshot}, failed(0), &snapshot, false, RecoveryUnavailable},
		// Round 11, PG: paths of another repository are not this one's.
		{"another repository's paths are not kept", previous, failed(time.Hour), &moved, false, RecoveryUnavailable},
		// Round 11, PG: a failure that persists does not keep old paths.
		{"paths older than three refresh intervals are dropped", previous, failed(6*time.Hour + time.Second), &snapshot, false, RecoveryUnavailable},
		{"paths three refresh intervals old are kept", previous, failed(6 * time.Hour), &snapshot, true, RecoveryAvailable},
	}
	for _, c := range cases {
		got := keepRecovery(c.previous, c.current, c.snapshot, nil, 2*time.Hour)
		if got.Stale != c.stale || got.State != c.state {
			t.Errorf("%s: %+v", c.name, got)
		}
		if c.state == RecoveryUnavailable && len(got.Paths) != 0 {
			t.Errorf("%s: paths kept: %+v", c.name, got.Paths)
		}
	}
	// The fresh snapshot's timeline warning and coverage reason replace
	// the kept ones.
	warned := previous
	warned.Recovery.TimelineWarning, warned.Recovery.CoverageReason = "00000003.history is invalid", "an old roll-up"
	if got := keepRecovery(warned, failed(time.Hour), &snapshot, nil, 2*time.Hour); got.TimelineWarning != "" || got.CoverageReason != "" {
		t.Errorf("the kept reasons survive the fresh snapshot: %+v", got)
	}
	// A short refresh interval keeps them an hour (keptFloor), no longer.
	if got := keepRecovery(previous, failed(time.Hour), &snapshot, nil, time.Minute); !got.Stale {
		t.Errorf("an hour with a one-minute interval: %+v", got)
	}
	if got := keepRecovery(previous, failed(time.Hour+time.Second), &snapshot, nil, time.Minute); got.Stale || len(got.Paths) != 0 {
		t.Errorf("past an hour with a one-minute interval: %+v", got)
	}
	if got := keepRecovery(previous, failed(7*time.Hour), &snapshot, nil, 2*time.Hour); !strings.Contains(got.Reason, reasonKeptTooOld) || !strings.HasPrefix(got.Reason, "timeout") {
		t.Errorf("dropped paths say %q", got.Reason)
	}
}

// Round 11, PG P2: the paths kept when reading them failed are capped
// against the fresh snapshot beside them, not only the one they were
// read with. After a failover whose timeline conclusion is not
// established, a kept path is no coverage: unknown, no frontier receipt,
// still stale, its scan time kept; the fresh snapshot's coverage reason
// and timeline warning replace the old ones.
func TestKeptRecoveryIsCappedByTheFreshSnapshot(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	for _, state := range []evidencev1alpha1.State{evidencev1alpha1.Unknown, evidencev1alpha1.Unhealthy} {
		t.Run("a fresh timeline "+string(state), func(t *testing.T) {
			fake := newFakeAgent(t, ca)
			snapshot := fixtureSnapshot(t)
			fake.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
			fake.recovery = fixtureRecovery(t, snapshot)
			fake.snapshot = snapshot
			object := testEvidenceAgent(testAgent, ca, snapshot, testNow)
			runner := newTestRunner(t, newFakeLister(t, object), fake, writeToken(t, testToken))
			runner.refresh(context.Background())
			first := runner.ForCluster(testNamespace, testCluster).Recovery
			if first.State != RecoveryAvailable || first.Paths[0].FrontierReceiptAt == nil || first.ScannedAt == nil {
				t.Fatalf("first read: %+v", first)
			}
			keptScan := *first.ScannedAt

			// The next refresh: a failover left the timeline not
			// established, and the recovery pages fail.
			fake.mu.Lock()
			fake.recovery = nil
			later := snapshot.CompletedAt.Add(time.Hour)
			fake.snapshot.CompletedAt = &later
			bc := *fake.snapshot.Details.BarmanCloud
			bc.Timeline = evidencev1alpha1.StateReason{State: state, Reason: evidencev1alpha1.Reason{Code: "x", Message: "history 2 is unreadable"}}
			fake.snapshot.Details.BarmanCloud = &bc
			fake.mu.Unlock()
			runner.refresh(context.Background())
			got := runner.ForCluster(testNamespace, testCluster).Recovery
			if got.State != RecoveryUncertain || !got.Stale || got.StaleReason == "" || got.ScannedAt == nil || !got.ScannedAt.Equal(keptScan) {
				t.Fatalf("kept: %+v", got)
			}
			if !strings.Contains(got.Reason, "history 2 is unreadable") {
				t.Errorf("reason %q does not follow the fresh snapshot", got.Reason)
			}
			for _, p := range got.Paths {
				if p.State == evidencev1alpha1.Healthy || p.State == evidencev1alpha1.Warning || p.FrontierReceiptAt != nil {
					t.Errorf("kept path still coverage: %+v", p)
				}
			}
		})
	}
}

func TestWALPosition(t *testing.T) {
	size := func(v uint64) *uint64 { return &v }
	name := func(v string) *string { return &v }
	timeline, position := walPosition(name("0000000200000001000000FF"), size(16<<20))
	if timeline == nil || *timeline != 2 || position == nil || *position != 256+255 {
		t.Fatalf("got %v %v", timeline, position)
	}
	for _, c := range []struct {
		name string
		wal  *string
		size *uint64
	}{
		{"no name", nil, size(16 << 20)},
		{"no size", name("000000010000000000000002"), nil},
		{"short", name("0000000100000000"), size(16 << 20)},
		{"not hex", name("00000001000000000000000Z"), size(16 << 20)},
		{"timeline zero", name("000000000000000000000002"), size(16 << 20)},
		{"segment past the rollover", name("000000010000000000000100"), size(16 << 20)},
		{"size not a power of two", name("000000010000000000000002"), size(3 << 20)},
		{"size too large", name("000000010000000000000002"), size(2 << 30)},
	} {
		if tl, pos := walPosition(c.wal, c.size); tl != nil || pos != nil {
			t.Errorf("%s: %v %v", c.name, tl, pos)
		}
	}
}
