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

// Package api is the console's JSON API: Huma operations
// over the view models of internal/console/viewmodel, each authorized by
// a SelfSubjectAccessReview with the signed-in user's token before anything is read,
// and the live-update stream of internal/console/sse. The OpenAPI 3.1
// document generated from these registrations is committed as
// openapi.yaml (make openapi; CI fails when it drifts) and is what the UI
// generates its client and MSW handlers from.
//
// Authorization comes first on every cluster route: a caller who may not
// get the Cluster receives 403 whether it exists or not, so the API is no
// oracle for names in the watched namespaces. Rows of the cluster list
// that fail the review are dropped; rows whose review could not be made
// are dropped and counted in "unchecked".
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	humasse "github.com/danielgtaylor/huma/v2/sse"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Version is the API version in the document's info block and paths.
const Version = "v1"

// ErrNotFound is returned by a Source for a Cluster that is not in the
// watched namespaces' read model.
var ErrNotFound = errors.New("cluster not found")

// Source is the read model the API serves (the console core). Every
// method returns the latest published snapshot; none performs long I/O.
type Source interface {
	ClusterList(ctx context.Context) (viewmodel.ClusterList, error)
	ClusterOverview(ctx context.Context, namespace, name string) (viewmodel.ClusterOverview, error)
	Instances(ctx context.Context, namespace, name string) (viewmodel.InstanceList, error)
	Events(ctx context.Context, namespace, name string) (viewmodel.EventList, error)
	Diagnostics(ctx context.Context, namespace, name string) (viewmodel.Diagnostics, error)
	Triage(ctx context.Context, namespace, name string) (viewmodel.Triage, error)
	ScreenSource
	InsightSource
	TimelineSource
}

// Authorizer is the slice of authz.Authorizer the API uses.
type Authorizer interface {
	Allowed(ctx context.Context, u authz.User, at authz.Attributes) authz.Decision
	VisibleClusters(ctx context.Context, u authz.User, refs []authz.ObjectRef) (visible, undecided []authz.ObjectRef, err error)
	ClusterCapabilities(ctx context.Context, u authz.User, namespace, name string) authz.Capabilities
}

// now is the request clock.
func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// Deps are what the operations read.
type Deps struct {
	Source Source
	Authz  Authorizer
	// Now is the request clock (the timeline's default window ends now);
	// nil is time.Now.
	Now    func() time.Time
	Broker *sse.Broker
	// Sessions re-checks an open stream's session on every heartbeat;
	// nil never re-checks (fixtures).
	Sessions SessionChecker
	// Heartbeat of the stream; sse.DefaultHeartbeat when zero.
	Heartbeat time.Duration
	// Logs reads pod logs; nil serves 501 (as does Features.Logs false).
	Logs LogReader
	// Operator is the operation service; nil serves 501.
	Operator Operator
	// DatabaseRoles creates per-person DatabaseRoles as the caller; nil
	// serves 501.
	DatabaseRoles DatabaseRoleCreator
	// Features are the console-wide switches, reported on the session.
	Features Features
	// Access serves the access-request endpoints (access.go); nil
	// serves 501.
	Access *Access
	// PgAdminURL is a fixed pgAdmin link (PGADMIN_URL) offered on every
	// Cluster after the PgAdmin objects' links; empty offers none.
	PgAdminURL string
	// Grants lists and revokes the caller's MCP grants (assistant.go);
	// nil: the console does not serve MCP, and the routes answer 404.
	Grants GrantManager
	// Assistant renders "Connect an assistant"; nil answers 404.
	Assistant AssistantSetup
	// PublicURL is the console's configured origin (PUBLIC_URL); empty
	// derives it from the request.
	PublicURL string
	// ClientOf attributes a request to a client address for the audit
	// log (auth.ClientOf with the trusted proxy hops); nil uses the peer.
	ClientOf func(*http.Request) auth.Client
	// Log receives the cause of a read-model failure, which the response
	// names only by reference; nil is slog.Default().
	Log *slog.Logger
	// streams counts open streams per session.
	streams *streamCounter
	// refreshes bounds the on-demand exporter reads per user.
	refreshes *refreshLimiter
}

