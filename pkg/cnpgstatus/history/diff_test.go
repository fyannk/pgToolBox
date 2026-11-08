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

package history

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// manifestObs builds an observation whose manifest is real JSON, which
// is what the differ parses. Hashes are supplied by the caller so the
// classification path stays under test control.
func manifestObs(t *testing.T, manifest map[string]any, specHash, statusHash string) Observation {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return Observation{
		Scope: "pods", Version: "v1", Kind: "Pod", Namespace: "db",
		Name: "cluster-a", UID: "a",
		Manifest: encoded, SpecHash: specHash, StatusHash: statusHash,
	}
}

// diffOf feeds two revisions of one object and diffs the newer one.
func diffOf(t *testing.T, before, after map[string]any) Diff {
	t.Helper()
	s, clock := newTestStore()
	s.Observe(manifestObs(t, before, "s1", "t1"))
	clock.advance(5 * time.Minute)
	s.Observe(manifestObs(t, after, "s2", "t1"))
	seq := entries(t, s)[0].Seq
	diff, ok := s.Diff(seq)
	if !ok {
		t.Fatal("diff of a retained revision not found")
	}
	return diff
}

func TestDiffReportsChangedAddedRemoved(t *testing.T) {
	diff := diffOf(t,
		map[string]any{"spec": map[string]any{"instances": 1, "keep": "x", "old": "gone"}},
		map[string]any{"spec": map[string]any{"instances": 2, "keep": "x", "fresh": "new"}},
	)
	if !diff.HasBase || diff.BaseSeq == 0 {
		t.Fatalf("diff carries no baseline: %+v", diff)
	}
	if len(diff.Entries) != 3 {
		t.Fatalf("entries = %+v, want 3", diff.Entries)
	}
	added, changed, removed := diff.Entries[0], diff.Entries[1], diff.Entries[2]
	if added.Path != ".spec.fresh" || added.Op != DiffAdded || added.After != `"new"` || added.Before != "" {
		t.Fatalf("added = %+v", added)
	}
	if changed.Path != ".spec.instances" || changed.Op != DiffChanged || changed.Before != "1" || changed.After != "2" {
		t.Fatalf("changed = %+v", changed)
	}
	if removed.Path != ".spec.old" || removed.Op != DiffRemoved || removed.Before != `"gone"` || removed.After != "" {
		t.Fatalf("removed = %+v", removed)
	}
}

