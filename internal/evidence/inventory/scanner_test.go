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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/redact"
	"github.com/fyannk/pgToolBox/internal/evidence/repository"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

func TestScannerPaginatesAndPublishesCompleteInventory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	var requests []store.ListRequest
	reader := &storetest.Fake{ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
		requests = append(requests, request)
		switch request.Cursor {
		case "":
			return store.Page{Objects: []store.Object{
				{Key: "alpha/base/20260727/backup.info", Size: 10, LastModified: now.Add(-3 * time.Minute)},
				{Key: "unrelated/object", Size: 5, LastModified: now.Add(-time.Minute)},
			}, NextCursor: "page-two"}, nil
		case "page-two":
			return store.Page{Objects: []store.Object{
				{Key: "alpha/wals/000000010000000000000001", Size: 20, LastModified: now.Add(-2 * time.Minute)},
			}}, nil
		default:
			t.Fatalf("unexpected cursor %q", request.Cursor)
			return store.Page{}, nil
		}
	}}
	scanner, cache, _ := newTestScanner(t, reader, 10, 2, 2, func() time.Time { return now })

	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if len(requests) != 2 || requests[0].Limit != 2 || requests[1].Limit != 2 {
		t.Fatalf("list requests = %#v", requests)
	}
	if !snapshot.TotalsKnown || snapshot.ObjectCount != 3 || snapshot.StoredBytes != 35 || snapshot.UnscopedObjectCount != 1 {
		t.Fatalf("inventory totals = %#v", snapshot)
	}
	if snapshot.PagesExamined != 2 || snapshot.ObjectsExamined != 3 || len(snapshot.Scopes) != 1 {
		t.Fatalf("bounded inventory shape = %#v", snapshot)
	}
	scope := snapshot.Scopes[0]
	if scope.Name != "alpha" || !scope.Recognized || scope.ObjectCount != 2 || scope.StoredBytes != 30 {
		t.Fatalf("scope = %#v", scope)
	}
	if len(snapshot.RecentObjects) != 2 || snapshot.RecentObjects[0].Key != "unrelated/object" || snapshot.RecentObjects[1].Key != "alpha/wals/000000010000000000000001" {
		t.Fatalf("recent objects = %#v", snapshot.RecentObjects)
	}
	if snapshot.Evidence.Completeness != evidence.Complete || snapshot.Evidence.State != evidence.Unknown || snapshot.Evidence.Reason != "backup semantics not evaluated" {
		t.Fatalf("overall evidence was overstated: %#v", snapshot.Evidence)
	}
	assertCapability(t, snapshot, evidence.ObjectInventory, evidence.Supported, evidence.Healthy)
	assertCapability(t, snapshot, evidence.CatalogListing, evidence.SupportUnknown, evidence.Unknown)
}

func TestScannerTreatsEmptyDestinationAsCompleteAndReady(t *testing.T) {
	t.Parallel()
	reader := &storetest.Fake{}
	scanner, cache, ready := newTestScanner(t, reader, 10, 10, 2, time.Now)
	if category := scanner.Probe(context.Background()); category != fault.Unknown {
		t.Fatalf("Probe() category = %s", category)
	}
	if !ready.Result().Ready {
		t.Fatalf("empty destination readiness = %#v", ready.Result())
	}
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if !snapshot.TotalsKnown || snapshot.ObjectCount != 0 || snapshot.StoredBytes != 0 || snapshot.Evidence.Completeness != evidence.Complete {
		t.Fatalf("empty inventory = %#v", snapshot)
	}
}

func TestScannerRetainsCompleteSnapshotAsStaleAfterRefreshFailure(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	reader := &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		if fail.Load() {
			return store.Page{}, categorizedError{category: fault.Throttled}
		}
		return store.Page{Objects: []store.Object{{Key: "alpha/base/backup.info", Size: 42}}}, nil
	}}
	scanner, cache, _ := newTestScanner(t, reader, 10, 10, 2, time.Now)
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	complete := cache.Load()
	fail.Store(true)
	if err := scanner.Refresh(context.Background()); fault.Categorize(err) != fault.Throttled {
		t.Fatalf("Refresh() error = %v", err)
	}
	stale := cache.Load()
	if !stale.TotalsKnown || stale.ObjectCount != complete.ObjectCount || stale.Evidence.Generation != complete.Evidence.Generation || !stale.Evidence.Stale {
		t.Fatalf("last complete generation was not retained: before=%#v after=%#v", complete, stale)
	}
	if stale.RefreshGeneration <= complete.RefreshGeneration || stale.RefreshFailure != fault.Throttled || stale.Evidence.State != evidence.Unknown {
		t.Fatalf("refresh failure was not exposed conservatively: %#v", stale)
	}
	assertCapability(t, stale, evidence.ObjectInventory, evidence.Supported, evidence.Unknown)
}

