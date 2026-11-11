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

package mcpoauth

// Hardening of the OAuth flow: each test names the attack it closes.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// sharedView is a replica's own index that says it is shared (as one over
// STATE_CONFIGMAP does), without the ConfigMap: what another replica wrote
// has not reached it yet.
type sharedView struct{ *sharedstate.Store }

func (sharedView) GrantsShared() bool { return true }

// unusable is an index that cannot be trusted (failing for too long, a
// document it cannot read).
type unusable struct{ *sharedstate.Store }

func (unusable) GrantsUsable() bool { return false }
func (unusable) GrantsShared() bool { return true }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sharedStores are two replicas' indexes over one ConfigMap of a fake API
// server, each running its watch.
func sharedStores(t *testing.T, c *clock) (*sharedstate.Store, *sharedstate.Store) {
	t.Helper()
	cs := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a"}})
	start := func() *sharedstate.Store {
		s := sharedstate.New(sharedstate.Options{
			Client: cs.CoreV1().ConfigMaps("team-a"), Name: "console-state", Now: c.Now,
			Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
			Backoff: func(int) time.Duration { return 5 * time.Millisecond },
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = s.Run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		return s
	}
	a, b := start(), start()
	eventually(t, "both indexes reading", func() bool { return a.GrantsUsable() && b.GrantsUsable() })
	return a, b
}

func grantOfCode(t *testing.T, r *replica, code string) string {
	t.Helper()
	var ac authorizationCode
	if !r.s.open(codePrefix, purposeCode, code, &ac) {
		t.Fatal("the code does not open")
	}
	return ac.ID
}

// two replicas, two indexes, one ConfigMap.
func TestRevokedGrantRefusedOnBothReplicas(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	sa, sb := sharedStores(t, c)
	a := newReplica(t, f, sa, c, nil)
	b := newReplica(t, f, sb, c, nil)
	id, tok := a.connect(t)
	pa, err := a.authenticate(tok.Access)
	if err != nil {
		t.Fatal(err)
	}
	// B accepts it at once (the replication grace), then because its index
	// holds it.
	if _, err := b.authenticate(tok.Access); err != nil {
		t.Fatalf("replica B refuses a grant of replica A: %v", err)
	}
	eventually(t, "B to learn the grant", func() bool { _, ok := sb.Grant(pa.SessionID); return ok })

	// A code A redeemed cannot be redeemed on B once B has read the write
	// that spent it (the same write records the grant it bought).
	code := a.consent(t, authorizeQuery(id, loopback), "approve").Get("code")
	if w, _, _ := a.token(t, codeForm(id, code)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	second := grantOfCode(t, a, code)
	eventually(t, "B to learn the second grant", func() bool { _, ok := sb.Grant(second); return ok })
	if w, _, e := b.token(t, codeForm(id, code)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Errorf("code replayed on B: %d %s", w.Code, w.Body)
	}
	// That replay revoked the second grant on both.
	eventually(t, "A to learn the replay's revocation", func() bool { return sa.GrantRevoked(second) })

	if err := b.s.RevokeGrant(context.Background(), auth.Client{}, "alice", pa.SessionID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "A to refuse the grant B revoked", func() bool {
		_, err := a.authenticate(tok.Access)
		return errors.Is(err, auth.ErrBearerRevoked)
	})
	if _, err := b.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("replica B: %v", err)
	}
	if w, _, _ := a.token(t, refreshForm(id, tok.Refresh)); w.Code != http.StatusBadRequest {
		t.Errorf("refresh on A after B's revocation: %d", w.Code)
	}
}

// a grant the index does not hold is refused, not trusted on the
// token alone; a restart of an in-memory index forgets no revocation.
func TestARestartForgetsNoRevocation(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	r1 := newReplica(t, f, memoryStore(c), c, nil)
	cid, tok := r1.connect(t)
	p, err := r1.authenticate(tok.Access)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.s.RevokeGrant(context.Background(), auth.Client{}, "alice", p.SessionID); err != nil {
		t.Fatal(err)
	}
	r2 := newReplica(t, f, memoryStore(c), c, nil) // the pod restarted
	if _, err := r2.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("access token after a restart: %v", err)
	}
	c.Add(time.Second)
	if w, _, e := r2.token(t, refreshForm(cid, tok.Refresh)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Errorf("refresh token after a restart: %d %s", w.Code, w.Body)
	}
}

// on a shared index, a token whose grant has not replicated yet is
// accepted for replicationGrace after it was issued, then refused.
func TestReplicationGrace(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	a := newReplica(t, f, memoryStore(c), c, nil)
	b := newReplica(t, f, sharedView{memoryStore(c)}, c, nil)
	id, tok := a.connect(t)
	if _, err := b.authenticate(tok.Access); err != nil {
		t.Fatalf("within the grace: %v", err)
	}
	// A refresh on B within the grace records the grant there.
	c.Add(replicationGrace / 2)
	w, next, _ := b.token(t, refreshForm(id, tok.Refresh))
	if w.Code != http.StatusOK {
		t.Fatalf("refresh within the grace: %d %s", w.Code, w.Body)
	}
	c.Add(replicationGrace)
	if _, err := b.authenticate(next.Access); err != nil {
		t.Errorf("a grant B recorded: %v", err)
	}

	// A grant B never hears of is refused once the grace is over.
	_, other := a.connect(t)
	c.Add(replicationGrace + time.Second)
	if _, err := b.authenticate(other.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("past the grace: %v", err)
	}
	if w, _, _ := b.token(t, refreshForm(id, other.Refresh)); w.Code != http.StatusBadRequest {
		t.Errorf("refresh past the grace: %d", w.Code)
	}
	// An unshared index has no grace at all.
	solo := newReplica(t, f, memoryStore(c), c, nil)
	_, fresh := a.connect(t)
	if _, err := solo.authenticate(fresh.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("an unshared index trusted a grant it does not hold: %v", err)
	}
}

// while the index cannot be trusted, nothing is granted or accepted,
// and the refusal is a failure to decide (503), not "revoked".
func TestAnUnusableIndexRefusesGrants(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	store := memoryStore(c)
	a := newReplica(t, f, store, c, nil)
	id, tok := a.connect(t)
	b := newReplica(t, f, unusable{store}, c, nil)
	_, err := b.authenticate(tok.Access)
	if err == nil || errors.Is(err, mcpauth.ErrUnauthenticated) || errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("authenticate: %v", err)
	}
	if w, _, e := b.token(t, refreshForm(id, tok.Refresh)); w.Code != http.StatusServiceUnavailable || e != "temporarily_unavailable" {
		t.Errorf("refresh: %d %s", w.Code, w.Body)
	}
	form := url.Values{"request": {b.consentPage(t, authorizeQuery(b.clientID(t), loopback))}, "decision": {"approve"}}
	if w := b.answer(t, form); w.Code != http.StatusServiceUnavailable {
		t.Errorf("decision: %d", w.Code)
	}
}

