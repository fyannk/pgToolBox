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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func TestEnginePaginatesOneImmutablePublicationDeterministically(t *testing.T) {
	source := sourceWithBackups(t, 3)
	engine := newTestEngine(t, source, 0x11)
	snapshot := engine.Snapshot()
	request := PageRequest{Revision: snapshot.Revision, Limit: 2}
	first, err := engine.Backups(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	if first.TotalItems == nil || *first.TotalItems != 3 || len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first page = %#v", first)
	}
	repeated, err := engine.Backups(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, repeated) {
		t.Fatal("identical page request produced different semantic output")
	}
	second, err := engine.Backups(PageRequest{Revision: snapshot.Revision, Limit: 2, Cursor: *first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].BackupID != "20260728T100002" || second.NextCursor != nil || second.EvidenceGeneration != snapshot.EvidenceGeneration {
		t.Fatalf("second page = %#v", second)
	}

	walRanges, err := engine.WALRanges(PageRequest{Revision: snapshot.Revision})
	if err != nil || walRanges.TotalItems == nil || *walRanges.TotalItems != 1 || len(walRanges.Items) != 1 {
		t.Fatalf("WAL ranges page = %#v, %v", walRanges, err)
	}
	walGaps, err := engine.WALGaps(PageRequest{Revision: snapshot.Revision})
	if err != nil || walGaps.TotalItems == nil || *walGaps.TotalItems != 0 || walGaps.Items == nil || len(walGaps.Items) != 0 {
		t.Fatalf("WAL gaps page = %#v, %v", walGaps, err)
	}
	recoveryPaths, err := engine.RecoveryPaths(PageRequest{Revision: snapshot.Revision})
	if err != nil || recoveryPaths.TotalItems == nil || *recoveryPaths.TotalItems != 1 || len(recoveryPaths.Items) != 1 {
		t.Fatalf("recovery paths page = %#v, %v", recoveryPaths, err)
	}
}

func TestEngineCursorAuthenticationAndBinding(t *testing.T) {
	engine := newTestEngine(t, sourceWithBackups(t, 3), 0x22)
	page, err := engine.Backups(PageRequest{Revision: 7, Limit: 1})
	if err != nil || page.NextCursor == nil {
		t.Fatalf("first page = %#v, %v", page, err)
	}
	cursor := *page.NextCursor
	payload, err := engine.decodeCursor(cursor)
	if err != nil || payload.Route != backupRoute || payload.Revision != 7 || payload.EvidenceGeneration != 7 || payload.Position != 1 || payload.Limit != 1 {
		t.Fatalf("cursor binding = %#v, %v", payload, err)
	}
	separator := strings.IndexByte(cursor, '.')
	tampered := cursor[:separator+1] + differentCharacter(cursor[separator+1]) + cursor[separator+2:]
	wrongVersion, _ := engine.encodeCursor(cursorPayload{Version: 2, Route: backupRoute, Revision: 7, EvidenceGeneration: 7, Position: 1, Limit: 1})
	wrongRevision, _ := engine.encodeCursor(cursorPayload{Version: 1, Route: backupRoute, Revision: 6, EvidenceGeneration: 7, Position: 1, Limit: 1})
	wrongGeneration, _ := engine.encodeCursor(cursorPayload{Version: 1, Route: backupRoute, Revision: 7, EvidenceGeneration: 6, Position: 1, Limit: 1})
	invalidPosition, _ := engine.encodeCursor(cursorPayload{Version: 1, Route: backupRoute, Revision: 7, EvidenceGeneration: 7, Position: 3, Limit: 1})

	tests := []struct {
		name    string
		request PageRequest
		call    func(PageRequest) error
	}{
		{name: "tampered", request: PageRequest{Revision: 7, Limit: 1, Cursor: tampered}, call: backupPageError(engine)},
		{name: "malformed", request: PageRequest{Revision: 7, Limit: 1, Cursor: "not-a-cursor"}, call: backupPageError(engine)},
		{name: "version mismatch", request: PageRequest{Revision: 7, Limit: 1, Cursor: wrongVersion}, call: backupPageError(engine)},
		{name: "revision mismatch", request: PageRequest{Revision: 7, Limit: 1, Cursor: wrongRevision}, call: backupPageError(engine)},
		{name: "generation mismatch", request: PageRequest{Revision: 7, Limit: 1, Cursor: wrongGeneration}, call: backupPageError(engine)},
		{name: "invalid position", request: PageRequest{Revision: 7, Limit: 1, Cursor: invalidPosition}, call: backupPageError(engine)},
		{name: "limit mismatch", request: PageRequest{Revision: 7, Limit: 2, Cursor: cursor}, call: backupPageError(engine)},
		{name: "route mismatch", request: PageRequest{Revision: 7, Limit: 1, Cursor: cursor}, call: walRangePageError(engine)},
		{name: "oversized", request: PageRequest{Revision: 7, Limit: 1, Cursor: strings.Repeat("a", MaximumCursorBytes+1)}, call: backupPageError(engine)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(test.request); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want invalid request", err)
			}
		})
	}

	restarted := newTestEngine(t, sourceWithBackups(t, 3), 0x23)
	if _, err := restarted.Backups(PageRequest{Revision: 7, Limit: 1, Cursor: cursor}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("cursor survived process-key replacement: %v", err)
	}
}

