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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func TestProjectPreservesImmutableGenerationAndOmitsRawKeys(t *testing.T) {
	source := completeSource(t)
	publication, err := Project(source, testProjectOptions())
	if err != nil {
		t.Fatal(err)
	}
	if publication.Snapshot.Revision != 7 || publication.Snapshot.EvidenceGeneration != 7 || publication.Snapshot.State != evidencev1alpha1.Healthy {
		t.Fatalf("publication identity/state = %#v", publication.Snapshot)
	}
	if len(publication.Backups) != 1 || len(publication.WALRanges) != 1 || len(publication.RecoveryPaths) != 1 {
		t.Fatalf("projected collections = %#v", publication)
	}
	if publication.RecoveryPaths[0].StartPosition == nil || *publication.RecoveryPaths[0].StartPosition != 0 {
		t.Fatalf("valid WAL position zero was lost: %#v", publication.RecoveryPaths[0])
	}
	for _, value := range []any{publication.Snapshot, publication.Backups, publication.WALRanges, publication.WALGaps, publication.RecoveryPaths} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "artifact-key-canary") || strings.Contains(string(encoded), "segment-name presence only") {
			t.Fatalf("raw internal value crossed projection: %s", encoded)
		}
	}
}

// TestProjectPublishesUnknownRecoveryPathsThatExplainTheRollup runs the
// recovery analysis on backups whose paths cannot be evaluated and checks
// that the contract carries an Unknown path for each, so an Unknown coverage
// rollup is never published without the path that explains it.
func TestProjectPublishesUnknownRecoveryPathsThatExplainTheRollup(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*barmancloud.Backup)
		timeline uint64
		reason   string
	}{
		{name: "segment size differs from the WAL catalog", mutate: func(b *barmancloud.Backup) { b.SegmentSize = 64 << 20 }, timeline: 1, reason: "backup WAL anchors"},
		{name: "timeline only in the begin WAL name", mutate: func(b *barmancloud.Backup) {
			b.Timeline, b.BeginWAL, b.EndWAL = 0, "000000020000000000000001", "000000030000000000000001"
		}, timeline: 2, reason: "backup WAL anchors"},
		{name: "conflicting system identifiers without a timeline", mutate: func(b *barmancloud.Backup) { b.Timeline = 0 }, timeline: 1, reason: "system identifiers"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := completeSource(t)
			backups := source.BarmanCatalog.Backups
			test.mutate(&backups[0])
			if test.reason == "system identifiers" {
				other := backups[0]
				other.ID, other.SystemID = "20260728T110000", "987654321"
				backups = append(backups, other)
				source.BarmanCatalog.Backups = backups
			}
			source.BarmanRecovery = barmancloud.AnalyzeRecovery(context.Background(), &storetest.Fake{}, backups, source.BarmanWAL, nil, false, barmancloud.RecoveryOptions{})
			if source.BarmanRecovery.Servers[0].CoverageState != evidence.Unknown {
				t.Fatalf("coverage = %#v", source.BarmanRecovery.Servers[0])
			}
			publication, err := Project(source, testProjectOptions())
			if err != nil {
				t.Fatal(err)
			}
			if len(publication.RecoveryPaths) != len(backups) {
				t.Fatalf("published paths = %#v, want one per backup", publication.RecoveryPaths)
			}
			for _, path := range publication.RecoveryPaths {
				if path.State != evidencev1alpha1.Unknown || path.TargetTimeline != test.timeline {
					t.Fatalf("published path = %#v", path)
				}
			}
		})
	}
}

func TestProjectFailedRefreshRetainsGenerationAndMakesCurrentEvidenceUnknown(t *testing.T) {
	source := completeSource(t)
	source.RefreshGeneration = 8
	source.LastAttemptAt = source.LastRefreshAt.Add(time.Minute)
	source.LastRefreshAt = source.LastAttemptAt.Add(time.Second)
	source.RefreshFailure = fault.Throttled
	source.Evidence.Stale = true
	source.Evidence.State = evidence.Unknown
	source.Evidence.Reason = "refresh failed: throttled"
	for index := range source.Evidence.Capabilities {
		source.Evidence.Capabilities[index].State = evidence.Unknown
		source.Evidence.Capabilities[index].Reason = "stale after refresh failed: throttled"
	}
	publication, err := Project(source, testProjectOptions())
	if err != nil {
		t.Fatal(err)
	}
	if publication.Snapshot.Revision != 8 || publication.Snapshot.EvidenceGeneration != 7 || !publication.Snapshot.Stale || publication.Snapshot.State != evidencev1alpha1.Unknown || publication.Snapshot.Reason.Code != "refresh-throttled" {
		t.Fatalf("stale publication = %#v", publication.Snapshot)
	}
	if len(publication.Backups) != 1 || publication.Snapshot.Details.BarmanCloud.BackupItems == nil {
		t.Fatal("failed refresh discarded the last complete generation")
	}
}

func TestProjectIncompleteFirstRefreshPublishesNoGenerationCounts(t *testing.T) {
	source := inventory.Initial(barmancloud.New().Descriptor())
	started := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	source.RefreshGeneration = 1
	source.LastAttemptAt = started
	source.LastRefreshAt = started.Add(time.Second)
	source.RefreshFailure = fault.Timeout
	source.Evidence.Generation = 1
	source.Evidence.StartedAt = started
	source.Evidence.Completeness = evidence.Incomplete
	source.Evidence.Reason = "refresh failed: timeout"
	publication, err := Project(source, testProjectOptions())
	if err != nil {
		t.Fatal(err)
	}
	if publication.Snapshot.EvidenceGeneration != 0 || publication.Snapshot.StartedAt != nil || publication.Snapshot.CompletedAt != nil || publication.Snapshot.State != evidencev1alpha1.Unknown {
		t.Fatalf("incomplete first publication = %#v", publication.Snapshot)
	}
	details := publication.Snapshot.Details.BarmanCloud
	if details.BackupItems != nil || details.WALRangeItems != nil || details.BackupStates != nil || details.WALCounts != nil {
		t.Fatalf("incomplete scan exposed generation counts: %#v", details)
	}
}

