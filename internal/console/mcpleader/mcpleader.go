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

// Package mcpleader makes one replica of a console the only one that
// serves MCP and writes the grant index (MCP_LEASE), as CloudNativePG's
// -rw Service points at the primary: the candidates elect a leader on a
// Lease (internal/console/leader), and the install routes the MCP paths
// (Paths) to it: the operator labels the holder's Pod, which its -mcp
// Service selects; the team chart runs the one candidate as its own
// Deployment. Any other replica answers those paths 503 with Retry-After
// (Gate): no proxying. A replica that is no candidate (MCP_CANDIDATE=false)
// never leads.
//
// Leading: load the grant index from the API server (LoadGrants), then
// serve. Losing: stop serving first, then forget the index (DropGrants).
package mcpleader

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/leader"
)

// Path is one path the exposure routes to the leader: Prefix matches the
// path and everything below it (by path element, as an Ingress Prefix
// does), otherwise the path exactly.
type Path struct {
	Path   string
	Prefix bool
}

// Paths are the paths that read or write the grant index, routed to the
// leader's Service. One list for the gate, the chart and the operator.
var Paths = []Path{
	// The MCP endpoint and its confirmation page: grant lookups and uses,
	// confirmation spends.
	{Path: "/mcp", Prefix: true},
	// Authorization, token and registration: consent answers spent,
	// codes exchanged, refresh tokens rotated.
	{Path: "/oauth", Prefix: true},
	// The Assistants page: list, disconnect, disconnect all.
	{Path: "/api/v1/session/grants", Prefix: true},
	// "Sign out and disconnect my assistants". Not gated: the sign-out
	// itself works anywhere, its disconnection is refused (Hook).
	{Path: "/logout"},
}

// Match reports whether p routes urlPath to the leader.
func (p Path) Match(urlPath string) bool {
	if !p.Prefix {
		return urlPath == p.Path
	}
	return urlPath == p.Path || strings.HasPrefix(urlPath, p.Path+"/")
}

// ErrNotLeader is a disconnection asked of a replica that does not lead.
var ErrNotLeader = errors.New("this replica does not serve MCP: the disconnection was not made")

// RetryAfter is the Retry-After of a refused request, in seconds: about
// one election retry period, the time a graceful hand-over takes.
const RetryAfter = "2"

// Grants is the grant index the leader loads and forgets
// (sharedstate.Store).
type Grants interface {
	LoadGrants(ctx context.Context) error
	DropGrants()
}

// Options configure a Leader.
type Options struct {
	Leases    leader.LeaseClient
	Grants    Grants
	Namespace string
	// Lease is the Lease's name (MCP_LEASE).
	Lease    string
	Identity string
	// NotCandidate never campaigns (MCP_CANDIDATE=false): this replica
	// never leads.
	NotCandidate bool
	// The election timings are the leader package's defaults when zero.
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	Logger                                    *slog.Logger
}

// Leader is this replica's part in the MCP election.
type Leader struct {
	o       Options
	elector *leader.Elector
	serving atomic.Bool
	// lead serializes the leaderships: a new one starts only once the
	// previous one has dropped the index.
	lead sync.Mutex
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.RetryPeriod <= 0 {
		o.RetryPeriod = leader.DefaultRetryPeriod
	}
	return o
}

// New builds a leader; nothing is contacted until Run.
func New(o Options) *Leader {
	o = o.withDefaults()
	l := &Leader{o: o}
	l.elector = leader.New(leader.Options{
		Client: o.Leases, Purpose: leader.MCP, Namespace: o.Namespace, Name: o.Lease, Identity: o.Identity,
		Lead: l.leadFor, LeaseDuration: o.LeaseDuration, RenewDeadline: o.RenewDeadline, RetryPeriod: o.RetryPeriod,
		Logger: o.Logger,
	})
	return l
}

// Run takes part in the election until ctx ends, and returns once the
// last leadership has dropped the index. A replica that is no candidate
// waits for ctx.
func (l *Leader) Run(ctx context.Context) error {
	if l.o.NotCandidate {
		<-ctx.Done()
		return nil
	}
	err := l.elector.Run(ctx)
	l.lead.Lock()
	defer l.lead.Unlock()
	return err
}

// Leading reports whether this replica serves MCP: it holds the Lease and
// has loaded the grant index.
func (l *Leader) Leading() bool { return l != nil && l.serving.Load() }

// leadFor is one leadership, until ctx ends.
func (l *Leader) leadFor(ctx context.Context) {
	l.lead.Lock()
	defer l.lead.Unlock()
	defer l.lose()
	for {
		err := l.o.Grants.LoadGrants(ctx)
		if err == nil {
			break
		}
		l.o.Logger.Warn("MCP leader: the grant index could not be loaded; retrying (MCP is not served meanwhile)",
			slog.Any("error", err))
		if !sleep(ctx, l.o.RetryPeriod) {
			return
		}
	}
	l.serving.Store(true)
	l.o.Logger.Info("MCP leader: this replica serves MCP", slog.String("lease", l.o.Lease))
	<-ctx.Done()
}

// lose ends a leadership: no request is served from here on, then the
// index is forgotten.
func (l *Leader) lose() {
	was := l.serving.Swap(false)
	l.o.Grants.DropGrants()
	if was {
		l.o.Logger.Info("MCP leader: this replica no longer serves MCP", slog.String("lease", l.o.Lease))
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Gate answers the leader's paths 503 with Retry-After while this
// replica does not serve MCP (/logout passes: Hook refuses its
// disconnection): an OAuth error on the token endpoint, a problem on the
// grants API, plain text elsewhere.
func (l *Leader) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.Leading() || !gated(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Retry-After", RetryAfter)
		h.Set("Cache-Control", "no-store")
		const detail = "this console replica does not serve MCP right now; retry"
		switch {
		case r.URL.Path == "/oauth/token":
			h.Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "temporarily_unavailable", "error_description": detail})
		case strings.HasPrefix(r.URL.Path, "/api/"):
			h.Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(auth.Problem{
				Status: http.StatusServiceUnavailable, Title: http.StatusText(http.StatusServiceUnavailable), Detail: detail,
			})
		default:
			http.Error(w, detail, http.StatusServiceUnavailable)
		}
	})
}

// gated reports whether the gate refuses urlPath on a replica that does
// not lead: every path of Paths but /logout.
func gated(urlPath string) bool {
	for _, p := range Paths {
		if p.Prefix && p.Match(urlPath) {
			return true
		}
	}
	return false
}

// Hook is the sign-out's "disconnect my assistants" on this replica:
// ErrNotLeader unless it leads (the signed-out page then says the
// assistants may still be connected).
func (l *Leader) Hook(f auth.SignOutHook) auth.SignOutHook {
	return func(ctx context.Context, c auth.Client, user string) error {
		if !l.Leading() {
			return ErrNotLeader
		}
		return f(ctx, c, user)
	}
}