// a consent page is good for one answer.
func TestOneConsentPageIsOneAnswer(t *testing.T) {
	_, _, r := setup(t)
	page := r.consentPage(t, authorizeQuery(r.clientID(t), loopback))
	form := url.Values{"request": {page}, "decision": {"approve"}}
	if w := r.answer(t, form); w.Code != http.StatusFound {
		t.Fatalf("first answer: %d %s", w.Code, w.Body)
	}
	for _, d := range []string{"approve", "deny"} {
		if w := r.answer(t, url.Values{"request": {page}, "decision": {d}}); w.Code != http.StatusConflict || w.Header().Get("Location") != "" {
			t.Errorf("second answer (%s): %d %s", d, w.Code, w.Header().Get("Location"))
		}
	}
}

// a person's consent answers are rate-limited.
func TestDecisionsAreRateLimited(t *testing.T) {
	_, _, r := setup(t)
	id := r.clientID(t)
	codes := make([]int, 0, decisionsBurst+1)
	for range decisionsBurst + 1 {
		form := url.Values{"request": {r.consentPage(t, authorizeQuery(id, loopback))}, "decision": {"deny"}}
		codes = append(codes, r.answer(t, form).Code)
	}
	if codes[0] != http.StatusFound || codes[decisionsBurst] != http.StatusTooManyRequests {
		t.Errorf("answers: %v", codes)
	}
}

