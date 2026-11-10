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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

// What the console's OAuth authorization server for MCP clients
// (internal/console/mcpoauth) needs from the sign-in layer:
// the session codec's AEAD under purposes of its own, the signed-in
// person's session with its IdP tokens, the IdP refresh, the API
// server's identity review and the revocation list.

// GrantProvider is the Principal.Provider of a request authenticated by a
// console grant (an MCP client's access token).
const GrantProvider = "grant"

// GrantReferencePrefix marks a grant ID: "grant-" and 32 lowercase hex.
// It names the grant in the audit log, the "Connected assistants" list
// and the revocation list's sessions key; it is not a credential.
const GrantReferencePrefix = "grant-"

// maxPurposeSealed bounds what OpenPurpose decodes: a grant carries an ID
// token and a refresh token, like the session cookie, plus a few fields.
const maxPurposeSealed = 16 << 10

// purposeAAD binds a sealed value to its purpose: a session cookie never
// opens as a grant, a grant never as an authorization code.
func purposeAAD(purpose string) string { return "pgtoolbox-console/v1/purpose/" + purpose }

// purposeLabel is the HKDF label of a sealing purpose's own key.
func purposeLabel(purpose string) string { return "pgtoolbox-console/v1/purpose-aead/" + purpose }

// SealPurpose encrypts v under a key derived for purpose alone from the
// session secrets (nothing new to keep secret, and nothing shared with
// the session cookie's key or another purpose's), bound to purpose.
func (c *Codec) SealPurpose(purpose string, v any) (string, error) {
	if purpose == "" {
		return "", errors.New("a sealing purpose is required")
	}
	aeads, err := c.sealer.purposeAEADs(purposeLabel(purpose))
	if err != nil {
		return "", err
	}
	return seal(aeads, purposeAAD(purpose), v)
}

// OpenPurpose decrypts s sealed by SealPurpose for the same purpose, under
// any configured secret (rotation).
func (c *Codec) OpenPurpose(purpose, s string, v any) error {
	if purpose == "" || s == "" || len(s) > base64.RawURLEncoding.EncodedLen(maxPurposeSealed) {
		return ErrMalformed
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return ErrMalformed
	}
	aeads, err := c.sealer.purposeAEADs(purposeLabel(purpose))
	if err != nil {
		return ErrUndecryptable
	}
	return openRaw(aeads, purposeAAD(purpose), raw, v)
}

// KeyedHashes are the 32-hex HMAC-SHA256 digests of value under a key
// derived for purpose from each session secret, the current secret's
// first: a name for value in shared state that cannot be recomputed from
// a guessed value without the secret.
func (c *Codec) KeyedHashes(purpose, value string) []string {
	keys, err := PurposeKeys(c.sealer.secrets, "pgtoolbox-console/v1/keyed-hash/"+purpose)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(value))
		out = append(out, hex.EncodeToString(m.Sum(nil)[:16]))
	}
	return out
}

// SessionGrant is the signed-in person's session as a grant is minted
// from it: who the API server says they are, and the tokens the session
// carries. The tokens never leave the console unsealed.
type SessionGrant struct {
	Principal    *Principal
	Provider     string
	Token        string
	RefreshToken string
	// TokenExpiry is zero when unknown (an opaque pasted token).
	TokenExpiry time.Time
}

// CurrentSession authenticates r like Require (sealed, unexpired,
// unrevoked session whose token the API server accepts) and returns its
// tokens. nil with no error means there is no usable session; reason is
// the sign-in page's notice ("" when there was no session at all).
func (h *Handler) CurrentSession(w http.ResponseWriter, r *http.Request) (*SessionGrant, string, error) {
	p, s, reason, err := h.authenticateSession(w, r)
	if p == nil || err != nil {
		return nil, reason, err
	}
	g := &SessionGrant{Principal: p, Provider: s.Provider, Token: s.Token, RefreshToken: s.RefreshToken}
	if s.TokenExpiry > 0 {
		g.TokenExpiry = time.Unix(s.TokenExpiry, 0).UTC()
	}
	return g, "", nil
}

// LoginURL is the sign-in page that comes back to rd (a same-origin path).
func LoginURL(rd string) string { return "/auth/login?rd=" + url.QueryEscape(SafeRedirect(rd)) }

// Limits is the session lifetime policy; a grant's refresh token never
// outlives MaxAge from its first sign-in.
func (h *Handler) Limits() Limits { return h.codec.Limits() }

// Codec is the session codec.
func (h *Handler) Codec() *Codec { return h.codec }

// PublicURL is the configured external origin ("" derives it per request).
func (h *Handler) PublicURL() string { return h.cfg.PublicURL }

// TrustedProxy is which X-Forwarded-For entries are believed.
func (h *Handler) TrustedProxy() ProxyTrust { return h.cfg.TrustedProxy }

// Review asks the API server whose token this is (SelfSubjectReview,
// positive answers cached). ErrTokenRejected: the API server refuses it.
func (h *Handler) Review(ctx context.Context, token string) (authz.User, error) {
	return h.identity.Review(ctx, token)
}

// Revoked reports whether the revocation list ends something user signed
// in at issued: any of refs (a grant ID, the session it came from) listed
// under sessions, the user under users, or notBefore.
func (h *Handler) Revoked(user string, issued time.Time, refs ...string) bool {
	if h.revocations.Revoked("", user, issued) {
		return true
	}
	for _, ref := range refs {
		if ref != "" && h.revocations.Revoked(ref, user, issued) {
			return true
		}
	}
	return false
}

