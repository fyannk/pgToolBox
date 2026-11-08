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
	"testing"

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
		fake.recovery = nil
		runner.refresh(context.Background())
		got := runner.ForCluster(testNamespace, testCluster).Recovery
		if got.State != RecoveryAvailable || !got.Stale || got.StaleReason == "" || len(got.Paths) != 1 {
			t.Errorf("%+v", got)
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
	good := Recovery{State: RecoveryAvailable, Complete: true, Paths: []RecoveryPath{{BackupID: "b1"}}}
	cases := []struct {
		name     string
		previous Recovery
		current  Recovery
		stale    bool
		state    string
	}{
		{"a failure keeps the last good paths", good, Recovery{State: RecoveryUnavailable, Reason: "timeout"}, true, RecoveryAvailable},
		{"a fresh read replaces them", good, Recovery{State: RecoveryAvailable, Complete: true}, false, RecoveryAvailable},
		{"unsupported is definitive", good, Recovery{State: RecoveryUnsupported}, false, RecoveryUnsupported},
		{"nothing to keep", Recovery{}, Recovery{State: RecoveryUnavailable}, false, RecoveryUnavailable},
	}
	for _, c := range cases {
		got := keepRecovery(c.previous, c.current)
		if got.Stale != c.stale || got.State != c.state {
			t.Errorf("%s: %+v", c.name, got)
		}
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
