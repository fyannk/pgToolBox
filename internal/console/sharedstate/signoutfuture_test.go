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

package sharedstate

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// A sign-out read back with a recording time far in this replica's
// future (a clock running ahead, or an entry written by hand) still
// revokes its session, and its time is kept as it was read, but the
// bounds order it as unrecorded: it cannot stand first in every bound's
// order for as long as its clock says. Within the skew, the time orders
// as given.
func TestAFutureRecordingTimeIsIgnored(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	far, near := sessionHash("far"), sessionHash("near")
	until := t0.Add(12 * time.Hour).Unix()
	farAt, nearAt := t0.Add(maxRecordedSkew+time.Second).Unix(), t0.Add(maxRecordedSkew).Unix()
	raw, err := json.Marshal(sessionsDoc{Version: formatVersion, SignedOut: map[string]signOut{
		far:  {Expiry: until, Recorded: farAt, Subject: tag("mallory")},
		near: {Expiry: until, Recorded: nearAt, Subject: tag("alice")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.mergeLocked(&corev1.ConfigMap{Data: map[string]string{KeySessions: string(raw)}})
	gotFar, gotNear := s.signedOut[far], s.signedOut[near]
	s.mu.Unlock()
	if !s.SignedOut("far") || !s.SignedOut("near") {
		t.Fatal("a sign-out read back was dropped")
	}
	if gotFar.Recorded != farAt || gotNear.Recorded != nearAt {
		t.Errorf("a recording time read back was changed: far %d, near %d", gotFar.Recorded, gotNear.Recorded)
	}
	// The local entry (recorded now) comes before the future one in the
	// bounds' order, and after the one within the skew.
	s.RecordSignOut("bob", tag("bob"), t0.Add(12*time.Hour))
	s.mu.Lock()
	defer s.mu.Unlock()
	bob, latest := sessionHash("bob"), s.latestRecordedLocked()
	if !signOutBefore(bob, s.signedOut[bob], far, s.signedOut[far], latest) {
		t.Error("an entry recorded now does not come before one recorded in the future")
	}
	if !signOutBefore(near, s.signedOut[near], bob, s.signedOut[bob], latest) {
		t.Error("an entry within the skew does not order by its time")
	}
	// Once this replica's clock catches up, the time orders as given.
	clock.Add(time.Minute)
	if !signOutBefore(far, s.signedOut[far], bob, s.signedOut[bob], s.latestRecordedLocked()) {
		t.Error("a recording time no longer in the future is still read as none")
	}
}

// Replica a's clock runs ten minutes ahead of b's, past maxRecordedSkew,
// and a records a sign-out. The replicas converge: b refuses the session,
// both hold the same document, and the writes stop. (b used to zero the
// recording time it read back and a to keep its own on merging, so each
// rewrote the other's copy on every round.)
func TestSkewedClocksConverge(t *testing.T) {
	api := newAPIServer()
	ca, cb := &testClock{now: t0.Add(10 * time.Minute)}, &testClock{now: t0}
	a, b := newStore(api, ca, nil), newStore(api, cb, nil)
	run(t, a)
	run(t, b)
	eventually(t, "both replicas reading", func() bool {
		return a.State().State == viewmodel.SourceLive && b.State().State == viewmodel.SourceLive
	})
	a.RecordSignOut("fedcba9876543210fedcba9876543210", tag("alice"), t0.Add(12*time.Hour))
	eventually(t, "b refuses a's sign-out", func() bool { return b.SignedOut("fedcba9876543210fedcba9876543210") })
	settle(t, api)
}

// settle waits for the writes to the ConfigMap to stop: none in a window
// many debounces long, within a few rounds.
func settle(t *testing.T, api *apiServer) {
	t.Helper()
	count := func() int {
		api.mu.Lock()
		defer api.mu.Unlock()
		return api.updates
	}
	for range 20 {
		before := count()
		time.Sleep(60 * time.Millisecond)
		if count() == before {
			return
		}
	}
	t.Fatalf("the replicas still write the ConfigMap: %d writes in all", count())
}

// Property: replicas whose clocks disagree by up to twice maxRecordedSkew
// converge on every document: after the work stops, a few rounds of
// read-merge-write leave every replica holding the same state and
// writing nothing. Nothing reaches its end or a bound here: what ends
// and what a full document keeps are decided by each replica's clock,
// so replicas whose clocks disagree may rewrite each other's copy of an
// entry for at most the length of that disagreement (documented).
func TestSkewedReplicasConvergeOnEveryDocument(t *testing.T) {
	seeds := uint64(8)
	if testing.Short() || raceEnabled {
		seeds = 2
	}
	for seed := range seeds {
		r := rand.New(rand.NewPCG(seed, 1013))
		n := 2 + r.IntN(3)
		st := make([]*Store, n)
		clocks := make([]*testClock, n)
		for i := range st {
			span := int(4 * maxRecordedSkew / time.Second)
			skew := time.Duration(r.IntN(span)-span/2) * time.Second
			clocks[i] = &testClock{now: t0.Add(skew)}
			st[i] = synced(t, Options{Name: "console-state", Now: clocks[i].Now})
		}
		cm := &corev1.ConfigMap{}
		flush := func(i int) bool {
			s := st[i]
			s.mu.Lock()
			defer s.mu.Unlock()
			s.mergeLocked(cm)
			d := s.desiredLocked()
			changed := !maps.Equal(d.Data, cm.Data)
			cm = d
			s.base = d.DeepCopy()
			return changed
		}
		for k := range 400 {
			if k%7 == 0 {
				for _, c := range clocks {
					c.Add(time.Second)
				}
			}
			i := r.IntN(n)
			s, now := st[i], clocks[i].Now()
			switch r.IntN(6) {
			case 0:
				flush(i)
			case 1:
				s.RecordSignOut(fmt.Sprintf("x%d-%d", seed, k), tag(fmt.Sprint("p", r.IntN(40))), now.Add(12*time.Hour))
			case 2:
				s.InstanceSince(fmt.Sprint("pod-", r.IntN(20)), fmt.Sprint("state-", r.IntN(3)), now)
			case 3:
				s.MarkDigestSent(fmt.Sprintf("20261105T%02d00Z", r.IntN(24)), "webhook", now)
			case 4:
				_ = s.UseCode(fmt.Sprint("code-", r.IntN(200)), uh(fmt.Sprint("u", r.IntN(10))), now.Add(time.Hour))
			case 5:
				s.KeepDrill(passed(fmt.Sprint("db-", r.IntN(10)), now))
			}
		}
		for round := range 10 {
			changed := false
			for i := range st {
				if flush(i) {
					changed = true
				}
			}
			if !changed {
				break
			}
			if round == 9 {
				t.Fatalf("seed %d (%d replicas): still writing after 10 rounds", seed, n)
			}
		}
		for i := 1; i < n; i++ {
			st[0].mu.Lock()
			want := st[0].encodeLocked()
			st[0].mu.Unlock()
			st[i].mu.Lock()
			got := st[i].encodeLocked()
			st[i].mu.Unlock()
			if !maps.Equal(got, want) {
				t.Fatalf("seed %d: replica %d holds other documents", seed, i)
			}
		}
	}
}
