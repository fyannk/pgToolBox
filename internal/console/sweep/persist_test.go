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

package sweep

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
)

func roundTrip(t *testing.T, in PersistedState) PersistedState {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(in); err != nil {
		t.Fatal(err)
	}
	var out PersistedState
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// restartFixture sweeps one primary pod twice (read, then failing) and
// returns its snapshot, the failure's "since" and a way to build a
// second sweeper on the same server, as the console after a restart.
type restartFixture struct {
	saved PersistedState
	since time.Time
	now   time.Time
	make  func(roster Roster, now *time.Time) *Sweeper
}

func newRestartFixture(t *testing.T) restartFixture {
	t.Helper()
	good := newPKI(t, "orders-rw.team-a.svc")
	var fail atomic.Bool
	srv, _ := statusServer(t, good, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// A primary: only a primary reports readyWalFiles.
		_, _ = fmt.Fprintf(w, report, true, "0/2000000", 3)
	})
	bundle := bundleFile(t, good.caPEM)
	mk := func(roster Roster, now *time.Time) *Sweeper {
		return sweeper(roster, bundle, port(t, srv), func() time.Time { return *now })
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s := mk(staticRoster{target("cluster-uid", "pod-uid")}, &now)
	s.SweepOnce(context.Background())
	now = now.Add(30 * time.Second)
	fail.Store(true)
	s.SweepOnce(context.Background())
	snap, _ := s.Snapshot(key)
	return restartFixture{saved: roundTrip(t, s.Export(now)), since: snap.Instances["orders-2"].Since, now: now, make: mk}
}

func target(clusterUID, podUID string) Target {
	return Target{
		Key: key, ClusterUID: clusterUID, ServerName: "orders-rw.team-a.svc",
		Pods: []PodTarget{{Name: "orders-2", UID: podUID, IP: "127.0.0.1", StatusPortTLS: true}},
	}
}

func TestSnapshotRestoresSinceReadingAndRings(t *testing.T) {
	f := newRestartFixture(t)
	// Five minutes down, then the first sweep after the restart fails
	// the same way: the state has held since before the restart.
	now := f.now.Add(5 * time.Minute)
	s := f.make(staticRoster{target("cluster-uid", "pod-uid")}, &now)
	if kept, read := s.Import(f.saved, now); kept != 1 || read != 1 {
		t.Fatalf("import kept %d of %d clusters", kept, read)
	}
	s.SweepOnce(context.Background())
	snap, _ := s.Snapshot(key)
	inst := snap.Instances["orders-2"]
	if inst.State != viewmodel.StatusReadBadResponse || !inst.Since.Equal(f.since) {
		t.Fatalf("since = %v (%s), want %v carried across the restart", inst.Since, inst.State, f.since)
	}
	if inst.Reading == nil || inst.Reading.ReadyWALFiles != 3 {
		t.Fatalf("reading = %+v, want the last good report restored", inst.Reading)
	}
	for _, sr := range s.Series(key, "orders-2") {
		if sr.Key != SeriesReadyWALFiles {
			continue
		}
		// read 3, failed, the restart gap, failed again.
		if len(sr.Points) != 4 || sr.Points[0].Value == nil || *sr.Points[0].Value != 3 || sr.Points[2].Value != nil {
			t.Fatalf("points = %+v, want the old samples, a gap, the new one", sr.Points)
		}
	}
}

func TestSnapshotOfARecreatedClusterOrPodIsNotInherited(t *testing.T) {
	cases := []struct {
		name               string
		clusterUID, podUID string
	}{
		{"cluster recreated under the same name", "other-cluster-uid", "pod-uid"},
		{"pod recreated under the same name", "cluster-uid", "other-pod-uid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t)
			now := f.now.Add(time.Minute)
			s := f.make(staticRoster{target(tc.clusterUID, tc.podUID)}, &now)
			s.Import(f.saved, now)
			s.SweepOnce(context.Background())
			inst := mustInstance(t, s)
			if !inst.Since.Equal(now) || inst.Reading != nil {
				t.Fatalf("instance = %+v, want a fresh clock and no reading", inst)
			}
			for _, sr := range s.Series(key, "orders-2") {
				if len(sr.Points) != 1 {
					t.Fatalf("%s points = %+v, want only the new sample", sr.Key, sr.Points)
				}
			}
		})
	}
}

func mustInstance(t *testing.T, s *Sweeper) Instance {
	t.Helper()
	snap, ok := s.Snapshot(key)
	if !ok {
		t.Fatal("not swept")
	}
	return snap.Instances["orders-2"]
}