// redeeming and replaying codes in a loop leaves at most
// MaxGrantsPerUser entries of that person in the index (revoked ones
// count), then refuses.
func TestReplayedCodesCannotGrowTheIndex(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	store := memoryStore(c)
	r := newReplica(t, f, store, c, nil).unthrottled()
	id := r.clientID(t)
	ids := map[string]bool{}
	refused := false
	for range 200 {
		w := r.authorize(authorizeQuery(id, loopback))
		m := consentRequestField.FindStringSubmatch(w.Body.String())
		if m == nil {
			refused = true
			break
		}
		w = r.answer(t, url.Values{"request": {m[1]}, "decision": {"approve"}})
		if w.Code != http.StatusFound {
			refused = true
			break
		}
		loc, _ := url.Parse(w.Header().Get("Location"))
		code := loc.Query().Get("code")
		ids[grantOfCode(t, r, code)] = true
		if w, _, _ := r.token(t, codeForm(id, code)); w.Code != http.StatusOK {
			refused = true
			break
		}
		if _, _, e := r.token(t, codeForm(id, code)); e != "invalid_grant" {
			t.Fatalf("replay: %q", e)
		}
	}
	kept := 0
	for gid := range ids {
		if _, ok := store.Grant(gid); ok {
			kept++
		}
	}
	if !refused || kept > sharedstate.MaxGrantsPerUser {
		t.Errorf("one person put %d entries in the index (bound %d), refused: %v", kept, sharedstate.MaxGrantsPerUser, refused)
	}
}

// a full table of spent codes is a capacity error, never a replay:
// nothing is revoked, and the same code works once there is room. One
// person's codes are bounded on their own.
func TestAFullCodeTableIsNotAReplay(t *testing.T) {
	c, _, r := setup(t)
	store := r.s.o.Store
	id := r.clientID(t)
	code := r.consent(t, authorizeQuery(id, loopback), "approve").Get("code")
	gid := grantOfCode(t, r, code)

	// Mallory's own flood stops at her bound.
	mallory, spent := r.s.userKey("mallory"), 0
	for i := range 2 * sharedstate.MaxCodesPerUser {
		if store.UseCode(fmt.Sprintf("m-%d", i), mallory, t0.Add(30*time.Second)) == nil {
			spent++
		}
	}
	if spent != sharedstate.MaxCodesPerUser {
		t.Errorf("mallory spent %d codes, bound %d", spent, sharedstate.MaxCodesPerUser)
	}
	// Many people's together fill the table: each takes what the fair
	// share lets them, until a newcomer is refused.
	full := false
	for u := 0; u < 10000 && !full; u++ {
		key := r.s.userKey(fmt.Sprintf("user-%d", u))
		full = store.UseCode(fmt.Sprintf("flood-%d-0", u), key, t0.Add(30*time.Second)) != nil
		for i := 1; i < sharedstate.MaxCodesPerUser && !full; i++ {
			if store.UseCode(fmt.Sprintf("flood-%d-%d", u, i), key, t0.Add(30*time.Second)) != nil {
				break
			}
		}
	}
	if !full {
		t.Fatal("the code table has no bound")
	}
	w, _, e := r.token(t, codeForm(id, code))
	if w.Code != http.StatusServiceUnavailable || e != "temporarily_unavailable" {
		t.Fatalf("redemption with a full table: %d %s", w.Code, w.Body)
	}
	if _, known := store.Grant(gid); known || store.GrantRevoked(gid) {
		t.Error("a capacity error revoked or recorded the grant")
	}
	c.Add(31 * time.Second)
	if w, _, _ := r.token(t, codeForm(id, code)); w.Code != http.StatusOK {
		t.Errorf("the same code once the flood expired: %d %s", w.Code, w.Body)
	}
}