// SessionChecker is auth.Handler's StillValid.
type SessionChecker interface {
	StillValid(ctx context.Context, p *auth.Principal) bool
}

// maxStreamsPerSession bounds the event streams one session may hold
// open (a few tabs).
const maxStreamsPerSession = 8

// Config is the Huma configuration: OpenAPI 3.1, JSON only (the default
// format, with the cluster list's memoized rows copied: jsonFormat), no docs page
// (it would load scripts from a CDN, which the CSP forbids), no $schema
// links in bodies. The document is served at /api/v1/openapi.{json,yaml}
// behind sign-in.
func Config() huma.Config {
	cfg := huma.DefaultConfig("pgToolBox console API", Version)
	cfg.Info.Description = "The JSON API of the pgToolBox console, the CloudNativePG observation console. " +
		"Every operation acts as the signed-in user: the console asks the Kubernetes API server " +
		"(SelfSubjectAccessReview, with your own token) before it reads anything on your behalf. Unsafe methods require " +
		"the header X-PgToolBox-Request: 1 and a same-origin request."
	cfg.OpenAPIPath = "/api/" + Version + "/openapi"
	cfg.DocsPath = ""
	cfg.SchemasPath = ""
	cfg.CreateHooks = nil
	cfg.Formats = map[string]huma.Format{
		"application/json": jsonFormat,
		"json":             jsonFormat,
	}
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"session": {
			Type: "apiKey", In: "cookie", Name: "__Host-pgtoolbox-console-0",
			Description: "The sealed session cookie set by the sign-in pages (/auth/login).",
		},
	}
	cfg.Security = []map[string][]string{{"session": {}}}
	return cfg
}

// New returns the API handler (to mount at /api/ behind auth.Require) and
// the Huma API (for the OpenAPI document).
func New(d Deps) (http.Handler, huma.API) {
	h, a, _ := NewWithReads(d)
	return h, a
}

// NewWithReads is New, and the Reads its routes call: a second face on
// the API (the MCP endpoint) reads through them, with the same
// authorization, the same errors and the same per-user budgets.
func NewWithReads(d Deps) (http.Handler, huma.API, *Reads) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	d.streams = &streamCounter{m: map[string]int{}}
	d.refreshes = newRefreshLimiter(time.Now)
	mux := http.NewServeMux()
	a := humago.New(mux, Config())
	a.UseMiddleware(observe, withRequest)
	Register(a, d)
	return mux, a, &Reads{d: d}
}

// ClusterPath identifies one Cluster (exported so Huma reads it when embedded).
type ClusterPath struct {
	Namespace string `path:"namespace" maxLength:"63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" doc:"Namespace of the Cluster (one of the console's watched namespaces)."`
	Name      string `path:"name" maxLength:"253" pattern:"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" doc:"Name of the Cluster."`
}

// SessionView describes the signed-in session.
type SessionView struct {
	Username       string     `json:"username" doc:"The user as the Kubernetes API server names them."`
	Groups         []string   `json:"groups,omitempty" doc:"Their groups, as the API server reports them."`
	Provider       string     `json:"provider" enum:"oidc,openshift,token,kubeconfig" doc:"How they signed in. kubeconfig is reserved for a local mode and not produced by this console."`
	Context        string     `json:"context,omitempty" doc:"Local mode only: the kubeconfig context the console runs with."`
	SessionID      string     `json:"sessionId" doc:"Reference of this session in the audit log and the revocation list; not a credential."`
	ExpiresAt      time.Time  `json:"expiresAt" doc:"When the session ends unless used or renewed."`
	AbsoluteExpiry time.Time  `json:"absoluteExpiresAt" doc:"When the session ends whatever happens."`
	TokenExpiresAt *time.Time `json:"tokenExpiresAt,omitempty" doc:"When the Kubernetes token the session carries expires, when known."`
	Refreshable    bool       `json:"refreshable" doc:"The session renews its token by itself (OIDC refresh)."`
	LogoutURL      string     `json:"logoutURL" doc:"POST here (a form, same origin) to sign out."`
	Features       Features   `json:"features" doc:"What this console serves at all; per-Cluster capabilities still apply."`
}

