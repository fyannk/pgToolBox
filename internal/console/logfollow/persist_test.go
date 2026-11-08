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

package logfollow

import (
	"bytes"
	"context"
	"encoding/gob"
	"io"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
)

// silent opens streams that say nothing: what a follower holds is then
// only what it restored.
type silent struct{}

func (silent) FollowLogs(ctx context.Context, _, _, _ string) (io.ReadCloser, error) {
	r, w := io.Pipe()
	go func() { <-ctx.Done(); _ = w.Close() }()
	return r, nil
}

var checkpointRule = logstream.Rule{ID: "checkpoint", Contains: []string{"could not locate a valid checkpoint"}, Summary: "no checkpoint"}

func savedState(t *testing.T, seen time.Time) PersistedState {
	t.Helper()
	state := PersistedState{Clusters: []PersistedCluster{{
		UID: "uid-orders", Namespace: "team-a", Name: "orders",
		Observations: []logstream.Observation{{
			RuleID: "checkpoint", Summary: "stale summary", Pod: "orders-1", Container: "postgres",
			Line: "PANIC: could not locate a valid checkpoint record", FirstSeen: seen, LastSeen: seen, Count: 3,
		}},
	}}}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(state); err != nil {
		t.Fatal(err)
	}
	var out PersistedState
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRestoredObservations(t *testing.T) {
	seen := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		uid  string
		now  time.Time
		want int
	}{
		{"same Cluster, inside the max age", "uid-orders", seen.Add(time.Hour), 1},
		{"Cluster recreated under the same name", "uid-orders-recreated", seen.Add(time.Hour), 0},
		{"past the max age", "uid-orders", seen.Add(7 * time.Hour), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ro := &roster{keys: []Key{{"team-a", "orders"}}, uids: map[Key]string{{"team-a", "orders"}: tc.uid}}
			f := New(Options{
				Roster: ro, Opener: silent{}, Rules: []logstream.Rule{checkpointRule}, MaxAge: 6 * time.Hour,
				Now: func() time.Time { return tc.now },
			})
			if kept, read := f.Import(savedState(t, seen), tc.now); kept != 1 || read != 1 {
				t.Fatalf("import kept %d of %d", kept, read)
			}
			f.reconcile(t.Context())
			defer f.stopAll()
			got := f.Observations("team-a", "orders").Observations()
			if len(got) != tc.want {
				t.Fatalf("observations = %+v, want %d", got, tc.want)
			}
			if tc.want == 1 && (got[0].Count != 3 || !got[0].FirstSeen.Equal(seen) || got[0].Summary != "no checkpoint") {
				t.Fatalf("observation = %+v, want the saved one under the rule's current summary", got[0])
			}
			if tc.want == 1 {
				if exported := f.Export(); len(exported.Clusters) != 1 || exported.Clusters[0].UID != tc.uid {
					t.Fatalf("export = %+v", exported)
				}
			}
		})
	}
}