// a grant is what was asked, at most what is offered, and what the
// person chose; an unknown scope is refused.
func TestScopes(t *testing.T) {
	for _, writeTools := range []bool{false, true} {
		s := &Server{o: Options{WriteTools: writeTools}}
		for requested, want := range map[string]string{
			"":                             ScopeRead,
			ScopeRead:                      ScopeRead,
			"openid profile email":         ScopeRead,
			"offline_access mcp:read":      ScopeRead,
			ScopeOperate:                   map[bool]string{false: ScopeRead, true: ScopeRead + " " + ScopeOperate}[writeTools],
			ScopeRead + " " + ScopeOperate: map[bool]string{false: ScopeRead, true: ScopeRead + " " + ScopeOperate}[writeTools],
			"mcp":                          "",
			"mcp:admin":                    "",
			"mcp:read admin":               "",
		} {
			got, err := s.grantScope(requested)
			if (want == "") != (err != nil) || got != want {
				t.Errorf("write tools %v: grantScope(%q) = %q, %v; want %q", writeTools, requested, got, err, want)
			}
		}
	}

	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	r := newReplica(t, f, memoryStore(c), c, func(o *Options) { o.WriteTools = true })
	id := r.clientID(t)
	q := authorizeQuery(id, loopback)
	q.Set("scope", ScopeRead+" "+ScopeOperate)
	if w := r.authorize(q); !strings.Contains(w.Body.String(), `value="operate"`) || !strings.Contains(w.Body.String(), `value="read" checked`) {
		t.Errorf("the consent page does not offer the choice, read first: %s", w.Body)
	} else if body := w.Body.String(); !strings.Contains(body, "a name it chose itself") || !strings.Contains(body, "could answer by itself") {
		t.Errorf("the consent page does not warn what read and operate lets the client do: %s", body)
	}
	for access, want := range map[string]string{"": ScopeRead, "read": ScopeRead, "operate": ScopeRead + " " + ScopeOperate} {
		code := r.consentWith(t, q, url.Values{"decision": {"approve"}, "access": {access}}).Get("code")
		if _, tok, _ := r.token(t, codeForm(id, code)); tok.Scope != want {
			t.Errorf("access %q: scope %q, want %q", access, tok.Scope, want)
		}
	}
	// A client that asked for read only cannot be given operate by the
	// form.
	code := r.consentWith(t, authorizeQuery(id, loopback), url.Values{"decision": {"approve"}, "access": {"operate"}}).Get("code")
	if _, tok, _ := r.token(t, codeForm(id, code)); tok.Scope != ScopeRead {
		t.Errorf("read request, operate answer: %q", tok.Scope)
	}
	// An unknown scope is the client's error, said on a page.
	q.Set("scope", "mcp:admin")
	if w := r.authorize(q); w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "(invalid_scope)") {
		t.Errorf("unknown scope: %d %s", w.Code, w.Header().Get("Location"))
	}
}

