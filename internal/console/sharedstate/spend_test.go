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
	"log/slog"
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
	s.setModeLocked(modeLive, "")
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