func TestEngineRefreshBetweenPagesReturnsPublicationChanged(t *testing.T) {
	engine := newTestEngine(t, sourceWithBackups(t, 3), 0x33)
	first, err := engine.Backups(PageRequest{Revision: 7, Limit: 1})
	if err != nil || first.NextCursor == nil {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	next := sourceWithBackups(t, 4)
	advanceCompleteSource(&next, 8)
	if err := engine.Publish(next); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Backups(PageRequest{Revision: 7, Limit: 1, Cursor: *first.NextCursor}); !errors.Is(err, ErrPublicationChanged) {
		t.Fatalf("old page continuation error = %v", err)
	}
	snapshot := engine.Snapshot()
	if snapshot.Revision != 8 || snapshot.EvidenceGeneration != 8 {
		t.Fatalf("new publication identity = (%d,%d)", snapshot.Revision, snapshot.EvidenceGeneration)
	}
}

func TestEngineFailedRefreshRetainsGenerationCollections(t *testing.T) {
	source := sourceWithBackups(t, 3)
	engine := newTestEngine(t, source, 0x44)
	failed := failedRefresh(source, 8)
	if err := engine.Publish(failed); err != nil {
		t.Fatal(err)
	}
	snapshot := engine.Snapshot()
	if snapshot.Revision != 8 || snapshot.EvidenceGeneration != 7 || !snapshot.Stale || snapshot.State != evidencev1alpha1.Unknown {
		t.Fatalf("failed-refresh snapshot = %#v", snapshot)
	}
	page, err := engine.Backups(PageRequest{Revision: 8, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.EvidenceGeneration != 7 || page.TotalItems == nil || *page.TotalItems != 3 || len(page.Items) != 2 {
		t.Fatalf("retained page = %#v", page)
	}
}

func TestEngineRejectsInvalidConflictingAndRegressivePublications(t *testing.T) {
	source := sourceWithBackups(t, 2)
	engine := newTestEngine(t, source, 0x55)
	if err := engine.Publish(source); err != nil {
		t.Fatalf("identical publication was not idempotent: %v", err)
	}

	conflicting := sourceWithBackups(t, 3)
	if err := engine.Publish(conflicting); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("conflicting revision error = %v", err)
	}
	regressive := sourceWithBackups(t, 2)
	advanceCompleteSource(&regressive, 6)
	if err := engine.Publish(regressive); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("regressive revision error = %v", err)
	}
	invalid := sourceWithBackups(t, 2)
	invalid.BarmanCatalog.Backups[0].Server = "other"
	if err := engine.Publish(invalid); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("invalid projection error = %v", err)
	}
	if snapshot := engine.Snapshot(); snapshot.Revision != 7 || snapshot.Details.BarmanCloud.BackupItems == nil || *snapshot.Details.BarmanCloud.BackupItems != 2 {
		t.Fatalf("failed publish replaced current evidence: %#v", snapshot)
	}
}

