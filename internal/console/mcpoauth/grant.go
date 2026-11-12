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

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// Token prefixes and sealing purposes. A prefix lets the MCP guard's
// chain tell a console grant from a raw API-server token without trying
// to open it; the purpose (its own key, and the AEAD's associated data)
// makes a value of one kind fail to open as another.
const (
	accessPrefix   = "pgc_at_"
	refreshPrefix  = "pgc_rt_"
	codePrefix     = "pgc_ac_"
	purposeAccess  = "mcp-access"
	purposeRefresh = "mcp-refresh"
	purposeCode    = "mcp-code"
	purposeConsent = "mcp-consent"
	purposePending = "mcp-pending"
)

// upstream is the person's IdP token as a grant carries it: the token, its
// refresh token (the grant's own, never the browser session's) and expiry
// (Unix seconds, 0 unknown).
type upstream struct {
	Provider string `json:"prv"`
	Token    string `json:"tok"`
	Refresh  string `json:"rft,omitempty"`
	Expiry   int64  `json:"tex,omitempty"`
}

// grantBase is what every token of one grant carries.
type grantBase struct {
	// ID is the grant reference (grant-<32 hex>).
	ID string `json:"gid"`
	// Client is the client ID; Name its declared name.
	Client string `json:"cid"`
	Name   string `json:"cn"`
	Scope  string `json:"scp"`
	// Resource is the audience: this console's /mcp (RFC 8707).
	Resource string `json:"aud"`
	// Subject is the username the API server reported at consent.
	Subject string `json:"sub"`
	// Session is the console session the consent was given in: the
	// revocation list's sessions key naming it ends the grant too.
	Session string `json:"sid"`
	// IssuedAt is the consent time (Unix seconds): the revocation list's
	// users and notBefore compare it.
	IssuedAt int64 `json:"iat"`
	// RedirectHost is the host the consent sent the code to (the client's
	// callback): the write tools' confirmation page names it beside the
	// declared name, which any client can claim. Empty in tokens issued
	// before it was recorded.
	RedirectHost string `json:"rh,omitempty"`
}

// accessToken is the sealed payload of an access token. It carries the
// IdP token but not its refresh token: renewing the IdP token is the
// token endpoint's job, where the renewed tokens can be handed back.
type accessToken struct {
	grantBase
	Upstream upstream `json:"up"`
	Expiry   int64    `json:"exp"`
	// TokenIssued is when this token was issued: the replication grace
	// runs from it.
	TokenIssued int64 `json:"tiat"`
}

// refreshToken is the sealed payload of a refresh token.
type refreshToken struct {
	grantBase
	Upstream upstream `json:"up"`
	// Expiry is the consent time plus the session's maximum age; no
	// refresh extends it.
	Expiry int64 `json:"exp"`
	// Generation must not be behind the grant index's: a refresh token is
	// good for one use.
	Generation  int   `json:"gen"`
	TokenIssued int64 `json:"tiat"`
}

// authorizationCode is the sealed payload of a code.
type authorizationCode struct {
	grantBase
	Upstream    upstream `json:"up"`
	Nonce       string   `json:"jti"`
	RedirectURI string   `json:"ru"`
	Challenge   string   `json:"pkce"`
	Expiry      int64    `json:"exp"`
}

// consentRequest is the sealed hidden field of the consent page.
type consentRequest struct {
	Session     string `json:"sid"`
	Subject     string `json:"sub"`
	Client      string `json:"cid"`
	Name        string `json:"cn"`
	ClientURL   string `json:"cu,omitempty"`
	RedirectURI string `json:"ru"`
	Challenge   string `json:"pkce"`
	State       string `json:"st"`
	Resource    string `json:"aud"`
	// Scope is the most the page offers; the person may choose less.
	Scope string `json:"scp"`
	// Nonce makes one page good for one answer.
	Nonce  string `json:"jti"`
	Expiry int64  `json:"exp"`
}

// pendingGrant is an approved consent waiting for its own IdP sign-in:
// the login-flow cookie carries it through the identity provider.
type pendingGrant struct {
	grantBase
	// Provider is the sign-in provider of the consent's session.
	Provider    string `json:"prv"`
	RedirectURI string `json:"ru"`
	Challenge   string `json:"pkce"`
	State       string `json:"st"`
	Expiry      int64  `json:"exp"`
}

func (s *Server) seal(prefix, purpose string, v any) (string, error) {
	sealed, err := s.codec.SealPurpose(purpose, v)
	if err != nil {
		return "", err
	}
	return prefix + sealed, nil
}

