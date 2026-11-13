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

// Package server assembles the console's HTTP surface: the public sign-in endpoints, the JSON API and the live
// stream behind the session and CSRF checks, the embedded UI, health and
// readiness, the security headers every response carries, the listener
// timeouts, graceful shutdown, the runner supervisor and the /metrics
// listener behind apiserver authentication and authorization.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// ContentSecurityPolicy is the default policy of every response. The UI's index.html sets its own, adding a per-request nonce; the
// sign-in pages set theirs, allowing their one hashed stylesheet.
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Paths of the HTTP surface.
const (
	PathHealth = "/healthz"
	PathReady  = "/readyz"
	PathAPI    = "/api/"
	PathAssets = "/assets/"
	// PathStream is the live-update stream; its responses are exempt from
	// the request write deadline.
	PathStream = "/api/v1/stream"
)

// Listener timeouts. There is no server-wide WriteTimeout because the
// live stream stays open for the session's lifetime; every other request
// gets requestWriteTimeout through its ResponseController.
const (
	readHeaderTimeout   = 10 * time.Second
	readTimeout         = 30 * time.Second
	idleTimeout         = 120 * time.Second
	requestWriteTimeout = 60 * time.Second
	maxHeaderBytes      = 64 << 10
	// maxAPIBody bounds request bodies under /api/.
	maxAPIBody = 1 << 20
	// DefaultShutdownTimeout leaves room under the chart's 40 s grace.
	DefaultShutdownTimeout = 25 * time.Second
)

// Options assemble a Server.
type Options struct {
	// Addr is the main listen address (LISTEN_ADDR, ":3000"): sign-in,
	// API, stream and UI.
	Addr string
	// ProbeAddr is the probe listener (PROBE_LISTEN_ADDR, ":3001"):
	// /healthz, /readyz and the authenticated /metrics. Kept apart so
	// probes work whatever the main listener requires.
	ProbeAddr string
	// Metrics serves /metrics on the probe listener (already wrapped in
	// MetricsFilter); nil leaves it out.
	Metrics http.Handler
	// TLSCertFile and TLSKeyFile enable HTTPS on the main listener (the
	// OpenShift service CA, or a re-encrypt Route) and on the probe
	// listener, so /metrics (and the probes beside it) is served over TLS
	// with the same certificate (checklist 21); unset, both are plain HTTP.
	TLSCertFile string
	TLSKeyFile  string
	// Auth authenticates every non-public request: the sign-in layer
	// (*auth.Handler).
	Auth Authenticator
	// PublicURL is the configured external origin for the CSRF check.
	PublicURL string
	// API serves /api/; UI serves everything else (assets publicly, the
	// rest after sign-in). UI may be nil until the SPA is embedded.
	API http.Handler
	UI  http.Handler
	// Ready is consulted by /readyz.
	Ready *Readiness
	// MCP mounts the MCP endpoint and its metadata outside
	// the CSRF check and the session middleware: it reads no cookie and
	// authenticates every request with its bearer. nil serves none.
	MCP Mounter
	// MCPGate refuses the MCP leader's paths on a replica that does not
	// lead (mcpleader.Leader.Gate); nil with MCP off.
	MCPGate func(http.Handler) http.Handler
	// Tracing wraps the main listener in a server span per request;
	// nil: no spans, no wrapper.
	Tracing *tracing.Tracing
	// Broker is closed at shutdown so open streams end.
	Broker *sse.Broker
	// ShutdownTimeout bounds the drain.
	ShutdownTimeout time.Duration
	// ShutdownDelay is how long the console keeps serving, not ready,
	// after the shutdown signal and before it stops accepting: the time
	// the endpoints controller and the edge take to stop sending it new
	// requests (SHUTDOWN_DELAY). Zero closes the listener at once.
	ShutdownDelay time.Duration
	Log           *slog.Logger
}

// Authenticator registers its public sign-in endpoints and guards every
// other request with the caller's principal.
type Authenticator interface {
	Register(mux *http.ServeMux)
	Require(next http.Handler) http.Handler
}

// Mounter registers routes of its own on the outermost mux.
type Mounter interface {
	Register(mux *http.ServeMux)
}

