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
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newerState fills api with what a console of format version 3 wrote:
// a sessions document holding the sign-out of sid in this version's
// layout plus a field this version does not know, an empty grants
// document, and that console's annotation.
func newerState(t *testing.T, api *apiServer, sid string) {
	t.Helper()
	m := map[string]signOut{sessionHash(sid): {Expiry: t0.Add(12 * time.Hour).Unix(), Recorded: t0.Unix(), Subject: tag("alice")}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	api.set(func(a *apiServer) {
		cm := a.cm.DeepCopy()
		cm.Data = map[string]string{
			KeySessions: `{"version":3,"signedOut":` + string(raw) + `,"x":1}`,
			KeyGrants:   `{"version":3,"grants":{},"codes":{},"x":1}`,
		}
		cm.Annotations = map[string]string{writerAnnotation(3): itoa(t0.Unix())}
		a.bumpLocked(cm)
	})
}

// newerWrites is a write of the newer console: its annotation changes
// (and so, optionally, a document), the rest of the ConfigMap kept.
func newerWrites(api *apiServer, at time.Time, sessions string) {
	api.set(func(a *apiServer) {
		cm := a.cm.DeepCopy()
		cm.Annotations[writerAnnotation(3)] = itoa(at.Unix())
		if sessions != "" {
			cm.Data[KeySessions] = sessions
		}
		a.bumpLocked(cm)
	})
}

func annotation(api *apiServer, key string) string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.cm.Annotations[key]
}