func (s *Server) open(prefix, purpose, tok string, v any) bool {
	rest, ok := strings.CutPrefix(tok, prefix)
	return ok && s.codec.OpenPurpose(purpose, rest, v) == nil
}

// errIndexUnavailable: the grant index cannot be trusted to refuse what
// was revoked (the shared state not read, or not written for a while):
// grants fail with 503 until it can.
var errIndexUnavailable = errors.New("the console's grant index is not available; retry shortly")

// Authenticate is the MCP guard's grant authenticator, first in the
// chain. Any bearer with this server's prefix is judged here, never
// passed on: a console grant's access token, opened, unexpired, for this
// resource, of a grant the index holds (or one issued within the
// replication grace) and has not revoked, not revoked by the revocation
// list (the grant, the session it came from, the user, notBefore), whose
// IdP token the API server still accepts for the same person. Anything
// else is ErrNotMine, for the next authenticator (raw API-server tokens).
func (s *Server) Authenticate(ctx context.Context, token string) (*auth.Principal, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return nil, mcpauth.ErrNotMine
	}
	var at accessToken
	if !s.open(accessPrefix, purposeAccess, token, &at) || at.ID == "" || at.Upstream.Token == "" {
		return nil, fmt.Errorf("%w: not an access token of this console", mcpauth.ErrUnauthenticated)
	}
	now := s.o.Now()
	if now.Unix() >= at.Expiry {
		// The client refreshes: the guard answers 401 invalid_token.
		return nil, auth.ErrBearerExpired
	}
	if want, ok := mcpauth.ResourceFrom(ctx); ok && !sameResource(at.Resource, want) {
		return nil, fmt.Errorf("%w: issued for another resource", mcpauth.ErrUnauthenticated)
	}
	if !s.o.Store.GrantsUsable() {
		return nil, errIndexUnavailable
	}
	issued := time.Unix(at.IssuedAt, 0).UTC()
	g, known := s.o.Store.Grant(at.ID)
	switch {
	case known && !g.Revoked.IsZero():
		return nil, auth.ErrBearerRevoked
	case !known && !s.withinGrace(at.TokenIssued, now):
		// Revoked and forgotten, or never recorded (a restart of an index
		// kept in memory): refused, never trusted on the token alone.
		return nil, fmt.Errorf("%w: the grant is not known to this console", auth.ErrBearerRevoked)
	case s.o.Sessions.Revoked(at.Subject, issued, at.ID, at.Session):
		return nil, auth.ErrBearerRevoked
	}
	u, err := s.o.Sessions.Review(ctx, at.Upstream.Token)
	if err != nil {
		return nil, err
	}
	if u.Name != at.Subject {
		return nil, fmt.Errorf("%w: the token now names another person", mcpauth.ErrUnauthenticated)
	}
	if known {
		s.touch(at.ID, now)
	}
	exp := time.Unix(at.Expiry, 0).UTC()
	p := &auth.Principal{
		User: u, Provider: auth.GrantProvider, SessionID: at.ID, Token: at.Upstream.Token,
		Expiry: exp, AbsoluteExpiry: exp, IssuedAt: issued, Scopes: strings.Fields(at.Scope), Assistant: at.Name,
		AssistantRedirect: at.RedirectHost,
	}
	if at.Upstream.Expiry > 0 {
		p.TokenExpiry = time.Unix(at.Upstream.Expiry, 0).UTC()
	}
	return p, nil
}

// touch records a use in the grant index, at most every touchEvery per
// grant and replica.
func (s *Server) touch(id string, now time.Time) {
	s.mu.Lock()
	last, ok := s.touched[id]
	if ok && now.Sub(last) < touchEvery {
		s.mu.Unlock()
		return
	}
	if len(s.touched) >= 4*sharedstate.MaxGrants {
		clear(s.touched)
	}
	s.touched[id] = now
	s.mu.Unlock()
	s.o.Store.TouchGrant(id, now)
}

// GrantInfo is one of a person's grants, for the session page.
type GrantInfo struct {
	ID       string
	Client   string
	Scopes   []string
	Created  time.Time
	LastUsed time.Time
	Expires  time.Time
}

