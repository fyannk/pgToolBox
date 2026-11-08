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
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The incident timeline and the daily digest.
const (
	OpGetTimeline  = "getTimeline"
	OpListDigests  = "listDigests"
	OpGetDigest    = "getDigest"
	maxTimelineDay = 31 * 24 * time.Hour
	// defaultTimelineWindow is status.DefaultTimelineWindow: the window a
	// query without `from` covers, ending at `to` (or now).
	defaultTimelineWindow = 24 * time.Hour
)

// TimelineQuery selects part of a Cluster's timeline. Zero times default
// to the last 24 hours; Limit 0 is the default (500).
type TimelineQuery struct {
	From, To time.Time
	Kinds    []string
	Limit    int
}

// DigestArchive is every stored digest, newest first, before the API
// removes the Clusters the caller may not get.
type DigestArchive struct {
	GeneratedAt time.Time
	Schedule    viewmodel.DigestSchedule
	Items       []viewmodel.Digest
}

// ErrDigestDisabled is returned by Digests on a console without a digest.
var ErrDigestDisabled = errors.New("the daily digest is disabled on this console")

// TimelineSource is the read model behind the timeline and digest
// endpoints.
type TimelineSource interface {
	Timeline(ctx context.Context, namespace, name string, q TimelineQuery) (viewmodel.Timeline, error)
	Digests(ctx context.Context) (DigestArchive, error)
}

type timelineInput struct {
	ClusterPath
	From  time.Time `query:"from" doc:"Start of the window (RFC 3339, UTC); default 24 hours before to."`
	To    time.Time `query:"to" doc:"End of the window, exclusive (RFC 3339, UTC); default now."`
	Kind  []string  `query:"kind" maxItems:"9" enum:"phase,primary,restart,backup,finding,revision,event,gap,console" doc:"Kinds to keep, comma-separated; none keeps every kind."`
	Limit int       `query:"limit" minimum:"0" maximum:"2000" doc:"Most entries returned, newest first (default 500)."`
}

type digestPath struct {
	ID string `path:"id" pattern:"^[0-9]{8}T[0-9]{4}Z$" maxLength:"14" doc:"The digest's end time, such as 20261103T0700Z."`
}

type (
	timelineOutput   struct{ Body viewmodel.Timeline }
	digestListOutput struct{ Body viewmodel.DigestList }
	digestOutput     struct{ Body viewmodel.Digest }
)

func registerTimeline(a huma.API, r *Reads) {
	huma.Register(a, huma.Operation{
		OperationID: OpGetTimeline, Method: http.MethodGet, Path: clusterPathPrefix + "/timeline",
		Summary: "A Cluster's incident timeline", Tags: []string{"clusters"},
		Description: "One time axis of what the console observed and what Kubernetes reported: phase and primary changes, restarts, " +
			"backup results, findings appearing and clearing, spec revisions, Events, and the console's own gaps. " +
			"Every entry says where it comes from and whether its time was observed or reported.",
		Errors: append([]int{http.StatusNotFound, http.StatusUnprocessableEntity}, problemCodes...),
	}, func(ctx context.Context, in *timelineInput) (*timelineOutput, error) {
		v, err := r.Timeline(ctx, in.Namespace, in.Name, TimelineQuery{From: in.From, To: in.To, Kinds: in.Kind, Limit: in.Limit})
		if err != nil {
			return nil, err
		}
		return &timelineOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpListDigests, Method: http.MethodGet, Path: "/api/" + Version + "/digests",
		Summary: "Daily digests", Tags: []string{"digests"},
		Description: "The console's daily summaries, newest first, each counted over the Clusters you may get.",
		Errors:      problemCodes,
	}, func(ctx context.Context, _ *struct{}) (*digestListOutput, error) {
		v, err := r.Digests(ctx)
		if err != nil {
			return nil, err
		}
		return &digestListOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpGetDigest, Method: http.MethodGet, Path: "/api/" + Version + "/digests/{id}",
		Summary: "One daily digest", Tags: []string{"digests"},
		Description: "One digest with, per Cluster you may get, what happened in its 24 hours. Its URL is the link the delivered message carries.",
		Errors:      append([]int{http.StatusNotFound}, problemCodes...),
	}, func(ctx context.Context, in *digestPath) (*digestOutput, error) {
		v, err := r.Digest(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &digestOutput{Body: v}, nil
	})
}

// filterDigests keeps, in each digest, the Clusters the caller may get
// (one SelfSubjectAccessReview per Cluster, cached), recounts the
// headline over them and only says how many were left out.
func filterDigests(ctx context.Context, az Authorizer, p *auth.Principal, items []viewmodel.Digest) []viewmodel.Digest {
	seen := map[authz.ObjectRef]bool{}
	var refs []authz.ObjectRef
	for _, dg := range items {
		for _, c := range dg.Clusters {
			r := authz.ObjectRef{Namespace: c.Ref.Namespace, Name: c.Ref.Name}
			if !seen[r] {
				seen[r] = true
				refs = append(refs, r)
			}
		}
	}
	visible := map[authz.ObjectRef]bool{}
	if len(refs) > 0 {
		v, _, _ := az.VisibleClusters(ctx, p.User, refs)
		for _, r := range v {
			visible[r] = true
		}
	}
	out := make([]viewmodel.Digest, 0, len(items))
	for _, dg := range items {
		kept := dg
		kept.Clusters = make([]viewmodel.DigestCluster, 0, len(dg.Clusters))
		namespaces := map[string]bool{}
		for _, c := range dg.Clusters {
			if visible[authz.ObjectRef{Namespace: c.Ref.Namespace, Name: c.Ref.Name}] {
				kept.Clusters = append(kept.Clusters, c)
				namespaces[c.Ref.Namespace] = true
			}
		}
		kept.Hidden = len(dg.Clusters) - len(kept.Clusters)
		kept.ConsoleGaps = make([]viewmodel.TimelineEntry, 0, len(dg.ConsoleGaps))
		for _, g := range dg.ConsoleGaps {
			if g.Subject == nil || namespaces[g.Subject.Name] {
				kept.ConsoleGaps = append(kept.ConsoleGaps, g)
			}
		}
		kept.Recount()
		out = append(out, kept)
	}
	return out
}