func TestEngineReturnsMutationIsolatedResources(t *testing.T) {
	source := sourceWithBackups(t, 2)
	options := testOptions()
	clusterName := "orders"
	options.ClusterName = &clusterName
	engine, err := NewEngine(EngineOptions{Projection: options, Initial: source, CursorEntropy: bytes.NewReader(bytes.Repeat([]byte{0x66}, cursorKeyBytes))})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := engine.Snapshot()
	*snapshot.Identity.Cluster.Name = "mutated"
	snapshot.Capabilities[0].Reason.Message = "mutated"
	*snapshot.Details.BarmanCloud.BackupItems = 99
	if current := engine.Snapshot(); *current.Identity.Cluster.Name != "orders" || current.Capabilities[0].Reason.Message == "mutated" || *current.Details.BarmanCloud.BackupItems != 2 {
		t.Fatalf("snapshot mutation reached engine: %#v", current)
	}

	backups, err := engine.Backups(PageRequest{Revision: 7})
	if err != nil {
		t.Fatal(err)
	}
	*backups.Items[0].Status = "MUTATED"
	currentBackups, err := engine.Backups(PageRequest{Revision: 7})
	if err != nil || *currentBackups.Items[0].Status == "MUTATED" {
		t.Fatalf("backup mutation reached engine: %#v, %v", currentBackups, err)
	}

	paths, err := engine.RecoveryPaths(PageRequest{Revision: 7})
	if err != nil {
		t.Fatal(err)
	}
	paths.Items[0].AssumptionCodes[0] = "mutated"
	currentPaths, err := engine.RecoveryPaths(PageRequest{Revision: 7})
	if err != nil || currentPaths.Items[0].AssumptionCodes[0] == "mutated" {
		t.Fatalf("recovery-path mutation reached engine: %#v, %v", currentPaths, err)
	}

	source.BarmanCatalog.Backups[0].ID = "mutated-after-publish"
	currentBackups, err = engine.Backups(PageRequest{Revision: 7})
	if err != nil || currentBackups.Items[0].BackupID == "mutated-after-publish" {
		t.Fatalf("source mutation reached engine: %#v, %v", currentBackups, err)
	}
}

