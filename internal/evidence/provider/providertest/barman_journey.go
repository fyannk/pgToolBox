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

// Package providertest contains provider-independent end-to-end acceptance
// contracts. It imports no provider SDK and is used only by provider tests.
package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/web"
)

// FixtureProfile names the fixture set; it is recorded in every JourneyResult.
const FixtureProfile = "barman-v1-recovery-v3"

// FixtureObject is one object to upload to the provider under test.
type FixtureObject struct {
	Key  string
	Data []byte
}

// StateCount is the number of items found in one evidence state.
type StateCount struct {
	State evidence.State `json:"state"`
	Count int            `json:"count"`
}

// Capability is the JSON form of one evidence capability in a JourneyResult.
type Capability struct {
	ID      evidence.CapabilityID `json:"id"`
	Support evidence.Support      `json:"support"`
	State   evidence.State        `json:"state"`
	Reason  string                `json:"reason"`
}

// JourneyResult is the provider-independent outcome of BarmanCatalogJourney;
// every provider must produce the same one.
type JourneyResult struct {
	FixtureProfile   string                `json:"fixture_profile"`
	RepositoryFormat string                `json:"repository_format"`
	ScopeKind        string                `json:"scope_kind"`
	Scopes           []string              `json:"scopes"`
	Completeness     evidence.Completeness `json:"completeness"`
	OverallState     evidence.State        `json:"overall_state"`
	BackupStates     []StateCount          `json:"backup_states"`
	Capabilities     []Capability          `json:"capabilities"`
	Recovery         RecoveryResult        `json:"recovery"`
	RenderedStates   []evidence.State      `json:"rendered_states"`
}

// RecoveryResult summarizes the recovery evidence of a JourneyResult.
type RecoveryResult struct {
	Servers         int          `json:"servers"`
	Paths           int          `json:"paths"`
	TimelineStates  []StateCount `json:"timeline_states"`
	CoverageStates  []StateCount `json:"coverage_states"`
	PathStates      []StateCount `json:"path_states"`
	RetentionStates []StateCount `json:"retention_states"`
}

// BarmanFixture returns a Barman repository with one backup in each catalog
// state (completed, started, failed, malformed, missing artifact, missing
// metadata) and one WAL segment.
func BarmanFixture(t *testing.T) []FixtureObject {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate provider fixture source")
	}
	goldenPath := filepath.Join(filepath.Dir(source), "..", "..", "formats", "barmancloud", "testdata", "barman-3.19.1", "completed", "backup.info")
	completed, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	completed = bytes.Replace(completed, []byte("version=None"), []byte("version=170005"), 1)
	started := bytes.Replace(completed, []byte("status=DONE"), []byte("status=STARTED"), 1)
	failed := bytes.Replace(completed, []byte("status=DONE"), []byte("status=FAILED"), 1)
	artifact := []byte("bounded-barman-artifact")
	return []FixtureObject{
		{Key: "alpha/base/completed/backup.info", Data: completed},
		{Key: "alpha/base/completed/data.tar.gz", Data: artifact},
		{Key: "alpha/base/completed/16384.tar.gz", Data: artifact},
		{Key: "alpha/base/started/backup.info", Data: started},
		{Key: "alpha/base/started/data.tar.gz", Data: artifact},
		{Key: "alpha/base/failed/backup.info", Data: failed},
		{Key: "alpha/base/failed/data.tar.gz", Data: artifact},
		{Key: "alpha/base/malformed/backup.info", Data: []byte("malformed Barman metadata\n")},
		{Key: "alpha/base/malformed/data.tar.gz", Data: artifact},
		{Key: "alpha/base/missing-artifact/backup.info", Data: completed},
		{Key: "alpha/base/missing-info/data.tar.gz", Data: artifact},
		{Key: "alpha/wals/0000000100000000/000000010000000000000001", Data: []byte("bounded-wal-fixture")},
	}
}

