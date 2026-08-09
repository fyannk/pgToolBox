/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

// Package server wires the proxy HTTP surface: reserved auth endpoints,
// session-based authentication, level-based authorization, and the
// per-route reverse proxies with identity-header hygiene.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/pages"
	"github.com/fyannk/pgtoolbox/internal/proxy/session"
)

// Identity headers the proxy sets from the verified session. They are
// stripped from every inbound request before proxying so clients can
// never forge them.
const (
	HeaderUser    = "X-Forwarded-User"
	HeaderLevel   = "X-PgToolBox-Level"
	HeaderGroups  = "X-Forwarded-Groups"
	accessReqPath = "/auth/access-request"
)

// strippedHeaders are removed from client requests before proxying.
var strippedHeaders = []string{HeaderUser, HeaderLevel, HeaderGroups}

// sessionContextKey carries the verified session to the proxy rewrite.
type sessionContextKey struct{}

// Provider is the seam implemented by each authentication mode (oidc,
// local; openshift later). A provider registers its handlers under the
// reserved /auth prefix and mints sessions through Env.IssueSession.
type Provider interface {
	// Register installs the provider's endpoints on mux.
	Register(mux *http.ServeMux)
}

// Runtime is one immutable configuration snapshot: everything derived
// from a single validated config. Swapped atomically on reload; sessions
// survive swaps as long as the cookie secrets are unchanged.
type Runtime struct {
	Config *config.Config
	Codec  *session.Codec
	// Users maps lowercased subject to user.
	Users  map[string]config.User
	Routes []*Route
}

// Route is a compiled route: longest-prefix match against PathPrefix.
type Route struct {
	Prefix   string
	MinLevel config.Level
	proxy    *httputil.ReverseProxy
}

// Env is the shared environment handed to providers and handlers; it
// always serves the current Runtime.
type Env struct {
	rt     atomic.Pointer[Runtime]
	Logger *slog.Logger
	// AccessRequests creates PgToolBoxAccessRequest objects; nil when the
	// flow is disabled or in-cluster configuration is unavailable.
	AccessRequests AccessRequestCreator
}

// NewEnv builds an Env around the initial runtime.
func NewEnv(rt *Runtime, logger *slog.Logger) *Env {
	e := &Env{Logger: logger}
	e.rt.Store(rt)
	return e
}

// Runtime returns the current snapshot.
func (e *Env) Runtime() *Runtime { return e.rt.Load() }

// Swap atomically installs a new snapshot.
func (e *Env) Swap(rt *Runtime) { e.rt.Store(rt) }

// BuildRuntime derives a Runtime from a validated config.
func BuildRuntime(cfg *config.Config) (*Runtime, error) {
	codec, err := session.NewCodec(cfg.Session.CookieSecrets)
	if err != nil {
		return nil, fmt.Errorf("building session codec: %w", err)
	}
	users := make(map[string]config.User, len(cfg.Users))
	for _, u := range cfg.Users {
		users[strings.ToLower(u.Subject)] = u
	}
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.Server.UpstreamDialTimeout.D(),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: cfg.Server.UpstreamResponseHeaderTimeout.D(),
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
	}
	rt := &Runtime{Config: cfg, Codec: codec, Users: users}
	for _, rc := range cfg.Routes {
		target, err := url.Parse(rc.Upstream)
		if err != nil {
			return nil, fmt.Errorf("route %q upstream: %w", rc.PathPrefix, err)
		}
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				// SetURL points the outbound request at the loopback
				// upstream, which also makes it the Host the upstream sees.
				// An upstream that builds absolute URLs then builds them
				// from 127.0.0.1:<port> and hands the browser an address
				// inside the Pod — pgAdmin's trailing-slash redirect did
				// exactly that. Preserve the client's Host, and state the
				// forwarding explicitly; SetXForwarded overwrites any
				// client-supplied X-Forwarded-* rather than appending to it.
				pr.Out.Host = pr.In.Host
				pr.SetXForwarded()
				// Strip any client-forged identity headers, then set
				// them exclusively from the verified session.
				for _, h := range strippedHeaders {
					pr.Out.Header.Del(h)
				}
				if sess, ok := pr.In.Context().Value(sessionContextKey{}).(session.Data); ok {
					pr.Out.Header.Set(HeaderUser, sess.Subject)
					pr.Out.Header.Set(HeaderLevel, sess.Level)
				}
			},
			Transport: transport,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				pages.Error(w, http.StatusBadGateway, "The upstream service is unavailable.")
			},
		}
		rt.Routes = append(rt.Routes, &Route{Prefix: rc.PathPrefix, MinLevel: rc.MinLevel, proxy: rp})
	}
	// Longest prefix first.
	for i := 0; i < len(rt.Routes); i++ {
		for j := i + 1; j < len(rt.Routes); j++ {
			if len(rt.Routes[j].Prefix) > len(rt.Routes[i].Prefix) {
				rt.Routes[i], rt.Routes[j] = rt.Routes[j], rt.Routes[i]
			}
		}
	}
	return rt, nil
}

// New builds the root handler: reserved endpoints plus the proxied
// routes. Providers must Register on the mux before it is served.
func New(env *Env) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /logout", env.handleLogout)
	mux.HandleFunc("POST "+accessReqPath, env.handleAccessRequest)
	mux.Handle("/", http.HandlerFunc(env.handleProxied))
	return mux
}

