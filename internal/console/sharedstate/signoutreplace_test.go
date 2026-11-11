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
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
)

func synced(t *testing.T, opts Options) *Store {
	t.Helper()
	s := New(opts)
	s.mu.Lock()
	s.mergeLocked(&corev1.ConfigMap{})
	s.mu.Unlock()
	return s
}

func exchange(to, from *Store) {
	from.mu.Lock()
	cm := from.desiredLocked()
	from.mu.Unlock()
	to.mu.Lock()
	to.mergeLocked(cm)
	to.mu.Unlock()
}

func othersOf(s *Store, who string) map[string]signOut {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]signOut{}
	for h, e := range s.signedOut {
		if e.Subject != who {
			out[h] = e
		}
	}
	return out
}

// Past 16 sign-outs a person's latest one used to be refused, and that
// is the one a fresh cookie theft needs (round 8). It is now stored, in
// the place of that person's own sign-out that ends first, even with
// the document full; nobody else's entry moves.
func TestAPersonsLatestSignOutReplacesTheirOwnOldest(t *testing.T) {
	clock := &testClock{now: t0}
	s := synced(t, Options{Name: "console-state", Now: clock.Now})
	// 47 other people at their share, and mallory at hers: the document
	// is full.
	for p := range MaxSignOuts/MaxSignOutsPerSubject - 1 {
		for i := range MaxSignOutsPerSubject {
			if !s.RecordSignOut(fmt.Sprintf("o-%d-%d", p, i), tag(fmt.Sprint("person-", p)), t0.Add(time.Hour+time.Duration(i)*time.Second)) {
				t.Fatalf("person %d's sign-out %d refused", p, i)
			}
		}
	}
	for i := range MaxSignOutsPerSubject / 2 {
		clock.Add(time.Second)
		end := t0.Add(2*time.Hour + time.Duration(i)*time.Second)
		s.RecordSignOut(fmt.Sprint("session-", i), tag("mallory"), end)
		s.RecordSignOut(fmt.Sprint("bearer-", i), tag("mallory"), end)
	}
	if n := len(othersOf(s, "")); n != MaxSignOuts {
		t.Fatalf("%d sign-outs held, want the document full", n)
	}
	others := othersOf(s, tag("mallory"))
	before := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedReplaced))
	clock.Add(time.Second)
	for _, sid := range []string{"session-16", "bearer-16"} {
		if !s.RecordSignOut(sid, tag("mallory"), t0.Add(3*time.Hour)) {
			t.Fatalf("mallory's 17th sign-out (%s) was refused", sid)
		}
	}
	if !s.SignedOut("session-16") || !s.SignedOut("bearer-16") {
		t.Fatal("the 17th sign-out is not held")
	}
	if s.SignedOut("session-0") || s.SignedOut("bearer-0") || !s.SignedOut("session-1") {
		t.Error("the person's own earliest-ending sign-out must be the one given up")
	}
	if got := othersOf(s, tag("mallory")); !maps.Equal(got, others) {
		t.Error("another person's entry moved")
	}
	if got := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedReplaced)) - before; got != 2 {
		t.Errorf("replacements counted: %v, want 2", got)
	}
	// Capping the document (what every merge does) keeps it as it is.
	s.mu.Lock()
	dropped := planSignOutCaps(s.signedOut)
	s.pruneLocked()
	s.mu.Unlock()
	for bound, want := range map[string]float64{"subject": MaxSignOutsPerSubject, "unnamed": 0, "total": MaxSignOuts} {
		if got := testutil.ToFloat64(signOutsHeld.WithLabelValues(bound)); got != want {
			t.Errorf("signouts{bound=%q} = %v, want %v", bound, got, want)
		}
	}
	if len(dropped) != 0 {
		t.Errorf("the bounds drop %v from a document a replacement made", dropped)
	}
}

// A replacement reaches the replicas that still hold the entry it
// replaced: they drop that one, keep the new one, and end identical.
func TestAReplacementReachesEveryReplica(t *testing.T) {
	clock := &testClock{now: t0}
	a := synced(t, Options{Name: "console-state", Now: clock.Now})
	b := synced(t, Options{Name: "console-state", Now: clock.Now})
	for i := range MaxSignOutsPerSubject {
		clock.Add(time.Second)
		a.RecordSignOut(fmt.Sprint("s-", i), tag("mallory"), t0.Add(time.Hour+time.Duration(i)*time.Second))
	}
	b.RecordSignOut("bob", tag("bob"), t0.Add(time.Hour))
	exchange(b, a)
	exchange(a, b)
	clock.Add(time.Second)
	a.RecordSignOut("latest", tag("mallory"), t0.Add(2*time.Hour))
	exchange(b, a)
	exchange(a, b)
	for name, s := range map[string]*Store{"a": a, "b": b} {
		if !s.SignedOut("latest") || s.SignedOut("s-0") || !s.SignedOut("s-1") || !s.SignedOut("bob") {
			t.Errorf("replica %s: latest %v, replaced %v, bob %v", name, s.SignedOut("latest"), s.SignedOut("s-0"), s.SignedOut("bob"))
		}
	}
	if !maps.Equal(a.signedOut, b.signedOut) {
		t.Fatalf("replicas hold %d and %d sign-outs, differently", len(a.signedOut), len(b.signedOut))
	}
}

