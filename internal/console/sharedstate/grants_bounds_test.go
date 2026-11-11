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
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// a few identities, each at its own bound, cannot fill the totals
// for everyone else (32 people at the former bounds, 32 grants and 64
// codes each, filled the tables of 1000 for hours).
func TestAFewPeopleCannotFillTheIndex(t *testing.T) {
	const few = 50
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	n := 0
	for p := range few {
		user := fmt.Sprint("hoarder", p)
		for s.AddGrant(testGrant(n, user, clock.Now())) == nil {
			n++
		}
		for i := 0; s.UseCode(fmt.Sprint(user, "/", i), uh(user), t0.Add(time.Minute)) == nil; i++ {
			if i > MaxCodesPerUser {
				t.Fatal("one person spent past the per-person bound")
			}
		}
	}
	if n != few*MaxGrantsPerUser {
		t.Fatalf("%d people hold %d grants, want %d each", few, n, MaxGrantsPerUser)
	}
	if err := s.AddGrant(testGrant(n, "newcomer", clock.Now())); err != nil {
		t.Errorf("%d people at their bound refused a newcomer's grant: %v", few, err)
	}
	if err := s.UseCode("newcomer/1", uh("newcomer"), t0.Add(time.Minute)); err != nil {
		t.Errorf("%d people at their bound refused a newcomer's sign-in: %v", few, err)
	}
}

// The worst full index (every grant with the costliest client name a
// client can declare, every code a confirmation approved and run: the
// marks MCP writes put on it) fits the budget with room for the other
// documents.
func TestAFullIndexFitsTheBudget(t *testing.T) {
	name := TruncateClientName(strings.Repeat("<& \"\\\x01é", 100))
	if !validClientName(name) || name == "" {
		t.Fatalf("truncated name %q is not valid", name)
	}
	if enc, _ := json.Marshal(name); len(enc) > maxClientName+2 {
		t.Fatalf("a client name encodes to %d bytes, bound %d", len(enc), maxClientName)
	}
	s := New(Options{Now: (&testClock{now: t0}).Now})
	for i := range MaxGrants {
		s.grants[grantID(i)] = grantEntry{
			User: uh(fmt.Sprint("user", i)), Client: name, Scope: strings.Repeat("s", maxScope),
			Created: t0.Unix(), Used: t0.Unix(), Expiry: t0.Unix(), Gen: 999999, Revoked: t0.Unix(),
		}
	}
	for i := range maxCodes {
		sum := sha256.Sum256([]byte(strconv.Itoa(i)))
		s.codes[hex.EncodeToString(sum[:16])] = codeEntry{Expiry: t0.Unix(), User: uh("x"), Marks: []string{"approve", "run"}}
	}
	size := len(s.encodeLocked()[KeyGrants])
	t.Logf("a full grants document: %d bytes (%d grants, %d codes)", size, MaxGrants, maxCodes)
	if size > 680<<10 {
		t.Errorf("a full grants document is %d bytes, more than 680 KiB of the %d budget", size, DefaultBudget)
	}
}

func TestClientNames(t *testing.T) {
	for in, want := range map[string]string{
		"Claude Code":                  "Claude Code",
		"a\x00b\nc":                    "abc",
		"bad\xffutf8":                  "badutf8",
		strings.Repeat("é", 100):       strings.Repeat("é", 64),
		strings.Repeat("<", 30):        strings.Repeat("<", 21),
		strings.Repeat("a", 127) + "é": strings.Repeat("a", 127),
	} {
		if got := TruncateClientName(in); got != want || !validClientName(got) {
			t.Errorf("TruncateClientName(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"a\x00", strings.Repeat("<", 22), "\xff"} {
		if validClientName(bad) {
			t.Errorf("validClientName(%q)", bad)
		}
	}
}
