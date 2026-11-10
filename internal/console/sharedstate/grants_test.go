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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// uh stands for the user key mcpoauth computes (a keyed hash).
func uh(user string) string {
	sum := sha256.Sum256([]byte("test/" + user))
	return hex.EncodeToString(sum[:16])
}

func grantID(n int) string { return fmt.Sprintf("grant-%032x", n) }

func testGrant(n int, user string, created time.Time) Grant {
	return Grant{
		ID: grantID(n), UserHash: uh(user), Client: "Claude Code", Scope: "mcp:read",
		Created: created, Expiry: created.Add(12 * time.Hour),
	}
}

func entries(s *Store) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.grants)
}

func TestGrantMergeRules(t *testing.T) {
	a := grantEntry{User: uh("alice"), Client: "x", Created: 100, Used: 150, Expiry: 1000, Gen: 2}
	b := grantEntry{User: uh("alice"), Client: "x", Created: 90, Used: 120, Expiry: 1000, Gen: 3, Revoked: 200}
	m := mergeGrant(a, b)
	if m.Created != 90 || m.Used != 150 || m.Gen != 3 || m.Revoked != 200 {
		t.Errorf("merge = %+v", m)
	}
	// A revocation is never undone, and the earliest one wins.
	m = mergeGrant(grantEntry{Revoked: 300}, grantEntry{Revoked: 0})
	if m.Revoked != 300 {
		t.Errorf("revocation undone: %+v", m)
	}
	if m := mergeGrant(grantEntry{Revoked: 300}, grantEntry{Revoked: 250}); m.Revoked != 250 {
		t.Errorf("latest revocation won: %+v", m)
	}
}

func TestGrantLifecycle(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	g := testGrant(1, "alice", t0)
	if err := s.AddGrant(g); err != nil {
		t.Fatal(err)
	}
	if err := s.AddGrant(Grant{ID: "grant-nothex", UserHash: uh("a"), Created: t0, Expiry: t0.Add(time.Hour)}); err == nil {
		t.Error("an invalid grant ID was accepted")
	}
	if got := s.GrantsOf(uh("alice")); len(got) != 1 || got[0].ID != g.ID {
		t.Fatalf("GrantsOf = %+v", got)
	}
	if got := s.GrantsOf(uh("bob")); len(got) != 0 {
		t.Errorf("bob sees %+v", got)
	}

	// Refresh rotation: one use per generation; an index behind the token
	// (another replica rotated it) catches up; a grant it does not know is
	// the caller's decision.
	if got := s.AdvanceGrant(g.ID, 0, t0); got != Advanced {
		t.Fatalf("first rotation: %v", got)
	}
	if got := s.AdvanceGrant(g.ID, 0, t0); got != Replayed {
		t.Fatalf("a reused generation: %v", got)
	}
	if got := s.AdvanceGrant(g.ID, 3, t0); got != Advanced {
		t.Fatalf("an index behind the token: %v", got)
	}
	if x, _ := s.Grant(g.ID); x.Generation != 4 {
		t.Fatalf("generation = %d, want 4", x.Generation)
	}
	if got := s.AdvanceGrant(grantID(99), 7, t0); got != Unknown {
		t.Errorf("a grant the index does not know: %v", got)
	}

	// Last use, debounced by the caller, monotonic here.
	s.TouchGrant(g.ID, t0.Add(time.Minute))
	s.TouchGrant(g.ID, t0)
	if got, _ := s.Grant(g.ID); !got.LastUsed.Equal(t0.Add(time.Minute)) {
		t.Errorf("LastUsed = %v", got.LastUsed)
	}

	if !s.RevokeGrant(g.ID, uh("alice"), t0.Add(time.Hour), g.Expiry) || !s.GrantRevoked(g.ID) {
		t.Fatal("revocation not recorded")
	}
	if s.RevokeGrant(g.ID, uh("alice"), t0.Add(2*time.Hour), g.Expiry) {
		t.Error("a second revocation reported a standing grant")
	}
	if got := s.AdvanceGrant(g.ID, 4, t0.Add(time.Hour)); got != Revoked {
		t.Errorf("a revoked grant advanced: %v", got)
	}
	if got := s.GrantsOf(uh("alice")); len(got) != 0 {
		t.Errorf("a revoked grant still stands: %+v", got)
	}
	// A revocation of a grant whose creation was not shared yet is kept,
	// under the user it belongs to.
	if s.RevokeGrant(grantID(2), uh("alice"), t0, t0.Add(5*time.Minute)) || !s.GrantRevoked(grantID(2)) {
		t.Error("an unknown grant's revocation was not recorded")
	}
	if x, _ := s.Grant(grantID(2)); x.UserHash != uh("alice") {
		t.Errorf("placeholder under %q", x.UserHash)
	}

	// A revoked entry goes RevokedKeep after its revocation, a placeholder
	// at its expiry, a standing grant at its expiry.
	clock.Set(t0.Add(time.Hour + RevokedKeep))
	s.mu.Lock()
	s.pruneGrantsLocked(clock.Now().Unix())
	s.mu.Unlock()
	if _, ok := s.Grant(g.ID); ok {
		t.Error("a revoked entry outlived RevokedKeep")
	}
	if _, ok := s.Grant(grantID(2)); ok {
		t.Error("an expired placeholder was kept")
	}
	if err := s.AddGrant(testGrant(3, "bob", clock.Now())); err != nil {
		t.Fatal(err)
	}
	clock.Add(13 * time.Hour)
	s.mu.Lock()
	s.pruneGrantsLocked(clock.Now().Unix())
	s.mu.Unlock()
	if n := entries(s); n != 0 {
		t.Errorf("%d grants left after expiry", n)
	}
}