// ClusterListView is the cluster list after authorization.
type ClusterListView struct {
	viewmodel.ClusterList
	Unchecked int `json:"unchecked,omitempty" doc:"How many Clusters were left out because the API server could not be asked whether you may see them; retry later. Their names are not disclosed."`
}

// ClusterOverviewView is a Cluster's overview with what the caller may do.
type ClusterOverviewView struct {
	viewmodel.ClusterOverview
	Capabilities authz.Capabilities `json:"capabilities" doc:"What the API server lets you do with this Cluster; decides which buttons exist."`
	// PgAdmins are the pgAdmins the Cluster is registered in that you
	// may open (connect pgadmins/<name>).
	PgAdmins []viewmodel.PgAdminLink `json:"pgAdmins" doc:"pgAdmins this Cluster is registered in that you may open (connect pgadmins/<name>)."`
	// PgAdminsUnread: the PgAdmin list failed, so an empty PgAdmins is
	// not "no pgAdmin".
	PgAdminsUnread bool `json:"pgAdminsUnread,omitempty" doc:"The console could not read the PgAdmins: pgAdmins may be incomplete."`
}

type (
	sessionOutput     struct{ Body SessionView }
	clusterListOutput struct{ Body ClusterListView }
	overviewOutput    struct{ Body ClusterOverviewView }
	instancesOutput   struct{ Body viewmodel.InstanceList }
	eventsOutput      struct{ Body viewmodel.EventList }
	diagnosticsOutput struct{ Body viewmodel.Diagnostics }
	triageOutput      struct{ Body viewmodel.Triage }
)

// Operation IDs, also the fixture file names.
const (
	OpGetSession      = "getSession"
	OpListClusters    = "listClusters"
	OpGetCluster      = "getCluster"
	OpListInstances   = "listInstances"
	OpListEvents      = "listEvents"
	OpGetDiagnostics  = "getDiagnostics"
	OpGetTriage       = "getTriage"
	OpStream          = "stream"
	clusterPathPrefix = "/api/" + Version + "/namespaces/{namespace}/clusters/{name}"
)

var problemCodes = []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable}