func TestSnapshotPastTheRetentionRestoresNothingButTheClock(t *testing.T) {
	f := newRestartFixture(t)
	// Two hours down with a one-hour retention: no sample and no reading
	// is still inside the window. The state's "since" is a fact about the
	// pod, not a sample, and is kept.
	now := f.now.Add(2 * time.Hour)
	s := f.make(staticRoster{target("cluster-uid", "pod-uid")}, &now)
	s.Import(f.saved, now)
	s.SweepOnce(context.Background())
	inst := mustInstance(t, s)
	if inst.Reading != nil || !inst.Since.Equal(f.since) {
		t.Fatalf("instance = %+v, want no reading and the old since", inst)
	}
	for _, sr := range s.Series(key, "orders-2") {
		if len(sr.Points) != 1 {
			t.Fatalf("%s points = %+v, want only the new sample", sr.Key, sr.Points)
		}
	}
}

func TestUnclaimedSnapshotExpires(t *testing.T) {
	f := newRestartFixture(t)
	now := f.now
	roster := &mutableRoster{}
	s := f.make(roster, &now)
	s.Import(f.saved, now)
	// The Cluster is not in the roster for longer than any part keeps:
	// it was deleted, and its state must not wait forever.
	now = now.Add(restoreKeep + time.Minute)
	s.SweepOnce(context.Background())
	roster.targets = []Target{target("cluster-uid", "pod-uid")}
	s.SweepOnce(context.Background())
	if inst := mustInstance(t, s); inst.Reading != nil || !inst.Since.Equal(now) {
		t.Fatalf("instance = %+v, want nothing restored", inst)
	}
}

func TestPersistedReadingSurvivesGob(t *testing.T) {
	// A gob encoder flattens pointers and omits zero values: every
	// pointer field of a Reading set to its zero value must come back
	// set, or be one the snapshot carries separately (IsFenced). A new
	// pointer field fails here until it is handled.
	var r instancestatus.Reading
	v := reflect.ValueOf(&r).Elem()
	handled := map[string]bool{"IsFenced": true}
	var pointers []string
	for i := range v.NumField() {
		field := v.Field(i)
		if field.Kind() == reflect.Pointer {
			field.Set(reflect.New(field.Type().Elem()))
			pointers = append(pointers, v.Type().Field(i).Name)
		}
	}
	back := roundTrip(t, PersistedState{Clusters: []PersistedCluster{{Instances: []PersistedInstance{{Reading: &r}}}}})
	got := reflect.ValueOf(back.Clusters[0].Instances[0].Reading).Elem()
	for _, name := range pointers {
		if got.FieldByName(name).IsNil() && !handled[name] {
			t.Errorf("Reading.%s holding a zero value comes back nil through gob: carry it beside the Reading", name)
		}
	}

	fenced := false
	p := PersistedState{Clusters: []PersistedCluster{{
		UID: "u", Namespace: "team-a", Name: "orders",
		Instances: []PersistedInstance{{
			Name: "orders-2", PodUID: "pod-uid", State: string(viewmodel.StatusReadOK),
			At: time.Now().UnixNano(), Since: time.Now().UnixNano(),
			Reading: &instancestatus.Reading{PodUID: "pod-uid", ObservedAt: time.Now(), IsFenced: &fenced}, Fenced: 1,
		}},
	}}}
	now := time.Now()
	s := New(staticRoster{{
		Key: Key{Namespace: "team-a", Name: "orders"}, ClusterUID: "u",
		Pods: []PodTarget{{Name: "orders-2", UID: "pod-uid"}},
	}}, Options{Interval: 5 * time.Second, Retention: time.Hour, Now: func() time.Time { return now }})
	s.Import(roundTrip(t, p), now)
	s.SweepOnce(context.Background())
	snap, _ := s.Snapshot(Key{Namespace: "team-a", Name: "orders"})
	if r := snap.Instances["orders-2"].Reading; r == nil || r.IsFenced == nil || *r.IsFenced {
		t.Fatalf("reading = %+v, want IsFenced restored as false", r)
	}
}

type earlierClock struct{ at time.Time }

func (c earlierClock) InstanceSince(_, _ string, local time.Time) time.Time {
	if c.at.Before(local) {
		return c.at
	}
	return local
}

func TestSharedClockWins(t *testing.T) {
	good := newPKI(t, "orders-rw.team-a.svc")
	srv, _ := statusServer(t, good, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, report, false, "0/2000000", 3)
	})
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)
	s := New(staticRoster{target("cluster-uid", "pod-uid")}, Options{
		Interval: 5 * time.Second, Timeout: 2 * time.Second, Concurrency: 1, Retention: time.Hour,
		Bundle: bundleFile(t, good.caPEM), StatusPort: port(t, srv), Now: func() time.Time { return now },
		Shared: earlierClock{at: earlier},
	})
	s.SweepOnce(context.Background())
	if inst := mustInstance(t, s); !inst.Since.Equal(earlier) {
		t.Fatalf("since = %v, want the other replica's %v", inst.Since, earlier)
	}
}