// an OIDC sign-in gives each grant its own IdP sign-in, by the same
// person; a sign-in that renews nothing carries the session's token and no
// IdP refresh token.
func TestEachGrantHasItsOwnIdPSignIn(t *testing.T) {
	c, f, r := setup(t)
	_, first := r.connect(t)
	_, second := r.connect(t)
	p1, _ := r.authenticate(first.Access)
	p2, _ := r.authenticate(second.Access)
	if p1 == nil || p2 == nil || p1.Token == p2.Token || p1.Token == "idp-alice" || f.grantLogins != 2 {
		t.Fatalf("grant tokens %v %v, %d grant sign-ins", p1, p2, f.grantLogins)
	}

	// Another person at the IdP than the one who approved: refused.
	f.mu.Lock()
	f.grantUser = "mallory"
	f.mu.Unlock()
	form := url.Values{"request": {r.consentPage(t, authorizeQuery(r.clientID(t), loopback))}, "decision": {"approve"}}
	if w := r.answer(t, form); w.Code != http.StatusForbidden || w.Header().Get("Location") != "" {
		t.Errorf("another person signed in: %d %s", w.Code, w.Header().Get("Location"))
	}
	f.mu.Lock()
	f.grantUser = ""
	f.mu.Unlock()

	// Signed out between Approve and the IdP's answer: refused.
	form = url.Values{"request": {r.consentPage(t, authorizeQuery(r.clientID(t), loopback))}, "decision": {"approve"}}
	signedOut := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathAuthorize, strings.NewReader(form.Encode()))
	signedOut.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := r.do(signedOut); w.Code != http.StatusOK {
		t.Fatalf("grant sign-in: %d", w.Code)
	}
	f.revoke("out:" + f.session.Principal.SessionID)
	if w := r.finishGrantSignIn(f.takePending()); w.Code != http.StatusForbidden || w.Header().Get("Location") != "" {
		t.Errorf("signed out before the grant sign-in completed: %d %s", w.Code, w.Header().Get("Location"))
	}
	f.mu.Lock()
	delete(f.revoked, "out:"+f.session.Principal.SessionID)
	f.mu.Unlock()

	// A round trip left half-way expires.
	form = url.Values{"request": {r.consentPage(t, authorizeQuery(r.clientID(t), loopback))}, "decision": {"approve"}}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathAuthorize, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := r.do(req); w.Code != http.StatusOK {
		t.Fatalf("grant sign-in: %d", w.Code)
	}
	pending := f.takePending()
	c.Add(pendingTTL)
	if w := r.finishGrantSignIn(pending); w.Code != http.StatusBadRequest {
		t.Errorf("an expired grant sign-in: %d", w.Code)
	}
	// A forged one does not open.
	if w := r.finishGrantSignIn("forged"); w.Code != http.StatusBadRequest {
		t.Errorf("a forged grant sign-in: %d", w.Code)
	}

	// A provider that renews nothing: no second sign-in, the session's
	// token, and the grant ends with it.
	c2 := &clock{now: t0}
	g := newSessions(t, c2, secretA)
	g.renews = false
	nr := newReplica(t, g, memoryStore(c2), c2, nil)
	_, tok := nr.connect(t)
	p, err := nr.authenticate(tok.Access)
	if err != nil || p.Token != "idp-alice" || g.grantLogins != 0 {
		t.Errorf("non-renewing: %+v %v, %d grant sign-ins", p, err, g.grantLogins)
	}
	if page := nr.authorize(authorizeQuery(nr.clientID(t), loopback)); !strings.Contains(page.Body.String(), "no longer than the token you signed in with") {
		t.Errorf("the consent page does not say the grant ends with the sign-in")
	}
}

// signing out with "disconnect my assistants" revokes every grant of
// the person.
func TestSignOutDisconnects(t *testing.T) {
	_, f, r := setup(t)
	_, a := r.connect(t)
	_, b := r.connect(t)
	if n := f.signOut(context.Background(), auth.Client{}, "bob"); n != 0 {
		t.Errorf("bob disconnected %d grants", n)
	}
	if n := f.signOut(context.Background(), auth.Client{}, "alice"); n != 2 {
		t.Errorf("alice disconnected %d grants, want 2", n)
	}
	for _, tok := range []tokens{a, b} {
		if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
			t.Errorf("after sign-out: %v", err)
		}
	}
	if n := r.s.DisconnectAll(context.Background(), auth.Client{}, "alice"); n != 0 {
		t.Errorf("a second disconnect revoked %d", n)
	}
	if gs := r.s.Grants(context.Background(), "alice"); len(gs) != 0 {
		t.Errorf("grants after disconnect: %+v", gs)
	}
}

// an anonymous caller makes the console fetch nothing and learns
// nothing about a client ID.
func TestAnonymousAuthorizeLooksAtNothing(t *testing.T) {
	d := newDocServer(t)
	c := &clock{now: t0}
	id := d.url("/client.json")
	d.serveJSON("/client.json", document(id, ""))
	f := newSessions(t, c, secretA)
	r := newReplica(t, f, memoryStore(c), c, func(o *Options) {
		o.ClientMetadataDocuments = true
		o.Fetcher = loopbackFetcher(d, c)
	})
	f.mu.Lock()
	f.session = nil
	f.mu.Unlock()
	for _, cid := range []string{id, "pgc_cl_forged", "nonsense"} {
		q := authorizeQuery(cid, loopback)
		q.Del("code_challenge") // a redirectable error, were the request read
		w := r.authorize(q)
		if loc := w.Header().Get("Location"); w.Code != http.StatusFound || !strings.HasPrefix(loc, "/auth/login?") {
			t.Errorf("%s: %d %s", cid, w.Code, loc)
		}
	}
	if d.hits.Load() != 0 {
		t.Errorf("%d documents fetched for an anonymous caller", d.hits.Load())
	}
}

