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

package status

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
)

// countingHistory serves one snapshot and counts the reads.
type countingHistory struct {
	HistoryReader
	snap  history.Snapshot
	calls int
}

func (h *countingHistory) Snapshot() (history.Snapshot, bool) { h.calls++; return h.snap, true }
func (h *countingHistory) Persistent() bool                   { return true }

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// TestDigestEntriesAtFleetScale: the digest reads 1,600 Clusters and
// 5,000 revisions with one history snapshot and one journal query, in
// bounded time and allocations (a pass per Cluster was 1,600 of each).
func TestDigestEntriesAtFleetScale(t *testing.T) {
	const clusters, revisions = 1600, 5000
	to := time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)
	from := to.Add(-24 * time.Hour)
	h := &countingHistory{}
	for i := range revisions {
		kind, change := "Cluster", history.ChangeSpec
		if i%5 == 0 {
			kind = "Pooler" // not digest news
		}
		h.snap.Entries = append(h.snap.Entries, history.Entry{
			Seq: uint64(i + 1), Kind: kind, Namespace: fmt.Sprintf("ns%d", i%16), Name: fmt.Sprintf("c%d", i%clusters),
			Change: change, ObservedAt: from.Add(time.Duration(i) * 17 * time.Second),
		})
	}
	j := timeline.New(timeline.Limits{}, fixedClock{to})
	for i := range clusters * 3 {
		j.Record(timeline.Event{
			Kind: timeline.KindBackup, Action: timeline.ActionFailed, Namespace: fmt.Sprintf("ns%d", i%16), Cluster: fmt.Sprintf("c%d", i%clusters),
			Key: fmt.Sprintf("b/%d", i), At: from.Add(time.Duration(i) * 13 * time.Second), Timing: timeline.TimingReported,
		})
	}
	s := New(Options{History: h, Timeline: j})
	start := time.Now()
	got := s.DigestEntries(context.Background(), from, to)
	elapsed := time.Since(start)
	if h.calls != 1 {
		t.Fatalf("history read %d times, want once for the whole fleet", h.calls)
	}
	n := 0
	for _, entries := range got {
		n += len(entries)
	}
	if want := revisions*4/5 + clusters*3; n != want {
		t.Fatalf("%d entries, want %d", n, want)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("DigestEntries took %v for the fleet", elapsed)
	}
	allocs := testing.AllocsPerRun(3, func() { s.DigestEntries(context.Background(), from, to) })
	t.Logf("%d entries, %v, %.0f allocations", n, elapsed, allocs)
	if perEntry := allocs / float64(n); perEntry > 8 {
		t.Fatalf("%.1f allocations per entry; the fleet's digest must stay linear and lean", perEntry)
	}
}
