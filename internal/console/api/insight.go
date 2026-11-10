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
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The insight operations: what a Cluster's exporters report through the
// pgToolBox insight pack. The console runs no SQL for them; the blocking
// read is the same exporter read as the sweep, of the primary only, at
// most once a second per Cluster whoever asks.
const (
	OpGetInsight          = "getInsight"
	OpGetInsightBlocking  = "getInsightBlocking"
	OpGetFleetStatements  = "getFleetStatements"
	insightBlockingSuffix = "/insight/blocking"
)

// InsightSource is the read model of the insight screens.
type InsightSource interface {
	Insight(ctx context.Context, namespace, name string) (viewmodel.Insight, error)
	// InsightBlocking re-reads the primary's exporter first when
	// refresh is set (rate-limited by the source).
	InsightBlocking(ctx context.Context, namespace, name string, refresh bool) (viewmodel.InsightBlocking, error)
	// FleetStatements covers the watched Clusters that visible keeps
	// (those the caller may get), so its bounds are taken over them.
	FleetStatements(ctx context.Context, visible func([]viewmodel.ClusterRef) []viewmodel.ClusterRef) (viewmodel.FleetStatements, error)
}

// Each user may ask for refreshRequests on-demand reads per
// refreshWindow, across Clusters: each is an exporter read that, with a
// metricsQueriesTTL of 0, runs every pack query on the primary. The
// sweep also bounds them per Cluster and shares its read slots with
// them.
const (
	refreshRequests = 10
	refreshWindow   = 10 * time.Second
	// maxRefreshUsers bounds the users tracked; past it, expired windows
	// are dropped first.
	maxRefreshUsers = 4096
)

// refreshLimiter counts the on-demand reads per user in fixed windows.
type refreshLimiter struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]refreshCount
}

type refreshCount struct {
	start time.Time
	n     int
}

func newRefreshLimiter(now func() time.Time) *refreshLimiter {
	return &refreshLimiter{now: now, m: map[string]refreshCount{}}
}

// allow counts one read for user, false when over the limit.
func (l *refreshLimiter) allow(user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	c := l.m[user]
	if now.Sub(c.start) >= refreshWindow || now.Before(c.start) {
		if len(l.m) >= maxRefreshUsers {
			for u, o := range l.m {
				if now.Sub(o.start) >= refreshWindow {
					delete(l.m, u)
				}
			}
			if len(l.m) >= maxRefreshUsers {
				return false
			}
		}
		c = refreshCount{start: now}
	}
	if c.n >= refreshRequests {
		return false
	}
	c.n++
	l.m[user] = c
	return true
}

// InsightView is the /insight screens with the pgAdmins the caller may
// open: a statement's text, and ending a backend, are done there.
type InsightView struct {
	viewmodel.Insight
	PgAdmins       []viewmodel.PgAdminLink `json:"pgAdmins" doc:"pgAdmins this Cluster is registered in that you may open (connect pgadmins/<name>)."`
	PgAdminsUnread bool                    `json:"pgAdminsUnread,omitempty" doc:"The console could not read the PgAdmins: pgAdmins may be incomplete."`
}

// InsightBlockingView is the lock graph with the pgAdmins the caller may
// open.
type InsightBlockingView struct {
	viewmodel.InsightBlocking
	PgAdmins       []viewmodel.PgAdminLink `json:"pgAdmins" doc:"pgAdmins this Cluster is registered in that you may open: ending a backend is done there."`
	PgAdminsUnread bool                    `json:"pgAdminsUnread,omitempty" doc:"The console could not read the PgAdmins: pgAdmins may be incomplete."`
}

// FleetStatementsView is the team-wide query insight over the Clusters
// the caller may see.
type FleetStatementsView struct {
	viewmodel.FleetStatements
	Unchecked int `json:"unchecked,omitempty" doc:"How many Clusters were left out because the API server could not be asked whether you may see them."`
}

type (
	insightOutput         struct{ Body InsightView }
	insightBlockingOutput struct{ Body InsightBlockingView }
	fleetStatementsOutput struct{ Body FleetStatementsView }
)

type insightBlockingInput struct {
	ClusterPath
	Refresh bool `query:"refresh" doc:"Read the primary's exporter again first (at most once a second per Cluster, and ten times in ten seconds per user; the exporter itself caches for the Cluster's metricsQueriesTTL)."`
}

// registerInsight adds the insight operations.
func registerInsight(a huma.API, r *Reads) {
	huma.Register(a, huma.Operation{
		OperationID: OpGetInsight, Method: http.MethodGet, Path: clusterPathPrefix + "/insight",
		Summary: "A Cluster's insight pack", Tags: []string{"clusters"},
		Description: "Whether the Cluster runs the insight pack, and what it reports: wraparound and vacuum, " +
			"query insight from pg_stat_statements (no statement text), index hygiene, connections and the lock graph, " +
			"with the console's own derivations labelled as such.",
		Errors: append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *ClusterPath) (*insightOutput, error) {
		v, err := r.Insight(ctx, in.Namespace, in.Name)
		if err != nil {
			return nil, err
		}
		return &insightOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpGetInsightBlocking, Method: http.MethodGet, Path: clusterPathPrefix + insightBlockingSuffix,
		Summary: "A Cluster's lock graph", Tags: []string{"clusters"},
		Description: "Backends waiting on a lock and the backends they wait for, per instance, root blockers first. " +
			"With refresh, the primary's exporter is read again first. Nothing here ends a backend: the pgAdmin links do.",
		Errors: append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *insightBlockingInput) (*insightBlockingOutput, error) {
		v, err := r.InsightBlocking(ctx, in.Namespace, in.Name, in.Refresh)
		if err != nil {
			return nil, err
		}
		return &insightBlockingOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpGetFleetStatements, Method: http.MethodGet, Path: "/api/" + Version + "/insight/statements",
		Summary: "Query insight across your Clusters", Tags: []string{"clusters"},
		Description: "The costliest statements and the regressions of every Cluster you may get, " +
			"with each Cluster's coverage: whether it runs the pack and whether a regression comparison can be made yet.",
		Errors: problemCodes,
	}, func(ctx context.Context, _ *struct{}) (*fleetStatementsOutput, error) {
		v, err := r.FleetStatements(ctx)
		if err != nil {
			return nil, err
		}
		return &fleetStatementsOutput{Body: v}, nil
	})
}