// the host allow-list.
func TestFetcherAllowList(t *testing.T) {
	d := newDocServer(t)
	c := &clock{now: t0}
	id := d.url("/client.json")
	d.serveJSON("/client.json", document(id, ""))
	o := FetcherOptions{
		TLS: d.tls(), Now: c.Now, AllowedHosts: []string{"claude.ai"},
		AllowAddress: func(ip netip.Addr) bool { return ip.IsLoopback() },
	}
	if _, err := NewFetcher(o).Fetch(context.Background(), id); !errors.Is(err, errHostNotAllowed) || d.hits.Load() != 0 {
		t.Errorf("a host outside the list: %v, %d fetches", err, d.hits.Load())
	}
	o.AllowedHosts = []string{"LOCALHOST"}
	if _, err := NewFetcher(o).Fetch(context.Background(), id); err != nil {
		t.Errorf("a listed host: %v", err)
	}
}

// a caller that gives up does not poison the cache for the others.
func TestAFetchGivenUpIsNotCached(t *testing.T) {
	d := newDocServer(t)
	c := &clock{now: t0}
	id := d.url("/client.json")
	d.serveJSON("/client.json", document(id, ""))
	fe := loopbackFetcher(d, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = fe.Fetch(ctx, id)
	if _, err := fe.Fetch(context.Background(), id); err != nil {
		t.Errorf("a legitimate fetch after one given up: %v", err)
	}
	if d.hits.Load() > 1 {
		t.Errorf("%d fetches, want at most 1", d.hits.Load())
	}
}

// the consent page names the loopback port and path, and the
// Approve button arms after a moment.
func TestConsentPageShowsWhereTheCodeGoes(t *testing.T) {
	_, _, r := setup(t)
	w := r.authorize(authorizeQuery(r.clientID(t), "http://127.0.0.1:51000/callback"))
	body := w.Body.String()
	if !strings.Contains(body, "127.0.0.1:51000/callback") || !strings.Contains(body, `class="armed"`) {
		t.Errorf("consent page: %s", body)
	}
	// Enter submits the form's first button: Deny, never the
	// unarmed Approve.
	deny, approve := strings.Index(body, `value="deny"`), strings.Index(body, `value="approve"`)
	if deny < 0 || approve < 0 || deny > approve {
		t.Errorf("Deny must come first in the markup: deny at %d, approve at %d", deny, approve)
	}
}

// No redirect URI admitted by the loopback parser leaves the computer.
func TestLoopbackTricks(t *testing.T) {
	for _, raw := range []string{
		`http://127.0.0.1:80\@evil.com/`, `http://evil.com\@127.0.0.1/`, `http://127.0.0.1\.evil.com/`,
		`http://127.0.0.1%2f@evil.com/`, `http://[::ffff:127.0.0.1]/`, `http://[::1%25eth0]:1/`, `http://localhost./`,
		`http://127.0.0.1:1/cb?x=1`, `http://127.0.0.1:/cb`, `http://127.0.0.1:1//evil.com/`, `http://LOCALHOST:1/cb`,
		"http://127.0.0.1:1/cb\t", `http://127.0.0.1:1:2/`, `http://127.0.0.1:1/%2e%2e/`, `HTTP://127.0.0.1/cb`,
		`http://127.0.0.1:99999/`, `http://0x7f.1/`, `http://127.0.0.1:1#`, `http://127.0.0.1:1/?`,
		`http://localhost:1/cb`, `http://[::1]:1/cb`,
	} {
		u, err := parseLoopback(raw)
		if err != nil {
			continue
		}
		switch h := strings.ToLower(u.Hostname()); {
		case u.Scheme != "http", h != "127.0.0.1" && h != "localhost" && h != "::1", u.User != nil, u.Fragment != "",
			strings.HasPrefix(u.EscapedPath(), "//"), strings.ContainsAny(u.String(), "\\\t@"):
			t.Errorf("%q admitted as %s", raw, u)
		}
	}
}

// A declared name keeps no character that displays other than it reads.
func TestCleanNameDropsFormatCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"Claude Code":                   "Claude Code",
		"\u202eedoC edualC":             "edoC edualC",
		"Claude\u200b Code\u200d":       "Claude Code",
		"Claude\u2066 Code\u2069\u00ad": "Claude Code",
		"\u200b":                        defaultName,
		"tab\there":                     "tabhere",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