// SessionMounter is a Mounter that also has routes on the session mux,
// behind the CSRF check.
type SessionMounter interface {
	RegisterSession(mux *http.ServeMux)
}

// Server is the console's main listener.
type Server struct {
	o       Options
	handler http.Handler
	probes  http.Handler
}

// New assembles the handler tree.
func New(o Options) *Server {
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Ready == nil {
		o.Ready = &Readiness{}
	}
	return &Server{o: o, handler: buildHandler(o), probes: buildProbes(o)}
}

// Handler is the main handler tree (tests and the harness).
func (s *Server) Handler() http.Handler { return s.handler }

// ProbeHandler is the probe listener's handler tree.
func (s *Server) ProbeHandler() http.Handler { return s.probes }

func buildProbes(o Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET "+PathReady, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := o.Ready.Check(); err != nil {
			o.Log.Warn("not ready", "reason", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready")
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	if o.Metrics != nil {
		mux.Handle("GET /metrics", o.Metrics)
	}
	return securityHeaders(mux)
}

func buildHandler(o Options) http.Handler {
	mux := http.NewServeMux()
	o.Auth.Register(mux)
	if o.API != nil {
		mux.Handle(PathAPI, o.Auth.Require(apiLimits(o.API)))
	} else {
		mux.Handle(PathAPI, http.NotFoundHandler())
	}
	ui := o.UI
	if ui == nil {
		ui = http.HandlerFunc(placeholderUI)
	}
	// Hashed bundle assets carry nothing about anyone: they are served
	// without a session so the sign-in redirect is the only round trip.
	mux.Handle(PathAssets, ui)
	mux.Handle("/", o.Auth.Require(ui))
	if sm, ok := o.MCP.(SessionMounter); ok {
		// The MCP authorization endpoint reads the session cookie and
		// takes the consent form: behind the CSRF check.
		sm.RegisterSession(mux)
	}
	h := auth.CSRF(o.PublicURL, mux)
	if o.MCP != nil {
		// Bearer-only routes: a cookie grants nothing there, so the CSRF
		// rules (which ask a browser's Origin) do not apply; the endpoint
		// applies MCP's own Origin rule.
		outer := http.NewServeMux()
		o.MCP.Register(outer)
		outer.Handle("/", h)
		h = outer
	}
	if o.MCPGate != nil {
		h = o.MCPGate(h)
	}
	// The live streams last a session: a span of hours tells nothing.
	return o.Tracing.Handler(securityHeaders(writeDeadline(h)), func(r *http.Request) bool {
		return r.URL.Path == PathStream || isLogStream(r.URL.Path)
	})
}

// placeholderUI answers until the SPA build is embedded.
func placeholderUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>pgToolBox console</title><p>The console interface is not part of this build.</p>`)
}

// apiLimits bounds API request bodies and marks API responses
// uncacheable.
func apiLimits(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxAPIBody)
		}
		h.ServeHTTP(w, r)
	})
}

// securityHeaders sets the headers every response carries; handlers may
// replace the CSP with a stricter or nonce-bearing one.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", ContentSecurityPolicy)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// isLogStream is a pod log follow (api: .../instances/{pod}/logs/stream),
// bounded by its own session tie and one-hour limit.
func isLogStream(path string) bool {
	return strings.HasPrefix(path, "/api/v1/namespaces/") && strings.HasSuffix(path, "/logs/stream")
}

// writeDeadline bounds every response but the live stream, which manages
// its own per-write deadline and lives as long as the session.
func writeDeadline(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathStream || isLogStream(r.URL.Path) {
			// Proxies must not buffer the stream (nginx honours
			// X-Accel-Buffering), nor anything cache it.
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("Cache-Control", "no-store")
		} else {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(requestWriteTimeout))
		}
		h.ServeHTTP(w, r)
	})
}

// Run serves both listeners until ctx ends, then drains: the broker is
// closed first so open streams end, then in-flight requests get
// ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.o.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.o.Addr, err)
	}
	var probe net.Listener
	if s.o.ProbeAddr != "" {
		if probe, err = lc.Listen(ctx, "tcp", s.o.ProbeAddr); err != nil {
			_ = ln.Close()
			return fmt.Errorf("listening on %s: %w", s.o.ProbeAddr, err)
		}
	}
	return s.Serve(ctx, ln, probe)
}

// newHTTPServer bases request contexts on ctx, which outlives the
// shutdown signal: a request in flight is cancelled only when the drain
// gives up on it, not when the drain begins.
func (s *Server) newHTTPServer(ctx context.Context, h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(s.o.Log.Handler(), slog.LevelWarn),
	}
}

// serve runs srv on ln: HTTPS with the configured key pair when
// TLSCertFile is set, plain HTTP otherwise. Both listeners share the
// choice, so /metrics is never plain HTTP beside an HTTPS console.
func (s *Server) serve(srv *http.Server, ln net.Listener) error {
	if s.o.TLSCertFile == "" {
		return srv.Serve(ln)
	}
	srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return srv.ServeTLS(ln, s.o.TLSCertFile, s.o.TLSKeyFile)
}

// endOnDrain ends the long-lived streams (the event stream, pod log
// follows) when drain ends, as the drain begins (after the shutdown
// delay, during which they keep running), since it would otherwise wait
// for them until its timeout; every other request keeps its context
// until it completes or the drain gives up.
func endOnDrain(drain context.Context, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathStream && !isLogStream(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(drain, cancel)
		defer stop()
		// ctx is the request's own, cancelled early by drain: the
		// linter cannot see through context.AfterFunc.
		h.ServeHTTP(w, r.WithContext(ctx)) //nolint:contextcheck // derived from r.Context() above
	})
}

// Serve is Run on existing listeners; probe may be nil.
//
// The shutdown sequence: ctx ends; /readyz answers not ready
// (SetDraining) while the listener keeps serving, streams included, for
// ShutdownDelay; the streams end; Shutdown stops accepting and waits up to ShutdownTimeout
// for the requests in flight, whose contexts are cancelled only after
// it returns.
func (s *Server) Serve(ctx context.Context, ln, probe net.Listener) error {
	reqCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	// The streams end when the drain begins, after the delay: ending them
	// at the signal would cut every viewer off while this replica still
	// receives traffic, and their reconnects would land here again.
	drain, beginDrain := context.WithCancel(reqCtx)
	defer beginDrain()
	srv := s.newHTTPServer(reqCtx, endOnDrain(drain, s.handler))
	errc := make(chan error, 2)
	go func() { errc <- s.serve(srv, ln) }()
	var psrv *http.Server
	if probe != nil {
		psrv = s.newHTTPServer(reqCtx, s.probes)
		psrv.ReadTimeout, psrv.WriteTimeout = 5*time.Second, 10*time.Second
		go func() { errc <- s.serve(psrv, probe) }()
		s.o.Log.Info("probe listener", "addr", probe.Addr().String(), "tls", s.o.TLSCertFile != "")
	}
	s.o.Log.Info("console listening", "addr", ln.Addr().String(), "tls", s.o.TLSCertFile != "")
	select {
	case err := <-errc:
		_ = srv.Close()
		if psrv != nil {
			_ = psrv.Close()
		}
		return fmt.Errorf("console listener: %w", err)
	case <-ctx.Done():
	}
	s.o.Log.Info("shutting down", "delay", s.o.ShutdownDelay, "timeout", s.o.ShutdownTimeout)
	s.o.Ready.SetDraining()
	if s.o.ShutdownDelay > 0 {
		// Keep accepting while the endpoints and the edge catch up with
		// not-ready; a listener closed first refuses requests still routed
		// here.
		t := time.NewTimer(s.o.ShutdownDelay)
		select {
		case <-t.C:
		case err := <-errc:
			t.Stop()
			s.o.Log.Warn("listener ended during the shutdown delay", "error", err)
		}
	}
	beginDrain()
	if s.o.Broker != nil {
		s.o.Broker.Close()
	}
	sctx, cancel := context.WithTimeout(reqCtx, s.o.ShutdownTimeout)
	defer cancel()
	// Probes keep answering while the main listener drains, so the kubelet
	// sees not-ready rather than a refused port.
	err := srv.Shutdown(sctx)
	if psrv != nil {
		_ = psrv.Shutdown(sctx)
	}
	// What the drain gave up on is cancelled now, and closed.
	cancelRequests()
	if err != nil {
		_ = srv.Close()
		if psrv != nil {
			_ = psrv.Close()
		}
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