// Two replicas filling the last place of the document at once each
// accept their sign-out; merging keeps one entry fewer (the fair share
// takes it from a person holding the most). That drop used to be
// neither counted nor logged (round 8): it is counted under merge and
// logged once.
func TestAMergeDropIsCountedAndLogged(t *testing.T) {
	clock := &testClock{now: t0}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	a := synced(t, Options{Name: "console-state", Now: clock.Now, Logger: log})
	b := synced(t, Options{Name: "console-state", Now: clock.Now})
	for i := range MaxSignOuts - 1 {
		a.RecordSignOut(fmt.Sprint("s-", i), tag(fmt.Sprint("person-", i/8)), t0.Add(time.Hour))
	}
	exchange(b, a)
	base := maps.Clone(a.signedOut)
	clock.Add(time.Second)
	before := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedMerge))
	for round := range 2 {
		if !a.RecordSignOut(fmt.Sprint("a-last-", round), tag("alice"), t0.Add(time.Hour)) ||
			!b.RecordSignOut(fmt.Sprint("b-last-", round), tag("bob"), t0.Add(time.Hour)) {
			t.Fatal("a sign-out below the bound was refused")
		}
		exchange(a, b)
		if len(a.signedOut) != MaxSignOuts {
			t.Fatalf("round %d: %d sign-outs held after the merge", round, len(a.signedOut))
		}
		// Back to one place left for the next round.
		a.mu.Lock()
		a.signedOut = maps.Clone(base)
		a.mu.Unlock()
		b.mu.Lock()
		b.signedOut = maps.Clone(base)
		b.mu.Unlock()
	}
	if got := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedMerge)) - before; got != 2 {
		t.Errorf("merge drops counted: %v, want 2 (one a round)", got)
	}
	if n := strings.Count(logs.String(), "merging the sessions document dropped"); n != 1 {
		t.Errorf("the merge drop was logged %d times, want once:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), sessionHash("a-last-0")) || strings.Contains(logs.String(), tag("alice")) {
		t.Error("the log names a session or a person")
	}
}

// A slot read back must be the entry's own (generation 0) or another's
// at a later generation: anything else is not taken in.
func TestForgedSlotsAreRefusedOnRead(t *testing.T) {
	clock := &testClock{now: t0}
	s := synced(t, Options{Name: "console-state", Now: clock.Now})
	exp := t0.Add(time.Hour).Unix()
	good, other := sessionHash("good"), sessionHash("other")
	for _, tc := range []struct {
		name string
		h    string
		e    signOut
		ok   bool
	}{
		{"own slot", good, signOut{Expiry: exp, Recorded: 1}, true},
		{"replacement", good, signOut{Expiry: exp, Recorded: 1, Slot: other, Gen: 1}, true},
		{"generation without a slot", good, signOut{Expiry: exp, Gen: 1}, false},
		{"own hash as another's slot", good, signOut{Expiry: exp, Slot: good, Gen: 1}, false},
		{"another's slot at generation 0", good, signOut{Expiry: exp, Slot: other}, false},
		{"negative generation", good, signOut{Expiry: exp, Slot: other, Gen: -1}, false},
		{"slot not a hash", good, signOut{Expiry: exp, Slot: "zz", Gen: 1}, false},
		{"generation past the bound", good, signOut{Expiry: exp, Slot: other, Gen: maxSignOutGen + 1}, false},
	} {
		if got := validSignOut(tc.h, tc.e); got != tc.ok {
			t.Errorf("%s: valid = %v, want %v", tc.name, got, tc.ok)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cm := &corev1.ConfigMap{Data: map[string]string{KeySessions: fmt.Sprintf(`{"version":%d,"signedOut":{%q:{"e":%d,"g":3}}}`, formatVersion, good, exp)}}
	s.mergeLocked(cm)
	if _, ok := s.signedOut[good]; ok {
		t.Error("a forged slot was taken in")
	}
}
