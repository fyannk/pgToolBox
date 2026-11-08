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
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/operations"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Reads are the API's read operations as functions. The REST routes are
// thin adapters over them, and the MCP endpoint (internal/console/mcp)
// calls the same functions, so both faces authorize and read
// through one code path: a Cluster the caller may not get is refused by
// the same SelfSubjectAccessReview whichever face asked.
//
// Every method acts as the principal of ctx (auth.WithPrincipal) and
// returns the API's errors: a huma.StatusError carrying the status and
// the display-safe text the REST face answers with.
type Reads struct{ d Deps }

// ClusterList is GET /clusters: every Cluster of the watched namespaces
// the caller may get, and the namespaces' states they may see.
func (r *Reads) ClusterList(ctx context.Context) (ClusterListView, error) {
	d := r.d
	p, err := principal(ctx)
	if err != nil {
		return ClusterListView{}, err
	}
	list, err := d.Source.ClusterList(ctx)
	if err != nil {
		return ClusterListView{}, huma.Error503ServiceUnavailable("the cluster list is not available yet", err)
	}
	refs := make([]authz.ObjectRef, len(list.Items))
	for i, row := range list.Items {
		refs[i] = authz.ObjectRef{Namespace: row.Ref.Namespace, Name: row.Ref.Name}
	}
	visible, undecided, _ := d.Authz.VisibleClusters(ctx, p.User, refs)
	keep := make(map[authz.ObjectRef]bool, len(visible))
	for _, r := range visible {
		keep[r] = true
	}
	out := ClusterListView{ClusterList: list, Unchecked: len(undecided)}
	out.Items = make([]viewmodel.ClusterRow, 0, len(visible))
	seen := map[string]bool{}
	for _, row := range list.Items {
		if keep[authz.ObjectRef{Namespace: row.Ref.Namespace, Name: row.Ref.Name}] {
			out.Items = append(out.Items, row)
			seen[row.Ref.Namespace] = true
		}
	}
	// A namespace's state (and the console's reasons for not reading
	// it) is shown to those who see a Cluster there or may list them.
	out.Namespaces = make([]viewmodel.NamespaceState, 0, len(list.Namespaces))
	for _, ns := range list.Namespaces {
		if seen[ns.Name] || d.Authz.Allowed(ctx, p.User, authz.Attributes{
			Namespace: ns.Name, Verb: "list", Group: authz.CNPGGroup, Resource: "clusters",
		}).Allowed {
			out.Namespaces = append(out.Namespaces, ns)
		}
	}
	return out, nil
}

// ClusterOverview is GET /namespaces/{namespace}/clusters/{name}.
func (r *Reads) ClusterOverview(ctx context.Context, namespace, name string) (ClusterOverviewView, error) {
	d := r.d
	p, err := authorizeCluster(ctx, d.Authz, &ClusterPath{Namespace: namespace, Name: name})
	if err != nil {
		return ClusterOverviewView{}, err
	}
	ov, err := d.Source.ClusterOverview(ctx, namespace, name)
	if err != nil {
		return ClusterOverviewView{}, d.sourceError(ctx, err)
	}
	caps := d.Authz.ClusterCapabilities(ctx, p.User, namespace, name)
	if !d.Features.Logs {
		caps.Logs = false
	}
	links, unread := pgAdminLinks(ctx, d, p, namespace, name)
	return ClusterOverviewView{ClusterOverview: ov, Capabilities: caps, PgAdmins: links, PgAdminsUnread: unread}, nil
}

// readCluster authorizes the caller on one Cluster, then reads it: the
// shape of every per-Cluster sub-view.
func readCluster[T any](ctx context.Context, d Deps, namespace, name string, read func(context.Context, Source, string, string) (T, error)) (T, error) {
	var zero T
	if _, err := authorizeCluster(ctx, d.Authz, &ClusterPath{Namespace: namespace, Name: name}); err != nil {
		return zero, err
	}
	v, err := read(ctx, d.Source, namespace, name)
	if err != nil {
		return zero, d.sourceError(ctx, err)
	}
	return v, nil
}

// Events is GET .../events.
func (r *Reads) Events(ctx context.Context, namespace, name string) (viewmodel.EventList, error) {
	return readCluster(ctx, r.d, namespace, name, func(ctx context.Context, s Source, ns, n string) (viewmodel.EventList, error) {
		return s.Events(ctx, ns, n)
	})
}

