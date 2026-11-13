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
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// grantsJSON is a grants document holding entries, as another replica
// wrote it.
func grantsJSON(t *testing.T, entries map[string]grantEntry) string {
	t.Helper()
	raw, err := json.Marshal(grantsDoc{Version: formatVersion, Grants: entries, Codes: map[string]codeEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func standingEntry(user string) grantEntry {
	return grantEntry{User: uh(user), Client: "Claude Code", Scope: "mcp:read", Created: t0.Unix(), Used: t0.Unix(), Expiry: t0.Add(12 * time.Hour).Unix()}
}

// leaderStore is a store under the MCP leader, its leadership in lead,
// with the ConfigMap read and the watch open.
func leaderStore(t *testing.T, api *apiServer, lead *atomic.Bool) *Store {
	t.Helper()
	s := newStore(api, &testClock{now: t0}, nil)
	s.SetLeader(lead.Load)
	s.mu.Lock()
	s.mergeLocked(api.cm.DeepCopy())
	s.watchOpenedLocked()
	s.mu.Unlock()
	return s
}

func storedGrants(t *testing.T, api *apiServer) grantsDoc {
	t.Helper()
	var doc grantsDoc
	if err := json.Unmarshal([]byte(api.data(KeyGrants)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// A new leader starts from the ConfigMap, not from a union with what it
// held: a grant revoked here, then pruned from the document, stays gone.
func TestLoadGrantsReplacesTheIndex(t *testing.T) {
	api := newAPIServer()
	api.cm.Data = map[string]string{KeyGrants: grantsJSON(t, map[string]grantEntry{grantID(2): standingEntry("bob")})}
	var lead atomic.Bool
	lead.Store(true)
	s := leaderStore(t, api, &lead)
	if s.GrantsUsable() {
		t.Fatal("grants usable before they were loaded")
	}
	if _, ok := s.Grant(grantID(2)); ok {
		t.Fatal("a replica that did not load the grants merged them")
	}
	if err := s.LoadGrants(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Grant(grantID(2)); !ok || !s.GrantsUsable() {
		t.Fatalf("grant 2 loaded = %v, usable = %v", ok, s.GrantsUsable())
	}
	// Recorded and revoked here, never written: the merge rules would keep
	// both (recorded, unwritten); a load drops them.
	if err := s.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	s.RevokeGrant(grantID(1), uh("alice"), t0, t0.Add(time.Hour))
	if err := s.UseCode("code", uh("alice"), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.LoadGrants(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Grant(grantID(1)); ok {
		t.Fatal("a grant the ConfigMap does not hold survived the load")
	}
	if s.CodeUsed("code") {
		t.Fatal("a code the ConfigMap does not hold survived the load")
	}
	if _, ok := s.Grant(grantID(2)); !ok {
		t.Fatal("the load lost grant 2")
	}
	s.DropGrants()
	if _, ok := s.Grant(grantID(2)); ok || s.GrantsUsable() {
		t.Fatal("grants kept or usable after DropGrants")
	}
}

// A replica that does not lead writes grants.json byte for byte as it
// read it, and writes no grant or spend through.
func TestANonLeaderWritesGrantsAsRead(t *testing.T) {
	api := newAPIServer()
	raw := fmt.Sprintf(`{"version":%d,  "grants":{%q:{"u":%q,"c":"x","s":"mcp:read","t":1,"e":%d}},  "codes":{}}`,
		formatVersion, grantID(3), uh("carol"), t0.Add(time.Hour).Unix())
	api.cm.Data = map[string]string{KeyGrants: raw}
	var lead atomic.Bool // never leads
	s := leaderStore(t, api, &lead)
	s.RecordSignOut("session", "", t0.Add(time.Hour))
	if err := s.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if api.updates != 1 || api.data(KeyGrants) != raw {
		t.Fatalf("updates = %d, grants.json = %s, want %s as read", api.updates, api.data(KeyGrants), raw)
	}
	if err := s.SpendCode(t.Context(), "code", uh("carol"), t0.Add(time.Minute)); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("spend on a non-leader = %v, want ErrNotRecorded", err)
	}
	if api.updates != 1 {
		t.Fatalf("a non-leader wrote a spend through (%d updates)", api.updates)
	}
}

// The new leader's write, conflicting with a late write of the old one,
// merges it: the old leader's revocation is kept.
func TestALateWriteOfTheOldLeaderIsMerged(t *testing.T) {
	api := newAPIServer()
	api.cm.Data = map[string]string{KeyGrants: grantsJSON(t, map[string]grantEntry{grantID(1): standingEntry("alice")})}
	var lead atomic.Bool
	lead.Store(true)
	s := leaderStore(t, api, &lead)
	if err := s.LoadGrants(t.Context()); err != nil {
		t.Fatal(err)
	}
	revoked := standingEntry("alice")
	revoked.Revoked = t0.Unix()
	late := api.cm.DeepCopy()
	late.Data[KeyGrants] = grantsJSON(t, map[string]grantEntry{grantID(1): revoked})
	api.mu.Lock()
	api.bumpLocked(late)
	api.mu.Unlock()
	s.TouchGrant(grantID(1), t0.Add(time.Minute))
	if err := s.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := storedGrants(t, api).Grants[grantID(1)]
	if e.Revoked != t0.Unix() || e.Used != t0.Add(time.Minute).Unix() {
		t.Fatalf("written %+v, want the old leader's revocation and this leader's use", e)
	}
}

// Every grants.json write checks the leadership before its attempt: one
// lost meanwhile writes nothing through, and the batch carries the
// document as read.
func TestALostLeadershipWritesNoGrant(t *testing.T) {
	api := newAPIServer()
	before := grantsJSON(t, map[string]grantEntry{})
	api.cm.Data = map[string]string{KeyGrants: before}
	var lead atomic.Bool
	lead.Store(true)
	s := leaderStore(t, api, &lead)
	if err := s.LoadGrants(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	lead.Store(false)
	if err := s.WriteGrant(t.Context(), grantID(1)); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("WriteGrant after the leadership ended = %v, want ErrNotRecorded", err)
	}
	s.RecordSignOut("session", "", t0.Add(time.Hour))
	if err := s.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := api.data(KeyGrants); got != before {
		t.Fatalf("grants.json = %s after the leadership ended, want it as read", got)
	}
}

// Without a ConfigMap the index is this replica's memory: usable once
// loaded and while leading only.
func TestAMemoryIndexUnderTheLeader(t *testing.T) {
	var lead atomic.Bool
	lead.Store(true)
	s := New(Options{Now: (&testClock{now: t0}).Now})
	s.SetLeader(lead.Load)
	if s.GrantsUsable() {
		t.Fatal("usable before LoadGrants")
	}
	if err := s.LoadGrants(t.Context()); err != nil || !s.GrantsUsable() {
		t.Fatalf("LoadGrants = %v, usable = %v", err, s.GrantsUsable())
	}
	lead.Store(false)
	if s.GrantsUsable() {
		t.Fatal("usable while not leading")
	}
}
