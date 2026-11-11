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
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// v1Sessions is a sessions document of format version 1 holding the
// sign-out of sid.
func v1Sessions(t *testing.T, sids ...string) string {
	t.Helper()
	m := map[string]signOut{}
	for _, sid := range sids {
		m[sessionHash(sid)] = signOut{Expiry: t0.Add(12 * time.Hour).Unix(), Recorded: t0.Unix(), Subject: tag("alice")}
	}
	raw, err := json.Marshal(sessionsDoc{Version: 1, SignedOut: m})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func version(t *testing.T, raw string) int {
	t.Helper()
	v, ok := documentVersion(raw)
	if !ok {
		t.Fatalf("not a document: %s", raw)
	}
	return v
}

func live(t *testing.T, stores ...*Store) {
	t.Helper()
	eventually(t, "every replica reading", func() bool {
		for _, s := range stores {
			if s.State().State != viewmodel.SourceLive {
				return false
			}
		}
		return true
	})
}

// After an upgrade, the ConfigMap still holds version-1 documents: the
// replicas read them (what they hold is kept: a sign-out, a spent code
// without marks), take them over at this version, and share again: a
// sign-out on one is refused by the other, MCP grants are usable.
func TestAnOlderVersionIsReadAndTakenOver(t *testing.T) {
	api := newAPIServer()
	codes := `{"` + codeHash("answered/1") + `":{"e":` + itoa(t0.Add(time.Hour).Unix()) + `,"u":"` + uh("alice") + `"}}`
	api.cm.Data = map[string]string{
		KeySessions: v1Sessions(t, "old-session"),
		KeyGrants:   `{"version":1,"grants":{},"codes":` + codes + `}`,
		KeySince:    `{"version":1,"facts":{}}`,
		KeyDigests:  `{"version":1,"sent":{"20261105T0700Z/webhook":` + itoa(t0.Unix()) + `}}`,
	}
	clock := &testClock{now: t0}
	a, b := newStore(api, clock, nil), newStore(api, clock, nil)
	run(t, a)
	run(t, b)
	live(t, a, b)
	if !a.SignedOut("old-session") || !b.SignedOut("old-session") {
		t.Fatal("a sign-out of a version-1 document is not refused")
	}
	if !a.CodeUsed("answered/1") {
		t.Fatal("a code of a version-1 document is not spent")
	}
	if _, ok := a.DigestSent("20261105T0700Z", "webhook"); !ok {
		t.Fatal("a delivery mark of a version-1 document is lost")
	}
	if !a.GrantsUsable() || !b.GrantsUsable() {
		t.Fatal("MCP grants refused over version-1 documents")
	}
	a.RecordSignOut("new-session", tag("bob"), t0.Add(12*time.Hour))
	eventually(t, "b refuses a's sign-out", func() bool { return b.SignedOut("new-session") })
	for _, key := range []string{KeySessions, KeyGrants, KeySince, KeyDigests} {
		eventually(t, key+" rewritten at this version", func() bool { return version(t, api.data(key)) == formatVersion })
		if got := testutil.ToFloat64(sharedForeign.WithLabelValues(key)); got != 0 {
			t.Errorf("%s counted foreign: %v", key, got)
		}
	}
	// The code's marks work on the document taken over.
	if err := a.MarkCode(t.Context(), "answered/1", "run", uh("alice")); err != nil {
		t.Fatal(err)
	}
	if !api.marked("answered/1", "run") {
		t.Error("a mark on a code read from version 1 is not written")
	}
}

// A document of a newer version than this console reads stays as it is,
// whatever this replica records; it is logged once, however often it is
// read, and counted foreign; MCP grants are refused over a newer grants
// document (a revocation in it is not known here).
func TestANewerVersionIsLeftAloneAndCounted(t *testing.T) {
	api := newAPIServer()
	newer := map[string]string{
		KeySessions: `{"version":3,"signedOut":{},"x":1}`,
		KeyGrants:   `{"version":3,"grants":{},"codes":{},"x":1}`,
	}
	api.cm.Data = map[string]string{KeySessions: newer[KeySessions], KeyGrants: newer[KeyGrants]}
	var mu sync.Mutex
	var logs bytes.Buffer
	s := newStore(api, &testClock{now: t0}, slog.New(slog.NewTextHandler(syncWriter{&mu, &logs}, nil)))
	run(t, s)
	live(t, s)
	s.RecordSignOut("sid", tag("alice"), t0.Add(time.Hour))
	s.InstanceSince("pod", "ok", t0)
	eventually(t, "the since document written", func() bool { return api.data(KeySince) != "" })
	// Several more writes, each merging the ConfigMap again.
	for i := range 3 {
		s.InstanceSince("pod", "state-"+itoa(int64(i)), t0.Add(time.Duration(i+1)*time.Second))
		time.Sleep(20 * time.Millisecond)
	}
	for key, want := range newer {
		if got := api.data(key); got != want {
			t.Errorf("a newer console's %s was rewritten: %s", key, got)
		}
		if got := testutil.ToFloat64(sharedForeign.WithLabelValues(key)); got != 1 {
			t.Errorf("%s not counted foreign: %v", key, got)
		}
	}
	if s.GrantsUsable() {
		t.Error("MCP grants usable over a newer grants document")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{KeySessions, KeyGrants} {
		if n := strings.Count(logs.String(), "document="+key); n != 1 {
			t.Errorf("%s logged %d times, want once: %s", key, n, logs.String())
		}
	}
}

// A rolling upgrade from version 1 to this one, then on to a newer one:
// the replica of this version takes over the version-1 document, keeps a
// sign-out an older replica writes meanwhile (merged, not lost), and
// leaves the document alone once a newer replica has written it.
func TestAMixedRollout(t *testing.T) {
	api := newAPIServer()
	api.cm.Data = map[string]string{KeySessions: v1Sessions(t, "s1")}
	clock := &testClock{now: t0}
	n := newStore(api, clock, nil)
	run(t, n)
	live(t, n)
	eventually(t, "taken over", func() bool { return version(t, api.data(KeySessions)) == formatVersion })
	// An older replica's write lands (it read the ConfigMap before the
	// take-over): its sign-out is merged and the document taken over again.
	api.set(func(a *apiServer) {
		cm := a.cm.DeepCopy()
		cm.Data[KeySessions] = v1Sessions(t, "s1", "s2")
		a.bumpLocked(cm)
	})
	eventually(t, "the older replica's sign-out refused", func() bool { return n.SignedOut("s2") })
	eventually(t, "taken over again", func() bool { return version(t, api.data(KeySessions)) == formatVersion })
	if !strings.Contains(api.data(KeySessions), sessionHash("s2")) {
		t.Fatal("the older replica's sign-out is not in the document taken over")
	}
	// A newer replica writes: left alone from then on.
	newer := `{"version":3,"signedOut":{}}`
	api.set(func(a *apiServer) {
		cm := a.cm.DeepCopy()
		cm.Data[KeySessions] = newer
		a.bumpLocked(cm)
	})
	eventually(t, "counted foreign", func() bool { return testutil.ToFloat64(sharedForeign.WithLabelValues(KeySessions)) == 1 })
	n.RecordSignOut("s3", tag("bob"), t0.Add(time.Hour))
	n.InstanceSince("pod", "ok", t0)
	eventually(t, "the since document written", func() bool { return api.data(KeySince) != "" })
	if got := api.data(KeySessions); got != newer {
		t.Fatalf("a newer replica's document was rewritten: %s", got)
	}
}

func itoa(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
