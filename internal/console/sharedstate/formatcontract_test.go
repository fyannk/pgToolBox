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
)

// newerDocs fills api with documents of version v that keep this
// version's fields and add one, and v's annotation.
func newerDocs(t *testing.T, api *apiServer, v int, sid string) {
	t.Helper()
	m := map[string]signOut{sessionHash(sid): {Expiry: t0.Add(12 * time.Hour).Unix(), Recorded: t0.Unix(), Subject: tag("alice")}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	ver := itoa(int64(v))
	api.set(func(a *apiServer) {
		cm := a.cm.DeepCopy()
		cm.Data = map[string]string{
			KeySessions: `{"version":` + ver + `,"signedOut":` + string(raw) + `,"added":{"x":1}}`,
			KeyGrants:   `{"version":` + ver + `,"grants":{},"codes":{}}`,
		}
		cm.Annotations = map[string]string{writerAnnotation(v): itoa(t0.Unix())}
		a.bumpLocked(cm)
	})
}

// The compatibility contract of formatVersion: a newer document below
// takeOverBelow keeps this version's fields, and a take-over reads them
// (the sign-out it holds stays refused); one of takeOverBelow or more is
// never taken over, however long no other version writes, and is never
// overwritten.
func TestTheFormatContract(t *testing.T) {
	for _, tc := range []struct {
		version  int
		takeOver bool
	}{
		{version: formatVersion + 1, takeOver: true},
		{version: takeOverBelow - 1, takeOver: true},
		{version: takeOverBelow, takeOver: false},
		{version: takeOverBelow + 7, takeOver: false},
	} {
		clock := &testClock{now: t0}
		api := newAPIServer()
		newerDocs(t, api, tc.version, "stolen")
		before := api.data(KeySessions)
		a := loaded(t, api, clock)
		for range 3 {
			clock.Add(takeOverAfter)
			a.tick()
			flushed(t, a)
			watchMerge(t, api, a)
		}
		taken := version(t, api.data(KeySessions)) == formatVersion
		if taken != tc.takeOver {
			t.Fatalf("version %d: taken over %v, want %v", tc.version, taken, tc.takeOver)
		}
		if tc.takeOver && !a.SignedOut("stolen") {
			t.Errorf("version %d: the sign-out of the document taken over is lost", tc.version)
		}
		if !tc.takeOver {
			if api.data(KeySessions) != before {
				t.Errorf("version %d: a document past the contract was overwritten", tc.version)
			}
			if usable(a) {
				t.Errorf("version %d: grants usable over a grants document never taken over", tc.version)
			}
		}
	}
}

// A document of no version (`{}`, or version 0, which no console writes)
// is not left alone for good as foreign (no version would ever take it
// over): it is read as empty, rewritten at this version, and logged once.
// For grants.json that is a reset.
func TestADocumentOfNoVersionIsRewritten(t *testing.T) {
	for _, raw := range []string{`{}`, `{"version":0}`, `{"version":-1}`, `{"grants":{}}`} {
		clock := &testClock{now: t0}
		api := newAPIServer()
		api.cm.Data = map[string]string{KeySessions: raw, KeyGrants: raw, KeyDigests: raw}
		var logs bytes.Buffer
		a := newStore(api, clock, slog.New(slog.NewTextHandler(&logs, nil)))
		for range 3 {
			watchMerge(t, api, a)
		}
		a.mu.Lock()
		a.setModeLocked(modeLive, "")
		a.mu.Unlock()
		if !a.GrantsUsable() {
			t.Errorf("%s: grants refused over a document of no version", raw)
		}
		flushed(t, a)
		for _, key := range []string{KeySessions, KeyGrants, KeyDigests} {
			if got := version(t, api.data(key)); got != formatVersion {
				t.Errorf("%s: %s not rewritten: %s", raw, key, api.data(key))
			}
		}
		if n := strings.Count(logs.String(), "carries no format version"); n != 3 {
			t.Errorf("%s: logged %d times, want once a document:\n%s", raw, n, logs.String())
		}
	}
}

// An annotation of another version that has not changed for takeOverAfter
// is dropped on this replica's next write, so that a replica started
// later does not send heartbeats for a version long gone; one that
// changed recently is kept, and none is dropped while a newer document is
// left alone (the take-over's wait would restart).
func TestAStaleAnnotationOfAGoneVersionIsDropped(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	api.cm.Annotations = map[string]string{writerAnnotation(1): "1"}
	a := loaded(t, api, clock)
	clock.Add(takeOverAfter - time.Second)
	a.RecordSignOut("sid", tag("alice"), t0.Add(24*time.Hour))
	flushed(t, a)
	if annotation(api, writerAnnotation(1)) == "" {
		t.Fatal("an annotation that changed less than takeOverAfter ago was dropped")
	}
	clock.Add(time.Second)
	a.RecordSignOut("sid2", tag("alice"), t0.Add(24*time.Hour))
	flushed(t, a)
	if annotation(api, writerAnnotation(1)) != "" {
		t.Fatal("a stale annotation of a gone version was kept")
	}
	// A replica started now writes no heartbeat.
	before := api.updates
	late := loaded(t, api, clock)
	for range 60 {
		clock.Add(time.Minute)
		watchMerge(t, api, late)
		late.tick()
		flushed(t, late)
	}
	if n := api.updates - before; n != 0 {
		t.Errorf("%d heartbeats for a version gone", n)
	}

	// Never while a newer document is left alone.
	api2 := newAPIServer()
	newerDocs(t, api2, takeOverBelow, "x")
	b := loaded(t, api2, clock)
	clock.Add(2 * takeOverAfter)
	b.RecordSignOut("sid", tag("alice"), clock.Now().Add(time.Hour))
	flushed(t, b)
	if annotation(api2, writerAnnotation(takeOverBelow)) == "" {
		t.Error("the annotation of a version whose document is left alone was dropped")
	}
}