func TestScannerFailureBeforeFirstCompleteScanKeepsTotalsUnknown(t *testing.T) {
	t.Parallel()
	reader := &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		return store.Page{}, categorizedError{category: fault.Authentication}
	}}
	scanner, cache, _ := newTestScanner(t, reader, 10, 10, 2, time.Now)
	if err := scanner.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() error = nil")
	}
	snapshot := cache.Load()
	if snapshot.TotalsKnown || snapshot.ObjectCount != 0 || snapshot.Evidence.Completeness != evidence.Incomplete || snapshot.Evidence.State != evidence.Unknown || !snapshot.Evidence.Stale {
		t.Fatalf("failed initial scan exposed partial totals: %#v", snapshot)
	}
}

func TestScannerSafetyCeilingFailsUnknownWithoutPublishingPartialTotals(t *testing.T) {
	t.Parallel()
	reader := &storetest.Fake{ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
		objects := make([]store.Object, request.Limit)
		for index := range objects {
			objects[index] = store.Object{Key: "alpha/wals/object-" + string(rune('a'+index)), Size: 1}
		}
		return store.Page{Objects: objects, NextCursor: "more"}, nil
	}}
	scanner, cache, _ := newTestScanner(t, reader, 3, 3, 2, time.Now)
	if err := scanner.Refresh(context.Background()); fault.Categorize(err) != fault.SafetyLimit {
		t.Fatalf("Refresh() error = %v", err)
	}
	if snapshot := cache.Load(); snapshot.TotalsKnown || snapshot.Evidence.Completeness != evidence.Incomplete {
		t.Fatalf("ceiling published partial inventory: %#v", snapshot)
	}
}

func TestScannerCancellationDoesNotReplaceCachedGeneration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	reader := &storetest.Fake{ListFunc: func(ctx context.Context, _ store.ListRequest) (store.Page, error) {
		cancel()
		<-ctx.Done()
		return store.Page{}, ctx.Err()
	}}
	scanner, cache, _ := newTestScanner(t, reader, 10, 10, 2, time.Now)
	before := cache.Load()
	if err := scanner.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Refresh() error = %v", err)
	}
	after := cache.Load()
	if after.RefreshGeneration != before.RefreshGeneration || after.Evidence.Completeness != before.Evidence.Completeness {
		t.Fatalf("canceled refresh published state: before=%#v after=%#v", before, after)
	}
}

func TestScannerPublishesOnlyAfterEveryPageCompletes(t *testing.T) {
	t.Parallel()
	secondPageStarted := make(chan struct{})
	releaseSecondPage := make(chan struct{})
	reader := &storetest.Fake{ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
		if request.Cursor == "" {
			return store.Page{Objects: []store.Object{{Key: "alpha/base/one", Size: 1}}, NextCursor: "two"}, nil
		}
		close(secondPageStarted)
		<-releaseSecondPage
		return store.Page{Objects: []store.Object{{Key: "alpha/base/two", Size: 1}}}, nil
	}}
	scanner, cache, _ := newTestScanner(t, reader, 10, 1, 2, time.Now)
	done := make(chan error, 1)
	go func() { done <- scanner.Refresh(context.Background()) }()
	<-secondPageStarted
	if during := cache.Load(); during.TotalsKnown || during.Evidence.Generation != 0 {
		t.Fatalf("partial generation became visible: %#v", during)
	}
	close(releaseSecondPage)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if after := cache.Load(); !after.TotalsKnown || after.ObjectCount != 2 || after.Evidence.Generation != 1 {
		t.Fatalf("complete generation not published: %#v", after)
	}
}

func TestScannerConcurrentReadersObserveValidSnapshots(t *testing.T) {
	t.Parallel()
	var scan atomic.Int64
	reader := &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		generation := scan.Add(1)
		return store.Page{Objects: []store.Object{{Key: "alpha/base/object", Size: generation}}}, nil
	}}
	scanner, cache, _ := newTestScanner(t, reader, 100, 100, 2, time.Now)
	var readers sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if err := cache.Load().Validate(); err != nil {
						t.Errorf("observed invalid snapshot: %v", err)
						return
					}
				}
			}
		}()
	}
	for range 50 {
		if err := scanner.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	readers.Wait()
}

