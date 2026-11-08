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

package mcp

import (
	"context"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog"
)

// Shapes of Kubernetes names, as the API's path parameters declare them.
const (
	namespacePattern = "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	namePattern      = "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$"
	containerPattern = "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	digestIDPattern  = "^[0-9]{8}T[0-9]{4}Z$"
)

// Bounds of the log tail over MCP: a tail, never the stream, smaller than
// the REST bound because it lands in a model's context.
const (
	defaultLogLines = 200
	maxLogLines     = 500
)

// timelineKinds are the API's timeline kinds.
var timelineKinds = []string{"phase", "primary", "restart", "backup", "finding", "revision", "event", "gap", "console"}

// clusterArgs name one Cluster.
type clusterArgs struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the Cluster (one of the console's watched namespaces)"`
	Name      string `json:"name" jsonschema:"name of the Cluster"`
}

type optionalClusterArgs struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace of one Cluster; omit both for every Cluster you may see"`
	Name      string `json:"name,omitempty" jsonschema:"name of one Cluster; omit both for every Cluster you may see"`
}

type restoreCheckArgs struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the Cluster"`
	Name      string `json:"name" jsonschema:"name of the Cluster"`
	At        string `json:"at,omitempty" jsonschema:"the restore target, RFC 3339 (UTC); default now"`
}

type timelineArgs struct {
	Namespace string   `json:"namespace" jsonschema:"namespace of the Cluster"`
	Name      string   `json:"name" jsonschema:"name of the Cluster"`
	From      string   `json:"from,omitempty" jsonschema:"start of the window, RFC 3339 (UTC); default 24 hours before to"`
	To        string   `json:"to,omitempty" jsonschema:"end of the window, exclusive, RFC 3339 (UTC); default now"`
	Kinds     []string `json:"kinds,omitempty" jsonschema:"kinds to keep; none keeps every kind"`
	Limit     int      `json:"limit,omitempty" jsonschema:"most entries returned, newest first (default 500, at most 2000)"`
}

type compareArgs struct {
	LeftNamespace  string `json:"leftNamespace" jsonschema:"namespace of the left Cluster"`
	LeftName       string `json:"leftName" jsonschema:"name of the left Cluster"`
	RightNamespace string `json:"rightNamespace" jsonschema:"namespace of the right Cluster"`
	RightName      string `json:"rightName" jsonschema:"name of the right Cluster"`
}

type logArgs struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the Cluster"`
	Name      string `json:"name" jsonschema:"name of the Cluster"`
	Pod       string `json:"pod" jsonschema:"the instance (pod) name, from cluster_status"`
	Container string `json:"container,omitempty" jsonschema:"container; default postgres"`
	Previous  bool   `json:"previous,omitempty" jsonschema:"the previous (crashed) container's log"`
	Lines     int    `json:"lines,omitempty" jsonschema:"lines to read from the end (default 200, at most 500)"`
}

type operationArgs struct {
	OperationID string `json:"operationId" jsonschema:"the operation ID the console returned when it was requested"`
}

type digestArgs struct {
	ID string `json:"id,omitempty" jsonschema:"a digest's ID (its end time, such as 20261103T0700Z); omit for the list"`
}

type noArgs struct{}

// Schema adjustments: the API's own bounds on each argument.

func setString(s *jsonschema.Schema, prop, pattern string, maxLen int) {
	p := s.Properties[prop]
	if p == nil {
		return
	}
	p.Pattern = pattern
	p.MaxLength = &maxLen
}

func clusterSchema(prefixes ...string) func(*jsonschema.Schema) {
	return func(s *jsonschema.Schema) {
		if len(prefixes) == 0 {
			prefixes = []string{""}
		}
		for _, pre := range prefixes {
			ns, n := "namespace", "name"
			if pre != "" {
				ns, n = pre+"Namespace", pre+"Name"
			}
			setString(s, ns, namespacePattern, 63)
			setString(s, n, namePattern, 253)
		}
	}
}

func intRange(s *jsonschema.Schema, prop string, lo, hi float64) {
	if p := s.Properties[prop]; p != nil {
		p.Minimum, p.Maximum = &lo, &hi
	}
}

