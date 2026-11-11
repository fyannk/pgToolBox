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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Session is the sealed payload of the session cookie,
// AES-GCM(provider, token, refresh?, exp). It carries the user's
// apiserver token and the bookkeeping needed to expire it; it never
// carries a username, a group or a permission, which are re-read from the
// API server (SubjectTag is a keyed hash of the name, for bookkeeping
// only, never for a decision).
type Session struct {
	// ID is a random reference to this session, printed in audit lines so
	// an admin can revoke exactly this session. It is not a credential.
	ID string `json:"sid"`
	// Provider names the provider that minted the session.
	Provider string `json:"prv"`
	// Token is the bearer token the API server accepts for the user.
	Token string `json:"tok"`
	// RefreshToken is the OIDC refresh token, when the IdP issued one.
	RefreshToken string `json:"rft,omitempty"`
	// TokenExpiry is when Token stops being accepted (Unix seconds); 0
	// when unknown (an opaque pasted token).
	TokenExpiry int64 `json:"tex,omitempty"`
	// IssuedAt is the sign-in time, LastSeen the last request that
	// refreshed the idle timer (Unix seconds).
	IssuedAt int64 `json:"iat"`
	LastSeen int64 `json:"lst"`
	// SubjectTag is the keyed hash (Codec.KeyedHashes, SignOutSubjectPurpose)
	// of the name the API server reported at sign-in: the person a
	// sign-out is counted against in the state ConfigMap's bounds, sealed
	// so that a token revoked before signing out still names its owner.
	// It authorizes nothing.
	SubjectTag string `json:"sbt,omitempty"`
}

// SignOutSubjectPurpose keys Session.SubjectTag.
const SignOutSubjectPurpose = "signout-subject"

// newSessionID returns 128 random bits, hex encoded.
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("crypto/rand unavailable: %w", err))
	}
	return hex.EncodeToString(b)
}

// Limits is the lifetime policy applied to a session.
type Limits struct {
	MaxAge      time.Duration
	IdleTimeout time.Duration
}

// Expiry is when the session ends unless the user is active again or the
// token is refreshed: the earliest of the absolute bound, the idle bound
// and, without a refresh token, the token's own expiry.
func (s Session) Expiry(l Limits) time.Time {
	end := time.Unix(s.IssuedAt, 0).Add(l.MaxAge)
	if idle := time.Unix(s.LastSeen, 0).Add(l.IdleTimeout); idle.Before(end) {
		end = idle
	}
	if s.TokenExpiry > 0 && s.RefreshToken == "" {
		if tok := time.Unix(s.TokenExpiry, 0); tok.Before(end) {
			end = tok
		}
	}
	return end.UTC()
}

// AbsoluteExpiry is the MaxAge bound alone, which no activity extends.
func (s Session) AbsoluteExpiry(l Limits) time.Time {
	return time.Unix(s.IssuedAt, 0).Add(l.MaxAge).UTC()
}

// check reports why s is not usable at now, or nil.
func (s Session) check(now time.Time, l Limits) error {
	switch {
	case s.ID == "" || s.Token == "" || s.Provider == "" || s.IssuedAt == 0:
		return errors.New("session is incomplete")
	case s.IssuedAt > now.Add(time.Minute).Unix():
		return errors.New("session is issued in the future")
	case !now.Before(s.Expiry(l)):
		return errSessionExpired
	}
	return nil
}

var errSessionExpired = errors.New("session expired")

// Cookie layout. The session cookie is split into chunks because a sealed
// OIDC ID token plus its refresh token can exceed the 4 KiB a browser
// keeps per cookie. Two chunks bound the Cookie header at about 7 KiB,
// under the 8 KiB header line most ingress controllers accept.
const (
	maxChunkBytes = 3500
	maxChunks     = 2
	// loginFlowMaxAge bounds the OAuth round trip.
	loginFlowMaxAge = 10 * time.Minute
)

// cookieNames are the cookie names for one deployment. With Secure the
// __Host- prefix makes the browser refuse a cookie set by a sibling host
// or for a sub-path: on OpenShift every Route under *.apps.<domain> is the
// same site, and only the prefix stops a neighbour from planting one.
type cookieNames struct {
	session string
	login   string
}

func namesFor(secure bool) cookieNames {
	if secure {
		return cookieNames{session: "__Host-pgtoolbox-console", login: "__Host-pgtoolbox-console-login"}
	}
	return cookieNames{session: "pgtoolbox-console", login: "pgtoolbox-console-login"}
}

func (n cookieNames) chunk(i int) string { return n.session + "-" + strconv.Itoa(i) }

// Codec reads and writes the session and login-flow cookies.
type Codec struct {
	sealer *sealer
	names  cookieNames
	secure bool
	limits Limits
	now    func() time.Time
}

// NewCodec builds the cookie codec from the session secrets.
func NewCodec(secrets [][]byte, secure bool, limits Limits) (*Codec, error) {
	s, err := newSealer(secrets)
	if err != nil {
		return nil, err
	}
	if limits.MaxAge <= 0 || limits.IdleTimeout <= 0 {
		return nil, errors.New("session limits must be positive")
	}
	return &Codec{sealer: s, names: namesFor(secure), secure: secure, limits: limits, now: time.Now}, nil
}

