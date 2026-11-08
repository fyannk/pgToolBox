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

package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	humasse "github.com/danielgtaylor/huma/v2/sse"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/operations"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The per-screen read operations: vocabulary, replication, backups,
// storage, objects, metrics, admin and the pgAdmin link-outs.
const (
	OpGetVocabulary     = "getVocabulary"
	OpGetReplication    = "getReplication"
	OpGetBackups        = "getBackups"
	OpGetRestoreCheck   = "getRestoreCheck"
	OpGetStorage        = "getStorage"
	OpGetObjects        = "getObjects"
	OpListHistory       = "listHistory"
	OpGetRevision       = "getRevision"
	OpGetMetrics        = "getMetrics"
	OpGetTopology       = "getTopology"
	OpGetLogTail        = "getLogTail"
	OpStreamLogs        = "streamLogs"
	OpListOperations    = "listOperations"
	OpRequestOperation  = "requestOperation"
	OpGetOperation      = "getOperation"
	OpGetAdmin          = "getAdmin"
	OpCompareClusters   = "compareClusters"
	maxLogTailLines     = 2000
	defaultLogTailLines = 500
)

// ScreenSource is the read model of the per-Cluster screens. Every method
// returns the latest published snapshot; none performs long I/O.
type ScreenSource interface {
	Replication(ctx context.Context, namespace, name string) (viewmodel.Replication, error)
	Backups(ctx context.Context, namespace, name string) (viewmodel.Backups, error)
	// RestoreCheck answers whether the Cluster can be restored to at,
	// read off the same recovery timeline Backups draws.
	RestoreCheck(ctx context.Context, namespace, name string, at time.Time) (viewmodel.RestoreCheck, error)
	Storage(ctx context.Context, namespace, name string) (viewmodel.Storage, error)
	Objects(ctx context.Context, namespace, name string) (viewmodel.Objects, error)
	History(ctx context.Context, namespace, name string) (viewmodel.History, error)
	// Revision returns ErrNotFound for a revision that does not belong to
	// the Cluster (or its declarative children).
	Revision(ctx context.Context, namespace, name string, seq int64) (viewmodel.Revision, error)
	Metrics(ctx context.Context, namespace, name string) (viewmodel.Metrics, error)
	// Topology draws the frames named in fold folded.
	Topology(ctx context.Context, namespace, name string, fold ...string) (viewmodel.Topology, error)
	// PgAdminLinks are the pgAdmins the Cluster is registered in, before
	// the caller's own access is checked.
	PgAdminLinks(ctx context.Context, namespace, name string) ([]viewmodel.PgAdminLink, error)
	// Admin is the admin page before per-namespace authorization.
	Admin(ctx context.Context) (viewmodel.Admin, error)
	// Compare is two Clusters side by side; ErrNotFound when either is
	// not in the read model.
	Compare(ctx context.Context, leftNamespace, leftName, rightNamespace, rightName string) (viewmodel.Comparison, error)
}

// LogOptions select a container log.
type LogOptions struct {
	Container string
	Previous  bool
	TailLines int
}

// ErrLogsDisabled is returned by a LogReader of a console configured
// without log access (ALLOW_LOGS=false).
var ErrLogsDisabled = errors.New("logs are disabled on this console")

// LogReader reads member pods' logs with the console's ServiceAccount,
// after the API has asked whether the caller may get pods/log.
type LogReader interface {
	// Tail reads a bounded tail once; ErrNotFound for a pod that is not
	// a member of the Cluster or a container it does not have.
	Tail(ctx context.Context, namespace, cluster, pod string, o LogOptions) (viewmodel.LogTail, error)
	// Follow streams the log until ctx ends or the bound is reached,
	// calling emit with LogStreamLine, LogStreamGap and LogStreamEnd.
	Follow(ctx context.Context, namespace, cluster, pod string, o LogOptions, emit func(any) error) error
}

// Operator is the operation service (internal/console/operations).
type Operator interface {
	Offers(ctx context.Context, p *auth.Principal, caps authz.Capabilities, namespace, name string) (viewmodel.Operations, error)
	Request(ctx context.Context, p *auth.Principal, namespace, name string, req viewmodel.OperationRequest) (viewmodel.OperationAccepted, error)
	Lookup(ctx context.Context, id operations.ID) (viewmodel.Operation, error)
}

