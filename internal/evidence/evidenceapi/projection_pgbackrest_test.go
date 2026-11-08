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

package evidenceapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest/pgbackrestfixture"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

var updateGoldens = flag.Bool("update", false, "rewrite the golden files")

func pgbackrestOptions() Options {
	return Options{
		ProducerVersion: "1.0.0", ProducerInstance: "0123456789abcdef0123456789abcdef",
		ClusterNamespace: "database-team", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191",
		Repository: evidencev1alpha1.RepositoryFingerprintInput{
			Provider: evidencev1alpha1.ProviderS3, Bucket: "synthetic-bucket", Prefix: "pgbr/orders",
			Format: evidencev1alpha1.FormatPGBackRest, ScopeKind: evidencev1alpha1.ScopeKindStanza, ScopeName: pgbackrestfixture.Stanza,
		},
		PGBackRestRepository: 1,
	}
}

// scanFixture runs the real scanner over a B1 fixture at a fixed clock.
func scanFixture(t testing.TB, repository *pgbackrestfixture.Repository, passphrase []byte) inventory.Snapshot {
	t.Helper()
	return scanFixtureWith(t, repository, passphrase, nil)
}

// scanFixtureWith is scanFixture with a retention expectation.
func scanFixtureWith(t testing.TB, repository *pgbackrestfixture.Repository, passphrase []byte, retention *pgbackrest.Retention) inventory.Snapshot {
	t.Helper()
	format := pgbackrest.New()
	cache, err := inventory.NewCache(inventory.Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC) }
	scanner, err := inventory.NewScanner(inventory.ScannerOptions{
		Store: repository.Reader(t), Format: format, ConfiguredScopes: []string{pgbackrestfixture.Stanza}, Cache: cache,
		Readiness: readiness.New(true, time.Hour, clock), RefreshInterval: time.Hour, MaxObjects: 100_000, PageSize: 50,
		RecentLimit: 5, Now: clock, Logger: slog.New(slog.DiscardHandler),
		PGBackRest: &inventory.PGBackRestOptions{Stanza: pgbackrestfixture.Stanza, ArchiveStanza: pgbackrestfixture.Stanza, Passphrase: passphrase, Retention: retention},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return cache.Load()
}

func segmentOf(t *testing.T, repository *pgbackrestfixture.Repository, name string) string {
	t.Helper()
	for _, object := range repository.Objects {
		if strings.HasPrefix(object.Key, "archive/") && strings.Contains(object.Key, "/"+name+"-") {
			return object.Key
		}
	}
	t.Fatalf("no segment %s", name)
	return ""
}

func TestProjectPGBackRestFixtures(t *testing.T) {
	t.Parallel()
	plain := pgbackrestfixture.Load(t, "plain")
	tests := []struct {
		name  string
		input func(*testing.T) inventory.Snapshot
		state evidencev1alpha1.State
		check func(*testing.T, *PGBackRestPublication)
	}{
		{
			name:  "plain",
			input: func(t *testing.T) inventory.Snapshot { return scanFixture(t, plain, nil) },
			state: evidencev1alpha1.Healthy,
			check: func(t *testing.T, publication *PGBackRestPublication) {
				details := publication.Snapshot.Details.PGBackRest
				if len(publication.Backups) != 4 || details.UsableBackups == nil || *details.UsableBackups != 4 || details.RecoveryWindow == nil || *details.Encrypted {
					t.Fatalf("details %+v", details)
				}
				if publication.Snapshot.Identity.Repository.Format != "pgbackrest" || publication.Snapshot.Identity.Repository.Scope.Kind != "stanza" {
					t.Fatalf("identity %+v", publication.Snapshot.Identity)
				}
				retention := publication.Snapshot.Capabilities[slices.IndexFunc(publication.Snapshot.Capabilities, func(capability evidencev1alpha1.Capability) bool {
					return capability.ID == evidencev1alpha1.RetentionExpectation
				})]
				// Without an expectation the comparison is supported but
				// not made: unknown, outside the roll-up, no block.
				if retention.Support != evidencev1alpha1.Supported || retention.State != evidencev1alpha1.Unknown || details.Retention != nil {
					t.Fatalf("retention %+v, block %+v", retention, details.Retention)
				}
			},
		},
		{
			name: "plain within a retention of two full backups",
			input: func(t *testing.T) inventory.Snapshot {
				return scanFixtureWith(t, plain, nil, &pgbackrest.Retention{Full: 2})
			},
			state: evidencev1alpha1.Healthy,
			check: func(t *testing.T, publication *PGBackRestPublication) {
				block := publication.Snapshot.Details.PGBackRest.Retention
				if block == nil || !block.Configured || block.State != evidencev1alpha1.Healthy || block.FullType != "count" || block.ArchiveType != "full" ||
					block.EffectiveArchive == nil || *block.EffectiveArchive != 2 || block.FullBackups != 1 || block.DifferentialBackups != 1 || block.IncrementalBackups != 2 {
					t.Fatalf("retention block %+v", block)
				}
				assertCapability(t, publication, evidencev1alpha1.RetentionExpectation, evidencev1alpha1.Healthy)
			},
		},
		{
			// repo-retention-full=1 expires the WAL before the full backup's
			// archive start at the end of that backup: the fixture keeps
			// segments 1 and 2, which the generator never expired.
			name: "plain beyond a retention of one full backup keeps WAL expire removes",
			input: func(t *testing.T) inventory.Snapshot {
				return scanFixtureWith(t, plain, nil, &pgbackrest.Retention{Full: 1, FullType: pgbackrest.RetentionCount})
			},
			state: evidencev1alpha1.Warning,
			check: func(t *testing.T, publication *PGBackRestPublication) {
				assertFinding(t, publication, evidencev1alpha1.FindingRetentionNotApplied, "archive/demo/17-1/000000010000000000000001")
				assertCapability(t, publication, evidencev1alpha1.RetentionExpectation, evidencev1alpha1.Warning)
			},
		},
		{
			name: "plain without repo-retention-full is not configured, not unknown",
			input: func(t *testing.T) inventory.Snapshot {
				return scanFixtureWith(t, plain, nil, &pgbackrest.Retention{})
			},
			state: evidencev1alpha1.Warning,
			check: func(t *testing.T, publication *PGBackRestPublication) {
				block := publication.Snapshot.Details.PGBackRest.Retention
				if block == nil || block.Configured || block.Full != nil || block.EffectiveArchive != nil || block.State != evidencev1alpha1.Warning {
					t.Fatalf("retention block %+v", block)
				}
				assertFinding(t, publication, evidencev1alpha1.FindingRetentionNotConfigured, "backup/demo/")
				assertCapability(t, publication, evidencev1alpha1.RetentionExpectation, evidencev1alpha1.Warning)
			},
		},
		{
			name: "encrypted: the incremental of the abandoned branch is a warning",
			input: func(t *testing.T) inventory.Snapshot {
				return scanFixture(t, pgbackrestfixture.Load(t, "encrypted"), []byte(pgbackrestfixture.Passphrase))
			},
			state: evidencev1alpha1.Warning,
			check: func(t *testing.T, publication *PGBackRestPublication) {
				if !*publication.Snapshot.Details.PGBackRest.Encrypted {
					t.Fatal("the encrypted repository is not reported encrypted")
				}
				index := slices.IndexFunc(publication.Backups, func(backup evidencev1alpha1.PGBackRestBackup) bool {
					return slices.Contains(backup.UnusableReasons, "off-target-timeline")
				})
				if index < 0 || publication.Backups[index].State != evidencev1alpha1.Warning {
					t.Fatalf("backups %+v", publication.Backups)
				}
			},
		},
		{
			name: "encrypted without a passphrase is unknown",
			input: func(t *testing.T) inventory.Snapshot {
				return scanFixture(t, pgbackrestfixture.Load(t, "encrypted"), nil)
			},
			state: evidencev1alpha1.Unknown,
		},
		{
			// The roll-up question of B1, answered as Barman answers it (a
			// FAILED Barman backup beside a good one makes the catalog, and
			// so the repository, unhealthy): the differential lost the WAL
			// that makes it consistent, the recovery window from the other
			// backups survives, and the repository is unhealthy.
			name: "an unusable backup beside a recovery window is unhealthy",
			input: func(t *testing.T) inventory.Snapshot {
				return scanFixture(t, plain.Without(segmentOf(t, plain, "000000010000000000000009")), nil)
			},
			state: evidencev1alpha1.Unhealthy,
			check: func(t *testing.T, publication *PGBackRestPublication) {
				details := publication.Snapshot.Details.PGBackRest
				if details.RecoveryWindow == nil || details.Catalog.State != evidencev1alpha1.Unhealthy {
					t.Fatalf("details %+v", details)
				}
				index := slices.IndexFunc(publication.Backups, func(backup evidencev1alpha1.PGBackRestBackup) bool { return backup.Type == "diff" })
				if publication.Backups[index].Usable || !slices.Equal(publication.Backups[index].UnusableReasons, []string{"wal-to-consistency-missing"}) {
					t.Fatalf("differential %+v", publication.Backups[index])
				}
			},
		},
		{
			name: "before the first scan",
			input: func(*testing.T) inventory.Snapshot {
				snapshot := inventory.Initial(pgbackrest.New().Descriptor())
				return snapshot
			},
			state: evidencev1alpha1.Unknown,
			check: func(t *testing.T, publication *PGBackRestPublication) {
				if publication.Snapshot.Details.PGBackRest.BackupItems != nil || publication.Snapshot.EvidenceGeneration != 0 {
					t.Fatalf("initial snapshot %+v", publication.Snapshot)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			publication, err := Project(test.input(t), pgbackrestOptions())
			if err != nil {
				t.Fatal(err)
			}
			native := publication.PGBackRest
			if native == nil || publication.Snapshot.APIVersion != "" {
				t.Fatal("a pgBackRest source did not project natively")
			}
			if native.Snapshot.State != test.state {
				t.Fatalf("state %s (%+v), want %s", native.Snapshot.State, native.Snapshot.Reason, test.state)
			}
			if test.check != nil {
				test.check(t, native)
			}
			encoded, _ := json.Marshal(native)
			if strings.Contains(string(encoded), pgbackrestfixture.Passphrase) {
				t.Fatal("the passphrase crossed the projection")
			}
		})
	}
}

func assertCapability(t *testing.T, publication *PGBackRestPublication, id evidencev1alpha1.CapabilityID, state evidencev1alpha1.State) {
	t.Helper()
	index := slices.IndexFunc(publication.Snapshot.Capabilities, func(capability evidencev1alpha1.Capability) bool { return capability.ID == id })
	if index < 0 || publication.Snapshot.Capabilities[index].Support != evidencev1alpha1.Supported || publication.Snapshot.Capabilities[index].State != state {
		t.Fatalf("capability %s in %+v, want supported and %s", id, publication.Snapshot.Capabilities, state)
	}
}

func assertFinding(t *testing.T, publication *PGBackRestPublication, code, subject string) {
	t.Helper()
	findings := publication.Snapshot.Details.PGBackRest.Findings
	if !slices.ContainsFunc(findings, func(finding evidencev1alpha1.PGBackRestFinding) bool {
		return finding.Code == code && finding.Subject == subject
	}) {
		t.Fatalf("no %s finding on %s in %+v", code, subject, findings)
	}
}

func TestProjectPGBackRestGolden(t *testing.T) {
	t.Parallel()
	publication, err := Project(scanFixture(t, pgbackrestfixture.Load(t, "plain"), nil), pgbackrestOptions())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := publication.PGBackRest.Snapshot
	snapshot.ProducerEpoch = 3
	compareGolden(t, filepath.Join("testdata", "pgbackrest-plain-snapshot.json"), snapshot)
	compareGolden(t, filepath.Join("testdata", "pgbackrest-plain-backups.json"), publication.PGBackRest.Backups)
	compareGolden(t, filepath.Join("testdata", "pgbackrest-plain-recovery-paths.json"), publication.PGBackRest.RecoveryPaths)

	// The same stanza compared with a time-based retention of 7 days.
	retained, err := Project(scanFixtureWith(t, pgbackrestfixture.Load(t, "plain"), nil, &pgbackrest.Retention{Full: 7, FullType: pgbackrest.RetentionTime, Diff: 2}), pgbackrestOptions())
	if err != nil {
		t.Fatal(err)
	}
	compareGolden(t, filepath.Join("testdata", "pgbackrest-plain-retention-summary.json"), retained.PGBackRest.Snapshot.Details.PGBackRest)
}

func compareGolden(t *testing.T, path string, value any) {
	t.Helper()
	actual, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	actual = append(actual, '\n')
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, actual, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(expected, actual) {
		t.Fatalf("%s differs from the projection (run go test -update):\n%s", path, actual)
	}
}

func TestProjectPGBackRestRefusesForeignEvidence(t *testing.T) {
	t.Parallel()
	source := scanFixture(t, pgbackrestfixture.Load(t, "plain"), nil)
	tests := map[string]func(*inventory.Snapshot, *Options){
		"another stanza's report": func(snapshot *inventory.Snapshot, _ *Options) { snapshot.PGBackRest[0].Stanza = "other" },
		"another archive stanza":  func(_ *inventory.Snapshot, options *Options) { options.PGBackRestArchiveStanza = "other" },
		"a Barman identity": func(_ *inventory.Snapshot, options *Options) {
			options.Repository.Format, options.Repository.ScopeKind = "barman-cloud", "barman-server"
		},
		"another scope name": func(_ *inventory.Snapshot, options *Options) { options.Repository.ScopeName = "orders" },
		"two reports": func(snapshot *inventory.Snapshot, _ *Options) {
			snapshot.PGBackRest = append(snapshot.PGBackRest, snapshot.PGBackRest[0])
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			copied, options := source, pgbackrestOptions()
			copied.PGBackRest = []pgbackrest.Report{pgbackrest.CloneReport(source.PGBackRest[0])}
			mutate(&copied, &options)
			if _, err := Project(copied, options); err == nil {
				t.Fatal("projected foreign evidence")
			}
		})
	}
	// A Barman source never carries pgBackRest reports.
	barman := completeSource(t)
	barman.PGBackRest = source.PGBackRest
	if _, err := Project(barman, testProjectOptions()); err == nil {
		t.Fatal("a Barman projection accepted pgBackRest reports")
	}
}

func newPGBackRestEngine(t *testing.T, source inventory.Snapshot) *Engine {
	t.Helper()
	engine, err := NewEngine(EngineOptions{
		Projection: pgbackrestOptions(), Initial: source,
		CursorEntropy: bytes.NewReader(bytes.Repeat([]byte{0x42}, cursorKeyBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestEnginePagesAPGBackRestPublication(t *testing.T) {
	t.Parallel()
	engine := newPGBackRestEngine(t, scanFixture(t, pgbackrestfixture.Load(t, "plain"), nil))
	snapshot, ok, err := engine.PGBackRestSnapshot(4)
	if !ok || err != nil || snapshot.ProducerEpoch != 4 {
		t.Fatalf("snapshot ok=%v err=%v epoch=%d", ok, err, snapshot.ProducerEpoch)
	}
	var labels []string
	request := PageRequest{Revision: snapshot.Revision, Limit: 3}
	for {
		page, err := engine.PGBackRestBackups(request)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			labels = append(labels, item.Label)
		}
		if page.NextCursor == nil {
			break
		}
		request.Cursor = *page.NextCursor
	}
	if len(labels) != 4 || !slices.IsSorted(labels) {
		t.Fatalf("labels %v", labels)
	}
	if page, err := engine.PGBackRestWALRanges(PageRequest{Revision: snapshot.Revision}); err != nil || len(page.Items) != 2 {
		t.Fatalf("WAL ranges %+v %v", page, err)
	}
	if page, err := engine.PGBackRestWALGaps(PageRequest{Revision: snapshot.Revision}); err != nil || len(page.Items) != 0 {
		t.Fatalf("WAL gaps %+v %v", page, err)
	}
	if page, err := engine.PGBackRestRecoveryPaths(PageRequest{Revision: snapshot.Revision}); err != nil || len(page.Items) != 4 {
		t.Fatalf("recovery paths %+v %v", page, err)
	}
	// The Barman routes refuse it cleanly.
	if _, err := engine.Backups(PageRequest{Revision: snapshot.Revision}); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("Barman backups of a pgBackRest publication: %v", err)
	}
	barmanView := engine.Snapshot()
	barmanView.ProducerEpoch = 1
	if barmanView.Validate() == nil {
		t.Fatal("a pgBackRest publication has a valid Barman snapshot")
	}
	if validateCollection(evidencev1alpha1.PGBackRestBackupPage{}) == nil {
		t.Fatal("the Barman collection validator accepted a pgBackRest page")
	}
	// And a Barman engine has no pgBackRest side.
	barman := newTestEngine(t, sourceWithBackups(t, 1), 0x91)
	if _, ok, _ := barman.PGBackRestSnapshot(1); ok {
		t.Fatal("a Barman engine served a pgBackRest snapshot")
	}
	if _, err := barman.PGBackRestBackups(PageRequest{Revision: barman.Snapshot().Revision}); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("pgBackRest backups of a Barman publication: %v", err)
	}
}

func TestEngineRepublishesAPGBackRestGeneration(t *testing.T) {
	t.Parallel()
	source := scanFixture(t, pgbackrestfixture.Load(t, "plain"), nil)
	engine := newPGBackRestEngine(t, source)
	if err := engine.Publish(source); err != nil {
		t.Fatalf("an identical republish failed: %v", err)
	}
	changed := source
	changed.Evidence.Reason = "something else"
	if err := engine.Publish(changed); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("a conflicting publish at the same revision: %v", err)
	}
	snapshot, _, _ := engine.PGBackRestSnapshot(1)
	if snapshot.Reason.Message == "something else" {
		t.Fatal("a refused publish replaced the publication")
	}
}

func TestAgentHandlerServesPGBackRest(t *testing.T) {
	t.Parallel()
	engine := newPGBackRestEngine(t, scanFixture(t, pgbackrestfixture.Load(t, "plain"), nil))
	handler, err := NewAgentHandler(AgentHandlerOptions{
		Engine: engine, Readiness: testReadiness(true), ProducerEpoch: 5,
		Authenticator: &fakeAuthenticator{decision: AuthDecision{Outcome: AuthAllowed, User: "system:serviceaccount:team-a:pgtoolbox-console"}},
		Audience:      "pgtoolbox-evidence-agent/team-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.SnapshotPath)
	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.Validate() != nil {
		t.Fatalf("snapshot %d %s", response.Code, response.Body.String())
	}
	if snapshot.ProducerEpoch != 5 || snapshot.Details.Type != evidencev1alpha1.PGBackRestDetailsType || snapshot.Details.PGBackRest.Stanza != "demo" {
		t.Fatalf("snapshot %+v", snapshot)
	}
	for path, kind := range map[string]string{
		evidencev1alpha1.BackupsPath:       evidencev1alpha1.PGBackRestBackupPageKind,
		evidencev1alpha1.WALRangesPath:     evidencev1alpha1.PGBackRestWALRangePageKind,
		evidencev1alpha1.WALGapsPath:       evidencev1alpha1.PGBackRestWALGapPageKind,
		evidencev1alpha1.RecoveryPathsPath: evidencev1alpha1.PGBackRestRecoveryPageKind,
	} {
		response := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, path+"?revision="+jsonNumber(snapshot.Revision)+"&limit=2")
		var header evidencev1alpha1.PageHeader
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &header) != nil || header.Kind != kind || header.APIVersion != evidencev1alpha1.APIVersion {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
	}
	response = agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.BackupsPath+"?revision="+jsonNumber(snapshot.Revision+1))
	assertAgentError(t, response, http.StatusConflict, evidencev1alpha1.ErrorPublicationChanged)
}

// TestPGBackRestCloneRefusesWithoutPanicking pins that a publication that
// does not round-trip through JSON (a time outside year 0-9999 does not
// encode) is ErrInvalidPublication on the serving path, never a panic.
func TestPGBackRestCloneRefusesWithoutPanicking(t *testing.T) {
	t.Parallel()
	engine := newPGBackRestEngine(t, scanFixture(t, pgbackrestfixture.Load(t, "plain"), nil))
	current := engine.current.Load()
	broken := *current.value.PGBackRest
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	broken.Snapshot.CompletedAt = &unencodable
	value := current.value
	value.PGBackRest = &broken
	engine.current.Store(&enginePublication{value: value})
	if _, ok, err := engine.PGBackRestSnapshot(1); !ok || !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("snapshot ok=%v err=%v, want ErrInvalidPublication", ok, err)
	}
	if _, err := clonePublication(value); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("clonePublication err=%v, want ErrInvalidPublication", err)
	}
	if len(broken.Backups) == 0 {
		t.Fatal("the fixture has no backup")
	}
	broken.Backups = slices.Clone(broken.Backups)
	broken.Backups[0].StartAt = &unencodable
	if _, err := engine.PGBackRestBackups(PageRequest{Revision: value.revision()}); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("backup page err=%v, want ErrInvalidPublication", err)
	}
}

