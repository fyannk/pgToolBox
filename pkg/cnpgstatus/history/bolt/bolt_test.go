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

package bolt

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// openTemp opens a journal on a fresh temp file.
func openTemp(t *testing.T, path string) *Journal {
	t.Helper()
	j, err := Open(path, observe.RealClock{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return j
}

// rev builds one revision.
func rev(seq uint64, uid string) history.Revision {
	return history.Revision{
		Seq:        seq,
		Scope:      "pods",
		Version:    "v1",
		Kind:       "Pod",
		Namespace:  "db",
		Name:       "cluster-" + uid,
		UID:        uid,
		Change:     history.ChangeCreated,
		Actor:      history.Actor{Manager: "kubelet", Operation: "Update"},
		ObservedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		SpecHash:   "s1",
		StatusHash: "t1",
		Owners:     []history.FieldOwner{{Manager: "kubelet", Paths: []string{".status.phase"}}},
		Manifest:   []byte(`{"kind":"Pod"}`),
	}
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	j := openTemp(t, path)
	j.Append(rev(1, "a"))
	j.Append(rev(2, "b"))
	j.PutObject("a", history.ObjectRecord{Scope: "pods", Kind: "Pod", Name: "cluster-a", SpecHash: "s1"})
	if err := j.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := j.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openTemp(t, path)
	defer func() { _ = reopened.db.Close() }()
	contents, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(contents.Revisions) != 2 || contents.Seq != 2 {
		t.Fatalf("revisions=%d seq=%d, want 2 and 2", len(contents.Revisions), contents.Seq)
	}
	if contents.Revisions[0].Seq != 1 || contents.Revisions[1].Seq != 2 {
		t.Fatal("revisions not in sequence order")
	}
	got := contents.Revisions[0]
	if got.UID != "a" || got.Kind != "Pod" || string(got.Manifest) != `{"kind":"Pod"}` || got.Actor.Manager != "kubelet" {
		t.Fatalf("revision did not survive the round trip: %+v", got)
	}
	if len(got.Owners) != 1 || got.Owners[0].Paths[0] != ".status.phase" {
		t.Fatalf("field ownership did not survive the round trip: %+v", got.Owners)
	}
	if rec, ok := contents.Objects["a"]; !ok || rec.SpecHash != "s1" {
		t.Fatalf("object state did not survive: %+v", contents.Objects)
	}
	if contents.Evicted {
		t.Fatal("evicted mark set without eviction")
	}
}

func TestEvictionAndDeletionPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	j := openTemp(t, path)
	defer func() { _ = j.db.Close() }()
	j.Append(rev(1, "a"))
	j.PutObject("a", history.ObjectRecord{Scope: "pods"})
	if err := j.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	j.Evict(1)
	j.DeleteObject("a")
	j.MarkEvicted()
	if err := j.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	contents, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(contents.Revisions) != 0 || len(contents.Objects) != 0 {
		t.Fatalf("revisions=%d objects=%d after eviction, want none", len(contents.Revisions), len(contents.Objects))
	}
	if !contents.Evicted {
		t.Fatal("evicted mark lost")
	}
	if contents.Seq != 1 {
		t.Fatalf("seq = %d, want 1: sequences outlive the revisions that carried them", contents.Seq)
	}
}

func TestAppendThenEvictInOneIntervalWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	j := openTemp(t, path)
	defer func() { _ = j.db.Close() }()
	j.Append(rev(1, "a"))
	j.Evict(1)
	if err := j.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	contents, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(contents.Revisions) != 0 {
		t.Fatal("a revision evicted before its first flush must never reach the file")
	}
	if contents.Seq != 1 {
		t.Fatalf("seq = %d, want 1 even for a never-persisted revision", contents.Seq)
	}
}

func TestUnflushedMutationsAreLostByDesign(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	j := openTemp(t, path)
	j.Append(rev(1, "a"))
	if err := j.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openTemp(t, path)
	defer func() { _ = reopened.db.Close() }()
	contents, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(contents.Revisions) != 0 {
		t.Fatal("write-behind means an unflushed append is lost on a hard stop; finding it means flush ran unexpectedly")
	}
}

func TestRunFlushesOnCancellationAndCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	j := openTemp(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- j.Run(ctx) }()

	j.Append(rev(1, "a"))
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on cancellation")
	}

	reopened := openTemp(t, path)
	defer func() { _ = reopened.db.Close() }()
	contents, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(contents.Revisions) != 1 {
		t.Fatal("the final flush on shutdown lost the pending revision")
	}
}

func TestJournalDrivesThePersistedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	j := openTemp(t, path)
	limits := history.Limits{PerObject: 10, MaxRevisions: 100, MaxBytes: 1 << 20}
	store, err := history.NewPersistedStore(limits, observe.RealClock{}, j)
	if err != nil {
		t.Fatalf("NewPersistedStore: %v", err)
	}
	store.Observe(history.Observation{
		Scope: "pods", Version: "v1", Kind: "Pod", Namespace: "db",
		Name: "cluster-a", UID: "a", SpecHash: "s1", StatusHash: "t1",
		Manifest: []byte(`{"kind":"Pod"}`),
	})
	if err := j.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := j.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openTemp(t, path)
	defer func() { _ = reopened.db.Close() }()
	resumed, err := history.NewPersistedStore(limits, observe.RealClock{}, reopened)
	if err != nil {
		t.Fatalf("NewPersistedStore: %v", err)
	}
	snap, ok := resumed.Snapshot()
	if !ok || len(snap.Entries) != 1 || snap.Entries[0].UID != "a" {
		t.Fatalf("resumed store lost the timeline: %+v", snap.Entries)
	}
}

func TestOpenFailsOnUnusablePath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing", "history.db"), observe.RealClock{}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("an unusable journal path must fail open, not degrade")
	}
}

func TestDrillRecordsWriteThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	j := openTemp(t, path)
	if err := j.PutDrill("team-a/drill-1", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.PutDrill("team-a/drill-2", []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.DeleteDrill("team-a/drill-1"); err != nil {
		t.Fatal(err)
	}
	if err := j.PutDrill("", []byte(`{}`)); err == nil {
		t.Error("empty key accepted")
	}
	if err := j.PutDrill("big", make([]byte, maxDrillRecord+1)); err == nil {
		t.Error("oversized record accepted")
	}
	// No flush: the record is written through, so a hard kill keeps it.
	if err := j.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTemp(t, path)
	got, err := reopened.LoadDrills()
	if err != nil || len(got) != 1 || string(got[0]) != `{"a":2}` {
		t.Errorf("reloaded %q, %v", got, err)
	}
	_ = reopened.db.Close()
}

func TestDrillsSinceIsRecordedOnceAndSurvivesAReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	j := openTemp(t, path)
	first := time.Date(2026, 8, 1, 10, 0, 0, 500, time.UTC)
	since, err := j.DrillsSince(first)
	if err != nil || !since.Equal(first.Truncate(time.Second)) {
		t.Fatalf("first call = %v, %v", since, err)
	}
	if err := j.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTemp(t, path)
	again, err := reopened.DrillsSince(first.Add(90 * 24 * time.Hour))
	if err != nil || !again.Equal(since) {
		t.Errorf("after a reopen = %v, %v; want the first start %v", again, err, since)
	}
	_ = reopened.db.Close()
}