// Limits returns the lifetime policy.
func (c *Codec) Limits() Limits { return c.limits }

// ErrSessionTooLarge is returned when a session cannot fit in the cookie
// budget even without its refresh token.
var ErrSessionTooLarge = errors.New("the identity provider's token is too large for a session cookie")

// WriteSession seals s into the session cookie chunks and expires any
// chunk the browser holds that the new value does not use. A session that
// does not fit loses its refresh token first (it then ends with the
// token), and fails only when the token alone does not fit.
func (c *Codec) WriteSession(w http.ResponseWriter, r *http.Request, s Session) (Session, error) {
	v, err := seal(c.sealer.session, c.names.session, s)
	if err != nil {
		return s, err
	}
	if len(v) > maxChunks*maxChunkBytes && s.RefreshToken != "" {
		s.RefreshToken = ""
		if v, err = seal(c.sealer.session, c.names.session, s); err != nil {
			return s, err
		}
	}
	if len(v) > maxChunks*maxChunkBytes {
		return s, ErrSessionTooLarge
	}
	maxAge := int(time.Until(s.AbsoluteExpiry(c.limits)).Seconds())
	if maxAge <= 0 {
		maxAge = -1
	}
	used := 0
	for len(v) > 0 {
		n := min(len(v), maxChunkBytes)
		c.set(w, c.names.chunk(used), v[:n], maxAge, http.SameSiteStrictMode)
		v = v[n:]
		used++
	}
	for i := used; i < maxChunks; i++ {
		if r != nil {
			if _, err := r.Cookie(c.names.chunk(i)); err == nil {
				c.set(w, c.names.chunk(i), "", -1, http.SameSiteStrictMode)
			}
		}
	}
	return s, nil
}

// ReadSession opens the session cookie and checks its lifetime. The
// revocation list and the API server's opinion of the token are checked by
// the Authenticator, not here.
func (c *Codec) ReadSession(r *http.Request) (Session, error) {
	var b strings.Builder
	for i := range maxChunks {
		ck, err := r.Cookie(c.names.chunk(i))
		if err != nil {
			break
		}
		b.WriteString(ck.Value)
	}
	if b.Len() == 0 {
		return Session{}, errNoSession
	}
	var s Session
	if err := open(c.sealer.session, c.names.session, b.String(), &s); err != nil {
		return Session{}, err
	}
	if err := s.check(c.now(), c.limits); err != nil {
		return Session{}, err
	}
	return s, nil
}

var errNoSession = errors.New("no session cookie")

// ClearSession expires every session chunk.
func (c *Codec) ClearSession(w http.ResponseWriter) {
	for i := range maxChunks {
		c.set(w, c.names.chunk(i), "", -1, http.SameSiteStrictMode)
	}
}

// loginFlow is the sealed state of one OAuth round trip, carried in the
// login cookie: SameSite=Lax because the IdP's redirect back is a
// cross-site top-level navigation that a Strict cookie would not survive.
type loginFlow struct {
	Provider   string `json:"prv"`
	State      string `json:"st"`
	Nonce      string `json:"nc,omitempty"`
	Verifier   string `json:"pk"`
	RedirectTo string `json:"rd"`
	Expiry     int64  `json:"exp"`
	// Grant is set by a grant sign-in (grantsignin.go): the callback hands
	// the tokens to the grant it names instead of opening a session.
	Grant string `json:"gr,omitempty"`
	// GrantClient is the OIDC client of a grant sign-in run with the
	// grant client (OIDC_GRANT_CLIENT_ID); empty: the console's client.
	GrantClient string `json:"gc,omitempty"`
}

func (c *Codec) writeLogin(w http.ResponseWriter, f loginFlow) error {
	v, err := seal(c.sealer.login, c.names.login, f)
	if err != nil {
		return err
	}
	c.set(w, c.names.login, v, int(loginFlowMaxAge.Seconds()), http.SameSiteLaxMode)
	return nil
}

func (c *Codec) readLogin(r *http.Request, provider string) (loginFlow, error) {
	ck, err := r.Cookie(c.names.login)
	if err != nil {
		return loginFlow{}, errors.New("no login-flow cookie")
	}
	var f loginFlow
	if err := open(c.sealer.login, c.names.login, ck.Value, &f); err != nil {
		return loginFlow{}, err
	}
	if f.Provider != provider || f.State == "" || f.Verifier == "" || c.now().Unix() >= f.Expiry {
		return loginFlow{}, errors.New("login flow is expired, incomplete or for another provider")
	}
	return f, nil
}

func (c *Codec) clearLogin(w http.ResponseWriter) {
	c.set(w, c.names.login, "", -1, http.SameSiteLaxMode)
}

func (c *Codec) set(w http.ResponseWriter, name, value string, maxAge int, sameSite http.SameSite) {
	// Secure follows the deployment (false only for plain-HTTP local
	// development, where the __Host- prefix is not used either); HttpOnly
	// and SameSite are literals.
	http.SetCookie(w, &http.Cookie{ // #nosec G124
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   c.secure,
		SameSite: sameSite,
	})
}