func TestDiffMatchesListsByName(t *testing.T) {
	diff := diffOf(t,
		map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "postgres", "image": "pg:17"},
			map[string]any{"name": "sidecar", "image": "sc:1"},
		}}},
		// Reordered, one image changed: the reorder alone is no change.
		map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "sidecar", "image": "sc:1"},
			map[string]any{"name": "postgres", "image": "pg:18"},
		}}},
	)
	if len(diff.Entries) != 1 {
		t.Fatalf("entries = %+v, want the one image change: a reordered named list is not a change", diff.Entries)
	}
	entry := diff.Entries[0]
	if entry.Path != ".spec.containers[name=postgres].image" || entry.Op != DiffChanged {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestDiffComparesUnnamedListsPositionally(t *testing.T) {
	diff := diffOf(t,
		map[string]any{"spec": map[string]any{"ports": []any{1, 2}}},
		map[string]any{"spec": map[string]any{"ports": []any{1, 3, 4}}},
	)
	if len(diff.Entries) != 2 {
		t.Fatalf("entries = %+v, want 2", diff.Entries)
	}
	if diff.Entries[0].Path != ".spec.ports[1]" || diff.Entries[0].Op != DiffChanged {
		t.Fatalf("first = %+v", diff.Entries[0])
	}
	if diff.Entries[1].Path != ".spec.ports[2]" || diff.Entries[1].Op != DiffAdded {
		t.Fatalf("second = %+v", diff.Entries[1])
	}
}

func TestDiffWithoutBaselineIsHonest(t *testing.T) {
	s, clock := newTestStore()

	// A first observation has nothing to compare against.
	s.Observe(manifestObs(t, map[string]any{"spec": 1}, "s1", "t1"))
	first := entries(t, s)[0].Seq
	if diff, ok := s.Diff(first); !ok || diff.HasBase {
		t.Fatalf("first observation diff = %+v ok=%v, want no baseline", diff, ok)
	}

	// A deletion carries no manifest.
	clock.advance(time.Minute)
	s.Observe(deletion("a"))
	deleted := entries(t, s)[0].Seq
	if diff, ok := s.Diff(deleted); !ok || diff.HasBase {
		t.Fatalf("deletion diff = %+v ok=%v, want no baseline", diff, ok)
	}

	// An unknown sequence is not found at all.
	if _, ok := s.Diff(9999); ok {
		t.Fatal("unknown sequence resolved a diff")
	}
}

func TestDiffAfterBaselineEvictionIsHonest(t *testing.T) {
	s, clock := newTestStore()
	s.limits = Limits{MaxRevisions: 1}
	s.Observe(manifestObs(t, map[string]any{"spec": 1}, "s1", "t1"))
	clock.advance(time.Minute)
	s.Observe(manifestObs(t, map[string]any{"spec": 2}, "s2", "t1"))

	seq := entries(t, s)[0].Seq
	diff, ok := s.Diff(seq)
	if !ok || diff.HasBase {
		t.Fatalf("diff = %+v ok=%v: an evicted baseline must mean no baseline, never a wrong one", diff, ok)
	}
}

func TestDiffAttributesUnambiguousOwnership(t *testing.T) {
	s, clock := newTestStore()
	s.Observe(manifestObs(t, map[string]any{
		"spec":   map[string]any{"instances": 1},
		"status": map[string]any{"phase": "Running"},
	}, "s1", "t1"))
	clock.advance(5 * time.Minute)
	after := manifestObs(t, map[string]any{
		"spec":   map[string]any{"instances": 2},
		"status": map[string]any{"phase": "Failed"},
	}, "s2", "t2")
	after.Owners = []FieldOwner{
		{Manager: "cloudnative-pg", Paths: []string{".spec.instances"}},
		{Manager: "kubelet", Paths: []string{".status"}},
	}
	s.Observe(after)

	diff, _ := s.Diff(entries(t, s)[0].Seq)
	if len(diff.Entries) != 2 {
		t.Fatalf("entries = %+v, want 2", diff.Entries)
	}
	if diff.Entries[0].Path != ".spec.instances" || diff.Entries[0].Manager != "cloudnative-pg" {
		t.Fatalf("spec entry = %+v, want an exact-path attribution", diff.Entries[0])
	}
	// ".status" owns the subtree, so the phase change inside attributes.
	if diff.Entries[1].Path != ".status.phase" || diff.Entries[1].Manager != "kubelet" {
		t.Fatalf("status entry = %+v, want a subtree attribution", diff.Entries[1])
	}
}

func TestDiffAmbiguousOwnershipAttributesNobody(t *testing.T) {
	s, clock := newTestStore()
	s.Observe(manifestObs(t, map[string]any{"spec": map[string]any{"instances": 1}}, "s1", "t1"))
	clock.advance(5 * time.Minute)
	after := manifestObs(t, map[string]any{"spec": map[string]any{"instances": 2}}, "s2", "t1")
	after.Owners = []FieldOwner{
		{Manager: "one", Paths: []string{".spec"}},
		{Manager: "two", Paths: []string{".spec.instances"}},
	}
	s.Observe(after)

	diff, _ := s.Diff(entries(t, s)[0].Seq)
	if diff.Entries[0].Manager != "" {
		t.Fatalf("manager = %q, want empty: a guessed attribution is worse than none", diff.Entries[0].Manager)
	}
}

func TestDiffBoundsEntriesAndValues(t *testing.T) {
	wide := map[string]any{}
	for i := 0; i < MaxDiffEntries+50; i++ {
		wide[fmt.Sprintf("key%04d", i)] = i
	}
	diff := diffOf(t, map[string]any{"spec": map[string]any{}}, map[string]any{"spec": wide})
	if len(diff.Entries) != MaxDiffEntries || !diff.Truncated {
		t.Fatalf("entries=%d truncated=%v, want the bound applied and reported", len(diff.Entries), diff.Truncated)
	}

	long := diffOf(t,
		map[string]any{"spec": "short"},
		map[string]any{"spec": strings.Repeat("x", 4096)},
	)
	if got := long.Entries[0].After; len(got) > maxDiffValueLen+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("value not bounded: %d bytes", len(got))
	}
}