// Every entry counts against the bounds, revoked ones and placeholders
// included: one person cannot grow the index by connecting and
// disconnecting, or by replaying codes.
func TestGrantLimitsCountEveryEntry(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	for i := range MaxGrantsPerUser {
		if err := s.AddGrant(testGrant(i, "alice", t0)); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	if err := s.AddGrant(testGrant(1000, "alice", t0)); !errors.Is(err, ErrTooManyGrants) {
		t.Fatalf("grant past the per-user bound: %v", err)
	}
	// A revoked grant still counts until RevokedKeep has passed.
	s.RevokeGrant(grantID(0), uh("alice"), t0, t0.Add(time.Hour))
	if err := s.AddGrant(testGrant(1000, "alice", t0)); !errors.Is(err, ErrTooManyGrants) {
		t.Fatalf("a revoked entry freed its place at once: %v", err)
	}
	if err := s.AddGrant(testGrant(1001, "bob", t0)); err != nil {
		t.Fatalf("another person is not bound by alice's grants: %v", err)
	}
	clock.Add(RevokedKeep)
	if err := s.AddGrant(testGrant(1000, "alice", clock.Now())); err != nil {
		t.Fatalf("grant after the revoked entry went: %v", err)
	}
	// Placeholders are new work: refused at the bound.
	if s.RevokeGrant(grantID(5000), uh("alice"), clock.Now(), clock.Now().Add(5*time.Minute)) || s.GrantRevoked(grantID(5000)) {
		t.Error("a placeholder grew the index past the per-user bound")
	}

	// The total bound.
	s = New(Options{Now: clock.Now})
	for i := range MaxGrants {
		if err := s.AddGrant(testGrant(i, fmt.Sprint("user", i), clock.Now())); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	if err := s.AddGrant(testGrant(MaxGrants, "late", clock.Now())); !errors.Is(err, ErrTooManyGrants) {
		t.Fatalf("grant past the total bound: %v", err)
	}
	if s.RevokeGrant(grantID(MaxGrants+1), uh("late"), clock.Now(), clock.Now().Add(time.Minute)) {
		t.Error("a placeholder grew the index past the total bound")
	}
	// A grant the index holds is always revocable.
	if !s.RevokeGrant(grantID(7), uh("user7"), clock.Now(), clock.Now().Add(time.Hour)) {
		t.Error("revoking a standing grant at the bound failed")
	}
	clock.Add(13 * time.Hour)
	if err := s.AddGrant(testGrant(MaxGrants, "late", clock.Now())); err != nil {
		t.Fatalf("grant after the others expired: %v", err)
	}
	if n := entries(s); n != 1 {
		t.Errorf("%d grants kept, want the expired ones pruned", n)
	}
}

// The review's repro: revocations of unknown IDs (the code-replay path)
// by one person are bounded, and a second replica still honours a
// revocation taken on the first.
func TestReplayedCodesCannotGrowTheIndex(t *testing.T) {
	api := newAPIServer()
	clock := &testClock{now: t0}
	a := newStore(api, clock, nil)
	run(t, a)
	eventually(t, "A live", func() bool { return a.State().State == viewmodel.SourceLive })
	victim := testGrant(1, "victim", t0)
	if err := a.AddGrant(victim); err != nil {
		t.Fatal(err)
	}
	a.RevokeGrant(victim.ID, victim.UserHash, t0, victim.Expiry)
	for i := range 4*MaxGrants + 10 {
		a.RevokeGrant(grantID(1_000_000+i), uh("mallory"), t0, t0.Add(5*time.Minute))
	}
	if n := entries(a); n > MaxGrantsPerUser+1 {
		t.Fatalf("one person put %d entries in the index (bound %d)", n-1, MaxGrantsPerUser)
	}
	eventually(t, "A to write", func() bool { return strings.Contains(api.data(KeyGrants), victim.ID) })
	b := newStore(api, clock, nil)
	run(t, b)
	eventually(t, "B live", func() bool { return b.State().State == viewmodel.SourceLive })
	if !b.GrantRevoked(victim.ID) {
		t.Fatal("B does not know the victim's grant is revoked")
	}
}

// A grants document larger than this console's bounds (written by
// another version, or before a bound was lowered) is merged whole: no
// revocation in it is lost, and this replica's write keeps every entry
// instead of replacing the document with a smaller view.
func TestAnOversizedDocumentIsMergedWhole(t *testing.T) {
	api := newAPIServer()
	clock := &testClock{now: t0}
	doc := grantsDoc{Version: 1, Grants: map[string]grantEntry{}, Codes: map[string]codeEntry{}}
	n := 4*MaxGrants + 10
	for i := range n {
		doc.Grants[grantID(i)] = grantEntry{User: uh(fmt.Sprint("u", i%400)), Client: "c", Created: t0.Unix(), Expiry: t0.Add(time.Hour).Unix(), Revoked: t0.Unix()}
	}
	victim := grantID(n - 1)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	api.cm.Data = map[string]string{KeyGrants: string(raw)}
	b := newStore(api, clock, nil)
	run(t, b)
	eventually(t, "B live", func() bool { return b.State().State == viewmodel.SourceLive })
	if !b.GrantRevoked(victim) || !b.GrantsUsable() {
		t.Fatalf("B does not honour a revocation of the document (usable %v)", b.GrantsUsable())
	}
	// New work is refused while over the bound; the write B makes for
	// another reason keeps every entry.
	if err := b.AddGrant(testGrant(n+1, "alice", t0)); !errors.Is(err, ErrTooManyGrants) {
		t.Fatalf("a grant past the total bound: %v", err)
	}
	b.RecordSignOut("sid", "", t0.Add(time.Hour))
	eventually(t, "B's write", func() bool { return api.data(KeySessions) != "" })
	var after grantsDoc
	if err := json.Unmarshal([]byte(api.data(KeyGrants)), &after); err != nil || len(after.Grants) != n {
		t.Fatalf("grants.json after B's write holds %d entries, want %d (%v)", len(after.Grants), n, err)
	}
	// No ping-pong: nothing more to write.
	api.mu.Lock()
	before := api.updates
	api.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.updates != before {
		t.Errorf("%d more writes with nothing changed", api.updates-before)
	}
}

// A grants document this console cannot read is never overwritten (it may
// hold revocations), and grants are refused meanwhile.
func TestAnUnreadableDocumentIsNeverOverwritten(t *testing.T) {
	for _, raw := range []string{`{"version":1,"grants":`, `{"version":2,"grants":{}}`} {
		api := newAPIServer()
		api.cm.Data = map[string]string{KeyGrants: raw}
		s := newStore(api, &testClock{now: t0}, nil)
		run(t, s)
		eventually(t, "live", func() bool { return s.State().State == viewmodel.SourceLive })
		if s.GrantsUsable() {
			t.Errorf("%s: grants usable over an unreadable document", raw)
		}
		s.RecordSignOut("sid", "", t0.Add(time.Hour))
		eventually(t, "the write", func() bool { return api.data(KeySessions) != "" })
		if got := api.data(KeyGrants); got != raw {
			t.Errorf("an unreadable grants document was rewritten: %s", got)
		}
	}
}

func TestGrantsUsable(t *testing.T) {
	clock := &testClock{now: t0}
	if !New(Options{Now: clock.Now}).GrantsUsable() {
		t.Error("a store of this replica's memory is the only authority: usable")
	}
	var none *Store
	if none.GrantsUsable() {
		t.Error("no store: unusable")
	}
	s := newStore(newAPIServer(), clock, nil)
	if s.GrantsUsable() {
		t.Error("a shared store not read yet is usable")
	}
	s.mu.Lock()
	s.setModeLocked(modeLive, "")
	s.mu.Unlock()
	if !s.GrantsUsable() {
		t.Error("a live store is unusable")
	}
	s.mu.Lock()
	s.setModeLocked(modeFailing, "x")
	s.mu.Unlock()
	if !s.GrantsUsable() {
		t.Error("a moment's failure disconnects every assistant")
	}
	clock.Add(staleGrace)
	if s.GrantsUsable() {
		t.Error("a lasting failure keeps grants usable")
	}
	for _, m := range []mode{modeMissing, modeForbidden} {
		s.mu.Lock()
		s.setModeLocked(m, "")
		s.mu.Unlock()
		if s.GrantsUsable() {
			t.Errorf("%s: usable", m)
		}
	}
}

func TestCodesAreSingleUseAndBounded(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	if err := s.UseCode("nonce-1", uh("alice"), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.UseCode("nonce-1", uh("alice"), t0.Add(time.Minute)); !errors.Is(err, ErrSpent) {
		t.Fatalf("a code redeemed twice: %v", err)
	}
	if !s.CodeUsed("nonce-1") || s.CodeUsed("nonce-2") {
		t.Fatal("CodeUsed does not tell a spent code from a fresh one")
	}
	if (*Store)(nil).CodeUsed("nonce-1") {
		t.Fatal("a nil store reports a code spent")
	}
	for i := range MaxCodesPerUser - 1 {
		if err := s.UseCode(fmt.Sprint("a", i), uh("alice"), t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// Full for alice: busy, not spent, and nothing recorded.
	if err := s.UseCode("one-too-many", uh("alice"), t0.Add(time.Minute)); !errors.Is(err, ErrBusy) {
		t.Fatalf("past the per-user bound: %v", err)
	}
	if err := s.UseCode("bob-1", uh("bob"), t0.Add(time.Minute)); err != nil {
		t.Fatalf("bob is not bound by alice's codes: %v", err)
	}
	for i := 0; s.UseCode(fmt.Sprint("u", i), uh(fmt.Sprint("user", i%1000)), t0.Add(time.Minute)) == nil; i++ {
		if i > maxCodes {
			t.Fatal("the code table grew past its bound")
		}
	}
	if err := s.UseCode("full", uh("carol"), t0.Add(time.Minute)); !errors.Is(err, ErrBusy) {
		t.Fatalf("a full table: %v", err)
	}
	clock.Add(2 * time.Minute)
	if s.CodeUsed("nonce-1") {
		t.Error("an expired code still reads as spent")
	}
	if err := s.UseCode("one-too-many", uh("alice"), clock.Now().Add(time.Minute)); err != nil {
		t.Errorf("expired codes were not pruned to make room: %v", err)
	}
	if err := s.UseCode("x", "not-a-key", clock.Now()); err == nil {
		t.Error("a malformed user key was accepted")
	}
}

func TestRevokeGrantsOf(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	for i, u := range []string{"alice", "alice", "bob"} {
		if err := s.AddGrant(testGrant(i, u, t0)); err != nil {
			t.Fatal(err)
		}
	}
	if ids := s.RevokeGrantsOf(uh("alice"), t0); len(ids) != 2 || ids[0] != grantID(0) {
		t.Fatalf("revoked %v", ids)
	}
	if len(s.GrantsOf(uh("alice"))) != 0 || len(s.GrantsOf(uh("bob"))) != 1 {
		t.Error("revoked the wrong grants")
	}
}

// The revocation, the code redemption and the refresh generation reach
// the other replica: what one replica decided, the other enforces.
func TestGrantsReachTheOtherReplica(t *testing.T) {
	api := newAPIServer()
	clock := &testClock{now: t0}
	a, b := newStore(api, clock, nil), newStore(api, clock, nil)
	run(t, a)
	run(t, b)
	eventually(t, "both replicas reading", func() bool {
		return a.State().State == viewmodel.SourceLive && b.State().State == viewmodel.SourceLive
	})
	g := testGrant(1, "alice", t0)
	if err := a.AddGrant(g); err != nil {
		t.Fatal(err)
	}
	eventually(t, "B to learn the grant", func() bool { _, ok := b.Grant(g.ID); return ok })
	if err := a.UseCode("nonce", uh("alice"), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "B to refuse the redeemed code", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.codes) == 1
	})
	if err := b.UseCode("nonce", uh("alice"), t0.Add(time.Minute)); !errors.Is(err, ErrSpent) {
		t.Fatalf("B redeemed A's code again: %v", err)
	}
	if !b.CodeUsed("nonce") {
		t.Fatal("B does not read A's spent code")
	}
	if got := a.AdvanceGrant(g.ID, 0, t0); got != Advanced {
		t.Fatalf("rotation: %v", got)
	}
	eventually(t, "B to learn the generation", func() bool { x, _ := b.Grant(g.ID); return x.Generation == 1 })
	if got := b.AdvanceGrant(g.ID, 0, t0); got != Replayed {
		t.Fatalf("B accepted a refresh token A already rotated: %v", got)
	}
	b.RevokeGrant(g.ID, g.UserHash, t0, g.Expiry)
	eventually(t, "A to refuse the grant B revoked", func() bool { return a.GrantRevoked(g.ID) })

	// No token or username reaches the ConfigMap.
	doc := api.data(KeyGrants)
	if strings.Contains(doc, "alice") || !strings.Contains(doc, uh("alice")) {
		t.Errorf("grants.json = %s", doc)
	}
}

// A revocation taken while the writer waits out a routine batch (a grant's
// last-used time, a "since" time: what a busy console changes all the
// time) is written within the urgent debounce, not at the end of the
// routine one. The routine debounce here is an hour: before the fix the
// other replica kept accepting the grant until then.
func TestARevocationDoesNotWaitForTheRoutineBatch(t *testing.T) {
	api := newAPIServer()
	clock := &testClock{now: t0}
	opts := func() Options {
		return Options{
			Client: api, Name: "console-state", Now: clock.Now,
			Debounce: time.Hour, UrgentDebounce: time.Millisecond,
			Backoff: func(int) time.Duration { return 5 * time.Millisecond },
		}
	}
	// The grant is in the ConfigMap already: both replicas read it at
	// start.
	g := testGrant(1, "alice", t0)
	seed := New(opts())
	if err := seed.AddGrant(g); err != nil {
		t.Fatal(err)
	}
	seed.mu.Lock()
	api.cm.Data = seed.encodeLocked()
	seed.mu.Unlock()
	a, b := New(opts()), New(opts())
	run(t, a)
	run(t, b)
	eventually(t, "both replicas to hold the grant", func() bool {
		_, okA := a.Grant(g.ID)
		_, okB := b.Grant(g.ID)
		return okA && okB
	})

	// A routine change starts an hour-long batch on A; the writer takes
	// its wake-up and waits.
	a.TouchGrant(g.ID, t0.Add(time.Minute))
	eventually(t, "A's writer to start the routine batch", func() bool { return len(a.wake) == 0 })
	time.Sleep(20 * time.Millisecond)

	if !a.RevokeGrant(g.ID, g.UserHash, t0.Add(time.Minute), g.Expiry) {
		t.Fatal("the grant was not standing")
	}
	eventually(t, "B to refuse the grant A revoked", func() bool { return b.GrantRevoked(g.ID) })
}
