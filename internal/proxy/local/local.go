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

// Package local implements the local authentication provider: a styled
// login form verified against operator-rendered bcrypt hashes, with a
// per-IP failure rate limiter.
package local

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/pages"
	"github.com/fyannk/pgtoolbox/internal/proxy/server"
)

const (
	// maxFailures is the number of failed attempts allowed per window.
	maxFailures = 10
	// window is the rate-limit window.
	window = time.Minute
)

// Provider is the local authentication provider.
type Provider struct {
	env     *server.Env
	limiter *rateLimiter
	// dummyHash is compared against for unknown identities so unknown
	// and known users cost the same bcrypt work (no enumeration via
	// timing).
	dummyHash []byte
}

// New builds the provider.
func New(env *server.Env) (*Provider, error) {
	dummy, err := bcrypt.GenerateFromPassword([]byte("pgtoolbox-dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return &Provider{env: env, limiter: newRateLimiter(), dummyHash: dummy}, nil
}

// Mode implements server.Provider.
func (p *Provider) Mode() string { return config.ModeLocal }

// Register implements server.Provider.
func (p *Provider) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/local/login", p.handleForm)
	mux.HandleFunc("POST /auth/local/login", p.handleSubmit)
}

// handleForm renders the login form.
func (p *Provider) handleForm(w http.ResponseWriter, r *http.Request) {
	pages.Login(w, http.StatusOK, pages.LoginData{
		RedirectTo: server.SafeRedirect(r.URL.Query().Get("rd")),
		External:   server.ExternalLogins(p.env),
	})
}

// handleSubmit verifies credentials and mints a session on success.
func (p *Provider) handleSubmit(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !p.limiter.allow(ip) {
		pages.Error(w, http.StatusTooManyRequests, "Too many failed attempts; try again later.")
		return
	}
	if err := r.ParseForm(); err != nil {
		pages.Error(w, http.StatusBadRequest, "The form could not be parsed.")
		return
	}
	rd := server.SafeRedirect(r.PostFormValue("rd"))
	subject := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	password := r.PostFormValue("password")

	hash := p.dummyHash
	user, known := p.env.LookupUser(subject)
	if known && user.LocalPasswordBcrypt != "" {
		hash = []byte(user.LocalPasswordBcrypt)
	}
	// Always exactly one bcrypt comparison, whatever the outcome path.
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil || !known {
		p.limiter.fail(ip)
		p.env.Logger.Warn("local login failed", "ip", ip)
		pages.Login(w, http.StatusUnauthorized, pages.LoginData{
			RedirectTo: rd,
			Error:      "Invalid identity or password.",
			External:   server.ExternalLogins(p.env),
		})
		return
	}
	p.limiter.reset(ip)
	if err := p.env.IssueSession(w, subject, user.Level, config.ModeLocal); err != nil {
		p.env.Logger.Error("issuing session failed", "error", err)
		pages.Error(w, http.StatusInternalServerError, "The session could not be created.")
		return
	}
	// rd was sanitised by server.SafeRedirect above, which is the check the
	// 0.1.1 open redirect turned on; G710 does not recognise it as one.
	http.Redirect(w, r, rd, http.StatusFound) // #nosec G710
}

// clientIP extracts the client IP for rate limiting. The proxy is the
// ingress boundary of the pod; RemoteAddr is the direct peer.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a stdlib-only per-IP failure counter over a sliding
// fixed window.
type rateLimiter struct {
	mu       sync.Mutex
	failures map[string]*rateEntry
	now      func() time.Time
}

type rateEntry struct {
	windowStart time.Time
	count       int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{failures: map[string]*rateEntry{}, now: time.Now}
}

// allow reports whether ip may attempt a login now.
func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e, ok := rl.failures[ip]
	if !ok || rl.now().Sub(e.windowStart) >= window {
		return true
	}
	return e.count < maxFailures
}

// fail records one failed attempt for ip.
func (rl *rateLimiter) fail(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e, ok := rl.failures[ip]
	if !ok || rl.now().Sub(e.windowStart) >= window {
		rl.failures[ip] = &rateEntry{windowStart: rl.now(), count: 1}
		return
	}
	e.count++
}

// reset clears the failure record of ip after a successful login.
func (rl *rateLimiter) reset(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.failures, ip)
}