func flushed(t *testing.T, s *Store) {
	t.Helper()
	if err := s.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func usable(s *Store) bool {
	s.mu.Lock()
	s.watchOpenedLocked()
	s.mu.Unlock()
	return s.GrantsUsable()
}

// After a rollback every replica runs this version, and the documents a
// newer console wrote are no longer left alone for good: once no console
// of another version has written the ConfigMap for takeOverAfter, on this
// replica's clock, they are taken over. A sign-out the newer document
// holds (in a layout this version reads) stays refused, sign-outs are
// shared again and MCP grants usable. Not a second earlier; and a replica
// started later counts from its own start.
func TestARollbackTakesOverANewerDocument(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	newerState(t, api, "stolen")
	a, b := loaded(t, api, clock), loaded(t, api, clock)
	if a.SignedOut("stolen") || usable(a) {
		t.Fatal("the newer documents are read before the take-over")
	}
	// Stranded, a writes its own annotation (the newer console, were it
	// running, would see it) and leaves the documents alone.
	flushed(t, a)
	if annotation(api, writerAnnotation(formatVersion)) == "" {
		t.Fatal("a stranded replica does not show itself")
	}
	if version(t, api.data(KeySessions)) != 3 || version(t, api.data(KeyGrants)) != 3 {
		t.Fatal("rewritten before the take-over")
	}
	clock.Add(takeOverAfter - time.Second)
	watchMerge(t, api, b)
	for _, s := range []*Store{a, b} {
		s.tick()
		flushed(t, s)
	}
	if version(t, api.data(KeySessions)) != 3 || version(t, api.data(KeyGrants)) != 3 {
		t.Fatal("taken over before takeOverAfter")
	}
	// A replica started now counts from its start.
	late := loaded(t, api, clock)
	clock.Add(time.Second)
	late.tick()
	if late.SignedOut("stolen") {
		t.Fatal("a replica started late took over at once")
	}
	a.tick()
	flushed(t, a)
	for _, key := range []string{KeySessions, KeyGrants} {
		if got := version(t, api.data(key)); got != formatVersion {
			t.Fatalf("%s not taken over: version %d", key, got)
		}
		if got := testutil.ToFloat64(sharedForeign.WithLabelValues(key)); got != 0 {
			t.Errorf("%s still counted foreign: %v", key, got)
		}
	}
	if !a.SignedOut("stolen") {
		t.Error("the newer document's sign-out is lost by the take-over")
	}
	if !usable(a) {
		t.Error("MCP grants still refused after the take-over")
	}
	watchMerge(t, api, b)
	if !b.SignedOut("stolen") || !usable(b) {
		t.Error("another replica does not read the document taken over")
	}
	b.RecordSignOut("after", tag("bob"), t0.Add(time.Hour))
	flushed(t, b)
	watchMerge(t, api, a)
	if !a.SignedOut("after") {
		t.Error("sign-outs are not shared after the take-over")
	}
}

// During a rolling upgrade the newer console is running: its writes,
// heartbeats among them, change its annotation at least every
// heartbeatEvery, and the older replica never takes its documents over,
// however long the rollout lasts.
func TestALiveNewerReplicaIsNeverOverwritten(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	newerState(t, api, "s1")
	a := loaded(t, api, clock)
	sessions := api.data(KeySessions)
	for step := range 36 { // three hours
		clock.Add(5 * time.Minute)
		if step%2 == 1 {
			newerWrites(api, clock.Now(), "")
			watchMerge(t, api, a)
		}
		a.tick()
		flushed(t, a)
		if got := api.data(KeySessions); got != sessions {
			t.Fatalf("after %v a live newer replica's document was rewritten: %s", clock.Now().Sub(t0), got)
		}
	}
	if usable(a) {
		t.Error("MCP grants usable over a live newer replica's grants document")
	}
}

// The take-over is decided on the ConfigMap as read and written with its
// resourceVersion: a newer replica writing in between makes it conflict,
// and the re-read leaves the document alone.
func TestATakeOverRacingANewerWriteLosesIt(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	newerState(t, api, "s1")
	hc := &hookClient{apiServer: api}
	a := newStore(hc, clock, nil)
	watchMerge(t, api, a)
	clock.Add(takeOverAfter)
	a.tick() // taken over in memory
	a.mu.Lock()
	took := a.takeOver
	a.mu.Unlock()
	if !took {
		t.Fatal("the test needs the take-over decided")
	}
	latest := `{"version":3,"signedOut":{},"x":2}`
	hc.beforeUpdate = func() error {
		newerWrites(api, clock.Now(), latest)
		return nil
	}
	if err := a.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := api.data(KeySessions); got != latest {
		t.Fatalf("a newer replica's write was overwritten: %s", got)
	}
	if version(t, api.data(KeyGrants)) != 3 {
		t.Fatal("a newer replica's grants document was overwritten")
	}
}

// A replica that finds a newer grants document drops the grants it holds:
// the document may revoke them in a layout it does not read, and the
// take-over resets it instead of reading it, so that no replica brings a
// revoked grant back.
func TestATakeOverNeverBringsBackAGrant(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := loaded(t, api, clock)
	if err := a.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	flushed(t, a)
	b := loaded(t, api, clock)
	if _, ok := b.Grant(grantID(1)); !ok {
		t.Fatal("the test needs the grant shared")
	}
	// The newer console revokes it, in a field this version does not read.
	api.set(func(x *apiServer) {
		cm := x.cm.DeepCopy()
		var doc grantsDoc
		if err := json.Unmarshal([]byte(cm.Data[KeyGrants]), &doc); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(doc.Grants)
		cm.Data[KeyGrants] = `{"version":3,"grants":` + string(raw) + `,"codes":{},"revokedIn3":["` + grantID(1) + `"]}`
		cm.Annotations[writerAnnotation(3)] = itoa(t0.Unix())
		x.bumpLocked(cm)
	})
	watchMerge(t, api, a)
	watchMerge(t, api, b)
	clock.Add(takeOverAfter)
	a.tick()
	flushed(t, a)
	if version(t, api.data(KeyGrants)) != formatVersion {
		t.Fatal("not taken over")
	}
	watchMerge(t, api, b)
	b.InstanceSince("pod", "ok", t0)
	flushed(t, b)
	for name, s := range map[string]*Store{"a": a, "b": b} {
		if _, ok := s.Grant(grantID(1)); ok {
			t.Errorf("%s holds the grant the newer console revoked", name)
		}
	}
	if strings.Contains(api.data(KeyGrants), grantID(1)) {
		t.Errorf("the grant came back in the document: %s", api.data(KeyGrants))
	}
}

// Replicas of one version alone write no heartbeat: nothing is written
// while nothing changes. An annotation another version left long ago
// costs a few heartbeats after a start, then none.
func TestOneVersionWritesNoHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		other bool
		max   int
	}{
		{name: "alone", max: 0},
		{name: "an old annotation of another version", other: true, max: int(takeOverAfter/heartbeatEvery) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			if tc.other {
				api.cm.Annotations = map[string]string{writerAnnotation(1): "1"}
			}
			a, b := loaded(t, api, clock), loaded(t, api, clock)
			a.RecordSignOut("sid", tag("alice"), t0.Add(24*time.Hour))
			flushed(t, a)
			watchMerge(t, api, b)
			flushed(t, b)
			before := api.updates
			for range 180 { // three hours, a tick a minute
				clock.Add(time.Minute)
				for _, s := range []*Store{a, b} {
					watchMerge(t, api, s)
					s.tick()
					flushed(t, s)
				}
				if clock.Now().Sub(t0) == takeOverAfter+time.Minute {
					if n := api.updates - before; n > tc.max {
						t.Fatalf("%d heartbeats in %v, want at most %d", n, takeOverAfter, tc.max)
					}
					before = api.updates
				}
			}
			if n := api.updates - before; n != 0 && tc.other {
				t.Errorf("%d heartbeats after %v", n, takeOverAfter)
			}
			if n := api.updates - before; tc.max == 0 && n != 0 {
				t.Errorf("%d writes with nothing changing", n)
			}
		})
	}
}

// An unreadable grants document is logged once an episode, not on every
// merge, and its recovery once.
func TestAnUnreadableGrantsDocumentIsLoggedOnceAnEpisode(t *testing.T) {
	api := newAPIServer()
	var logs bytes.Buffer
	s := newStore(api, &testClock{now: t0}, slog.New(slog.NewTextHandler(&logs, nil)))
	setGrants := func(raw string) {
		api.set(func(a *apiServer) {
			cm := a.cm.DeepCopy()
			cm.Data = map[string]string{KeyGrants: raw}
			a.bumpLocked(cm)
		})
		for range 3 {
			watchMerge(t, api, s)
		}
	}
	setGrants(`{"version":2,"grants":[]}`)
	setGrants(`{"version":2,"grants":"x"}`)
	if n := strings.Count(logs.String(), "does not parse"); n != 1 {
		t.Fatalf("logged %d times in one episode, want once:\n%s", n, logs.String())
	}
	setGrants(`{"version":2,"grants":{},"codes":{}}`)
	if n := strings.Count(logs.String(), "readable again"); n != 1 {
		t.Fatalf("the recovery logged %d times, want once:\n%s", n, logs.String())
	}
	setGrants(`{"version":2,"grants":[]}`)
	if n := strings.Count(logs.String(), "does not parse"); n != 2 {
		t.Fatalf("a second episode logged %d times in all, want 2", n)
	}
}