// TestPublishedFindingsKeepEveryCode: a crafted chain of hundreds of
// backups whose manifests are missing fills the findings bound with
// ManifestMissing; the cut still publishes one finding of every other
// code (why the scan stopped, a WAL gap, an unapplied retention), sorted
// and within the contract's bound.
func TestPublishedFindingsKeepEveryCode(t *testing.T) {
	t.Parallel()
	report := pgbackrest.Report{Stanza: "demo", ArchiveStanza: "demo"}
	for index := range 300 {
		report.Findings = append(report.Findings, pgbackrest.Finding{
			Code: pgbackrest.FindingManifestMissing, Severity: pgbackrest.SeverityError,
			Subject: fmt.Sprintf("backup/demo/20261107-000000F_20261107-%06dI", index), Message: "manifest is missing",
		})
	}
	report.Findings = append(report.Findings,
		pgbackrest.Finding{Code: pgbackrest.FindingManifestBudgetExceeded, Severity: pgbackrest.SeverityWarning, Subject: "backup/demo/", Message: "the scan reads at most 128 manifests"},
		pgbackrest.Finding{Code: pgbackrest.FindingWALGap, Severity: pgbackrest.SeverityError, Subject: "archive/demo/17-1/000000010000000000000009", Message: "1 segment missing"},
		pgbackrest.Finding{Code: pgbackrest.FindingRetentionNotApplied, Severity: pgbackrest.SeverityWarning, Subject: "20261101-000000F", Message: "expire would have removed it"},
	)
	summary := projectPGBackRestSummary(false, Options{}, report, &PGBackRestPublication{})
	if !summary.FindingsTruncated || len(summary.Findings) != maxPublishedFindings {
		t.Fatalf("%d findings, truncated %v; want the bound %d and truncated", len(summary.Findings), summary.FindingsTruncated, maxPublishedFindings)
	}
	codes := map[string]int{}
	for index, finding := range summary.Findings {
		codes[finding.Code]++
		if index > 0 && compareFindings(summary.Findings[index-1], finding) >= 0 {
			t.Fatalf("findings not sorted at %d", index)
		}
	}
	for _, code := range []pgbackrest.FindingCode{pgbackrest.FindingManifestBudgetExceeded, pgbackrest.FindingWALGap, pgbackrest.FindingRetentionNotApplied} {
		if codes[string(code)] != 1 {
			t.Errorf("%s published %d times, want once", code, codes[string(code)])
		}
	}
	if codes[string(pgbackrest.FindingManifestMissing)] != maxPublishedFindings-3 {
		t.Errorf("%d ManifestMissing, want the rest of the bound", codes[string(pgbackrest.FindingManifestMissing)])
	}
}