func TestScannerWrongFormatObjectsNeverBecomeHealthyCatalogEvidence(t *testing.T) {
	t.Parallel()
	reader := &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		return store.Page{Objects: []store.Object{{Key: "backup/demo/backup.info", Size: 10}}}, nil
	}}
	scanner, cache, _ := newTestScanner(t, reader, 10, 10, 2, time.Now)
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if snapshot.UnscopedObjectCount != 1 || len(snapshot.Scopes) != 0 || snapshot.Evidence.State != evidence.Unknown {
		t.Fatalf("wrong-format layout was accepted: %#v", snapshot)
	}
	assertCapability(t, snapshot, evidence.CatalogListing, evidence.SupportUnknown, evidence.Unknown)
}

func TestScannerPublishesBarmanCatalogOnlyAfterArtifactConfirmation(t *testing.T) {
	t.Parallel()
	reader := &storetest.Fake{
		ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
			return store.Page{Objects: []store.Object{
				{Key: "alpha/base/20260727/backup.info"},
				{Key: "alpha/base/20260727/data.tar.gz", Size: 8},
				{Key: "alpha/wals/0000000100000000/000000010000000000000001", Size: 16 << 20},
			}}, nil
		},
		OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("status=DONE\nsize=8\ncompression=gzip\nversion=180000\nxlog_segment_size=16777216\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000001\n")), nil
		},
		StatFunc: func(context.Context, string) (store.Object, error) {
			return store.Object{Key: "alpha/base/20260727/data.tar.gz", Size: 8}, nil
		},
	}
	scanner, cache, _ := newTestScanner(t, reader, 10, 10, 2, time.Now)
	scanner.analyzeBarmanCatalog = true
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if len(snapshot.BarmanCatalog.Backups) != 1 || snapshot.BarmanCatalog.Backups[0].State != evidence.Healthy || len(snapshot.BarmanWAL.Servers) != 1 || snapshot.BarmanWAL.Servers[0].State != evidence.Healthy || snapshot.Evidence.State != evidence.Healthy {
		t.Fatalf("Barman catalog evidence = %#v", snapshot)
	}
}

func TestScannerConfinesBarmanAnalysisToConfiguredServer(t *testing.T) {
	t.Parallel()
	reader := &storetest.Fake{
		ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
			return store.Page{Objects: []store.Object{
				{Key: "alpha/base/id/backup.info"},
				{Key: "alpha/base/id/data.tar", Size: 8},
				{Key: "alpha/wals/0000000100000000/000000010000000000000001", Size: 16 << 20},
				{Key: "beta/base/id/backup.info"},
				{Key: "beta/base/id/data.tar", Size: 8},
				{Key: "beta/wals/0000000100000000/000000010000000000000001", Size: 16 << 20},
			}}, nil
		},
		OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
			if !strings.HasPrefix(request.Key, "alpha/") {
				t.Fatalf("opened out-of-scope key %q", request.Key)
			}
			return io.NopCloser(strings.NewReader("status=DONE\nsize=8\nversion=180000\nxlog_segment_size=16777216\n")), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) {
			if !strings.HasPrefix(key, "alpha/") {
				t.Fatalf("statted out-of-scope key %q", key)
			}
			return store.Object{Key: key, Size: 8}, nil
		},
	}
	scanner, cache, _ := newTestScanner(t, reader, 10, 10, 2, time.Now)
	scanner.configuredScopes = []string{"alpha"}
	scanner.analyzeBarmanCatalog = true
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if len(snapshot.BarmanCatalog.Backups) != 1 || snapshot.BarmanCatalog.Backups[0].Server != "alpha" || len(snapshot.BarmanWAL.Servers) != 1 || snapshot.BarmanWAL.Servers[0].Server != "alpha" || snapshot.UnscopedObjectCount != 3 {
		t.Fatalf("configured Barman scope escaped: %#v", snapshot)
	}
}