// Features are the console-wide switches the UI needs to know about.
type Features struct {
	Logs       bool `json:"logs" doc:"The console serves pod logs (ALLOW_LOGS); the per-Cluster logs capability still applies."`
	History    bool `json:"history" doc:"The console keeps a revision history."`
	Metrics    bool `json:"metrics" doc:"The console scrapes the instances' exporters."`
	Evidence   bool `json:"evidence" doc:"The console reads repository evidence from EvidenceAgents."`
	Operations bool `json:"operations" doc:"The operations endpoints are served; each still needs the caller's own RBAC."`
	Digest     bool `json:"digest" doc:"The console makes a daily digest (DIGEST_ENABLED)."`
	// MCP and MCPWriteTools are set by the server when it mounts the MCP
	// endpoint (internal/console/server), not by configuration alone.
	MCP           bool `json:"mcp" doc:"The console serves MCP (MCP_ENABLED): assistants can connect, and /api/v1/assistant-setup and /api/v1/session/grants answer."`
	MCPWriteTools bool `json:"mcpWriteTools" doc:"The MCP endpoint's operating tools are on (MCP_WRITE_TOOLS): a connected assistant may also request operations, as its person."`
}

type (
	vocabularyOutput  struct{ Body viewmodel.Vocabulary }
	replicationOutput struct{ Body viewmodel.Replication }
	backupsOutput     struct{ Body viewmodel.Backups }
	restoreOutput     struct{ Body viewmodel.RestoreCheck }
	storageOutput     struct{ Body viewmodel.Storage }
	objectsOutput     struct{ Body viewmodel.Objects }
	historyOutput     struct{ Body viewmodel.History }
	revisionOutput    struct{ Body viewmodel.Revision }
	metricsOutput     struct{ Body viewmodel.Metrics }
	topologyOutput    struct{ Body viewmodel.Topology }
	logTailOutput     struct{ Body viewmodel.LogTail }
	operationsOutput  struct{ Body viewmodel.Operations }
	operationOutput   struct{ Body viewmodel.Operation }
	adminOutput       struct{ Body viewmodel.Admin }
	compareOutput     struct{ Body viewmodel.Comparison }
	acceptedOutput    struct {
		Status int
		Body   viewmodel.OperationAccepted
	}
)

type topologyInput struct {
	ClusterPath
	Fold []string `query:"fold" maxItems:"4" enum:"pool,cluster,backup,kubeStorage" doc:"Frames to draw folded."`
}

type restoreCheckInput struct {
	ClusterPath
	At time.Time `query:"at" required:"true" doc:"The restore target, RFC 3339 (UTC recommended)."`
}

// compareInput names the two Clusters of a comparison.
type compareInput struct {
	LeftNamespace  string `query:"leftNamespace" required:"true" maxLength:"63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" doc:"Namespace of the left Cluster."`
	LeftName       string `query:"leftName" required:"true" maxLength:"253" pattern:"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" doc:"Name of the left Cluster."`
	RightNamespace string `query:"rightNamespace" required:"true" maxLength:"63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" doc:"Namespace of the right Cluster."`
	RightName      string `query:"rightName" required:"true" maxLength:"253" pattern:"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" doc:"Name of the right Cluster."`
}

type revisionPath struct {
	ClusterPath
	Seq int64 `path:"seq" minimum:"1" doc:"The revision's seq, from the history list."`
}

// PodPath identifies one instance of a Cluster.
type PodPath struct {
	ClusterPath
	Pod       string `path:"pod" maxLength:"253" pattern:"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" doc:"Instance (pod) name."`
	Container string `query:"container" maxLength:"63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" doc:"Container; default postgres."`
}

type logTailInput struct {
	PodPath
	Previous  bool `query:"previous" doc:"The previous (crashed) container's log."`
	TailLines int  `query:"tailLines" minimum:"1" maximum:"2000" doc:"Lines to read; default 500. The byte bound may cut fewer."`
}