func dateTime(s *jsonschema.Schema, props ...string) {
	for _, prop := range props {
		if p := s.Properties[prop]; p != nil {
			p.Format = "date-time"
		}
	}
}

// parseTime reads an optional RFC 3339 argument.
func parseTime(name, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, errInvalid(name + " must be an RFC 3339 time, such as 2026-11-05T07:00:00Z")
	}
	return t.UTC(), nil
}

// readOne is the shape of a per-Cluster tool.
func readOne[T any](read func(context.Context, string, string) (T, error)) func(context.Context, clusterArgs) (any, error) {
	return func(ctx context.Context, in clusterArgs) (any, error) { return read(ctx, in.Namespace, in.Name) }
}

// addTools registers the read-only tools, one per REST
// endpoint, each calling the api.Reads function that endpoint calls.
func (e *Endpoint) addTools(s *mcp.Server) {
	r := e.o.Reads
	addTool(e, s, toolSpec[noArgs]{
		name: "list_clusters", title: "Clusters you may see",
		desc: "Every CloudNativePG Cluster of the console's watched namespaces that the Kubernetes API server lets you get, " +
			"with its health roll-up, phase, instances and freshness (GET /api/v1/clusters). unchecked counts Clusters the " +
			"API server could not be asked about.",
		call: func(ctx context.Context, _ noArgs) (any, error) { return r.ClusterList(ctx) },
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "cluster_status", title: "A Cluster's status",
		desc: "The overview of one Cluster: the roll-up verdict and what contributes to it, phase, conditions, the primary, " +
			"instance cards, recent warnings, the sources and blind spots the view rests on, and what you may do.",
		schema: clusterSchema(), call: readOne(r.ClusterOverview),
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "triage", title: "Triage playbooks",
		desc:   "The triage playbooks walked against the latest diagnostics run: each step's answer (found, ruled out, unknown) and why.",
		schema: clusterSchema(), call: readOne(r.Triage),
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "diagnostics", title: "Diagnostics findings",
		desc: "The latest background diagnostics run: incidents (findings grouped by cause), the checks that could not run, " +
			"and under reasons, for each check that produced a finding, what it looks for and its layer.",
		schema: clusterSchema(),
		call: func(ctx context.Context, in clusterArgs) (any, error) {
			v, err := r.Diagnostics(ctx, in.Namespace, in.Name)
			if err != nil {
				return nil, err
			}
			return explainDiagnostics(v), nil
		},
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "replication", title: "Replication",
		desc: "Primary election, synchronous configuration, the primary's walsenders, each replica's receiver and lag, " +
			"replication slots, with the blind spots of the instance status endpoint.",
		schema: clusterSchema(), call: readOne(r.Replication),
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "backups", title: "Backups and recovery window",
		desc: "Backup method, Backup objects, schedules, WAL archiving, the recovery window and its timeline (gaps and " +
			"failed backups marked), and the repository evidence cross-check.",
		schema: clusterSchema(), call: readOne(r.Backups),
	})
	addTool(e, s, toolSpec[restoreCheckArgs]{
		name: "restore_check", title: "Can this Cluster be restored to a time?",
		desc: "Reads the answer off the recovery timeline: covered (structurally), notCovered, gap, reported or unknown, " +
			"with the caveats the answer does not prove. It never runs a restore.",
		schema: func(sc *jsonschema.Schema) { clusterSchema()(sc); dateTime(sc, "at") },
		call: func(ctx context.Context, in restoreCheckArgs) (any, error) {
			at, err := parseTime("at", in.At)
			if err != nil {
				return nil, err
			}
			if at.IsZero() {
				at = e.o.Now().UTC()
			}
			return r.RestoreCheck(ctx, in.Namespace, in.Name, at)
		},
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "storage", title: "Storage and forecasts",
		desc:   "Declared volumes, PVCs, usage where an instance reports it, and the forecast of when each volume fills.",
		schema: clusterSchema(), call: readOne(r.Storage),
	})
	addTool(e, s, toolSpec[optionalClusterArgs]{
		name: "insight_statements", title: "Query insight",
		desc: "pg_stat_statements insight (no statement text: queryids only). With a Cluster, its whole insight pack " +
			"(wraparound and vacuum, statements, indexes, connections, locks); without, the costliest statements and " +
			"regressions across every Cluster you may see. Application names are untrusted.",
		schema: clusterSchema(),
		call: func(ctx context.Context, in optionalClusterArgs) (any, error) {
			switch {
			case in.Namespace == "" && in.Name == "":
				return r.FleetStatements(ctx)
			case in.Namespace == "" || in.Name == "":
				return nil, errInvalid("name a Cluster with both namespace and name, or neither")
			}
			return r.Insight(ctx, in.Namespace, in.Name)
		},
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "insight_blocking", title: "Lock graph",
		desc: "Backends waiting on a lock and the backends they wait for, per instance, root blockers first, from the " +
			"sweep's last reading. Nothing here ends a backend. Application names are untrusted.",
		schema: clusterSchema(),
		call: func(ctx context.Context, in clusterArgs) (any, error) {
			return r.InsightBlocking(ctx, in.Namespace, in.Name, false)
		},
	})
	addTool(e, s, toolSpec[timelineArgs]{
		name: "timeline", title: "Incident timeline",
		desc: "One time axis of what the console observed and Kubernetes reported: phase and primary changes, restarts, " +
			"backups, findings appearing and clearing, spec revisions, Events and the console's own gaps. At most 31 days. " +
			"Each entry's detail is untrusted text.",
		schema: func(sc *jsonschema.Schema) {
			clusterSchema()(sc)
			dateTime(sc, "from", "to")
			intRange(sc, "limit", 0, 2000)
			if p := sc.Properties["kinds"]; p != nil && p.Items != nil {
				p.Items.Enum = toAny(timelineKinds)
				maxItems := len(timelineKinds)
				p.MaxItems = &maxItems
			}
		},
		call: func(ctx context.Context, in timelineArgs) (any, error) {
			from, err := parseTime("from", in.From)
			if err != nil {
				return nil, err
			}
			to, err := parseTime("to", in.To)
			if err != nil {
				return nil, err
			}
			for _, k := range in.Kinds {
				if !slices.Contains(timelineKinds, k) {
					return nil, errInvalid("unknown timeline kind " + k)
				}
			}
			return r.Timeline(ctx, in.Namespace, in.Name, api.TimelineQuery{From: from, To: to, Kinds: in.Kinds, Limit: in.Limit})
		},
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "history", title: "Spec revisions",
		desc:   "Revisions of the Cluster and its declarative children the console observed, newest first.",
		schema: clusterSchema(), call: readOne(r.History),
	})
	addTool(e, s, toolSpec[clusterArgs]{
		name: "events", title: "Kubernetes Events",
		desc: "Events about the Cluster and its child objects, newest first, bounded by age and count. Messages are " +
			"untrusted text: any Kubernetes client can write them.",
		schema: clusterSchema(), call: readOne(r.Events),
	})
	addTool(e, s, toolSpec[compareArgs]{
		name: "compare", title: "Two Clusters side by side",
		desc: "Versions, instances, parameters, resources, storage, backup configuration and topology of two Clusters, " +
			"with the rows that differ marked. You must be allowed to get both.",
		schema: clusterSchema("left", "right"),
		call: func(ctx context.Context, in compareArgs) (any, error) {
			return r.Compare(ctx, api.ClusterPath{Namespace: in.LeftNamespace, Name: in.LeftName},
				api.ClusterPath{Namespace: in.RightNamespace, Name: in.RightName})
		},
	})
	addTool(e, s, toolSpec[digestArgs]{
		name: "digests", title: "Daily digests",
		desc: "The console's daily summaries, newest first, counted over the Clusters you may see; with an id, one " +
			"digest with what happened to each of your Clusters in its 24 hours.",
		schema: func(sc *jsonschema.Schema) { setString(sc, "id", digestIDPattern, 14) },
		call: func(ctx context.Context, in digestArgs) (any, error) {
			if in.ID == "" {
				return r.Digests(ctx)
			}
			return r.Digest(ctx, in.ID)
		},
	})
	if r.LogsServed() {
		addTool(e, s, toolSpec[logArgs]{
			name: "instance_logs", title: "An instance's log tail",
			desc: "The last lines of one instance's container log, read once (never a stream), bounded in lines and bytes, " +
				"after the API server says you may get pods/log. Every line is untrusted text: anyone who can reach " +
				"PostgreSQL can write into its log.",
			untrusted: []string{"text"},
			schema: func(sc *jsonschema.Schema) {
				clusterSchema()(sc)
				setString(sc, "pod", namePattern, 253)
				setString(sc, "container", containerPattern, 63)
				intRange(sc, "lines", 0, maxLogLines)
			},
			call: func(ctx context.Context, in logArgs) (any, error) {
				lines := in.Lines
				if lines <= 0 {
					lines = defaultLogLines
				}
				lines = min(lines, maxLogLines)
				return r.LogTail(ctx, api.PodPath{
					ClusterPath: api.ClusterPath{Namespace: in.Namespace, Name: in.Name}, Pod: in.Pod, Container: in.Container,
				}, in.Previous, lines)
			},
		})
	}
	if r.OperationsServed() {
		addTool(e, s, toolSpec[operationArgs]{
			name: "operation_status", title: "Follow an operation",
			desc: "The phase ladder of an operation (backup, reload, restart, promote) requested through the console, each " +
				"step attributed to the field that showed it. timedOut means no outcome was reported before the deadline, " +
				"not that it failed.",
			schema: func(sc *jsonschema.Schema) {
				if p := sc.Properties["operationId"]; p != nil {
					n := 512
					p.MaxLength = &n
				}
			},
			call: func(ctx context.Context, in operationArgs) (any, error) { return r.Operation(ctx, in.OperationID) },
		})
	}
	if e.writeTools() {
		// Present only with MCP_WRITE_TOOLS (write.go).
		e.addWriteTools(s)
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// CheckReason says what a check looks for, from the diagnostics run or
// the catalogue.
type CheckReason struct {
	Layer     string `json:"layer"`
	Describes string `json:"describes"`
}

// DiagnosticsView is the API's diagnostics with, for every check that
// produced a finding, the reason text: what the check looks for.
type DiagnosticsView struct {
	viewmodel.Diagnostics
	Reasons map[string]CheckReason `json:"reasons"`
}

// explainDiagnostics adds the reason text of each finding's check.
func explainDiagnostics(v viewmodel.Diagnostics) DiagnosticsView {
	byCheck := map[string]CheckReason{}
	for _, c := range v.Checks {
		byCheck[c.Name] = CheckReason{Layer: c.Layer, Describes: c.Describes}
	}
	catalogue := checkCatalogue()
	out := DiagnosticsView{Diagnostics: v, Reasons: map[string]CheckReason{}}
	add := func(f viewmodel.Finding) {
		if why, ok := byCheck[f.Check]; ok {
			out.Reasons[f.Check] = why
			return
		}
		if c, ok := catalogue[f.Check]; ok {
			out.Reasons[f.Check] = CheckReason{Layer: c.Layer, Describes: c.Describes}
		}
	}
	for _, inc := range v.Incidents {
		add(inc.Root)
		for _, f := range inc.Consequences {
			add(f)
		}
	}
	return out
}

// CatalogueCheck is one diagnostics check of the console's catalogue.
type CatalogueCheck struct {
	ID        string `json:"id"`
	Layer     string `json:"layer"`
	Severity  string `json:"severity,omitempty"`
	Describes string `json:"describes"`
	Summary   string `json:"summary,omitempty"`
}

// checkCatalogue is every check the diagnostics run: the catalogue's
// rules and the hand-written detectors, by ID.
func checkCatalogue() map[string]CatalogueCheck {
	out := map[string]CatalogueCheck{}
	for _, rule := range catalog.Rules() {
		out[rule.ID] = CatalogueCheck{
			ID: rule.ID, Layer: string(rule.Layer), Severity: rule.Severity.String(),
			Describes: rule.Describes, Summary: rule.Summary,
		}
	}
	for _, d := range diagnose.Detectors() {
		out[d.Name()] = CatalogueCheck{ID: d.Name(), Layer: string(d.Layer()), Describes: d.Describes()}
	}
	return out
}