func TestScannerIncompleteRefreshNeitherConfirmsNorClearsBarmanGap(t *testing.T) {
	t.Parallel()
	var mode atomic.Int32
	reader := &storetest.Fake{
		ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
			if mode.Load() == 1 {
				return store.Page{}, categorizedError{category: fault.Throttled}
			}
			objects := []store.Object{
				{Key: "alpha/base/id/backup.info"},
				{Key: "alpha/base/id/data.tar", Size: 8},
				{Key: "alpha/wals/0000000100000000/000000010000000000000001", Size: 16 << 20},
				{Key: "alpha/wals/0000000100000000/000000010000000000000004", Size: 16 << 20},
			}
			if mode.Load() == 2 {
				objects = append(objects[:3],
					store.Object{Key: "alpha/wals/0000000100000000/000000010000000000000002", Size: 16 << 20},
					store.Object{Key: "alpha/wals/0000000100000000/000000010000000000000003", Size: 16 << 20},
					objects[3],
				)
			}
			return store.Page{Objects: objects}, nil
		},
		OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("status=DONE\nsize=8\nversion=180000\nxlog_segment_size=16777216\n")), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) {
			return store.Object{Key: key, Size: 8}, nil
		},
	}
	scanner, cache, _ := newTestScanner(t, reader, 20, 20, 10, time.Now)
	scanner.analyzeBarmanCatalog = true
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := cache.Load()
	if len(first.BarmanWAL.Servers) != 1 || len(first.BarmanWAL.Servers[0].Gaps) != 1 || first.BarmanWAL.Servers[0].Gaps[0].Status != barmancloud.GapCandidate {
		t.Fatalf("first scan gap = %#v", first.BarmanWAL)
	}
	mode.Store(1)
	if err := scanner.Refresh(context.Background()); fault.Categorize(err) != fault.Throttled {
		t.Fatalf("incomplete refresh error = %v", err)
	}
	incomplete := cache.Load()
	if !incomplete.Evidence.Stale || incomplete.BarmanWAL.Servers[0].Gaps[0].Status != barmancloud.GapCandidate {
		t.Fatalf("incomplete refresh changed gap lifecycle: %#v", incomplete)
	}
	assertCapability(t, incomplete, evidence.WALContinuity, evidence.Supported, evidence.Unknown)
	mode.Store(0)
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	confirmed := cache.Load()
	if confirmed.BarmanWAL.Servers[0].Gaps[0].Status != barmancloud.GapConfirmed {
		t.Fatalf("next complete refresh did not confirm gap: %#v", confirmed.BarmanWAL)
	}
	mode.Store(2)
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleared := cache.Load()
	if len(cleared.BarmanWAL.Servers[0].Gaps) != 0 || cleared.BarmanWAL.Servers[0].State != evidence.Healthy {
		t.Fatalf("complete filled gap did not clear lifecycle: %#v", cleared.BarmanWAL)
	}
}

func TestScannerPublishesBarmanTimelineAndObservedRecoveryCoverage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	reader := &storetest.Fake{
		ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
			return store.Page{Objects: []store.Object{
				{Key: "alpha/base/id/backup.info"},
				{Key: "alpha/base/id/data.tar", Size: 8},
				{Key: "alpha/wals/0000000100000000/000000010000000000000001", LastModified: now.Add(-4 * time.Minute)},
				{Key: "alpha/wals/0000000100000000/000000010000000000000002", LastModified: now.Add(-3 * time.Minute)},
				{Key: "alpha/wals/00000002.history", LastModified: now.Add(-2 * time.Minute)},
				{Key: "alpha/wals/0000000200000000/000000020000000000000002", LastModified: now.Add(-time.Minute)},
				{Key: "alpha/wals/0000000200000000/000000020000000000000003", LastModified: now},
			}}, nil
		},
		OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
			if strings.HasSuffix(request.Key, ".history") {
				return io.NopCloser(strings.NewReader("1\t0/02000000\tpromotion\n")), nil
			}
			return io.NopCloser(strings.NewReader("status=DONE\nsize=8\nversion=180000\nxlog_segment_size=16777216\ntimeline=1\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000001\nend_xlog=0/01000000\n")), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) { return store.Object{Key: key, Size: 8}, nil },
	}
	scanner, cache, _ := newTestScanner(t, reader, 20, 20, 10, func() time.Time { return now })
	scanner.analyzeBarmanCatalog = true
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if len(snapshot.BarmanRecovery.Servers) != 1 || snapshot.BarmanRecovery.Servers[0].TimelineState != evidence.Healthy || snapshot.BarmanRecovery.Servers[0].CoverageState != evidence.Healthy || len(snapshot.BarmanRecovery.Servers[0].Paths) != 2 {
		t.Fatalf("Barman recovery snapshot = %#v", snapshot.BarmanRecovery)
	}
	assertCapability(t, snapshot, evidence.TimelineTraversal, evidence.Supported, evidence.Healthy)
	assertCapability(t, snapshot, evidence.RecoveryCoverage, evidence.Supported, evidence.Healthy)
}

