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

package providertest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/evidenceapi"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/web"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// PGBackRestFixtureProfile names the repositories the generator's provider
// mode (pgbackrest-generator/generate-providers.sh) writes; it is recorded
// in every PGBackRestJourneyResult.
const PGBackRestFixtureProfile = "pgbackrest-2.60.0-provider-v1"

// PGBackRestRepositoryPrefix is the store prefix of the journey's
// repositories: the repo path /pgbr/orders without its leading slash.
const PGBackRestRepositoryPrefix = "pgbr/orders"

// PGBackRestEncryptedPrefix is the encrypted repository's (/pgbr/orders-enc).
const PGBackRestEncryptedPrefix = "pgbr/orders-enc"

// PGBackRestPassphrase is the encrypted repository's test-only passphrase.
const PGBackRestPassphrase = "pgtoolbox-fixture-test-only-passphrase"

// PGBackRestBackupResult is one backup of a PGBackRestJourneyResult, without
// its label (a timestamp, different in every repository).
type PGBackRestBackupResult struct {
	Type   string         `json:"type"`
	State  evidence.State `json:"state"`
	Usable bool           `json:"usable"`
	CNPG   string         `json:"cnpg_backup_name"`
}

// PGBackRestJourneyResult is the provider-independent outcome of
// PGBackRestJourney; the plain S3 and Azure repositories must produce the
// same one.
type PGBackRestJourneyResult struct {
	FixtureProfile   string                   `json:"fixture_profile"`
	RepositoryFormat string                   `json:"repository_format"`
	ScopeKind        string                   `json:"scope_kind"`
	Scopes           []string                 `json:"scopes"`
	Completeness     evidence.Completeness    `json:"completeness"`
	OverallState     evidence.State           `json:"overall_state"`
	Encrypted        bool                     `json:"encrypted"`
	InfoFormat       int                      `json:"info_format"`
	ArchiveIDs       []string                 `json:"archive_ids"`
	SegmentSize      int64                    `json:"segment_size"`
	Backups          []PGBackRestBackupResult `json:"backups"`
	WALRanges        int                      `json:"wal_ranges"`
	WALGaps          int                      `json:"wal_gaps"`
	WindowUsable     int                      `json:"window_usable_backups"`
	FindingCodes     []string                 `json:"finding_codes"`
	Capabilities     []Capability             `json:"capabilities"`
}