// Diagnostics is GET .../diagnostics.
func (r *Reads) Diagnostics(ctx context.Context, namespace, name string) (viewmodel.Diagnostics, error) {
	return readCluster(ctx, r.d, namespace, name, func(ctx context.Context, s Source, ns, n string) (viewmodel.Diagnostics, error) {
		return s.Diagnostics(ctx, ns, n)
	})
}

// Triage is GET .../triage.
func (r *Reads) Triage(ctx context.Context, namespace, name string) (viewmodel.Triage, error) {
	return readCluster(ctx, r.d, namespace, name, func(ctx context.Context, s Source, ns, n string) (viewmodel.Triage, error) {
		return s.Triage(ctx, ns, n)
	})
}

// Replication is GET .../replication.
func (r *Reads) Replication(ctx context.Context, namespace, name string) (viewmodel.Replication, error) {
	return readCluster(ctx, r.d, namespace, name, func(ctx context.Context, s Source, ns, n string) (viewmodel.Replication, error) {
		return s.Replication(ctx, ns, n)
	})
}

// Backups is GET .../backups.
func (r *Reads) Backups(ctx context.Context, namespace, name string) (viewmodel.Backups, error) {
	return readCluster(ctx, r.d, namespace, name, func(ctx context.Context, s Source, ns, n string) (viewmodel.Backups, error) {
		return s.Backups(ctx, ns, n)
	})
}

// Storage is GET .../storage.
func (r *Reads) Storage(ctx context.Context, namespace, name string) (viewmodel.Storage, error) {
	return readCluster(ctx, r.d, namespace, name, func(ctx context.Context, s Source, ns, n string) (viewmodel.Storage, error) {
		return s.Storage(ctx, ns, n)
	})
}

// History is GET .../history.
func (r *Reads) History(ctx context.Context, namespace, name string) (viewmodel.History, error) {
	return readCluster(ctx, r.d, namespace, name, func(ctx context.Context, s Source, ns, n string) (viewmodel.History, error) {
		return s.History(ctx, ns, n)
	})
}

// RestoreCheck is GET .../backups/restore-check.
func (r *Reads) RestoreCheck(ctx context.Context, namespace, name string, at time.Time) (viewmodel.RestoreCheck, error) {
	return readCluster(ctx, r.d, namespace, name, func(ctx context.Context, s Source, ns, n string) (viewmodel.RestoreCheck, error) {
		return s.RestoreCheck(ctx, ns, n, at)
	})
}

// Compare is GET /compare: both sides are authorized before either is
// read, and either refusal is a 403 whether the Cluster exists or not.
func (r *Reads) Compare(ctx context.Context, left, right ClusterPath) (viewmodel.Comparison, error) {
	d := r.d
	for _, side := range []ClusterPath{left, right} {
		if _, err := authorizeCluster(ctx, d.Authz, &side); err != nil {
			return viewmodel.Comparison{}, err
		}
	}
	v, err := d.Source.Compare(ctx, left.Namespace, left.Name, right.Namespace, right.Name)
	if err != nil {
		return viewmodel.Comparison{}, d.sourceError(ctx, err)
	}
	return v, nil
}

// Vocabulary is GET /vocabulary (static, behind sign-in).
func (r *Reads) Vocabulary(ctx context.Context) (viewmodel.Vocabulary, error) {
	if _, err := principal(ctx); err != nil {
		return viewmodel.Vocabulary{}, err
	}
	return viewmodel.NewVocabulary(), nil
}

// Insight is GET .../insight.
func (r *Reads) Insight(ctx context.Context, namespace, name string) (InsightView, error) {
	d := r.d
	p, err := authorizeCluster(ctx, d.Authz, &ClusterPath{Namespace: namespace, Name: name})
	if err != nil {
		return InsightView{}, err
	}
	v, err := d.Source.Insight(ctx, namespace, name)
	if err != nil {
		return InsightView{}, d.sourceError(ctx, err)
	}
	links, unread := pgAdminLinks(ctx, d, p, namespace, name)
	return InsightView{Insight: v, PgAdmins: links, PgAdminsUnread: unread}, nil
}

// InsightBlocking is GET .../insight/blocking. refresh re-reads the
// primary's exporter first, within the caller's refresh budget.
func (r *Reads) InsightBlocking(ctx context.Context, namespace, name string, refresh bool) (InsightBlockingView, error) {
	d := r.d
	p, err := authorizeCluster(ctx, d.Authz, &ClusterPath{Namespace: namespace, Name: name})
	if err != nil {
		return InsightBlockingView{}, err
	}
	limited := false
	if refresh && d.refreshes != nil && !d.refreshes.allow(p.User.Name) {
		refresh, limited = false, true
	}
	v, err := d.Source.InsightBlocking(ctx, namespace, name, refresh)
	if err != nil {
		return InsightBlockingView{}, d.sourceError(ctx, err)
	}
	if limited && v.Available {
		v.Text = "Not read again: you asked for more than " + strconv.Itoa(refreshRequests) + " refreshes in " +
			refreshWindow.String() + "; this is the sweep's last reading."
	}
	links, unread := pgAdminLinks(ctx, d, p, namespace, name)
	return InsightBlockingView{InsightBlocking: v, PgAdmins: links, PgAdminsUnread: unread}, nil
}

