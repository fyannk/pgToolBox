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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// loaded is a replica that has read the ConfigMap once and then hears
// nothing more: the window before its watch delivers the others' writes.
func loaded(t *testing.T, api *apiServer, clock *testClock) *Store {
	t.Helper()
	s := newStore(api, clock, slog.New(slog.DiscardHandler))
	cm, err := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.mergeLocked(cm)
	s.watchOpenedLocked()
	s.mu.Unlock()
	return s
}

func (a *apiServer) holds(code string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return codeIn(a.cm, codeHash(code))
}

func (a *apiServer) set(f func(*apiServer)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

// A spend written through is in the ConfigMap when SpendCode returns,
// and another replica that has not read it yet finds it on its own
// write's conflict: a confirmation spent on one replica cannot be spent
// on another in the watch window.
func TestSpendCodeIsWrittenThrough(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a, b := loaded(t, api, clock), loaded(t, api, clock)
	until := t0.Add(time.Minute)
	if err := a.SpendCode(t.Context(), "confirm-1", uh("alice"), until); err != nil {
		t.Fatal(err)
	}
	if !api.holds("confirm-1") {
		t.Fatal("SpendCode returned before the spend was in the ConfigMap")
	}
	if b.CodeUsed("confirm-1") {
		t.Fatal("the test needs b not to have read a's write")
	}
	if err := b.SpendCode(t.Context(), "confirm-1", uh("alice"), until); !errors.Is(err, ErrSpent) {
		t.Fatalf("b spent a confirmation a spent: %v", err)
	}
	if !b.CodeUsed("confirm-1") {
		t.Error("b does not remember the spend it found")
	}
	// b's own spend lands, beside a's.
	if err := b.SpendCode(t.Context(), "confirm-2", uh("alice"), until); err != nil {
		t.Fatal(err)
	}
	if !api.holds("confirm-1") || !api.holds("confirm-2") {
		t.Error("a write-through lost the other replica's spend")
	}
	// The same replica twice: spent locally, no write.
	updates := api.updates
	if err := b.SpendCode(t.Context(), "confirm-2", uh("alice"), until); !errors.Is(err, ErrSpent) || api.updates != updates {
		t.Errorf("a local replay: %v, %d writes", err, api.updates-updates)
	}
}

// A spend that cannot be written is refused and leaves nothing behind.
func TestSpendCodeRefusedWhenNotWritten(t *testing.T) {
	clock := &testClock{now: t0}
	until := t0.Add(time.Minute)
	api := newAPIServer()
	s := loaded(t, api, clock)
	api.set(func(a *apiServer) { a.forbidden = true })
	if err := s.SpendCode(t.Context(), "confirm-1", uh("alice"), until); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("a refused write: %v", err)
	}
	if s.CodeUsed("confirm-1") {
		t.Error("an unrecorded spend is kept")
	}
	// Forbidden now: refused without a write.
	if err := s.SpendCode(t.Context(), "confirm-2", uh("alice"), until); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("the ConfigMap forbidden: %v", err)
	}
	// Not read yet: refused.
	fresh := newStore(newAPIServer(), clock, slog.New(slog.DiscardHandler))
	if err := fresh.SpendCode(t.Context(), "c", uh("alice"), until); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("before the first read: %v", err)
	}
	// Endless conflicts: refused, nothing kept.
	api = newAPIServer()
	s = loaded(t, api, clock)
	api.set(func(a *apiServer) { a.conflicts = maxAttempts + 1 })
	if err := s.SpendCode(t.Context(), "confirm-3", uh("alice"), until); !errors.Is(err, ErrNotRecorded) || s.CodeUsed("confirm-3") {
		t.Errorf("still conflicting: %v", err)
	}
	// Without a ConfigMap (one replica): the local table.
	one := New(Options{Now: clock.Now})
	if err := one.SpendCode(t.Context(), "c", uh("alice"), until); err != nil {
		t.Fatal(err)
	}
	if err := one.SpendCode(t.Context(), "c", uh("alice"), until); !errors.Is(err, ErrSpent) {
		t.Errorf("one replica, twice: %v", err)
	}
}

