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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// readWire decodes one of the contract's wire goldens.
func readWire(t *testing.T, name string, value any) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "testdata", "wire", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, value); err != nil {
		t.Fatal(err)
	}
}

// pgbackrestFixture is the contract's pgBackRest wire goldens (stanza
// orders, two backups: a full and an incremental on it), bound to the
// test agent and its publication.
func pgbackrestFixture(t *testing.T) (evidencev1alpha1.RepositoryEvidenceSnapshot, evidencev1alpha1.PGBackRestBackupPage, evidencev1alpha1.PGBackRestRecoveryPathPage) {
	t.Helper()
	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	var page evidencev1alpha1.PGBackRestBackupPage
	var recovery evidencev1alpha1.PGBackRestRecoveryPathPage
	readWire(t, "pgbackrest-snapshot.json", &snapshot)
	readWire(t, "pgbackrest-backups.json", &page)
	readWire(t, "pgbackrest-recovery-paths.json", &recovery)
	snapshot.Identity.Cluster.Namespace = testNamespace
	snapshot.ProducerEpoch = testEpoch
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	page.Revision, page.EvidenceGeneration = snapshot.Revision, snapshot.EvidenceGeneration
	recovery.Revision, recovery.EvidenceGeneration = snapshot.Revision, snapshot.EvidenceGeneration
	return snapshot, page, recovery
}

// pgbackrestAgent is an EvidenceAgent that resolved the stanza.
func pgbackrestAgent(ca testCA, snapshot evidencev1alpha1.RepositoryEvidenceSnapshot, stanza string) *pgtoolboxv1alpha1.EvidenceAgent {
	object := testEvidenceAgent(testAgent, ca, snapshot, testNow)
	object.Status.ServerName = ""
	object.Status.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestStatus{
		Plugin: "pgbackrest.cnpg.opera.com", Kind: "Archive", ObjectName: "orders-archive", Stanza: stanza, Repository: 1,
	}
	return object
}