// LoginPath returns where an unauthenticated request is sent to sign in.
//
// With local accounts enabled that is the local page, because it is the one
// with a form, and it offers the other enabled providers as buttons beside
// it. With no local accounts there is nothing to type, so the single
// external provider's start path is the whole of the login flow and the
// browser goes straight there.
func LoginPath(rt *Runtime) string {
	if rt.Config.LocalEnabled() {
		return "/auth/local/login"
	}
	for _, mode := range rt.Config.Provider.Modes {
		if mode == config.ModeOpenShift {
			return "/auth/openshift/login"
		}
	}
	return "/auth/oidc/login"
}

// ExternalLogins lists the providers the login page offers as buttons,
// which is every enabled provider that is not the local form itself.
func ExternalLogins(rt *Runtime) []pages.ExternalLogin {
	var logins []pages.ExternalLogin
	for _, mode := range rt.Config.Provider.Modes {
		switch mode {
		case config.ModeOIDC:
			logins = append(logins, pages.ExternalLogin{Label: "Sign in with SSO", Path: "/auth/oidc/login"})
		case config.ModeOpenShift:
			logins = append(logins, pages.ExternalLogin{Label: "Sign in with OpenShift", Path: "/auth/openshift/login"})
		}
	}
	return logins
}

// SafeRedirect validates a post-login redirect target: only relative
// paths starting with exactly one slash and containing no backslash
// (some browsers treat "\" as "/") or control characters are honored;
// everything else falls back to "/".
func SafeRedirect(p string) string {
	if strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.ContainsAny(p, "\\\r\n") {
		return p
	}
	return "/"
}

// IssueSession mints a session cookie for subject at level using the
// current runtime. Level may be config.LevelNone for authenticated
// identities unknown to the console. Mode names the provider that
// authenticated the subject, which the session records: with several
// providers enabled, "who let this person in" is no longer answerable
// from the configuration alone.
func (e *Env) IssueSession(w http.ResponseWriter, subject string, level config.Level, mode string) error {
	rt := e.Runtime()
	cfg := rt.Config
	d := rt.Codec.NewData(subject, string(level), mode, cfg.Session.MaxAge.D())
	return rt.Codec.SetCookie(w, cfg.Session.CookieName, cfg.Session.CookieSecure(), cfg.Session.MaxAge.D(), d)
}

// LookupUser returns the configured user for subject, lowercasing first.
func (e *Env) LookupUser(subject string) (config.User, bool) {
	u, ok := e.Runtime().Users[strings.ToLower(subject)]
	return u, ok
}

// handleLogout clears the session cookie and redirects to login. Local
// logout only; no RP-initiated logout for OIDC.
func (e *Env) handleLogout(w http.ResponseWriter, r *http.Request) {
	rt := e.Runtime()
	session.ClearCookie(w, rt.Config.Session.CookieName, rt.Config.Session.CookieSecure())
	http.Redirect(w, r, LoginPath(rt), http.StatusFound)
}

// handleProxied authenticates, authorizes, and proxies one request.
func (e *Env) handleProxied(w http.ResponseWriter, r *http.Request) {
	rt := e.Runtime()
	route := matchRoute(rt, r.URL.Path)
	if route == nil {
		pages.Error(w, http.StatusNotFound, "This path is not served by the console.")
		return
	}
	sess, err := rt.Codec.ReadCookie(r, rt.Config.Session.CookieName)
	if err != nil {
		if r.Method == http.MethodGet {
			login := LoginPath(rt) + "?rd=" + url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, login, http.StatusFound)
			return
		}
		pages.Error(w, http.StatusUnauthorized, "Authentication is required.")
		return
	}
	level := config.Level(sess.Level)
	if config.Rank(level) < config.Rank(route.MinLevel) {
		if level == config.LevelNone {
			// Authenticated but unknown identity: offer the
			// request-access flow.
			e.renderDeniedUnknown(w, rt, sess)
			return
		}
		pages.DeniedKnown(w, pages.DeniedKnownData{
			Subject:  sess.Subject,
			Level:    string(level),
			MinLevel: string(route.MinLevel),
			Path:     r.URL.Path,
		})
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, sess))
	route.proxy.ServeHTTP(w, r)
}

func (e *Env) renderDeniedUnknown(w http.ResponseWriter, rt *Runtime, sess session.Data) {
	arc := rt.Config.AccessRequest
	pages.DeniedUnknown(w, pages.DeniedUnknownData{
		Subject:     sess.Subject,
		CSRFToken:   rt.Codec.CSRFToken(sess),
		ShowForm:    arc.Enabled && e.AccessRequests != nil,
		ConsoleName: arc.ConsoleName,
	})
}

// matchRoute returns the longest-prefix route for path, or nil.
func matchRoute(rt *Runtime, path string) *Route {
	for _, r := range rt.Routes {
		if pathMatchesPrefix(path, r.Prefix) {
			return r
		}
	}
	return nil
}

// pathMatchesPrefix matches prefix "/" against everything and any other
// prefix on segment boundaries.
func pathMatchesPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