func TestEngineRejectsInvalidBoundsAndPagesWithoutCompleteEvidence(t *testing.T) {
	engine := newTestEngine(t, sourceWithBackups(t, 1), 0x77)
	for _, request := range []PageRequest{
		{Revision: 0},
		{Revision: 7, Limit: -1},
		{Revision: 7, Limit: MaximumPageSize + 1},
	} {
		if _, err := engine.Backups(request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("request %#v error = %v", request, err)
		}
	}
	if _, err := engine.Backups(PageRequest{Revision: 6}); !errors.Is(err, ErrPublicationChanged) {
		t.Fatalf("old revision error = %v", err)
	}

	incomplete := inventory.Initial(barmancloud.New().Descriptor())
	started := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	incomplete.RefreshGeneration = 1
	incomplete.LastAttemptAt = started
	incomplete.LastRefreshAt = started.Add(time.Second)
	incomplete.RefreshFailure = fault.Timeout
	incomplete.Evidence.Generation = 1
	incomplete.Evidence.StartedAt = started
	incomplete.Evidence.Completeness = evidence.Incomplete
	incomplete.Evidence.Reason = "refresh failed: timeout"
	withoutGeneration := newTestEngine(t, incomplete, 0x78)
	if _, err := withoutGeneration.Backups(PageRequest{Revision: 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("generationless page error = %v", err)
	}
}

func TestEngineAppliesDefaultAndMaximumPageSizes(t *testing.T) {
	engine := newTestEngine(t, sourceWithBackups(t, MaximumPageSize+1), 0x79)
	defaultPage, err := engine.Backups(PageRequest{Revision: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultPage.Items) != DefaultPageSize || defaultPage.NextCursor == nil {
		t.Fatalf("default page contains %d items and cursor %v", len(defaultPage.Items), defaultPage.NextCursor != nil)
	}
	maximumPage, err := engine.Backups(PageRequest{Revision: 7, Limit: MaximumPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if len(maximumPage.Items) != MaximumPageSize || maximumPage.NextCursor == nil {
		t.Fatalf("maximum page contains %d items and cursor %v", len(maximumPage.Items), maximumPage.NextCursor != nil)
	}
}

func TestNewEngineRequiresCompleteCursorEntropy(t *testing.T) {
	_, err := NewEngine(EngineOptions{
		Projection: testOptions(), Initial: completeSource(t),
		CursorEntropy: bytes.NewReader(make([]byte, cursorKeyBytes-1)),
	})
	if err == nil {
		t.Fatal("short cursor entropy was accepted")
	}
}

func TestEngineConcurrentPublishAndReadUsesWholePublications(t *testing.T) {
	engine := newTestEngine(t, sourceWithBackups(t, 3), 0x88)
	sources := make([]inventory.Snapshot, 0, 57)
	for revision := uint64(8); revision <= 64; revision++ {
		source := sourceWithBackups(t, int(revision%4)+1)
		advanceCompleteSource(&source, revision)
		sources = append(sources, source)
	}
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for _, source := range sources {
			if err := engine.Publish(source); err != nil {
				t.Errorf("publish revision %d: %v", source.RefreshGeneration, err)
				return
			}
		}
	}()
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 200 {
				snapshot := engine.Snapshot()
				if err := snapshot.Validate(); err != nil {
					t.Errorf("snapshot: %v", err)
					return
				}
				page, err := engine.Backups(PageRequest{Revision: snapshot.Revision, Limit: 1})
				if errors.Is(err, ErrPublicationChanged) {
					continue
				}
				if err != nil {
					t.Errorf("page: %v", err)
					return
				}
				if page.Revision != snapshot.Revision || page.EvidenceGeneration != snapshot.EvidenceGeneration {
					t.Errorf("mixed publication snapshot=(%d,%d) page=(%d,%d)", snapshot.Revision, snapshot.EvidenceGeneration, page.Revision, page.EvidenceGeneration)
					return
				}
			}
		}()
	}
	wait.Wait()
}

func newTestEngine(t testing.TB, source inventory.Snapshot, keyByte byte) *Engine {
	t.Helper()
	engine, err := NewEngine(EngineOptions{
		Projection: testOptions(), Initial: source,
		CursorEntropy: bytes.NewReader(bytes.Repeat([]byte{keyByte}, cursorKeyBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func sourceWithBackups(t testing.TB, count int) inventory.Snapshot {
	t.Helper()
	source := completeSource(t)
	base := source.BarmanCatalog.Backups[0]
	source.BarmanCatalog.Backups = make([]barmancloud.Backup, 0, count)
	for index := range count {
		backup := base
		backup.ID = fmt.Sprintf("20260728T%06d", 100000+index)
		backup.Artifacts = append([]string(nil), base.Artifacts...)
		source.BarmanCatalog.Backups = append(source.BarmanCatalog.Backups, backup)
	}
	source.ObjectCount = int64(count + 2)
	source.StoredBytes = int64(count*10 + 20)
	source.Scopes[0].ObjectCount = source.ObjectCount
	source.Scopes[0].StoredBytes = source.StoredBytes
	source.BarmanRecovery.Servers[0].Retention.VisibleBackups = count
	source.BarmanRecovery.Servers[0].Retention.StructurallyUsable = count
	if err := source.Validate(); err != nil {
		t.Fatal(err)
	}
	return source
}

func advanceCompleteSource(source *inventory.Snapshot, revision uint64) {
	started := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC).Add(time.Duration(revision) * time.Minute)
	source.RefreshGeneration = revision
	source.LastAttemptAt = started
	source.LastRefreshAt = started.Add(3 * time.Second)
	source.RefreshFailure = ""
	source.Evidence.Generation = revision
	source.Evidence.StartedAt = started
	source.Evidence.CompletedAt = started.Add(3 * time.Second)
	source.Evidence.Stale = false
}

func failedRefresh(source inventory.Snapshot, revision uint64) inventory.Snapshot {
	source.RefreshGeneration = revision
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
	return source
}

func differentCharacter(value byte) string {
	if value == 'A' {
		return "B"
	}
	return "A"
}

func backupPageError(engine *Engine) func(PageRequest) error {
	return func(request PageRequest) error {
		_, err := engine.Backups(request)
		return err
	}
}

func walRangePageError(engine *Engine) func(PageRequest) error {
	return func(request PageRequest) error {
		_, err := engine.WALRanges(request)
		return err
	}
}

func TestEngineStampsOneRandomProducerInstancePerProcess(t *testing.T) {
	source := sourceWithBackups(t, 1)
	first := newTestEngine(t, source, 0x21)
	second := newTestEngine(t, source, 0x21)
	instance := first.Snapshot().ProducerInstance
	if !evidencev1alpha1.ValidProducerInstance(instance) {
		t.Fatalf("producer instance %q is not a valid contract value", instance)
	}
	if second.Snapshot().ProducerInstance == instance {
		t.Fatal("two engines share a producer instance; a restart would be indistinguishable")
	}
	// Same generation, same cursor key: only the instance tells the two
	// processes apart, which is exactly the consumer cache key.
	if first.Snapshot().EvidenceGeneration != second.Snapshot().EvidenceGeneration {
		t.Fatal("test precondition: equal generations expected")
	}
	refreshed := source
	refreshed.RefreshGeneration++
	refreshed.Evidence.Generation++
	if err := first.Publish(refreshed); err != nil {
		t.Fatal(err)
	}
	if first.Snapshot().ProducerInstance != instance {
		t.Fatal("producer instance changed within one process")
	}

	options := testOptions()
	options.ProducerInstance = "00112233445566778899aabbccddeeff"
	pinned, err := NewEngine(EngineOptions{Projection: options, Initial: source, CursorEntropy: bytes.NewReader(bytes.Repeat([]byte{1}, cursorKeyBytes))})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Snapshot().ProducerInstance != options.ProducerInstance {
		t.Fatal("explicit producer instance was not kept")
	}
}