// Grants lists the person's standing grants, newest first.
func (s *Server) Grants(_ context.Context, user string) []GrantInfo {
	gs := make([]sharedstate.Grant, 0, sharedstate.MaxGrantsPerUser)
	for _, key := range s.userKeys(user) {
		gs = append(gs, s.o.Store.GrantsOf(key)...)
	}
	slices.SortStableFunc(gs, func(a, b sharedstate.Grant) int { return b.Created.Compare(a.Created) })
	out := make([]GrantInfo, 0, len(gs))
	for _, g := range gs {
		out = append(out, GrantInfo{ID: g.ID, Client: g.Client, Scopes: strings.Fields(g.Scope), Created: g.Created, LastUsed: g.LastUsed, Expires: g.Expiry})
	}
	return out
}

// ErrNoSuchGrant means no standing grant of this person has that ID.
var ErrNoSuchGrant = errors.New("no such grant")

// ErrNotRecorded means a revocation was made on this replica but could not
// be written to the shared state: this replica refuses the grant, the
// others may not until it is written (the next batch tries again). The
// person is told to retry.
var ErrNotRecorded = errors.New("the disconnection could not be recorded for every replica; retry")

// ErrNotConfirmed means the shared state could not be read (or this
// replica would not read it now: too many disconnections asked at once),
// so the disconnection is not confirmed: grants recorded on another
// replica may still be served, none may have been revoked. The person is
// told so, and to retry. Some grants this replica knew may have been
// revoked (and written) meanwhile.
var ErrNotConfirmed = errors.New("the disconnection could not be confirmed: the shared state could not be read; retry")

// errReadThrottled is a read of the shared state this replica refused to
// make (readGrants).
var errReadThrottled = errors.New("too many disconnections asked at once")

// readGrants is the read of the shared index a disconnection makes,
// charged to the person's bucket, then the replica's: nil once the index
// holds the shared state as the API server answered it, ErrNotConfirmed
// otherwise.
func (s *Server) readGrants(ctx context.Context, user string) error {
	if !s.subjectGrantReads.Allow(user) || !s.grantReads.Allow(processKey) {
		requests.WithLabelValues("revoke", "throttled").Inc()
		return fmt.Errorf("%w: %w", ErrNotConfirmed, errReadThrottled)
	}
	if err := s.o.Store.ReadGrants(ctx); err != nil {
		requests.WithLabelValues("revoke", "unconfirmed").Inc()
		return fmt.Errorf("%w: %w", ErrNotConfirmed, err)
	}
	return nil
}

// RevokeGrant revokes one of the person's grants, written through to the
// shared state before it returns nil: every replica refuses its tokens
// within about a second. ErrNotRecorded when the write cannot be made (the
// revocation stays on this replica, and is written with its next batch).
//
// A grant the index does not know is looked for again in the ConfigMap as
// the API server holds it now (ReadGrants): this replica's watch may not
// have delivered it yet, or may have ended (a replica cut off from the API
// server still takes requests), and the page shows ErrNoSuchGrant as the
// assistant disconnected. ErrNoSuchGrant is so an answer only once that
// read shows no such grant of the person (or the ConfigMap is missing,
// and every replica refuses every grant); a read that fails, or is
// refused (readGrants), is ErrNotConfirmed (round 21, security P2-1). An
// ID that is no grant reference is ErrNoSuchGrant at once, with no read.
// Every grant an assistant holds was written to the ConfigMap before its
// first token (redeemCode), so the read shows every one there is.
func (s *Server) RevokeGrant(ctx context.Context, c auth.Client, user, id string) error {
	if !sharedstate.ValidGrantID(id) {
		return ErrNoSuchGrant
	}
	g, ok := s.o.Store.Grant(id)
	if !ok || !slices.Contains(s.userKeys(user), g.UserHash) {
		if err := s.readGrants(ctx, user); err != nil {
			return err
		}
		g, ok = s.o.Store.Grant(id)
		if !ok || !slices.Contains(s.userKeys(user), g.UserHash) {
			return ErrNoSuchGrant
		}
	}
	if g.Revoked.IsZero() {
		now := s.o.Now().UTC()
		s.o.Store.RevokeGrant(id, g.UserHash, now, g.Expiry)
		requests.WithLabelValues("revoke", "ok").Inc()
		s.audit.Record(ctx, c, auth.AuditMCPRevoke, "user", user, "grant", id, "client", g.Client)
	} else if !slices.Contains(s.o.Store.UnwrittenRevocationsOf(g.UserHash), id) {
		// Revoked, and written: nothing left of it to disconnect.
		return ErrNoSuchGrant
	}
	// A retry after ErrNotRecorded finds the grant revoked here, not
	// written yet: it writes again.
	return s.written(ctx, s.o.Store.WriteRevocations(ctx, id), 1)
}