// CSRFHeader documents and enforces the anti-CSRF custom header on
// every unsafe operation; auth.CSRF checks it (and the origin) before the
// request reaches Huma, so this field is the contract the generated
// client must honour.
type CSRFHeader struct {
	CSRF string `header:"X-PgToolBox-Request" required:"true" enum:"1" doc:"Always 1. Unsafe API requests without it are refused (403) before anything runs; the request must also be same-origin (Origin or Sec-Fetch-Site)."`
}

type requestOperationInput struct {
	ClusterPath
	CSRFHeader
	Body viewmodel.OperationRequest
}

type operationPath struct {
	ID string `path:"id" maxLength:"512" doc:"The operationId returned by POST .../operations."`
}

// registerScreens adds the per-screen read operations.
func registerScreens(a huma.API, d Deps, r *Reads) {
	huma.Register(a, huma.Operation{
		OperationID: OpGetVocabulary, Method: http.MethodGet, Path: "/api/" + Version + "/vocabulary",
		Summary: "The vocabulary the UI draws by", Tags: []string{"session"},
		Description: "Static: the health roll-up order (sort by index), the health of each source state, the cluster-relative sections a link may name.",
		Errors:      []int{http.StatusUnauthorized},
	}, func(ctx context.Context, _ *struct{}) (*vocabularyOutput, error) {
		v, err := r.Vocabulary(ctx)
		if err != nil {
			return nil, err
		}
		return &vocabularyOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpCompareClusters, Method: http.MethodGet, Path: "/api/" + Version + "/compare",
		Summary: "Two Clusters side by side", Tags: []string{"clusters"},
		Description: "Versions, instances, parameters, resources, storage, backup configuration and topology of two Clusters, " +
			"with the rows that differ marked. You must be allowed to get both (one SelfSubjectAccessReview each); " +
			"either refusal answers 403 whether the Cluster exists or not.",
		Errors: append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *compareInput) (*compareOutput, error) {
		v, err := r.Compare(ctx, ClusterPath{Namespace: in.LeftNamespace, Name: in.LeftName}, ClusterPath{Namespace: in.RightNamespace, Name: in.RightName})
		if err != nil {
			return nil, err
		}
		return &compareOutput{Body: v}, nil
	})

	registerCluster(a, d, OpGetReplication, "/replication", "A Cluster's replication",
		"Primary election, synchronous configuration and the set PostgreSQL applied (FailoverQuorum), the primary's walsenders, each replica's receiver and the replication slots, with the blind spots of /pg/status.",
		func(ctx context.Context, _ Source, ns, n string) (*replicationOutput, error) {
			v, err := r.Replication(ctx, ns, n)
			return &replicationOutput{Body: v}, err
		})
	registerCluster(a, d, OpGetBackups, "/backups", "A Cluster's backups",
		"Backup method, Backup objects, schedules, archiving, the recovery window and its timeline (gaps and failed backups marked), and the repository evidence cross-check.",
		func(ctx context.Context, _ Source, ns, n string) (*backupsOutput, error) {
			v, err := r.Backups(ctx, ns, n)
			return &backupsOutput{Body: v}, err
		})
	huma.Register(a, huma.Operation{
		OperationID: OpGetRestoreCheck, Method: http.MethodGet, Path: clusterPathPrefix + "/backups/restore-check",
		Summary: "Can this Cluster be restored to a time?", Tags: []string{"clusters"},
		Description: "Reads the answer off the recovery timeline of the backups view: covered (structurally: a base backup and " +
			"contiguous archived WAL span the time, judged from segment names), notCovered, gap, reported (only the operator's " +
			"unverified window covers it) or unknown, with the caveats the answer does not prove. It never runs a restore.",
		Errors: append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *restoreCheckInput) (*restoreOutput, error) {
		v, err := r.RestoreCheck(ctx, in.Namespace, in.Name, in.At)
		if err != nil {
			return nil, err
		}
		return &restoreOutput{Body: v}, nil
	})
	registerCluster(a, d, OpGetStorage, "/storage", "A Cluster's storage",
		"Declared volumes, PVCs with the operator's PVC group, volume usage where an instance reports it, and the forecast of when each volume fills.",
		func(ctx context.Context, _ Source, ns, n string) (*storageOutput, error) {
			v, err := r.Storage(ctx, ns, n)
			return &storageOutput{Body: v}, err
		})
	registerCluster(a, d, OpGetObjects, "/objects", "A Cluster's declarative objects",
		"Databases, DatabaseRoles, managed roles, tablespaces, publications, subscriptions, poolers and the image catalog.",
		func(ctx context.Context, s Source, ns, n string) (*objectsOutput, error) {
			v, err := s.Objects(ctx, ns, n)
			return &objectsOutput{Body: v}, err
		})
	registerCluster(a, d, OpListHistory, "/history", "A Cluster's history",
		"Revisions of the Cluster and its declarative children the console observed, newest first.",
		func(ctx context.Context, _ Source, ns, n string) (*historyOutput, error) {
			v, err := r.History(ctx, ns, n)
			return &historyOutput{Body: v}, err
		})
	registerCluster(a, d, OpGetMetrics, "/metrics", "A Cluster's metrics",
		"The latest exporter instants as tiles and the short retained series; the monitoring link-out when configured.",
		func(ctx context.Context, s Source, ns, n string) (*metricsOutput, error) {
			v, err := s.Metrics(ctx, ns, n)
			return &metricsOutput{Body: v}, err
		})
	huma.Register(a, huma.Operation{
		OperationID: OpGetTopology, Method: http.MethodGet, Path: clusterPathPrefix + "/topology",
		Summary: "A Cluster's wiring diagram", Tags: []string{"clusters"},
		Description: "The wiring diagram with its geometry computed by the console: draw it as given. " +
			"The frames named in fold are drawn folded, each as one summary box, and the drawing is laid out again around them.",
		Errors: append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *topologyInput) (*topologyOutput, error) {
		if _, err := authorizeCluster(ctx, d.Authz, &in.ClusterPath); err != nil {
			return nil, err
		}
		v, err := d.Source.Topology(ctx, in.Namespace, in.Name, in.Fold...)
		if err != nil {
			return nil, d.sourceError(ctx, err)
		}
		return &topologyOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpGetRevision, Method: http.MethodGet, Path: clusterPathPrefix + "/history/{seq}",
		Summary: "One revision with its diff", Tags: []string{"clusters"},
		Description: "The definition at the revision, the base it is compared with and the structural diff.",
		Errors:      append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *revisionPath) (*revisionOutput, error) {
		if _, err := authorizeCluster(ctx, d.Authz, &in.ClusterPath); err != nil {
			return nil, err
		}
		v, err := d.Source.Revision(ctx, in.Namespace, in.Name, in.Seq)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, huma.Error404NotFound("no such revision of this Cluster")
			}
			return nil, d.sourceError(ctx, err)
		}
		return &revisionOutput{Body: v}, nil
	})

	registerLogs(a, d, r)
	registerTimeline(a, r)
	registerOperations(a, r)

	huma.Register(a, huma.Operation{
		OperationID: OpGetAdmin, Method: http.MethodGet, Path: "/api/" + Version + "/admin",
		Summary: "PgAdmins, EvidenceAgents and console health", Tags: []string{"admin"},
		Description: "Per watched namespace, the PgAdmin and EvidenceAgent objects you may list there; the console's own health if you may list pods in its namespace.",
		Errors:      problemCodes,
	}, func(ctx context.Context, _ *struct{}) (*adminOutput, error) {
		p, err := principal(ctx)
		if err != nil {
			return nil, err
		}
		v, err := d.Source.Admin(ctx)
		if err != nil {
			return nil, huma.Error503ServiceUnavailable("the admin view is not available yet", err)
		}
		return &adminOutput{Body: filterAdmin(ctx, d.Authz, p, v)}, nil
	})
}

