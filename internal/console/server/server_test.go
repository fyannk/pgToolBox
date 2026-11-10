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

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/sse"
)

type reviewer map[string]authz.User

func (r reviewer) Review(_ context.Context, tok string) (authz.User, error) {
	if tok == "down" {
		return authz.User{}, errors.New("apiserver down")
	}
	if u, ok := r[tok]; ok {
		return u, nil
	}
	return authz.User{}, auth.ErrTokenRejected
}

type sar map[string]bool // user -> allowed

func (s sar) Review(_ context.Context, in *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	out := in.DeepCopy()
	if in.Spec.User == "flaky" {
		return nil, errors.New("503")
	}
	out.Status.Allowed = s[in.Spec.User]
	return out, nil
}

func testAuth(t *testing.T) *auth.Handler {
	t.Helper()
	codec, err := auth.NewCodec([][]byte{[]byte(strings.Repeat("s", 40))}, true, auth.Limits{MaxAge: time.Hour, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h, err := auth.New(t.Context(), auth.Options{
		Config:   auth.Config{Providers: []string{auth.ProviderToken}, CookieSecure: true, IdPTimeout: time.Second},
		Codec:    codec,
		Identity: reviewer{"good": {Name: "jane"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func testServer(t *testing.T, ready *Readiness) *Server {
	t.Helper()
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "asset") })
	return New(Options{Auth: testAuth(t), API: api, UI: ui, Ready: ready, Broker: sse.NewBroker()})
}

func get(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), method, "https://c.example"+path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRoutesAndHeaders(t *testing.T) {
	t.Parallel()
	ready := &Readiness{}
	var fail atomic.Bool
	ready.Add("cache", func() error {
		if fail.Load() {
			return errors.New("not synced")
		}
		return nil
	})
	h := testServer(t, ready).Handler()
	cases := []struct {
		method, path string
		hdr          map[string]string
		status       int
		body         string
	}{
		{http.MethodGet, "/healthz", nil, http.StatusFound, ""},
		{http.MethodGet, "/api/v1/clusters", nil, http.StatusUnauthorized, "loginURL"},
		{http.MethodGet, "/assets/app.js", nil, http.StatusOK, "asset"},
		{http.MethodGet, "/", nil, http.StatusFound, ""},
		{http.MethodGet, "/auth/login", nil, http.StatusOK, "Sign in"},
		{http.MethodPost, "/api/v1/x", map[string]string{"Origin": "https://c.example"}, http.StatusForbidden, "X-PgToolBox-Request"},
	}
	for _, tc := range cases {
		w := get(h, tc.method, tc.path, tc.hdr)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("%s %s = %d %q, want %d containing %q", tc.method, tc.path, w.Code, w.Body, tc.status, tc.body)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: security headers missing: %v", tc.path, w.Header())
		}
	}
	srv := testServer(t, ready)
	probes := srv.ProbeHandler()
	for _, p := range []string{"/healthz", "/readyz"} {
		if w := get(probes, http.MethodGet, p, nil); w.Code != http.StatusOK || w.Body.String() != "ok" {
			t.Errorf("probe %s = %d %q", p, w.Code, w.Body)
		}
	}
	if w := get(probes, http.MethodGet, "/api/v1/clusters", nil); w.Code != http.StatusNotFound {
		t.Errorf("the probe listener serves the API: %d", w.Code)
	}
	if csp := get(probes, http.MethodGet, "/healthz", nil).Header().Get("Content-Security-Policy"); csp != ContentSecurityPolicy || strings.Contains(csp, "unsafe") {
		t.Errorf("default CSP = %q", csp)
	}
	// The SPA self-hosts its type (IBM Plex, ui/src/ds/fonts): fonts come from
	// the console's own origin and nowhere else, never from a data: URI.
	if !strings.Contains(ContentSecurityPolicy, "font-src 'self';") {
		t.Errorf("default CSP must allow same-origin fonts only: %q", ContentSecurityPolicy)
	}
	fail.Store(true)
	if w := get(probes, http.MethodGet, "/readyz", nil); w.Code != http.StatusServiceUnavailable || w.Body.String() != "not ready" {
		t.Errorf("readyz while not ready = %d %q (the body must stay constant)", w.Code, w.Body)
	}
	w := get(h, http.MethodGet, PathStream, nil)
	if w.Header().Get("X-Accel-Buffering") != "no" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("stream headers = %v", w.Header())
	}
}

func TestGracefulShutdownEndsStreams(t *testing.T) {
	t.Parallel()
	broker := sse.NewBroker()
	streaming := make(chan struct{})
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := broker.Subscribe()
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		close(streaming)
		_ = sse.Run(r.Context(), sub, nopSender{}, sse.Options{})
	})
	s := New(Options{Auth: testAuth(t), API: api, Broker: broker, ShutdownTimeout: 5 * time.Second})
	// Exercise the stream path without a session by serving the API
	// handler directly behind the same server plumbing.
	s.handler = securityHeaders(writeDeadline(api))
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, nil) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + PathStream) //nolint:noctx // test client
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-streaming:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not open")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown hung on an open stream")
	}
}