// PGBackRestJourney scans reader (rooted at a repository the generator
// wrote) as the agent does for stanza demo, checks the evidence, its
// wire projection and the rendered page, and returns the normalized
// result. Repository keys must be root-relative: backup/demo/...,
// archive/demo/... It fails t on any mismatch.
func PGBackRestJourney(t *testing.T, reader store.Reader, passphrase []byte) PGBackRestJourneyResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	format := pgbackrest.New()
	cache, err := inventory.NewCache(inventory.Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	probe := readiness.New(true, time.Minute, time.Now)
	scanner, err := inventory.NewScanner(inventory.ScannerOptions{
		Store: reader, Format: format, ConfiguredScopes: []string{"demo"}, Cache: cache, Readiness: probe,
		RefreshInterval: time.Minute, MaxObjects: 100_000, PageSize: 7, RecentLimit: 20,
		Now: time.Now, Logger: slog.New(slog.DiscardHandler),
		PGBackRest: &inventory.PGBackRestOptions{Stanza: "demo", ArchiveStanza: "demo", Passphrase: passphrase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if category := scanner.Probe(ctx); category != fault.Unknown || !probe.Result().Ready {
		t.Fatalf("provider probe = %s, readiness = %#v", category, probe.Result())
	}
	if err := scanner.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if !snapshot.TotalsKnown || snapshot.PagesExamined < 2 || len(snapshot.Scopes) != 1 || snapshot.Scopes[0].Name != "demo" || !snapshot.Scopes[0].Recognized || len(snapshot.PGBackRest) != 1 {
		t.Fatalf("pgBackRest inventory: totals %v, pages %d, scopes %+v, reports %d", snapshot.TotalsKnown, snapshot.PagesExamined, snapshot.Scopes, len(snapshot.PGBackRest))
	}
	report := snapshot.PGBackRest[0]
	if snapshot.Evidence.State != evidence.Healthy || report.Window == nil || len(report.Backups) != 3 {
		encoded, _ := json.MarshalIndent(report, "", "  ")
		t.Fatalf("pgBackRest evidence %s (%s): %s", snapshot.Evidence.State, snapshot.Evidence.Reason, encoded)
	}
	if report.Encrypted != (len(passphrase) > 0) {
		t.Fatalf("encrypted = %v with a passphrase of %d bytes", report.Encrypted, len(passphrase))
	}
	for index, want := range []pgbackrest.BackupType{pgbackrest.BackupFull, pgbackrest.BackupDifferential, pgbackrest.BackupIncremental} {
		backup := report.Backups[index]
		if backup.Backup.Type != want || !backup.Usable || !backup.Manifest.Inspected {
			t.Fatalf("backup %d: %+v", index, backup)
		}
	}

	publication, err := evidenceapi.Project(snapshot, evidenceapi.Options{
		ProducerVersion: "journey", ProducerInstance: "0123456789abcdef0123456789abcdef",
		ClusterNamespace: "database-team", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191",
		Repository: evidencev1alpha1.RepositoryFingerprintInput{
			Provider: evidencev1alpha1.ProviderS3, Bucket: "pgbr-proof", Prefix: PGBackRestRepositoryPrefix,
			Format: evidencev1alpha1.FormatPGBackRest, ScopeKind: evidencev1alpha1.ScopeKindStanza, ScopeName: "demo",
		},
	})
	if err != nil || publication.PGBackRest == nil || len(publication.PGBackRest.Backups) != 3 {
		t.Fatalf("evidence projection: %v", err)
	}

	handler, err := web.New(web.Options{
		Logger: slog.New(slog.DiscardHandler), Provider: "normalized-provider",
		Format: format.Descriptor(), Inventory: cache.Load, Readiness: probe.Result,
		RequestID: func() string { return "pgbackrest-provider-proof" }, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.Routes().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "<h3>Stanza demo</h3>") || !strings.Contains(body, "provider-repo") {
		t.Fatalf("render status = %d", response.Code)
	}
	if len(passphrase) > 0 && strings.Contains(body, string(passphrase)) {
		t.Fatal("the rendered page shows the passphrase")
	}

	result := PGBackRestJourneyResult{
		FixtureProfile: PGBackRestFixtureProfile, RepositoryFormat: snapshot.Evidence.RepositoryFormat,
		ScopeKind: snapshot.Evidence.Scope.Kind, Scopes: []string{"demo"}, Completeness: snapshot.Evidence.Completeness,
		OverallState: snapshot.Evidence.State, Encrypted: report.Encrypted, InfoFormat: report.RepositoryFormat(),
		ArchiveIDs: report.ArchiveIDs, SegmentSize: report.SegmentSize, WindowUsable: report.Window.UsableBackups,
		FindingCodes: []string{},
	}
	for index, assessment := range report.Assess() {
		backup := report.Backups[index].Backup
		// The annotation names the repository; the parity compares its role.
		name := backup.Annotations["cnpg-backup-name"]
		if _, role, ok := strings.Cut(name, "-repo"); ok && len(role) > 2 {
			name = "provider-repoN" + role[1:]
		}
		result.Backups = append(result.Backups, PGBackRestBackupResult{Type: string(backup.Type), State: assessment.State, Usable: assessment.Usable, CNPG: name})
	}
	for _, wal := range report.WAL {
		result.WALRanges += len(wal.Ranges)
		result.WALGaps += len(wal.Gaps)
	}
	for _, finding := range report.Findings {
		result.FindingCodes = append(result.FindingCodes, string(finding.Code))
	}
	for _, capability := range snapshot.Evidence.Capabilities {
		result.Capabilities = append(result.Capabilities, Capability{ID: capability.ID, Support: capability.Support, State: capability.State, Reason: capability.Reason})
	}
	return result
}

// WritePGBackRestJourneyResult writes result as JSON to the file named by
// EVIDENCE_AGENT_NORMALIZED_RESULT; it does nothing when that is unset.
func WritePGBackRestJourneyResult(t *testing.T, result PGBackRestJourneyResult) {
	t.Helper()
	output := os.Getenv("EVIDENCE_AGENT_NORMALIZED_RESULT")
	if output == "" {
		return
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// AssertPGBackRestKeyMapping checks the keys a store rooted at the bucket
// (no prefix) lists: pgBackRest's repo path /pgbr/orders is the key prefix
// pgbr/orders/, with no leading slash and no empty segment.
func AssertPGBackRestKeyMapping(t *testing.T, root store.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen := map[string]bool{}
	cursor := ""
	for range 1_000 {
		page, err := root.List(ctx, store.ListRequest{Cursor: cursor, Limit: store.MaxPageObjects})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			if strings.HasPrefix(object.Key, "/") || strings.Contains(object.Key, "//") {
				t.Fatalf("pgBackRest wrote key %q", object.Key)
			}
			seen[object.Key] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for _, key := range []string{PGBackRestRepositoryPrefix + "/backup/demo/backup.info", PGBackRestRepositoryPrefix + "/backup/demo/backup.info.copy", PGBackRestRepositoryPrefix + "/archive/demo/archive.info"} {
		if !seen[key] {
			t.Fatalf("no key %q among %d listed", key, len(seen))
		}
	}
}