// filterAdmin keeps what the caller may list.
func filterAdmin(ctx context.Context, az Authorizer, p *auth.Principal, v viewmodel.Admin) viewmodel.Admin {
	out := viewmodel.Admin{GeneratedAt: v.GeneratedAt, Namespaces: make([]viewmodel.AdminNamespace, 0, len(v.Namespaces))}
	for _, ns := range v.Namespaces {
		row := viewmodel.AdminNamespace{Name: ns.Name, PgAdmins: []viewmodel.PgAdminRow{}, EvidenceAgents: []viewmodel.EvidenceAgentRow{}, Sources: ns.Sources}
		if az.Allowed(ctx, p.User, authz.Attributes{Namespace: ns.Name, Verb: "list", Group: authz.PgToolBoxGroup, Resource: "pgadmins"}).Allowed {
			row.PgAdminsVisible, row.PgAdmins = true, ns.PgAdmins
		}
		if az.Allowed(ctx, p.User, authz.Attributes{Namespace: ns.Name, Verb: "list", Group: authz.PgToolBoxGroup, Resource: "evidenceagents"}).Allowed {
			row.EvidenceAgentsVisible, row.EvidenceAgents = true, ns.EvidenceAgents
		}
		row.Sources = []viewmodel.SourceState{}
		for _, st := range ns.Sources {
			if (st.Source == "pgAdmins" && row.PgAdminsVisible) || (st.Source == "evidenceAgents" && row.EvidenceAgentsVisible) {
				row.Sources = append(row.Sources, st)
			}
		}
		out.Namespaces = append(out.Namespaces, row)
	}
	if v.Console != nil && v.Console.Namespace != "" &&
		az.Allowed(ctx, p.User, authz.Attributes{Namespace: v.Console.Namespace, Verb: "list", Resource: "pods"}).Allowed {
		out.Console = v.Console
	}
	return out
}