// TestGracefulShutdownLetsRequestsInFlightComplete is the drain's
// contract: the shutdown signal makes the console not ready, keeps the
// listener open for the delay, and lets a request already in flight run
// to completion with a live context.
func TestGracefulShutdownLetsRequestsInFlightComplete(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var cancelledEarly atomic.Bool
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			_, _ = io.WriteString(w, "fast")
			return
		}
		close(entered)
		<-release
		if r.Context().Err() != nil {
			cancelledEarly.Store(true)
		}
		_, _ = io.WriteString(w, "done")
	})
	ready := &Readiness{}
	s := New(Options{Auth: testAuth(t), API: slow, Ready: ready, ShutdownTimeout: 5 * time.Second, ShutdownDelay: 300 * time.Millisecond})
	s.handler = slow
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, nil) }()
	type result struct {
		body string
		err  error
	}
	fetch := func(path string) result {
		resp, err := http.Get(base + path) //nolint:noctx // test client
		if err != nil {
			return result{err: err}
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		return result{string(b), err}
	}
	inflight := make(chan result, 1)
	go func() { inflight <- fetch("/slow") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the handler")
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for ready.Check() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the console stayed ready after the shutdown signal")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Within the delay the listener still accepts.
	if got := fetch("/fast"); got.err != nil || got.body != "fast" {
		t.Fatalf("request during the shutdown delay = %q, %v", got.body, got.err)
	}
	close(release)
	select {
	case got := <-inflight:
		if got.err != nil || got.body != "done" {
			t.Fatalf("request in flight = %q, %v", got.body, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request in flight did not complete")
	}
	if cancelledEarly.Load() {
		t.Fatal("the request in flight saw its context cancelled by the shutdown signal")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown hung")
	}
}

// TestGracefulShutdownCancelsWhatOutlivesTheDrain is the other side: a
// request still running when ShutdownTimeout ends has its context
// cancelled, and Serve says the drain was cut short.
func TestGracefulShutdownCancelsWhatOutlivesTheDrain(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	stuck := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	})
	s := New(Options{Auth: testAuth(t), API: stuck, ShutdownTimeout: 100 * time.Millisecond})
	s.handler = stuck
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, nil) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/stuck") //nolint:noctx // test client
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve reported a clean drain over a request it gave up on")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown hung")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the request the drain gave up on was never cancelled")
	}
}

type nopSender struct{}

func (nopSender) Event(any) error      { return nil }
func (nopSender) Comment(string) error { return nil }

func TestSupervisorRestartsAndReportsCore(t *testing.T) {
	t.Parallel()
	s := NewSupervisor(slog.New(slog.DiscardHandler))
	s.backoff = func(int) time.Duration { return 50 * time.Millisecond }
	var runs, panics atomic.Int32
	failing := make(chan struct{}, 1)
	s.Add(Runner{Name: "cache", Core: true, Run: func(ctx context.Context) error {
		if runs.Add(1) == 1 {
			failing <- struct{}{}
			return errors.New("watch broke")
		}
		<-ctx.Done()
		return nil
	}})
	s.Add(Runner{Name: "panicky", Run: func(ctx context.Context) error {
		if panics.Add(1) == 1 {
			panic("boom")
		}
		<-ctx.Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	<-failing
	deadline := time.Now().Add(5 * time.Second)
	sawDown := false
	for time.Now().Before(deadline) {
		if err := s.Check(); err != nil && strings.Contains(err.Error(), "cache") {
			sawDown = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !sawDown {
		t.Fatal("a failed core runner did not show in Check")
	}
	for time.Now().Before(deadline) && (runs.Load() < 2 || panics.Load() < 2) {
		time.Sleep(5 * time.Millisecond)
	}
	if runs.Load() < 2 || panics.Load() < 2 {
		t.Fatalf("runs=%d panics=%d: runners were not restarted", runs.Load(), panics.Load())
	}
	if err := s.Check(); err != nil {
		t.Fatalf("after restart Check = %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestMetricsFilter(t *testing.T) {
	t.Parallel()
	az := authz.NewAuthorizer(sar{"prometheus": true}, authz.Options{})
	ident := reviewer{"prom-token": {Name: "prometheus"}, "user-token": {Name: "jane"}, "flaky-token": {Name: "flaky"}}
	h, err := MetricsFilter(ident, az, 0)(logrDiscard(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pgtoolbox_console_up 1")
	}))
	if err != nil {
		t.Fatal(err)
	}
	for auth, want := range map[string]int{
		"":                   http.StatusUnauthorized,
		"Basic x":            http.StatusUnauthorized,
		"Bearer nope":        http.StatusUnauthorized,
		"Bearer user-token":  http.StatusForbidden,
		"Bearer down":        http.StatusServiceUnavailable,
		"Bearer flaky-token": http.StatusServiceUnavailable,
		"Bearer prom-token":  http.StatusOK,
	} {
		w := get(h, http.MethodGet, "/metrics", map[string]string{"Authorization": auth})
		if w.Code != want {
			t.Errorf("Authorization %q = %d, want %d", auth, w.Code, want)
		}
	}
}

func logrDiscard() logr.Logger { return logr.Discard() }