func TestProjectRejectsSnapshotOutsideExactScope(t *testing.T) {
	source := completeSource(t)
	source.Scopes = append(source.Scopes, inventory.Scope{Name: "other"})
	if _, err := Project(source, testProjectOptions()); err == nil {
		t.Fatal("multi-scope standalone snapshot was accepted by one-scope producer")
	}
}

func TestProjectRejectsCrossScopeFormatEvidence(t *testing.T) {
	source := completeSource(t)
	source.BarmanCatalog.Backups[0].Server = "other"
	if _, err := Project(source, testProjectOptions()); err == nil {
		t.Fatal("cross-scope format evidence was accepted")
	}
}

func completeSource(t testing.TB) inventory.Snapshot {
	t.Helper()
	started := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	completed := started.Add(3 * time.Second)
	snapshot := inventory.Initial(barmancloud.New().Descriptor())
	snapshot.RefreshGeneration = 7
	snapshot.LastAttemptAt = started
	snapshot.LastRefreshAt = completed
	snapshot.TotalsKnown = true
	snapshot.ObjectCount = 3
	snapshot.StoredBytes = 30
	snapshot.Scopes = []inventory.Scope{{Name: "orders", Recognized: true, ObjectCount: 3, StoredBytes: 30}}
	snapshot.Evidence.Generation = 7
	snapshot.Evidence.StartedAt = started
	snapshot.Evidence.CompletedAt = completed
	snapshot.Evidence.Completeness = evidence.Complete
	snapshot.Evidence.Compatibility = evidence.Healthy
	snapshot.Evidence.State = evidence.Healthy
	snapshot.Evidence.Reason = "Barman catalog, WAL, timeline, recovery, and retention evidence evaluated"
	for index := range snapshot.Evidence.Capabilities {
		snapshot.Evidence.Capabilities[index].Support = evidence.Supported
		snapshot.Evidence.Capabilities[index].State = evidence.Healthy
		snapshot.Evidence.Capabilities[index].Reason = "evidence evaluated"
	}
	firstWAL, err := barmancloud.WALName(1, 0, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	lastWAL, err := barmancloud.WALName(1, 1, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.BarmanCatalog = barmancloud.Catalog{Backups: []barmancloud.Backup{{
		Server: "orders", ID: "20260728T100000", Status: "DONE", Type: "full", State: evidence.Healthy, Reason: "backup is structurally usable",
		SystemID: "123456789", PostgreSQLVersion: 180000, Timeline: 1, SegmentSize: 16 << 20,
		BeginWAL: firstWAL, EndWAL: lastWAL, BeginLSN: "0/0", EndLSN: "0/1000000", BeginAt: started, EndAt: completed,
		LogicalBytes: 20, StoredBytes: 10, Artifacts: []string{"orders/base/20260728T100000/artifact-key-canary"},
	}}}
	snapshot.BarmanWAL = barmancloud.WALCatalog{Servers: []barmancloud.ServerWAL{{
		Server: "orders", State: evidence.Healthy, Reason: "no segment-name gaps observed within compact ranges",
		PostgreSQLVersion: 180000, SegmentSize: 16 << 20, Counts: barmancloud.WALCounts{Segments: 2},
		Ranges:               []barmancloud.WALRange{{Timeline: 1, Start: 0, End: 1, Count: 2, First: firstWAL, Last: lastWAL, LatestReceipt: completed, EndReceipt: completed}},
		LatestArchiveReceipt: completed,
	}}}
	snapshot.BarmanRecovery = barmancloud.RecoveryCatalog{Servers: []barmancloud.ServerRecovery{{
		Server: "orders", TimelineState: evidence.Healthy, TimelineReason: "timeline 1 requires no history file",
		CoverageState: evidence.Healthy, CoverageReason: "observed recovery coverage evaluated",
		Paths: []barmancloud.RecoveryPath{{
			Server: "orders", BackupID: "20260728T100000", TargetTimeline: 1, State: evidence.Healthy, Reason: "contiguous archive frontier observed", Stop: barmancloud.CoverageFrontier,
			LowerBound: completed, StartTimeline: 1, StartPosition: 0, StartWAL: firstWAL, FrontierTimeline: 1, FrontierPosition: 1, FrontierWAL: lastWAL, FrontierReceipt: completed,
			Assumptions: []string{"segment-name presence only", "timeline history metadata only", "WAL bytes and restore execution not verified", "backup system identifier retained; WAL objects do not carry it"},
		}},
		Retention: barmancloud.RetentionSummary{VisibleBackups: 1, StructurallyUsable: 1, OldestCompletion: completed, NewestCompletion: completed, State: evidence.Healthy, Reason: "retention evidence is descriptive"},
	}}}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// testProjectOptions are testOptions with the producer instance NewEngine
// would otherwise draw: Project alone requires one.
func testProjectOptions() Options {
	options := testOptions()
	options.ProducerInstance = "0123456789abcdef0123456789abcdef"
	return options
}

func testOptions() Options {
	return Options{
		ProducerVersion: "1.0.0", ClusterNamespace: "database-team", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191",
		Repository: evidencev1alpha1.RepositoryFingerprintInput{Provider: evidencev1alpha1.ProviderS3, Bucket: "synthetic-bucket", Prefix: "cluster", Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders"},
	}
}
