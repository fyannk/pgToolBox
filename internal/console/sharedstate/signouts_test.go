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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
)

// tag stands for the keyed tag the sign-in layer seals in a session.
func tag(person string) string {
	sum := sha256.Sum256([]byte("test-tag\x00" + person))
	return hex.EncodeToString(sum[:SubjectTagLen/2])
}

// One person signing in and out in a loop within their sessions'
// lifetime (two entries a sign-out) used to grow the sessions document
// past the API server's 1 MiB, after which every write of the ConfigMap
// failed. They now fill only their own share: past it each new sign-out
// replaces their own that ends first (counted), so their latest ones are
// shared, and never another person's is dropped.
func TestOnePersonsSignOutsAreBounded(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Name: "console-state", Now: clock.Now})
	s.RecordSignOut("victim-session", tag("bob"), t0.Add(12*time.Hour))
	refusedBefore := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedReplaced))
	for i := range 6700 {
		clock.Add(time.Second)
		s.RecordSignOut(fmt.Sprintf("session-%d", i), tag("mallory"), t0.Add(12*time.Hour+time.Duration(i)*time.Second))
		s.RecordSignOut(fmt.Sprintf("bearer-%d", i), tag("mallory"), t0.Add(12*time.Hour+time.Duration(i)*time.Second))
	}
	if got := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedReplaced)) - refusedBefore; got != 2*6700-MaxSignOutsPerSubject {
		t.Errorf("replacements counted: %v, want %d", got, 2*6700-MaxSignOutsPerSubject)
	}
	s.mu.Lock()
	s.pruneLocked()
	n, size := len(s.signedOut), len(s.encodeLocked()[KeySessions])
	s.mu.Unlock()
	if n != MaxSignOutsPerSubject+1 {
		t.Errorf("%d sign-outs kept, want mallory's %d and bob's", n, MaxSignOutsPerSubject)
	}
	if size > 6<<10 {
		t.Errorf("the sessions document is %d bytes", size)
	}
	if !s.SignedOut("victim-session") {
		t.Error("another person's sign-out was dropped")
	}
	if s.SignedOut("session-0") || s.SignedOut("bearer-0") || !s.SignedOut("session-6699") || !s.SignedOut("bearer-6699") ||
		!s.SignedOut(fmt.Sprint("session-", 6700-MaxSignOutsPerSubject/2)) {
		t.Error("a person past their share must keep their latest sign-outs, giving up their earliest-ending ones")
	}
}

// At MaxSignOuts the document is shared fairly: a newcomer takes the
// place of the last entry of a person holding the most (counted under
// evicted), a person holding as much as the most replaces their own, and,
// with a full grants index beside it, the ConfigMap is still one the API
// server accepts.
func TestSignOutsAtTheSizeLimit(t *testing.T) {
	api := newAPIServer()
	api.maxBytes = 1 << 20
	clock := &testClock{now: t0}
	s := newStore(api, clock, nil)
	s.mu.Lock()
	s.mergeLocked(api.cm.DeepCopy())
	s.watchOpenedLocked()
	name := TruncateClientName(strings.Repeat("<& é\\", 100))
	for i := range MaxGrants {
		s.grants[grantID(i)] = grantEntry{
			User: uh(fmt.Sprint("user", i)), Client: name, Scope: strings.Repeat("s", maxScope),
			Created: t0.Unix(), Used: t0.Unix(), Expiry: t0.Add(24 * time.Hour).Unix(), Gen: 999999, Revoked: t0.Unix(),
		}
	}
	for i := range maxCodes {
		sum := sha256.Sum256([]byte(strconv.Itoa(i)))
		s.codes[hex.EncodeToString(sum[:16])] = codeEntry{Expiry: t0.Add(24 * time.Hour).Unix(), User: uh("x")}
	}
	s.mu.Unlock()
	for i := range MaxSignOuts {
		// Many people, a few sign-outs each, all within their bounds.
		if !s.RecordSignOut(fmt.Sprintf("s-%d", i), tag(fmt.Sprint("person-", i/8)), t0.Add(time.Hour+time.Duration(i)*time.Second)) {
			t.Fatalf("sign-out %d refused below MaxSignOuts", i)
		}
	}
	others := othersOf(s, tag("person-late"))
	evictedBefore := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedEvicted))
	replacedBefore := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedReplaced))
	if !s.RecordSignOut("late", tag("person-late"), t0.Add(2*time.Hour)) || !s.SignedOut("late") {
		t.Fatal("a newcomer's sign-out was refused by a full document")
	}
	if got := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedEvicted)) - evictedBefore; got != 1 {
		t.Errorf("evictions counted: %v, want 1", got)
	}
	gone := 0
	for h, e := range others {
		if _, ok := s.signedOut[h]; !ok {
			gone++
			if held := heldBy(s, e.Subject); held != 7 {
				t.Errorf("the evicted entry's person holds %d now, want 7 of 8", held)
			}
		}
	}
	if gone != 1 || len(s.signedOut) != MaxSignOuts {
		t.Fatalf("%d entries left the document for the newcomer, %d held", gone, len(s.signedOut))
	}
	// A person holding as much as the most replaces their own.
	if !s.RecordSignOut("again", tag("person-0"), t0.Add(2*time.Hour)) || s.SignedOut("s-0") {
		t.Error("a person holding the most did not replace their own earliest-ending sign-out")
	}
	if got := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedReplaced)) - replacedBefore; got != 1 {
		t.Errorf("replacements counted: %v, want 1", got)
	}
	if err := s.flush(t.Context()); err != nil {
		t.Fatalf("the full ConfigMap was refused: %v", err)
	}
	size := 0
	for k, v := range api.cm.Data {
		size += len(k) + len(v)
	}
	t.Logf("sessions document %d bytes, ConfigMap data %d bytes", len(api.data(KeySessions)), size)
	if len(api.data(KeySessions)) > 170<<10 {
		t.Errorf("a full sessions document is %d bytes, more than 170 KiB", len(api.data(KeySessions)))
	}
	if size > MaxData {
		t.Errorf("the ConfigMap's data is %d bytes, past %d", size, MaxData)
	}
}

// Two replicas each under the bound but above it together keep the same
// sign-outs after merging: the bound is a total order, so a union never
// brings back what it dropped.
func TestCappedSignOutsConverge(t *testing.T) {
	clock := &testClock{now: t0}
	a := New(Options{Name: "console-state", Now: clock.Now})
	b := New(Options{Name: "console-state", Now: clock.Now})
	for _, s := range []*Store{a, b} {
		s.mu.Lock()
		s.mergeLocked(&corev1.ConfigMap{})
		s.mu.Unlock()
	}
	for i := range MaxSignOutsPerSubject {
		a.RecordSignOut(fmt.Sprint("a-", i), tag("mallory"), t0.Add(time.Hour+time.Duration(2*i)*time.Second))
		b.RecordSignOut(fmt.Sprint("b-", i), tag("mallory"), t0.Add(time.Hour+time.Duration(2*i+1)*time.Second))
	}
	a.mu.Lock()
	cmA := a.desiredLocked()
	a.mu.Unlock()
	b.mu.Lock()
	cmB := b.desiredLocked()
	b.mergeLocked(cmA)
	b.mu.Unlock()
	a.mu.Lock()
	a.mergeLocked(cmB)
	a.mu.Unlock()
	if !maps.Equal(a.signedOut, b.signedOut) || len(a.signedOut) != MaxSignOutsPerSubject {
		t.Fatalf("replicas kept %d and %d sign-outs, differently", len(a.signedOut), len(b.signedOut))
	}
}