// BarmanCatalogJourney probes and scans reader, which must hold BarmanFixture,
// checks the catalog, WAL and recovery evidence and the rendered page, and
// returns the normalized result. It fails t on any mismatch.
func BarmanCatalogJourney(t *testing.T, reader store.Reader) JourneyResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	format := barmancloud.New()
	cache, err := inventory.NewCache(inventory.Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	probe := readiness.New(true, time.Minute, time.Now)
	scanner, err := inventory.NewScanner(inventory.ScannerOptions{
		Store: reader, Format: format, Cache: cache, Readiness: probe,
		RefreshInterval: time.Minute, MaxObjects: 1000, PageSize: store.MaxPageObjects,
		RecentLimit: 20, AnalyzeBarmanCatalog: true,
		Now: time.Now, Logger: slog.New(slog.DiscardHandler),
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
	if !snapshot.TotalsKnown || snapshot.Evidence.Completeness != evidence.Complete || len(snapshot.Scopes) != 1 || snapshot.Scopes[0].Name != "alpha" || !snapshot.Scopes[0].Recognized {
		t.Fatalf("Barman inventory = %#v", snapshot)
	}
	wantNamedStates := map[string]evidence.State{
		"started": evidence.Warning, "failed": evidence.Unhealthy,
		"malformed": evidence.Unknown, "missing-artifact": evidence.Unhealthy,
		"missing-info": evidence.Unknown,
	}
	counts := map[evidence.State]int{}
	for _, backup := range snapshot.BarmanCatalog.Backups {
		counts[backup.State]++
		if want, exists := wantNamedStates[backup.ID]; exists {
			if backup.State != want {
				t.Fatalf("backup %s state = %s, want %s: %#v", backup.ID, backup.State, want, backup)
			}
			delete(wantNamedStates, backup.ID)
		}
	}
	if len(wantNamedStates) != 0 || counts[evidence.Healthy] != 1 || counts[evidence.Warning] != 1 || counts[evidence.Unhealthy] != 2 || counts[evidence.Unknown] != 2 {
		t.Fatalf("catalog state matrix = %#v, missing named states = %#v", counts, wantNamedStates)
	}
	if snapshot.Evidence.State != evidence.Unknown {
		t.Fatalf("mixed catalog rollup = %#v", snapshot.Evidence)
	}
	if len(snapshot.BarmanWAL.Servers) != 1 {
		t.Fatalf("Barman WAL servers = %#v", snapshot.BarmanWAL)
	}
	wal := snapshot.BarmanWAL.Servers[0]
	if wal.Server != "alpha" || wal.PostgreSQLVersion != 170005 || wal.SegmentSize != 16<<20 || wal.Counts.Segments != 1 || len(wal.Ranges) != 1 || len(wal.Gaps) != 0 || wal.State != evidence.Healthy {
		t.Fatalf("Barman WAL continuity = %#v", wal)
	}
	if len(snapshot.BarmanRecovery.Servers) != 1 {
		t.Fatalf("Barman recovery servers = %#v", snapshot.BarmanRecovery)
	}
	recovery := snapshot.BarmanRecovery.Servers[0]
	if recovery.Server != "alpha" || recovery.TimelineState != evidence.Healthy || recovery.CoverageState != evidence.Unknown || len(recovery.Paths) != 1 || recovery.Paths[0].State != evidence.Unknown || recovery.Retention.State != evidence.Healthy {
		t.Fatalf("Barman recovery evidence = %#v", recovery)
	}

	handler, err := web.New(web.Options{
		Logger: slog.New(slog.DiscardHandler), Provider: "normalized-provider",
		Format: format.Descriptor(), Inventory: cache.Load, Readiness: probe.Result,
		RequestID: func() string { return "provider-parity-proof" }, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.Routes().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("render status = %d", response.Code)
	}
	for _, expected := range []string{"started", "failed", "malformed", "missing-artifact", "missing-info", ">healthy<", ">warning<", ">unhealthy<", ">unknown<", "A structurally usable backup", "Observed recovery coverage", "Archive receipt is not transaction time"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("rendered catalog does not contain %q", expected)
		}
	}

	result := JourneyResult{
		FixtureProfile: FixtureProfile, RepositoryFormat: snapshot.Evidence.RepositoryFormat,
		ScopeKind: snapshot.Evidence.Scope.Kind, Completeness: snapshot.Evidence.Completeness,
		OverallState: snapshot.Evidence.State,
		BackupStates: []StateCount{
			{State: evidence.Healthy, Count: counts[evidence.Healthy]},
			{State: evidence.Warning, Count: counts[evidence.Warning]},
			{State: evidence.Unhealthy, Count: counts[evidence.Unhealthy]},
			{State: evidence.Unknown, Count: counts[evidence.Unknown]},
		},
		Recovery: RecoveryResult{
			Servers: 1, Paths: 1,
			TimelineStates:  []StateCount{{State: recovery.TimelineState, Count: 1}},
			CoverageStates:  []StateCount{{State: recovery.CoverageState, Count: 1}},
			PathStates:      []StateCount{{State: recovery.Paths[0].State, Count: 1}},
			RetentionStates: []StateCount{{State: recovery.Retention.State, Count: 1}},
		},
		RenderedStates: []evidence.State{evidence.Healthy, evidence.Warning, evidence.Unhealthy, evidence.Unknown},
	}
	for _, scope := range snapshot.Scopes {
		result.Scopes = append(result.Scopes, scope.Name)
	}
	for _, capability := range snapshot.Evidence.Capabilities {
		result.Capabilities = append(result.Capabilities, Capability{ID: capability.ID, Support: capability.Support, State: capability.State, Reason: capability.Reason})
	}
	sort.Strings(result.Scopes)
	return result
}

// WriteJourneyResult writes result as JSON to the file named by
// EVIDENCE_AGENT_NORMALIZED_RESULT, for the cross-provider comparison;
// it does nothing when the variable is unset.
func WriteJourneyResult(t *testing.T, result JourneyResult) {
	t.Helper()
	output := os.Getenv("EVIDENCE_AGENT_NORMALIZED_RESULT")
	if output == "" {
		return
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(output, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
