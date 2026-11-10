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

package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// taggedSignOuts records what is shared, with the person's tag.
type taggedSignOuts struct {
	mu   sync.Mutex
	tags map[string]string
}

func (s *taggedSignOuts) RecordSignOut(sid, tag string, _ time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags[sid] = tag
	return true
}

func (s *taggedSignOuts) SignedOut(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tags[sid]
	return ok
}

func (s *taggedSignOuts) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.tags))
	for k, v := range s.tags {
		out[k] = v
	}
	return out
}

// sessionOf opens the browser's session cookie.
func sessionOf(t *testing.T, b *browser) Session {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://c.example/", nil)
	for _, c := range b.jar {
		r.AddCookie(c)
	}
	s, err := b.env.codec.ReadSession(r)
	if err != nil {
		t.Fatalf("the session does not open: %v", err)
	}
	return s
}

func logout(t *testing.T, b *browser) {
	t.Helper()
	if w := b.do(http.MethodPost, "/logout", url.Values{}, sameOriginForm); w.Code != http.StatusOK {
		t.Fatalf("sign-out = %d", w.Code)
	}
}

// A sign-out is counted against the person sealed in the session at
// sign-in, not against what the API server says at sign-out: a session
// whose review fails at sign-out used to be shared naming nobody, which
// skipped the per-person bound of the state ConfigMap (round 7).
func TestASignOutNamesThePersonSealedAtSignIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		provider string
		signIn   func(*testing.T, *browser)
		// fail makes the review at sign-out fail.
		fail func(*testEnv)
		// shared: the sign-out reaches the state ConfigMap.
		shared bool
	}{
		{
			name: "API server unreachable", provider: ProviderToken, signIn: signIn,
			fail:   func(e *testEnv) { e.rev.mu.Lock(); e.rev.down = true; e.rev.mu.Unlock() },
			shared: true,
		},
		{
			name: "refreshable session, token rejected", provider: ProviderOIDC,
			signIn: func(t *testing.T, b *browser) {
				t.Helper()
				if w := b.oidcLogin("/clusters"); w.Code != http.StatusFound {
					t.Fatalf("OIDC sign-in = %d %s", w.Code, w.Body)
				}
			},
			fail:   func(e *testEnv) { e.rev.mu.Lock(); delete(e.rev.users, "eyJ*"); e.rev.mu.Unlock() },
			shared: true,
		},
		{
			// Dead on every replica already: sharing it would only spend
			// the ConfigMap's bounds.
			name: "token rejected, nothing to renew it", provider: ProviderToken, signIn: signIn,
			fail:   func(e *testEnv) { e.rev.mu.Lock(); delete(e.rev.users, "sa-token"); e.rev.mu.Unlock() },
			shared: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t, tc.provider)
			shared := &taggedSignOuts{tags: map[string]string{}}
			env.h.signOuts = shared
			b := env.browser(t)
			tc.signIn(t, b)
			s := sessionOf(t, b)
			if len(s.SubjectTag) != 32 {
				t.Fatalf("the session seals no person's tag: %q", s.SubjectTag)
			}
			tc.fail(env)
			logout(t, b)
			if !env.h.SignedOut(s.ID) || !env.h.SignedOut(BearerReference(s.Token)) {
				t.Fatal("the session is not refused on its own replica")
			}
			got := shared.snapshot()
			if !tc.shared {
				if len(got) != 0 {
					t.Fatalf("a dead session's sign-out was shared: %v", got)
				}
				return
			}
			if len(got) != 2 || got[s.ID] != s.SubjectTag || got[BearerReference(s.Token)] != s.SubjectTag {
				t.Fatalf("shared %v, want the session and its bearer under %q", got, s.SubjectTag)
			}
		})
	}
}

// The tag is keyed by the session secret: the same person gets the same
// tag on every sign-in (so their sign-outs group), and the tag is not a
// plain hash of the name anyone could recompute.
func TestTheSubjectTagIsKeyed(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	a, b := env.browser(t), env.browser(t)
	signIn(t, a)
	signIn(t, b)
	ta, tb := sessionOf(t, a).SubjectTag, sessionOf(t, b).SubjectTag
	if ta == "" || ta != tb {
		t.Fatalf("one person, two tags: %q %q", ta, tb)
	}
	other := testCodec(t, true)
	other2, err := NewCodec([][]byte{[]byte("another-session-secret-of-32-bytes!")}, true, other.Limits())
	if err != nil {
		t.Fatal(err)
	}
	if k := other2.KeyedHashes(SignOutSubjectPurpose, "system:serviceaccount:team-a:pgtoolbox-console-breakglass"); len(k) == 0 || k[0] == ta {
		t.Fatal("the tag does not depend on the session secret")
	}
}

// This replica's own table of signed-out sessions is bounded: a person
// past their share forgets their own earliest-ending sign-out, the
// sessions naming nobody share one bound, and a full table forgets the
// sign-outs that end first.
func TestLoggedOutIsBounded(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	h := env.h
	now := time.Now()
	h.markLoggedOut("victim", "bob-tag", now.Add(12*time.Hour), false)
	for i := range 3 * maxLoggedOutPerSubject {
		h.markLoggedOut(fmt.Sprint("m-", i), "mallory-tag", now.Add(time.Hour+time.Duration(i)*time.Second), false)
	}
	for i := range 2 * maxLoggedOutUnnamed {
		h.markLoggedOut(fmt.Sprint("u-", i), "", now.Add(time.Hour+time.Duration(i)*time.Second), false)
	}
	count := func(tag string) int {
		h.mu.Lock()
		defer h.mu.Unlock()
		n := 0
		for _, e := range h.loggedOut {
			if e.tag == tag {
				n++
			}
		}
		return n
	}
	if n := count("mallory-tag"); n != maxLoggedOutPerSubject {
		t.Errorf("one person holds %d entries, want %d", n, maxLoggedOutPerSubject)
	}
	if n := count(""); n != maxLoggedOutUnnamed {
		t.Errorf("the unnamed hold %d entries, want %d", n, maxLoggedOutUnnamed)
	}
	if !h.SignedOut("victim") || !h.SignedOut(fmt.Sprint("m-", 3*maxLoggedOutPerSubject-1)) || h.SignedOut("m-0") {
		t.Error("a person's bound must forget their own earliest-ending sign-outs only")
	}
	h.mu.Lock()
	h.loggedOutMax = 100
	h.mu.Unlock()
	for i := range 300 {
		h.markLoggedOut(fmt.Sprint("p-", i), fmt.Sprint("tag-", i), now.Add(2*time.Hour+time.Duration(i)*time.Second), false)
	}
	h.mu.Lock()
	n := len(h.loggedOut)
	h.mu.Unlock()
	if n > 100 {
		t.Errorf("the table holds %d entries past its bound of 100", n)
	}
	if !h.SignedOut("victim") || !h.SignedOut("p-299") {
		t.Error("a full table must forget the sign-outs that end first")
	}
}