// FleetStatements is GET /insight/statements: the source builds the view
// over the visible Clusters only, so its cut to the costliest statements
// is taken after filtering.
func (r *Reads) FleetStatements(ctx context.Context) (FleetStatementsView, error) {
	d := r.d
	p, err := principal(ctx)
	if err != nil {
		return FleetStatementsView{}, err
	}
	unchecked := 0
	visible := func(all []viewmodel.ClusterRef) []viewmodel.ClusterRef {
		refs := make([]authz.ObjectRef, len(all))
		for i, c := range all {
			refs[i] = authz.ObjectRef{Namespace: c.Namespace, Name: c.Name}
		}
		seen, undecided, _ := d.Authz.VisibleClusters(ctx, p.User, refs)
		unchecked = len(undecided)
		keep := make(map[authz.ObjectRef]bool, len(seen))
		for _, r := range seen {
			keep[r] = true
		}
		out := make([]viewmodel.ClusterRef, 0, len(seen))
		for _, c := range all {
			if keep[authz.ObjectRef{Namespace: c.Namespace, Name: c.Name}] {
				out = append(out, c)
			}
		}
		return out
	}
	v, err := d.Source.FleetStatements(ctx, visible)
	if err != nil {
		return FleetStatementsView{}, huma.Error503ServiceUnavailable("query insight is not available yet", err)
	}
	return FleetStatementsView{FleetStatements: v, Unchecked: unchecked}, nil
}

// Timeline is GET .../timeline. Zero times default to the last 24 hours
// ending now; the window is at most 31 days.
func (r *Reads) Timeline(ctx context.Context, namespace, name string, q TimelineQuery) (viewmodel.Timeline, error) {
	d := r.d
	if _, err := authorizeCluster(ctx, d.Authz, &ClusterPath{Namespace: namespace, Name: name}); err != nil {
		return viewmodel.Timeline{}, err
	}
	// The bound holds for the window the defaults make too: `from`
	// alone runs to now, however long ago it is.
	to, from := q.To, q.From
	if to.IsZero() {
		to = d.now()
	}
	if from.IsZero() {
		from = to.Add(-defaultTimelineWindow)
	}
	switch {
	case !from.Before(to):
		return viewmodel.Timeline{}, huma.Error422UnprocessableEntity("from must be before to")
	case to.Sub(from) > maxTimelineDay:
		return viewmodel.Timeline{}, huma.Error422UnprocessableEntity("the window is at most 31 days")
	}
	v, err := d.Source.Timeline(ctx, namespace, name, q)
	if err != nil {
		return viewmodel.Timeline{}, d.sourceError(ctx, err)
	}
	return v, nil
}

// Digests is GET /digests: every stored digest, each counted over the
// Clusters the caller may get.
func (r *Reads) Digests(ctx context.Context) (viewmodel.DigestList, error) {
	d := r.d
	p, err := principal(ctx)
	if err != nil {
		return viewmodel.DigestList{}, err
	}
	out := viewmodel.DigestList{Items: []viewmodel.DigestSummary{}}
	arch, err := d.Source.Digests(ctx)
	out.GeneratedAt = arch.GeneratedAt
	switch {
	case errors.Is(err, ErrDigestDisabled):
		out.Source = viewmodel.SourceState{Source: "digest", State: viewmodel.SourceDisabled, Reason: "DIGEST_ENABLED is false"}
		return out, nil
	case err != nil:
		return viewmodel.DigestList{}, huma.Error503ServiceUnavailable("the digests are not available yet", err)
	}
	out.Source = viewmodel.SourceState{Source: "digest", State: viewmodel.SourceLive}
	out.Schedule = arch.Schedule
	for _, dg := range filterDigests(ctx, d.Authz, p, arch.Items) {
		out.Items = append(out.Items, dg.DigestSummary)
	}
	return out, nil
}