// A confirmation is one entry of the codes table however many steps it
// records (answered, then approved, then run: MarkCode), so that the
// steps after the one the fair share admitted are never refused for want
// of room. 80 hoarders fill the table to 1,280 codes; a person then runs
// confirmations to the late bound, and every step of each succeeds.
func TestConfirmationStepsAreOneEntryPastTheLateBound(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	codes := 0
	for p := range 80 {
		user := fmt.Sprint("hoarder", p)
		for s.UseCode(fmt.Sprint(user, "/", codes), uh(user), t0.Add(time.Hour)) == nil {
			codes++
		}
	}
	if codes*100 < maxCodes*fairFromPercent {
		t.Fatalf("the test needs the table past %d %%: %d codes", fairFromPercent, codes)
	}
	op := uh("operator")
	until := t0.Add(3 * time.Minute)
	for w := range codesBound.lateBound() {
		answered := fmt.Sprint("answered/", w)
		if err := s.SpendCode(t.Context(), answered, op, until); err != nil {
			t.Fatalf("confirmation %d: answered: %v", w+1, err)
		}
		for _, mark := range []string{"approve", "run"} {
			if err := s.MarkCode(t.Context(), answered, mark, op); err != nil {
				t.Fatalf("confirmation %d: %s: %v", w+1, mark, err)
			}
			if !s.CodeMarked(answered, mark) {
				t.Fatalf("confirmation %d: %s not marked", w+1, mark)
			}
		}
		if err := s.MarkCode(t.Context(), answered, "run", op); !errors.Is(err, ErrSpent) {
			t.Fatalf("confirmation %d run twice: %v", w+1, err)
		}
	}
	// The next confirmation is refused at its first step, before anything
	// is spent: the fair share counts confirmations.
	if err := s.SpendCode(t.Context(), "answered/next", op, until); !errors.Is(err, ErrBusy) {
		t.Errorf("a confirmation past the late bound: %v", err)
	}
	if got := len(s.codes); got != codes+codesBound.lateBound() {
		t.Errorf("%d entries, want %d: a step took an entry of its own", got, codes+codesBound.lateBound())
	}
}

// A mark goes only on a value the person spent and that has not expired.
func TestMarkCodeNeedsTheSpentValue(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	until := t0.Add(time.Minute)
	if err := s.UseCode("c", uh("alice"), until); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code, mark, user string
		want                   error
	}{
		{"never spent", "other", "run", uh("alice"), ErrNotSpent},
		{"someone else's", "c", "run", uh("bob"), ErrNotSpent},
	} {
		if err := s.MarkCode(t.Context(), tc.code, tc.mark, tc.user); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	for _, bad := range []string{"", "RUN", "run!", strings.Repeat("a", maxMark+1)} {
		if err := s.MarkCode(t.Context(), "c", bad, uh("alice")); err == nil {
			t.Errorf("mark %q accepted", bad)
		}
	}
	for i := range maxMarks {
		if err := s.MarkCode(t.Context(), "c", string(rune('a'+i)), uh("alice")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkCode(t.Context(), "c", "z", uh("alice")); !errors.Is(err, ErrBusy) {
		t.Errorf("a mark past maxMarks: %v", err)
	}
	clock.Add(time.Minute)
	if err := s.MarkCode(t.Context(), "c", "a", uh("alice")); !errors.Is(err, ErrNotSpent) || s.CodeMarked("c", "a") {
		t.Errorf("an expired value: %v", err)
	}
}

// A mark is written through like a spend: on the ConfigMap when MarkCode
// returns, found on another replica's conflict, merged as a union, and
// forgotten when it cannot be written.
func TestMarkCodeIsWrittenThrough(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := loaded(t, api, clock)
	until := t0.Add(time.Minute)
	if err := a.SpendCode(t.Context(), "confirm-1", uh("alice"), until); err != nil {
		t.Fatal(err)
	}
	b := loaded(t, api, clock)
	if err := a.MarkCode(t.Context(), "confirm-1", "run", uh("alice")); err != nil {
		t.Fatal(err)
	}
	if !api.marked("confirm-1", "run") {
		t.Fatal("MarkCode returned before the mark was in the ConfigMap")
	}
	if b.CodeMarked("confirm-1", "run") {
		t.Fatal("the test needs b not to have read a's write")
	}
	if err := b.MarkCode(t.Context(), "confirm-1", "run", uh("alice")); !errors.Is(err, ErrSpent) {
		t.Fatalf("b ran a confirmation a ran: %v", err)
	}
	if !b.CodeMarked("confirm-1", "run") {
		t.Error("b does not remember the mark it found")
	}
	// b's own mark lands beside a's.
	if err := b.MarkCode(t.Context(), "confirm-1", "approve", uh("alice")); err != nil {
		t.Fatal(err)
	}
	if !api.marked("confirm-1", "run") || !api.marked("confirm-1", "approve") {
		t.Error("a write-through lost the other replica's mark")
	}
	// A mark that cannot be written is refused and forgotten.
	api.set(func(a *apiServer) { a.conflicts = maxAttempts + 1 })
	if err := b.MarkCode(t.Context(), "confirm-1", "decline", uh("alice")); !errors.Is(err, ErrNotRecorded) || b.CodeMarked("confirm-1", "decline") {
		t.Errorf("still conflicting: %v", err)
	}
}

func (a *apiServer) marked(code, mark string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return markIn(a.cm, codeHash(code), mark)
}