// pgAdminLinks keeps the pgAdmins the caller may open; unread when the
// PgAdmin list failed, so an empty list says nothing about pgAdmin.
func pgAdminLinks(ctx context.Context, d Deps, p *auth.Principal, ns, name string) (out []viewmodel.PgAdminLink, unread bool) {
	out = []viewmodel.PgAdminLink{}
	links, err := d.Source.PgAdminLinks(ctx, ns, name)
	if err != nil {
		// The view itself was served: the links alone are missing.
		unread = !errors.Is(err, ErrNotFound)
		links = nil
	}
	for _, l := range links {
		if d.Authz.Allowed(ctx, p.User, authz.Attributes{Namespace: ns, Verb: "get", Group: authz.PgToolBoxGroup, Resource: "pgadmins", Name: l.Name}).Allowed {
			out = append(out, l)
		}
	}
	if d.PgAdminURL != "" {
		// A pgAdmin installed without the operator: no
		// PgAdmin object says which Clusters it registers or how it is,
		// so the link is offered on every Cluster the caller may read,
		// with its health unknown. pgAdmin authenticates on its own; the
		// link grants nothing.
		out = append(out, viewmodel.PgAdminLink{Name: "pgadmin", URL: d.PgAdminURL, Health: viewmodel.HealthUnknown})
	}
	return out, unread
}

// authorizeLogs asks whether the caller may get the pod's log, after the
// Cluster itself.
func authorizeLogs(ctx context.Context, d Deps, in *PodPath) error {
	p, err := authorizeCluster(ctx, d.Authz, &in.ClusterPath)
	if err != nil {
		return err
	}
	if d.Logs == nil || !d.Features.Logs {
		return huma.Error501NotImplemented("this console is configured without log access (ALLOW_LOGS=false)")
	}
	dec := d.Authz.Allowed(ctx, p.User, authz.Attributes{Namespace: in.Namespace, Verb: "get", Resource: "pods", Subresource: "log", Name: in.Pod})
	switch {
	case dec.Err != nil:
		return huma.Error503ServiceUnavailable("the Kubernetes API server could not be asked whether you may read this log; retry")
	case !dec.Allowed:
		return huma.Error403Forbidden("you may not get pods/log " + in.Pod + " in namespace " + in.Namespace + ": ask for the pgtoolbox-console-viewer role there")
	}
	return nil
}

func logOptions(in *PodPath, previous bool, lines int) LogOptions {
	o := LogOptions{Container: in.Container, Previous: previous, TailLines: lines}
	if o.Container == "" {
		o.Container = "postgres"
	}
	if o.TailLines <= 0 {
		o.TailLines = defaultLogTailLines
	}
	o.TailLines = min(o.TailLines, maxLogTailLines)
	return o
}