// TestScannerPublishesDespiteAControlCharacterKeyUnderWALs keeps one
// hostile key under <server>/wals/ from rejecting the whole publication: it
// becomes an escaped WAL diagnostic and the snapshot stays fresh.
func TestScannerPublishesDespiteAControlCharacterKeyUnderWALs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	reader := &storetest.Fake{
		ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
			return store.Page{Objects: []store.Object{
				{Key: "alpha/base/id/backup.info"},
				{Key: "alpha/base/id/data.tar", Size: 8},
				{Key: "alpha/wals/0000000100000000/000000010000000000000001", LastModified: now.Add(-time.Minute)},
				{Key: "alpha/wals/0000000100000000/\x1b[2Jevil", LastModified: now},
			}}, nil
		},
		OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("status=DONE\nsize=8\nversion=180000\nxlog_segment_size=16777216\ntimeline=1\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000001\nend_xlog=0/01000000\n")), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) { return store.Object{Key: key, Size: 8}, nil },
	}
	scanner, cache, _ := newTestScanner(t, reader, 20, 20, 10, func() time.Time { return now })
	scanner.analyzeBarmanCatalog = true
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if snapshot.Evidence.Stale || snapshot.RefreshFailure != "" || len(snapshot.BarmanWAL.Servers) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	diagnostics := snapshot.BarmanWAL.Servers[0].Diagnostics
	if len(diagnostics) != 1 || diagnostics[0].Key != `alpha/wals/0000000100000000/\x1b[2Jevil` {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestBarmanCatalogRollupPreservesNonHealthyStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		backups []barmancloud.Backup
		want    evidence.State
	}{
		{name: "healthy", backups: []barmancloud.Backup{{State: evidence.Healthy}}, want: evidence.Healthy},
		{name: "in progress warning", backups: []barmancloud.Backup{{State: evidence.Healthy}, {State: evidence.Warning}}, want: evidence.Warning},
		{name: "failed unhealthy", backups: []barmancloud.Backup{{State: evidence.Healthy}, {State: evidence.Warning}, {State: evidence.Unhealthy}}, want: evidence.Unhealthy},
		{name: "unknown dominates", backups: []barmancloud.Backup{{State: evidence.Unhealthy}, {State: evidence.Unknown}}, want: evidence.Unknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := Initial(barmancloud.New().Descriptor())
			snapshot.BarmanCatalog.Backups = test.backups
			applyBarmanEvidence(&snapshot)
			if snapshot.Evidence.State != test.want {
				t.Fatalf("state = %s, want %s: %#v", snapshot.Evidence.State, test.want, snapshot.Evidence)
			}
			assertCapability(t, snapshot, evidence.CatalogListing, evidence.Supported, test.want)
			assertCapability(t, snapshot, evidence.StructuralValidation, evidence.Supported, test.want)
		})
	}
}

