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

package inventory

import (
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/repository"
)

func TestSnapshotRejectsPartialTotals(t *testing.T) {
	t.Parallel()
	snapshot := Initial(repository.Descriptor{ID: "synthetic", ScopeKind: "vault", Capabilities: []evidence.CapabilityID{evidence.ObjectInventory}})
	snapshot.ObjectCount = 1
	if err := snapshot.Validate(); err == nil {
		t.Fatal("partial total was accepted while totals were unknown")
	}
}

func TestCacheDeepClonesPublishedInventory(t *testing.T) {
	t.Parallel()
	snapshot := Initial(repository.Descriptor{ID: "synthetic", ScopeKind: "vault", Capabilities: []evidence.CapabilityID{evidence.ObjectInventory}})
	snapshot.Evidence.Completeness = evidence.Complete
	snapshot.TotalsKnown = true
	snapshot.Scopes = []Scope{{Name: "alpha"}}
	snapshot.BarmanWAL = barmancloud.WALCatalog{Servers: []barmancloud.ServerWAL{{
		Server: "alpha", State: evidence.Healthy, Reason: "complete", PostgreSQLVersion: 180000, SegmentSize: 16 << 20,
		Ranges: []barmancloud.WALRange{{Timeline: 1, Start: 1, End: 1, Count: 1, First: "000000010000000000000001", Last: "000000010000000000000001"}},
	}}}
	snapshot.BarmanRecovery = barmancloud.RecoveryCatalog{Servers: []barmancloud.ServerRecovery{{Server: "alpha", TimelineState: evidence.Healthy, TimelineReason: "complete", CoverageState: evidence.Healthy, CoverageReason: "complete", Histories: []barmancloud.TimelineHistory{{Server: "alpha", Key: "alpha/wals/00000002.history", Timeline: 2, State: evidence.Healthy, Reason: "complete", Edges: []barmancloud.HistoryEdge{{Parent: 1, Child: 2, SwitchLSN: 1, SwitchPosition: 1, SwitchWAL: "000000020000000000000001"}}}}, Paths: []barmancloud.RecoveryPath{{Server: "alpha", BackupID: "id", TargetTimeline: 2, State: evidence.Healthy, Reason: "complete", Stop: barmancloud.CoverageFrontier, Assumptions: []string{"bounded"}}}, Retention: barmancloud.RetentionSummary{State: evidence.Healthy, Reason: "complete"}}}}
	cache, err := NewCache(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Scopes[0].Name = "mutated"
	snapshot.BarmanWAL.Servers[0].Ranges[0].First = "mutated"
	snapshot.BarmanRecovery.Servers[0].Histories[0].Edges[0].Parent = 9
	snapshot.BarmanRecovery.Servers[0].Paths[0].Assumptions[0] = "mutated"
	loaded := cache.Load()
	loaded.Scopes[0].Name = "also-mutated"
	loaded.BarmanWAL.Servers[0].Ranges[0].First = "also-mutated"
	loaded.BarmanRecovery.Servers[0].Histories[0].Edges[0].Parent = 8
	if got := cache.Load().Scopes[0].Name; got != "alpha" {
		t.Fatalf("cached scope was mutable: %q", got)
	}
	if got := cache.Load().BarmanWAL.Servers[0].Ranges[0].First; got != "000000010000000000000001" {
		t.Fatalf("cached WAL range was mutable: %q", got)
	}
	if got := cache.Load().BarmanRecovery.Servers[0].Histories[0].Edges[0].Parent; got != 1 {
		t.Fatalf("cached history edge was mutable: %d", got)
	}
}