// Digest is GET /digests/{id}.
func (r *Reads) Digest(ctx context.Context, id string) (viewmodel.Digest, error) {
	d := r.d
	p, err := principal(ctx)
	if err != nil {
		return viewmodel.Digest{}, err
	}
	arch, err := d.Source.Digests(ctx)
	switch {
	case errors.Is(err, ErrDigestDisabled):
		return viewmodel.Digest{}, huma.Error404NotFound("the daily digest is disabled on this console")
	case err != nil:
		return viewmodel.Digest{}, huma.Error503ServiceUnavailable("the digests are not available yet", err)
	}
	for _, dg := range arch.Items {
		if dg.ID == id {
			return filterDigests(ctx, d.Authz, p, []viewmodel.Digest{dg})[0], nil
		}
	}
	return viewmodel.Digest{}, huma.Error404NotFound("no such digest: it may have left the retention")
}

// LogTail is GET .../instances/{pod}/logs: a bounded tail read once,
// after the Cluster and pods/log are authorized. lines <= 0 is the
// default (500); more than 2000 is cut to 2000.
func (r *Reads) LogTail(ctx context.Context, pod PodPath, previous bool, lines int) (viewmodel.LogTail, error) {
	d := r.d
	if err := authorizeLogs(ctx, d, &pod); err != nil {
		return viewmodel.LogTail{}, err
	}
	v, err := d.Logs.Tail(ctx, pod.Namespace, pod.Name, pod.Pod, logOptions(&pod, previous, lines))
	if err != nil {
		return viewmodel.LogTail{}, logError(err)
	}
	return v, nil
}

// LogsServed reports whether this console serves pod logs at all.
func (r *Reads) LogsServed() bool { return r.d.Logs != nil && r.d.Features.Logs }

// OperationsServed reports whether this console serves operations at all.
func (r *Reads) OperationsServed() bool { return r.d.Operator != nil }

// DigestServed reports whether this console makes a daily digest.
func (r *Reads) DigestServed() bool { return r.d.Features.Digest }

// Operation is GET /operations/{id}: the Cluster the ID names is
// authorized before the tracker is consulted.
func (r *Reads) Operation(ctx context.Context, rawID string) (viewmodel.Operation, error) {
	d := r.d
	id, err := operations.ParseID(rawID)
	if err != nil {
		return viewmodel.Operation{}, huma.Error404NotFound("no such operation")
	}
	if _, err := authorizeCluster(ctx, d.Authz, &ClusterPath{Namespace: id.Namespace, Name: id.Cluster}); err != nil {
		return viewmodel.Operation{}, err
	}
	if d.Operator == nil {
		return viewmodel.Operation{}, huma.Error501NotImplemented("operations are not served by this console")
	}
	v, err := d.Operator.Lookup(ctx, id)
	if err != nil {
		return viewmodel.Operation{}, d.sourceError(ctx, err)
	}
	return v, nil
}

// OperationOffers is GET /operations: what the caller may do with a
// Cluster, each offer with the API server's answer for its verbs and the
// preconditions not met now.
func (r *Reads) OperationOffers(ctx context.Context, namespace, name string) (viewmodel.Operations, error) {
	d := r.d
	p, err := authorizeCluster(ctx, d.Authz, &ClusterPath{Namespace: namespace, Name: name})
	if err != nil {
		return viewmodel.Operations{}, err
	}
	if d.Operator == nil {
		return viewmodel.Operations{}, huma.Error501NotImplemented("operations are not served by this console")
	}
	caps := d.Authz.ClusterCapabilities(ctx, p.User, namespace, name)
	v, err := d.Operator.Offers(ctx, p, caps, namespace, name)
	if err != nil {
		return viewmodel.Operations{}, d.sourceError(ctx, err)
	}
	return v, nil
}

// RequestOperation is POST /operations: the typed confirmation, a live
// read as the caller, the preconditions on that read, then the write as
// the caller (the API server authorizes and audits it under their name),
// the console's audit line and the operation tracker. The MCP write
// tools call it too: one operation path whichever face
// asked, and the context's operations.Origin says which one did.
func (r *Reads) RequestOperation(ctx context.Context, namespace, name string, req viewmodel.OperationRequest) (viewmodel.OperationAccepted, error) {
	d := r.d
	p, err := authorizeCluster(ctx, d.Authz, &ClusterPath{Namespace: namespace, Name: name})
	if err != nil {
		return viewmodel.OperationAccepted{}, err
	}
	if d.Operator == nil {
		return viewmodel.OperationAccepted{}, huma.Error501NotImplemented("operations are not served by this console")
	}
	v, err := d.Operator.Request(ctx, p, namespace, name, req)
	if err != nil {
		return viewmodel.OperationAccepted{}, operationError(err)
	}
	return v, nil
}