func TestScannerUsesFormatOwnedSyntheticScopeGrammar(t *testing.T) {
	t.Parallel()
	reader := &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		return store.Page{Objects: []store.Object{
			{Key: "vault:one/object", Size: 3},
			{Key: "alpha/base/backup.info", Size: 5},
		}}, nil
	}}
	format := syntheticFormat{}
	cache, err := NewCache(Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	ready := readiness.New(true, time.Hour, time.Now)
	scanner, err := NewScanner(ScannerOptions{
		Store: reader, Format: format, Cache: cache, Readiness: ready,
		RefreshInterval: time.Hour, MaxObjects: 10, PageSize: 10, RecentLimit: 2,
		Now: time.Now, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if len(snapshot.Scopes) != 1 || snapshot.Scopes[0].Name != "one" || snapshot.UnscopedObjectCount != 1 || snapshot.Evidence.Scope.Kind != "vault" {
		t.Fatalf("synthetic format was forced through Barman grammar: %#v", snapshot)
	}
}

func newTestScanner(t *testing.T, reader store.Reader, maxObjects, pageSize, recentLimit int, now Clock) (*Scanner, *Cache, *readiness.ProbeState) {
	t.Helper()
	format := barmancloud.New()
	cache, err := NewCache(Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	ready := readiness.New(true, time.Hour, func() time.Time { return now() })
	scanner, err := NewScanner(ScannerOptions{
		Store: reader, Format: format, Cache: cache, Readiness: ready,
		RefreshInterval: time.Hour, MaxObjects: maxObjects, PageSize: pageSize,
		RecentLimit: recentLimit, Now: now, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return scanner, cache, ready
}

func assertCapability(t *testing.T, snapshot Snapshot, id evidence.CapabilityID, support evidence.Support, state evidence.State) {
	t.Helper()
	for _, capability := range snapshot.Evidence.Capabilities {
		if capability.ID == id {
			if capability.Support != support || capability.State != state {
				t.Fatalf("capability %s = %#v", id, capability)
			}
			return
		}
	}
	t.Fatalf("capability %s missing", id)
}

type categorizedError struct{ category fault.Category }

func (e categorizedError) Error() string            { return "safe synthetic failure" }
func (e categorizedError) Category() fault.Category { return e.category }

type syntheticFormat struct{}

func (syntheticFormat) Descriptor() repository.Descriptor {
	return repository.Descriptor{
		ID: "synthetic", DisplayName: "Synthetic Repository", ScopeKind: "vault",
		Capabilities: []evidence.CapabilityID{evidence.ObjectInventory},
	}
}

func (syntheticFormat) NewScopeMatcher([]string) (repository.ScopeMatcher, error) {
	return syntheticMatcher{}, nil
}

type syntheticMatcher struct{}

func (syntheticMatcher) InitialScopes() []string { return nil }

func (syntheticMatcher) Match(key string) (repository.ScopeMatch, bool) {
	const prefix = "vault:"
	if !strings.HasPrefix(key, prefix) {
		return repository.ScopeMatch{}, false
	}
	name, _, found := strings.Cut(strings.TrimPrefix(key, prefix), "/")
	if !found || name == "" {
		return repository.ScopeMatch{}, false
	}
	return repository.ScopeMatch{Name: name, Recognized: true}, true
}

// projectionRejectingCache stands in for the agent's evidence-publishing
// cache, whose projection rejects a complete snapshot built from malformed
// repository content while still accepting the stale failure snapshot.
type projectionRejectingCache struct {
	*Cache
	reject atomic.Bool
}

func (c *projectionRejectingCache) Publish(snapshot Snapshot) error {
	if c.reject.Load() && !snapshot.Evidence.Stale {
		return errors.New("project evidence publication: synthetic rejection")
	}
	return c.Cache.Publish(snapshot)
}

func TestScannerPublishesStaleIncompatibleWhenPublicationIsRejected(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	reader := &storetest.Fake{ListFunc: func(_ context.Context, _ store.ListRequest) (store.Page, error) {
		return store.Page{Objects: []store.Object{{Key: "alpha/wals/000000010000000000000001", Size: 1, LastModified: now}}}, nil
	}}
	format := barmancloud.New()
	inner, err := NewCache(Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	cache := &projectionRejectingCache{Cache: inner}
	var logs strings.Builder
	scanner, err := NewScanner(ScannerOptions{
		Store: reader, Format: format, Cache: cache, Readiness: readiness.New(true, time.Hour, func() time.Time { return now }),
		RefreshInterval: time.Hour, MaxObjects: 10, PageSize: 10, RecentLimit: 2,
		Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	good := cache.Load()
	if good.Evidence.Stale || good.RefreshFailure != "" {
		t.Fatalf("first refresh = %#v", good.Evidence)
	}

	cache.reject.Store(true)
	err = scanner.Refresh(context.Background())
	if fault.Categorize(err) != fault.Incompatible {
		t.Fatalf("Refresh() error = %v; want %s", err, fault.Incompatible)
	}
	failed := cache.Load()
	if !failed.Evidence.Stale || failed.Evidence.State != evidence.Unknown || failed.RefreshFailure != fault.Incompatible ||
		failed.RefreshGeneration != good.RefreshGeneration+1 || failed.Evidence.Generation != good.Evidence.Generation {
		t.Fatalf("rejected publication left evidence = gen %d/%d stale=%v state=%s failure=%s",
			failed.RefreshGeneration, failed.Evidence.Generation, failed.Evidence.Stale, failed.Evidence.State, failed.RefreshFailure)
	}
	if strings.Contains(logs.String(), "failure snapshot publication failed") {
		t.Fatal("stale failure snapshot itself was rejected")
	}
}

type causeCarryingError struct{ cause string }

func (e causeCarryingError) Error() string            { return "provider request failed: unavailable" }
func (e causeCarryingError) Category() fault.Category { return fault.Unavailable }
func (e causeCarryingError) RootCause() string        { return e.cause }

func TestScannerLogsRedactedRootCauseOncePerCycle(t *testing.T) {
	t.Parallel()
	secret := "secret-key-canary-value"
	cause := "dial tcp: lookup store.example.test: no such host; signing with " + secret + "; Authorization: AWS4-HMAC-SHA256 Credential=canary-credential"
	reader := &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		return store.Page{}, causeCarryingError{cause: cause}
	}}
	format := barmancloud.New()
	cache, err := NewCache(Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	scanner, err := NewScanner(ScannerOptions{
		Store: reader, Format: format, Cache: cache, Readiness: readiness.New(true, time.Hour, time.Now),
		RefreshInterval: time.Hour, MaxObjects: 10, PageSize: 10, RecentLimit: 2, Now: time.Now,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Redactor: redact.New([]byte(secret)),
	})
	if err != nil {
		t.Fatal(err)
	}
	scanner.runCycle(context.Background())
	output := logs.String()
	if strings.Contains(output, secret) || strings.Contains(output, "canary-credential") {
		t.Fatalf("root cause logged unredacted: %s", output)
	}
	if strings.Count(output, "no such host") != 1 || !strings.Contains(output, `"category":"unavailable"`) {
		t.Fatalf("probe and refresh share one cause and must log it once per cycle: %s", output)
	}
	if !strings.Contains(output, "inventory refresh failed") {
		t.Fatalf("refresh failure not logged: %s", output)
	}
	scanner.runCycle(context.Background())
	if strings.Count(logs.String(), "no such host") != 2 {
		t.Fatal("the next cycle must log the cause again")
	}
}

func TestScannerLogsSuccessfulRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	reader := &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		return store.Page{Objects: []store.Object{{Key: "alpha/wals/000000010000000000000001", Size: 1, LastModified: now}}}, nil
	}}
	format := barmancloud.New()
	cache, err := NewCache(Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	scanner, err := NewScanner(ScannerOptions{
		Store: reader, Format: format, Cache: cache, Readiness: readiness.New(true, time.Hour, func() time.Time { return now }),
		RefreshInterval: time.Hour, MaxObjects: 10, PageSize: 10, RecentLimit: 2, Now: func() time.Time { return now },
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"msg":"inventory refresh completed"`, `"refresh_generation":1`, `"evidence_generation":1`, `"objects":1`, `"duration_ms":0`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("success log lacks %s: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "alpha/wals") {
		t.Fatal("success log disclosed an object key")
	}
}

type keyedCauseError struct{ requestID string }

func (e keyedCauseError) Error() string            { return "provider request failed: authorization" }
func (e keyedCauseError) Category() fault.Category { return fault.Authorization }
func (e keyedCauseError) RootCause() string {
	return "operation error S3: ListObjectsV2, RequestID: " + e.requestID + ", api error AccessDenied"
}
func (e keyedCauseError) CauseKey() string { return "list/AccessDenied" }

// Service-error texts carry a fresh request ID per call; the probe and the
// refresh failing with the same code must still log one cause per cycle.
func TestScannerDeduplicatesServiceCausesByStableKey(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	reader := &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		return store.Page{}, keyedCauseError{requestID: fmt.Sprintf("REQ-%d", calls.Add(1))}
	}}
	format := barmancloud.New()
	cache, err := NewCache(Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	scanner, err := NewScanner(ScannerOptions{
		Store: reader, Format: format, Cache: cache, Readiness: readiness.New(true, time.Hour, time.Now),
		RefreshInterval: time.Hour, MaxObjects: 10, PageSize: 10, RecentLimit: 2, Now: time.Now,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	scanner.runCycle(context.Background())
	if calls.Load() < 2 || strings.Count(logs.String(), "AccessDenied") != 1 {
		t.Fatalf("calls=%d logs=%s", calls.Load(), logs.String())
	}
}