func TestCapFindings(t *testing.T) {
	t.Parallel()
	finding := func(code, severity, subject string) evidencev1alpha1.PGBackRestFinding {
		return evidencev1alpha1.PGBackRestFinding{Code: code, Severity: severity, Subject: subject, Message: "m"}
	}
	all := []evidencev1alpha1.PGBackRestFinding{
		finding("A", "warning", "1"), finding("A", "warning", "2"), finding("B", "warning", "1"),
		finding("C", "error", "1"), finding("C", "error", "2"), finding("D", "warning", "1"),
	}
	for _, tt := range []struct {
		limit     int
		want      []string
		truncated bool
	}{
		{6, []string{"A1", "A2", "B1", "C1", "C2", "D1"}, false},
		{5, []string{"A1", "B1", "C1", "C2", "D1"}, true},
		{4, []string{"A1", "B1", "C1", "D1"}, true},
		// Fewer slots than codes: errors first, then codes in order.
		{2, []string{"A1", "C1"}, true},
	} {
		got, truncated := capFindings(slices.Clone(all), tt.limit)
		var names []string
		for _, value := range got {
			names = append(names, value.Code+value.Subject)
		}
		if !slices.Equal(names, tt.want) || truncated != tt.truncated {
			t.Errorf("limit %d: %v %v, want %v %v", tt.limit, names, truncated, tt.want, tt.truncated)
		}
	}
}