func registerLogs(a huma.API, d Deps, r *Reads) {
	huma.Register(a, huma.Operation{
		OperationID: OpGetLogTail, Method: http.MethodGet, Path: clusterPathPrefix + "/instances/{pod}/logs",
		Summary: "The tail of an instance's log", Tags: []string{"logs"},
		Description: "Read once from the API server with the console's ServiceAccount after asking whether you may get pods/log; " +
			"bounded in lines and bytes. 501 when the console is configured without log access.",
		Errors: append([]int{http.StatusNotFound, http.StatusNotImplemented}, problemCodes...),
	}, func(ctx context.Context, in *logTailInput) (*logTailOutput, error) {
		v, err := r.LogTail(ctx, in.PodPath, in.Previous, in.TailLines)
		if err != nil {
			return nil, err
		}
		return &logTailOutput{Body: v}, nil
	})

	humasse.Register(a, huma.Operation{
		OperationID: OpStreamLogs, Method: http.MethodGet, Path: clusterPathPrefix + "/instances/{pod}/logs/stream",
		Summary: "Follow an instance's log", Tags: []string{"logs"},
		Description: "Server-sent events: line {at, text}, end {reason}; gap {at, reason} is reserved for lines known to be missed " +
			"(draw a marker, never join across it). Starts with the last 100 lines. A comment line every 15 s. The stream ends " +
			"when the session ends or is revoked, when your pods/log access is withdrawn (checked every 30 s), after an hour, " +
			"or when the container stops. A refusal (403, 501, 404) is sent as a single end event with the reason.",
		Errors: append([]int{http.StatusNotFound, http.StatusNotImplemented}, problemCodes...),
	}, map[string]any{
		"line": viewmodel.LogStreamLine{},
		"gap":  viewmodel.LogStreamGap{},
		"end":  viewmodel.LogStreamEnd{},
	}, func(ctx context.Context, in *PodPath, send humasse.Sender) {
		out := &lockedSender{send: send}
		if err := authorizeLogs(ctx, d, in); err != nil {
			_ = out.data(viewmodel.LogStreamEnd{Reason: err.Error()})
			return
		}
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return
		}
		if d.streams != nil {
			key := "logs/" + p.SessionID
			if !d.streams.inc(key) {
				_ = out.data(viewmodel.LogStreamEnd{Reason: "too many open log streams for this session"})
				return
			}
			defer d.streams.dec(key)
		}
		// The follow lives no longer than the session, and is cut when
		// the session is revoked or the pods/log grant withdrawn.
		ctx, cancel := context.WithDeadline(ctx, p.Expiry)
		defer cancel()
		var ended sync.Once
		endWith := func(reason string) {
			ended.Do(func() { _ = out.data(viewmodel.LogStreamEnd{Reason: reason}) })
			cancel()
		}
		go keepAlive(ctx, d, p, in, out, endWith)
		err := d.Logs.Follow(ctx, in.Namespace, in.Name, in.Pod, logOptions(in, false, 100), func(ev any) error {
			if end, isEnd := ev.(viewmodel.LogStreamEnd); isEnd {
				endWith(end.Reason)
				return nil
			}
			return out.data(ev)
		})
		switch {
		case err != nil && ctx.Err() == nil:
			endWith("the log could not be read: " + safeReason(err))
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			endWith("the session ended; sign in again to keep following")
		}
	})
}

// Log-stream liveness: a heartbeat comment, and the session and the
// pods/log grant re-checked (variables for the tests only).
var (
	logHeartbeat = 15 * time.Second
	logRecheck   = 30 * time.Second
)

func keepAlive(ctx context.Context, d Deps, p *auth.Principal, in *PodPath, out *lockedSender, endWith func(string)) {
	beat := time.NewTicker(logHeartbeat)
	defer beat.Stop()
	check := time.NewTicker(logRecheck)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			if out.comment("heartbeat") != nil {
				endWith("the client went away")
				return
			}
		case <-check.C:
			if d.Sessions != nil && !d.Sessions.StillValid(ctx, p) {
				endWith("the session ended or was revoked")
				return
			}
			dec := d.Authz.Allowed(ctx, p.User, authz.Attributes{Namespace: in.Namespace, Verb: "get", Resource: "pods", Subresource: "log", Name: in.Pod})
			if !dec.Allowed {
				endWith("your access to this log could not be confirmed any more")
				return
			}
		}
	}
}