// UpstreamTokens are a provider's renewed tokens.
type UpstreamTokens struct {
	Token        string
	RefreshToken string
	// Expiry is zero when unknown.
	Expiry time.Time
	// Provider, set on a grant sign-in's tokens, is what RefreshUpstream
	// renews them with (ProviderOIDCGrant for the OIDC grant client).
	Provider string
}

// ErrNotRefreshable means the provider cannot renew a token (OpenShift, a
// pasted token) or there is no refresh token.
var ErrNotRefreshable = errors.New("this sign-in cannot renew its token")

// RefreshUpstream renews an IdP token with its refresh token. Concurrent
// calls with the same key (a grant ID) share one refresh, so a rotating
// refresh token is redeemed once.
func (h *Handler) RefreshUpstream(ctx context.Context, provider, key, refreshToken string) (UpstreamTokens, error) {
	if refreshToken == "" {
		return UpstreamTokens{}, ErrNotRefreshable
	}
	var ref refresher
	for _, p := range h.oauth {
		if p.name() == provider {
			ref, _ = p.(refresher)
		}
	}
	if provider == ProviderOIDCGrant && h.grantOIDC != nil {
		ref = h.grantOIDC
	}
	if ref == nil {
		return UpstreamTokens{}, ErrNotRefreshable
	}
	v, err, _ := h.refreshes.Do("grant/"+key, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.IdPTimeout)
		defer cancel()
		return ref.refresh(ctx, refreshToken)
	})
	if err != nil {
		tokenRefreshes.WithLabelValues("failure").Inc()
		return UpstreamTokens{}, refreshFailure(err)
	}
	tokenRefreshes.WithLabelValues("success").Inc()
	ts, _ := v.(tokenSet)
	return UpstreamTokens{Token: ts.token, RefreshToken: ts.refresh, Expiry: ts.expiry}, nil
}

// refreshFailure keeps what a log line may say of a failed refresh: an
// identity provider's error answer is reduced to its error code (its body
// may echo what was sent).
func refreshFailure(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return err
	}
	code := re.ErrorCode
	switch {
	case code != "":
	case re.Response != nil:
		code = "HTTP " + strconv.Itoa(re.Response.StatusCode)
	default:
		code = "no error code"
	}
	if len(code) > 64 || strings.ContainsFunc(code, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
		code = "unreadable error code"
	}
	return fmt.Errorf("%w: the identity provider refused the refresh (%s)", ErrNotRefreshable, code)
}

// IsGrantReference reports whether s is a grant ID.
func IsGrantReference(s string) bool {
	ref, ok := strings.CutPrefix(s, GrantReferencePrefix)
	return ok && isLowerHex(ref, 32)
}

func isLowerHex(v string, n int) bool {
	return len(v) == n && !strings.ContainsFunc(v, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) })
}

// ConsentView is the consent page of the MCP authorization server.
type ConsentView struct {
	Subject      string
	ClientName   string
	ClientURL    string
	RedirectHost string
	Console      string
	Access       string
	Operate      bool
	Lifetime     string
	Action       string
	Request      string
	// RedirectOrigin is the client's loopback redirect origin
	// (http://127.0.0.1:<port>): the form's answer redirects there, which
	// the page's form-action must allow (browsers apply form-action to the
	// redirects a form submission follows).
	RedirectOrigin string
}

// ConsentPage renders the consent page with the sign-in pages' CSP, frame
// refusal and no-store.
func ConsentPage(w http.ResponseWriter, v ConsentView) {
	csp := PageCSP
	if o := v.RedirectOrigin; o != "" && !strings.ContainsAny(o, " ;,'\"\n\r") {
		csp = strings.Replace(csp, "form-action 'self'", "form-action 'self' "+o, 1)
	}
	renderPageCSP(w, http.StatusOK, "consent", "Connect an assistant", v, csp)
}

// MCPConfirmView is the confirmation page of an MCP write tool: the
// operation, its Cluster, the person, the assistant asking (by its
// declared name), and the name to type.
type MCPConfirmView struct {
	Subject, Assistant                        string
	Title, Summary, Consequence, Duration     string
	Namespace, Name, Instance, Typed, Expires string
	// Credential is what asked, Asked when (UTC clock time); From is the
	// primary a promote moves the role from.
	Credential, Asked, From string
	// Problem is shown above the form (a mistyped name).
	Problem string
	Action  string
	Request string
}

// MCPConfirmPage renders an MCP write tool's confirmation page with the
// sign-in pages' CSP, frame refusal and no-store.
func MCPConfirmPage(w http.ResponseWriter, status int, v MCPConfirmView) {
	renderPage(w, status, "mcp-confirm", "Confirm an operation", v)
}

// NoticePage renders a page of one title and one message.
func NoticePage(w http.ResponseWriter, status int, title, message string) {
	renderPage(w, status, "notice", title, errorPage{Title: title, Message: message})
}

// ContinuePage sends the browser on to target (a same-origin path) by a
// page of this origin rather than a redirect: after a cross-site sign-in
// round trip, a navigation this page starts is same-site, so the
// SameSite=Strict session cookie is sent with it.
func ContinuePage(w http.ResponseWriter, target string) {
	renderPage(w, http.StatusOK, "continue", "Continuing", struct{ Target string }{SafeRedirect(target)})
}