// Register adds every operation to a.
func Register(a huma.API, d Deps) {
	r := &Reads{d: d}
	huma.Register(a, huma.Operation{
		OperationID: OpGetSession, Method: http.MethodGet, Path: auth.SessionPath,
		Summary: "The signed-in session", Tags: []string{"session"},
		Description: "Who the API server says you are and when the session ends. On a reauth stream event, " +
			"calling this renews a refreshable session.",
		Errors: []int{http.StatusUnauthorized, http.StatusServiceUnavailable},
	}, func(ctx context.Context, _ *struct{}) (*sessionOutput, error) {
		p, err := principal(ctx)
		if err != nil {
			return nil, err
		}
		v := SessionView{
			Username: p.User.Name, Groups: p.User.Groups, Provider: p.Provider, Context: p.KubeContext, SessionID: p.SessionID,
			ExpiresAt: p.Expiry.UTC(), AbsoluteExpiry: p.AbsoluteExpiry.UTC(), Refreshable: p.Refreshable, LogoutURL: "/logout", Features: d.Features,
		}
		if !p.TokenExpiry.IsZero() {
			t := p.TokenExpiry.UTC()
			v.TokenExpiresAt = &t
		}
		return &sessionOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpListClusters, Method: http.MethodGet, Path: "/api/" + Version + "/clusters",
		Summary: "Clusters you may see", Tags: []string{"clusters"},
		Description: "Every Cluster of the watched namespaces that the API server lets you get " +
			"(one SelfSubjectAccessReview per Cluster, cached 10 s).",
		Errors: problemCodes,
	}, func(ctx context.Context, _ *struct{}) (*clusterListOutput, error) {
		v, err := r.ClusterList(ctx)
		if err != nil {
			return nil, err
		}
		return &clusterListOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpGetCluster, Method: http.MethodGet, Path: clusterPathPrefix,
		Summary: "A Cluster's overview", Tags: []string{"clusters"},
		Description: "The roll-up verdict with its contributors, the primary election, instance cards, " +
			"conditions, recent warnings and the sources and blind spots the view rests on, with your capabilities.",
		Errors: append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *ClusterPath) (*overviewOutput, error) {
		v, err := r.ClusterOverview(ctx, in.Namespace, in.Name)
		if err != nil {
			return nil, err
		}
		return &overviewOutput{Body: v}, nil
	})

	registerCluster(a, d, OpListInstances, "/instances", "A Cluster's instances",
		"One row per member pod and per instance the operator reports without one; an instance whose status could not be read is an error row.",
		func(ctx context.Context, s Source, ns, n string) (*instancesOutput, error) {
			v, err := s.Instances(ctx, ns, n)
			return &instancesOutput{Body: v}, err
		})
	registerCluster(a, d, OpListEvents, "/events", "A Cluster's events",
		"Events about the Cluster and its child objects, newest first, bounded by age and count.",
		func(ctx context.Context, s Source, ns, n string) (*eventsOutput, error) {
			v, err := s.Events(ctx, ns, n)
			return &eventsOutput{Body: v}, err
		})
	registerCluster(a, d, OpGetDiagnostics, "/diagnostics", "A Cluster's diagnostics",
		"The latest background diagnostics run: incidents, findings and the checks that could not run.",
		func(ctx context.Context, s Source, ns, n string) (*diagnosticsOutput, error) {
			v, err := s.Diagnostics(ctx, ns, n)
			return &diagnosticsOutput{Body: v}, err
		})
	registerCluster(a, d, OpGetTriage, "/triage", "A Cluster's triage",
		"Playbooks walked against the latest diagnostics run.",
		func(ctx context.Context, s Source, ns, n string) (*triageOutput, error) {
			v, err := s.Triage(ctx, ns, n)
			return &triageOutput{Body: v}, err
		})

	registerScreens(a, d, r)
	registerInsight(a, r)
	registerAccess(a, d)
	RegisterPgAdminDatabaseRoles(a, d, d.DatabaseRoles)
	registerStream(a, d)
	registerAssistant(a, d)
}

// registerCluster registers a read of one Cluster's sub-view.
func registerCluster[O any](a huma.API, d Deps, id, suffix, summary, desc string, read func(context.Context, Source, string, string) (*O, error)) {
	huma.Register(a, huma.Operation{
		OperationID: id, Method: http.MethodGet, Path: clusterPathPrefix + suffix,
		Summary: summary, Description: desc, Tags: []string{"clusters"},
		Errors: append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *ClusterPath) (*O, error) {
		return readCluster(ctx, d, in.Namespace, in.Name, read)
	})
}

func principal(ctx context.Context) (*auth.Principal, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("sign in to use the console")
	}
	return p, nil
}

// authorizeCluster asks whether the caller may get the Cluster, before
// the read model is consulted.
func authorizeCluster(ctx context.Context, az Authorizer, in *ClusterPath) (*auth.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	d := az.Allowed(ctx, p.User, authz.ClusterView(in.Namespace, in.Name))
	switch {
	case d.Err != nil:
		return nil, huma.Error503ServiceUnavailable("the Kubernetes API server could not be asked whether you may see this Cluster; retry")
	case !d.Allowed:
		return nil, huma.Error403Forbidden("you may not get clusters.postgresql.cnpg.io/" + in.Name + " in namespace " + in.Namespace +
			": ask for the pgtoolbox-console-viewer role there")
	}
	return p, nil
}

