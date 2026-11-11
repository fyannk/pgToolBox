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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// A sign-out read back with a recording time far in this replica's
// future (a clock running ahead, or an entry written by hand) still
// revokes its session, but its time is ignored: it cannot stand first in
// every bound's order for as long as its clock says. Within the skew, the
// time is kept.
func TestAFutureRecordingTimeIsIgnored(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	far, near := sessionHash("far"), sessionHash("near")
	until := t0.Add(12 * time.Hour).Unix()
	raw, err := json.Marshal(sessionsDoc{Version: formatVersion, SignedOut: map[string]signOut{
		far:  {Expiry: until, Recorded: t0.Add(maxRecordedSkew + time.Second).Unix(), Subject: tag("mallory")},
		near: {Expiry: until, Recorded: t0.Add(maxRecordedSkew).Unix(), Subject: tag("alice")},
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
	if gotFar.Recorded != 0 {
		t.Errorf("a recording time %s ahead was kept: %d", maxRecordedSkew+time.Second, gotFar.Recorded)
	}
	if gotNear.Recorded != t0.Add(maxRecordedSkew).Unix() {
		t.Errorf("a recording time within the skew was changed: %d", gotNear.Recorded)
	}
	// The local entry (recorded now) comes before the future one.
	s.RecordSignOut("bob", tag("bob"), t0.Add(12*time.Hour))
	s.mu.Lock()
	defer s.mu.Unlock()
	bob := sessionHash("bob")
	if !slotBefore(bob, s.signedOut[bob], far, s.signedOut[far]) {
		t.Error("an entry recorded now does not come before one recorded in the future")
	}
}