// written is the outcome of a revocation's write-through. While the
// shared state ConfigMap does not exist, every replica refuses every
// grant (each one's watch delivers the deletion), so the disconnection is
// in effect everywhere: it is answered done, logged, and written once the
// ConfigMap is back (sharedstate.ErrNoConfigMap). A ConfigMap the console
// may not read or write is not answered done: the API server authorizes a
// watch only when it opens, so a replica whose watch was opened before the
// permission was withdrawn still serves grants until its next freshness
// read is refused (sharedstate.DefaultFreshness), so that stays
// ErrNotRecorded, whatever this replica read before.
func (s *Server) written(ctx context.Context, err error, grants int) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sharedstate.ErrNoConfigMap):
		requests.WithLabelValues("revoke", "held").Inc()
		s.o.Log.WarnContext(ctx, "MCP: a disconnection was made while the shared state ConfigMap does not exist; every replica "+
			"refuses every grant until it does, and this replica writes the revocation to it then (lost if it restarts first)",
			slog.Int("grants", grants))
		return nil
	}
	requests.WithLabelValues("revoke", "unrecorded").Inc()
	return fmt.Errorf("%w: %w", ErrNotRecorded, err)
}

// DisconnectAll revokes every standing grant of the person, written
// through to the shared state, and returns how many ("disconnect all" on
// the session page). The ConfigMap is read first (readGrants), so that
// the grants recorded on another replica that this one's watch has not
// delivered, or never will (a replica cut off from the API server still
// takes requests), are revoked too: every grant an assistant holds was
// written there before its first token (redeemCode), so nil means none
// of the person's grants is served by any replica once each has merged
// the revocations (about a second). ErrNotConfirmed whenever that read
// fails or is refused: the grants it would have shown are not revoked,
// whatever the write of the others does (round 21, security P2-1); the
// ones this replica knows are revoked here either way. ErrNotRecorded,
// as RevokeGrant, when the read worked and the write did not.
func (s *Server) DisconnectAll(ctx context.Context, c auth.Client, user string) (int, error) {
	read := s.readGrants(ctx, user)
	ids := s.revokeAll(ctx, c, user)
	n := len(ids)
	// A retry after ErrNotRecorded writes again what the first attempt
	// revoked here.
	for _, key := range s.userKeys(user) {
		ids = append(ids, s.o.Store.UnwrittenRevocationsOf(key)...)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		// Nothing to revoke in the ConfigMap as the API server holds it,
		// or no answer.
		return 0, read
	}
	err := s.written(ctx, s.o.Store.WriteRevocations(ctx, ids...), len(ids))
	if read != nil {
		return n, read
	}
	return n, err
}

// disconnectAll is DisconnectAll at a sign-out that asked to disconnect
// the person's assistants: the sign-out goes on whatever the outcome,
// which the signed-out page shows (nil: disconnected everywhere). One not
// confirmed or not written is logged; a revocation made here is written
// with the next batch, but a grant recorded on another replica that this
// one could not read is not revoked at all: the person is told to use
// Disconnect all.
func (s *Server) disconnectAll(ctx context.Context, c auth.Client, user string) error {
	n, err := s.DisconnectAll(ctx, c, user)
	switch {
	case errors.Is(err, ErrNotConfirmed):
		s.o.Log.WarnContext(ctx, "MCP: the disconnection at a sign-out could not read the shared state; grants recorded on "+
			"another replica may still be served, until the person disconnects them again", slog.Int("grants", n))
	case err != nil:
		s.o.Log.WarnContext(ctx, "MCP: the disconnection at a sign-out could not be written to the shared state yet; "+
			"this replica refuses the grants it revoked, the others once its next write lands", slog.Int("grants", n))
	}
	return err
}

// revokeAll revokes the person's standing grants on this replica and
// returns their IDs.
func (s *Server) revokeAll(ctx context.Context, c auth.Client, user string) []string {
	now := s.o.Now().UTC()
	var all []string
	for _, key := range s.userKeys(user) {
		ids := s.o.Store.RevokeGrantsOf(key, now)
		for _, id := range ids {
			requests.WithLabelValues("revoke", "ok").Inc()
			s.audit.Record(ctx, c, auth.AuditMCPRevoke, "user", user, "grant", id, "reason", "disconnect all")
		}
		all = append(all, ids...)
	}
	return all
}