func TestRunnerReadsPgBackRestEvidence(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	run := func(t *testing.T, stanza string, script func(*fakeAgent)) Result {
		t.Helper()
		snapshot, page, recovery := pgbackrestFixture(t)
		fake := newFakeAgent(t, ca)
		fake.snapshot = snapshot
		fake.pgbackrestPages = []evidencev1alpha1.PGBackRestBackupPage{page}
		fake.pgbackrestRecovery = &recovery
		script(fake)
		runner := newTestRunner(t, newFakeLister(t, pgbackrestAgent(ca, snapshot, stanza)), fake, writeToken(t, testToken))
		runner.refresh(context.Background())
		return runner.ForCluster(testNamespace, testCluster)
	}

	t.Run("labels, chains and recovery paths of the stanza", func(t *testing.T) {
		got := run(t, "orders", func(*fakeAgent) {})
		if got.State != viewmodel.EvidenceLive || !got.BackupsComplete || len(got.Backups) != 2 {
			t.Fatalf("state %s complete %v backups %+v (%s)", got.State, got.BackupsComplete, got.Backups, got.Reason)
		}
		full, incr := got.Backups[0], got.Backups[1]
		if full.ID != "20261107-081054F" || full.PgBackRest == nil || full.PgBackRest.Type != evidencev1alpha1.PGBackRestFull || !full.PgBackRest.Usable {
			t.Errorf("full = %+v %+v", full, full.PgBackRest)
		}
		if full.PgBackRest.CNPGBackupName == nil || *full.PgBackRest.CNPGBackupName != "orders-20261107081054" {
			t.Errorf("the cnpg-backup-name annotation is lost: %+v", full.PgBackRest)
		}
		if incr.PgBackRest == nil || incr.PgBackRest.Type != evidencev1alpha1.PGBackRestIncremental || incr.PgBackRest.Prior == nil || *incr.PgBackRest.Prior != full.ID {
			t.Errorf("incremental chain = %+v", incr.PgBackRest)
		}
		if got.PgBackRestPlugin != "pgbackrest.cnpg.opera.com" || got.PgBackRestObject != "orders-archive" {
			t.Errorf("the agent's resolved object is lost: %q %q", got.PgBackRestPlugin, got.PgBackRestObject)
		}
		if !got.BackupIDs[full.ID] || !got.BackupIDs[incr.ID] {
			t.Errorf("label set = %v", got.BackupIDs)
		}
		if full.EndTimeline == nil || *full.EndTimeline != 1 || full.EndPosition == nil || *full.EndPosition != 4 {
			t.Errorf("end of the full backup = %v %v", full.EndTimeline, full.EndPosition)
		}
		rec := got.Recovery
		if rec.State != RecoveryAvailable || !rec.Complete || len(rec.Paths) != 2 {
			t.Fatalf("recovery = %+v", rec)
		}
		p := rec.Paths[0]
		if p.BackupID != full.ID || p.LowerBoundAt == nil || !p.LowerBoundAt.Equal(*full.EndAt) || p.Consistent == nil || !*p.Consistent ||
			p.StartTimeline == nil || *p.StartTimeline != 1 {
			t.Errorf("path = %+v", p)
		}
	})

	t.Run("a snapshot of another stanza is refused", func(t *testing.T) {
		got := run(t, "elsewhere", func(*fakeAgent) {})
		if got.State != viewmodel.EvidenceUnavailable || got.Snapshot != nil {
			t.Errorf("state %s snapshot %v, want unavailable on an identity mismatch", got.State, got.Snapshot != nil)
		}
	})

	t.Run("items of another stanza are not this Cluster's", func(t *testing.T) {
		got := run(t, "orders", func(a *fakeAgent) {
			for i := range a.pgbackrestPages[0].Items {
				a.pgbackrestPages[0].Items[i].Stanza = "other"
			}
		})
		if len(got.Backups) != 0 || len(got.BackupIDs) != 0 {
			t.Errorf("backups of another stanza kept: %+v", got.Backups)
		}
	})

	t.Run("a Barman page on a pgBackRest snapshot is a contract failure", func(t *testing.T) {
		got := run(t, "orders", func(a *fakeAgent) {
			a.pgbackrestPages = nil
			a.pages = backupPages(a.snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
		})
		if got.State != viewmodel.EvidenceUnavailable {
			t.Errorf("state %s, want unavailable", got.State)
		}
	})
}

func TestCheckIdentityPgBackRest(t *testing.T) {
	snapshot, _, _ := pgbackrestFixture(t)
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	barman := fixtureSnapshot(t)
	tests := []struct {
		name   string
		agent  func() *pgtoolboxv1alpha1.EvidenceAgent
		snap   evidencev1alpha1.RepositoryEvidenceSnapshot
		refuse bool
	}{
		{"the resolved stanza", func() *pgtoolboxv1alpha1.EvidenceAgent { return pgbackrestAgent(ca, snapshot, "orders") }, snapshot, false},
		{"a stanza not resolved yet", func() *pgtoolboxv1alpha1.EvidenceAgent { return pgbackrestAgent(ca, snapshot, "") }, snapshot, false},
		{"another stanza", func() *pgtoolboxv1alpha1.EvidenceAgent { return pgbackrestAgent(ca, snapshot, "main") }, snapshot, true},
		{"another repository number", func() *pgtoolboxv1alpha1.EvidenceAgent {
			a := pgbackrestAgent(ca, snapshot, "orders")
			a.Status.PgBackRest.Repository = 2
			return a
		}, snapshot, true},
		{"a Barman answer to a pgBackRest agent", func() *pgtoolboxv1alpha1.EvidenceAgent {
			a := pgbackrestAgent(ca, barman, "orders")
			return a
		}, barman, true},
		{"a pgBackRest answer to a Barman agent", func() *pgtoolboxv1alpha1.EvidenceAgent {
			a := testEvidenceAgent(testAgent, ca, snapshot, testNow)
			a.Status.ServerName = "orders"
			return a
		}, snapshot, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := tt.snap
			err := checkIdentity(tt.agent(), &snap)
			if (err != nil) != tt.refuse {
				t.Errorf("checkIdentity = %v, want refused %v", err, tt.refuse)
			}
		})
	}
}