// lockedSender serialises the follow's events and the heartbeat.
type lockedSender struct {
	mu   sync.Mutex
	send humasse.Sender
}

func (s *lockedSender) data(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send.Data(v)
}

func (s *lockedSender) comment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send.Comment(text)
}

func logError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return huma.Error404NotFound("no such instance or container in this Cluster")
	case errors.Is(err, ErrLogsDisabled):
		return huma.Error501NotImplemented("this console is configured without log access (ALLOW_LOGS=false)")
	default:
		return huma.Error503ServiceUnavailable("the log could not be read: " + safeReason(err))
	}
}

// safeReason is the error's closed, display-safe category when it has
// one.
func safeReason(err error) string {
	var s interface{ SafeText() string }
	if errors.As(err, &s) {
		return s.SafeText()
	}
	if errors.Is(err, ErrNotFound) {
		return "not found"
	}
	return "unavailable"
}

func registerOperations(a huma.API, r *Reads) {
	huma.Register(a, huma.Operation{
		OperationID: OpListOperations, Method: http.MethodGet, Path: clusterPathPrefix + "/operations",
		Summary: "What you may do with a Cluster", Tags: []string{"operations"},
		Description: "The operations offered to you here, each with the API server's answer for its verbs, the preconditions not met now, " +
			"the typed confirmation it needs and, for promote, the replica roster with eligibility; and the operations this console replica issued.",
		Errors: append([]int{http.StatusNotFound, http.StatusNotImplemented}, problemCodes...),
	}, func(ctx context.Context, in *ClusterPath) (*operationsOutput, error) {
		v, err := r.OperationOffers(ctx, in.Namespace, in.Name)
		if err != nil {
			return nil, err
		}
		return &operationsOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpRequestOperation, Method: http.MethodPost, Path: clusterPathPrefix + "/operations",
		Summary: "Request an operation", Tags: []string{"operations"},
		Description: "Pessimistic: the console re-checks the preconditions against a live read, then writes as you (your own token; " +
			"the API server authorizes and audits the write under your name). 202 means the API server accepted the write, not that " +
			"the operator acted: follow GET /api/v1/operations/{operationId}. 409: a precondition is not met or the Cluster changed " +
			"since it was read; 422: the confirmation does not match. Requires the X-PgToolBox-Request: 1 header and a same-origin request.",
		DefaultStatus: http.StatusAccepted,
		Errors:        append([]int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusNotImplemented}, problemCodes...),
	}, func(ctx context.Context, in *requestOperationInput) (*acceptedOutput, error) {
		v, err := r.RequestOperation(ctx, in.Namespace, in.Name, in.Body)
		if err != nil {
			return nil, err
		}
		return &acceptedOutput{Status: http.StatusAccepted, Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpGetOperation, Method: http.MethodGet, Path: "/api/" + Version + "/operations/{id}",
		Summary: "Track an operation", Tags: []string{"operations"},
		Description: "The operation's phase ladder, each step attributed to the field that showed it, resolved by any console replica " +
			"from the Cluster's current state. timedOut means no outcome was reported before the deadline, not that it failed; " +
			"notObservable means CloudNativePG reports no outcome for this operation at all (reload).",
		Errors: append([]int{http.StatusNotFound, http.StatusNotImplemented}, problemCodes...),
	}, func(ctx context.Context, in *operationPath) (*operationOutput, error) {
		v, err := r.Operation(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &operationOutput{Body: v}, nil
	})
}

// operationError maps the operation service's errors onto problems.
func operationError(err error) error {
	var rejected interface {
		HTTPStatus() int
		SafeText() string
	}
	switch {
	case errors.As(err, &rejected):
		return huma.NewError(rejected.HTTPStatus(), rejected.SafeText())
	case errors.Is(err, ErrNotFound):
		return huma.Error404NotFound("no such Cluster in the console's watched namespaces")
	default:
		return huma.Error503ServiceUnavailable("the operation could not be requested: " + safeReason(err))
	}
}