// sourceError maps a read-model error onto the response. A rejection that
// carries its own safe text and a missing Cluster say so; anything else is
// a 503 whose body is fixed and names a reference, while the cause (which
// may quote API server or cache internals) goes to the console's log
// under that reference.
func (d Deps) sourceError(ctx context.Context, err error) error {
	var rejected interface {
		HTTPStatus() int
		SafeText() string
	}
	if errors.As(err, &rejected) {
		return huma.NewError(rejected.HTTPStatus(), rejected.SafeText())
	}
	if errors.Is(err, ErrNotFound) {
		return huma.Error404NotFound("no such Cluster in the console's watched namespaces")
	}
	ref := newReference()
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	log.WarnContext(ctx, "the read model could not answer a request",
		slog.String("reference", ref), slog.String("error", err.Error()))
	return huma.Error503ServiceUnavailable("the read model could not answer; the console log has the cause under reference " + ref)
}

// newReference is a short random identifier joining an error response to
// its log line.
func newReference() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type streamInput struct{}

// registerStream registers GET /api/v1/stream.
func registerStream(a huma.API, d Deps) {
	humasse.Register(a, huma.Operation{
		OperationID: OpStream, Method: http.MethodGet, Path: "/api/" + Version + "/stream",
		Summary: "Live updates", Tags: []string{"stream"},
		Description: "Server-sent events: hello on connect (refetch everything), invalidate {topic, namespace, name} " +
			"(refetch that), resync (refetch everything), reauth {expiresAt, refreshable} before the session ends " +
			"(call GET /api/v1/session). A comment line every 15 s; no byte for 35 s means the stream is dead. " +
			"While it is down, poll every 2 s backing off to 10 s. The stream closes when the session ends.",
	}, map[string]any{
		"hello":      sse.Hello{},
		"invalidate": sse.Invalidate{},
		"reauth":     sse.Reauth{},
		"resync":     sse.Resync{},
	}, func(ctx context.Context, _ *streamInput, send humasse.Sender) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok || d.Broker == nil {
			return
		}
		if !d.streams.inc(p.SessionID) {
			_ = send.Data(sse.Resync{Reason: "too many open streams for this session"})
			return
		}
		defer d.streams.dec(p.SessionID)
		var recheck func(context.Context) bool
		if d.Sessions != nil {
			recheck = func(ctx context.Context) bool { return d.Sessions.StillValid(ctx, p) }
		}
		sub := d.Broker.Subscribe()
		_ = sse.Run(ctx, sub, &sender{send: send}, sse.Options{
			Heartbeat:   d.Heartbeat,
			ReauthAt:    p.ReauthAt(),
			EndAt:       p.Expiry,
			Refreshable: p.Refreshable,
			Recheck:     recheck,
			Visible: func(ctx context.Context, ev sse.Invalidate) (bool, error) {
				if ev.Topic != sse.TopicCluster {
					return true, nil
				}
				return streamVisible(d.Authz.Allowed(ctx, p.User, authz.ClusterView(ev.Namespace, ev.Name)))
			},
		})
	})
}

// streamVisible turns the decision on one invalidation into the stream's
// answer: an undecided check is an error (the stream sends a resync in its
// place), and a token the API server no longer accepts (the SelfReviewer's
// 401) ends the stream with its session.
func streamVisible(dec authz.Decision) (bool, error) {
	switch {
	case errors.Is(dec.Err, auth.ErrTokenRejected):
		return false, fmt.Errorf("%w: %w", sse.ErrSessionEnded, dec.Err)
	case dec.Err != nil:
		return false, dec.Err
	}
	return dec.Allowed, nil
}

// sender adapts Huma's SSE sender; the first event carries the reconnect
// delay.
type sender struct {
	send    humasse.Sender
	started bool
}

func (s *sender) Event(data any) error {
	msg := humasse.Message{Data: data}
	if !s.started {
		msg.Retry = sse.RetryMillis
		s.started = true
	}
	return s.send(msg)
}

func (s *sender) Comment(text string) error { return s.send.Comment(text) }

type streamCounter struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *streamCounter) inc(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[key] >= maxStreamsPerSession {
		return false
	}
	c.m[key]++
	return true
}

func (c *streamCounter) dec(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[key]--; c.m[key] <= 0 {
		delete(c.m, key)
	}
}
