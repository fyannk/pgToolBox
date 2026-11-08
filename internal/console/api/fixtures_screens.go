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
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/operations"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// Compare implements ScreenSource: orders against billing, either way
// round; any other known pair answers the same rows under its names.
func (FixtureSource) Compare(_ context.Context, lns, ln, rns, rn string) (viewmodel.Comparison, error) {
	left, err := refFor(lns, ln)
	if err != nil {
		return viewmodel.Comparison{}, err
	}
	right, err := refFor(rns, rn)
	if err != nil {
		return viewmodel.Comparison{}, err
	}
	v := fixtures.Comparison()
	v.Left.Ref, v.Right.Ref = left, right
	return v, nil
}

// Replication implements ScreenSource.
func (FixtureSource) Replication(_ context.Context, ns, name string) (viewmodel.Replication, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Replication()
	v.Ref = ref
	return v, err
}

// Backups implements ScreenSource.
func (FixtureSource) Backups(_ context.Context, ns, name string) (viewmodel.Backups, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Backups()
	if name == "billing" {
		// billing backs up with the Dalibo pgBackRest plugin.
		v = fixtures.BackupsPgBackRest()
	}
	v.Ref = ref
	return v, err
}

// RestoreCheck implements ScreenSource: orders' canned answer at the
// requested time.
func (FixtureSource) RestoreCheck(_ context.Context, ns, name string, at time.Time) (viewmodel.RestoreCheck, error) {
	ref, err := refFor(ns, name)
	v := fixtures.RestoreCheck()
	v.Ref, v.At = ref, at.UTC()
	return v, err
}

// Storage implements ScreenSource.
func (FixtureSource) Storage(_ context.Context, ns, name string) (viewmodel.Storage, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Storage()
	if name == "billing" {
		// billing runs a build that reports its volumes.
		v = fixtures.StorageForecasts()
	}
	v.Ref = ref
	return v, err
}

// Objects implements ScreenSource.
func (FixtureSource) Objects(_ context.Context, ns, name string) (viewmodel.Objects, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Objects()
	v.Ref = ref
	return v, err
}

// History implements ScreenSource.
func (FixtureSource) History(_ context.Context, ns, name string) (viewmodel.History, error) {
	ref, err := refFor(ns, name)
	v := fixtures.History()
	v.Ref = ref
	return v, err
}

// Revision implements ScreenSource: only seq 42 exists.
func (FixtureSource) Revision(_ context.Context, ns, name string, seq int64) (viewmodel.Revision, error) {
	ref, err := refFor(ns, name)
	if err != nil {
		return viewmodel.Revision{}, err
	}
	v := fixtures.Revision()
	if seq != v.Entry.Seq {
		return viewmodel.Revision{}, ErrNotFound
	}
	v.Ref = ref
	return v, nil
}

// Metrics implements ScreenSource.
func (FixtureSource) Metrics(_ context.Context, ns, name string) (viewmodel.Metrics, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Metrics()
	v.Ref = ref
	return v, err
}

// Topology implements ScreenSource.
func (FixtureSource) Topology(_ context.Context, ns, name string, _ ...string) (viewmodel.Topology, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Topology()
	v.Ref = ref
	return v, err
}

// PgAdminLinks implements ScreenSource.
func (FixtureSource) PgAdminLinks(_ context.Context, ns, name string) ([]viewmodel.PgAdminLink, error) {
	if _, err := refFor(ns, name); err != nil {
		return nil, err
	}
	return fixtures.PgAdminLinks(), nil
}

// Timeline implements TimelineSource: billing's entries for any Cluster,
// with the kind filter applied.
func (FixtureSource) Timeline(_ context.Context, ns, name string, q TimelineQuery) (viewmodel.Timeline, error) {
	ref, err := refFor(ns, name)
	if err != nil {
		return viewmodel.Timeline{}, err
	}
	v := fixtures.Timeline()
	v.Ref = ref
	if len(q.Kinds) > 0 {
		want := map[string]bool{}
		for _, k := range q.Kinds {
			want[k] = true
		}
		v.Kinds = append([]string{}, q.Kinds...)
		items := v.Items[:0:0]
		for _, e := range v.Items {
			if want[e.Kind] {
				items = append(items, e)
			}
		}
		v.Items = items
	}
	return v, nil
}

// Digests implements TimelineSource.
func (FixtureSource) Digests(context.Context) (DigestArchive, error) {
	return DigestArchive{GeneratedAt: fixtures.Now, Schedule: fixtures.DigestSchedule(), Items: fixtures.Digests()}, nil
}

// Admin implements ScreenSource.
func (FixtureSource) Admin(context.Context) (viewmodel.Admin, error) { return fixtures.Admin(), nil }

// FixtureLogs serves the fixture tail and a short follow.
type FixtureLogs struct{}

// Tail implements LogReader.
func (FixtureLogs) Tail(_ context.Context, ns, cluster, pod string, o LogOptions) (viewmodel.LogTail, error) {
	ref, err := refFor(ns, cluster)
	if err != nil {
		return viewmodel.LogTail{}, err
	}
	v := fixtures.LogTail()
	if pod != v.Pod || o.Container != v.Container {
		return viewmodel.LogTail{}, ErrNotFound
	}
	v.Ref = ref
	return v, nil
}

// Follow implements LogReader.
func (f FixtureLogs) Follow(ctx context.Context, ns, cluster, pod string, o LogOptions, emit func(any) error) error {
	v, err := f.Tail(ctx, ns, cluster, pod, o)
	if err != nil {
		return err
	}
	for _, l := range v.Lines {
		if err := emit(viewmodel.LogStreamLine(l)); err != nil {
			return err
		}
	}
	if err := emit(viewmodel.LogStreamGap{At: fixtures.Now, Reason: "the container restarted"}); err != nil {
		return err
	}
	return emit(viewmodel.LogStreamEnd{Reason: "the fixture ends here"})
}

// FixtureOperator answers from the fixture scenarios.
type FixtureOperator struct{}

// Offers implements Operator: the offers follow the caller's capabilities.
func (FixtureOperator) Offers(_ context.Context, _ *auth.Principal, caps authz.Capabilities, ns, name string) (viewmodel.Operations, error) {
	ref, err := refFor(ns, name)
	if err != nil {
		return viewmodel.Operations{}, err
	}
	v := fixtures.Operations()
	v.Ref = ref
	allowed := map[viewmodel.OperationKind]bool{
		viewmodel.OperationBackup: caps.Backup, viewmodel.OperationReload: caps.Reload,
		viewmodel.OperationRestart: caps.Restart, viewmodel.OperationPromote: caps.Promote,
	}
	for i := range v.Offers {
		v.Offers[i].Allowed = allowed[v.Offers[i].Kind]
		if v.Offers[i].Kind != viewmodel.OperationPromote {
			v.Offers[i].Confirmation = name
		}
	}
	if !caps.Operations {
		v.Recent = []viewmodel.Operation{}
	}
	return v, nil
}

// Request implements Operator: a backup of orders is accepted; anything
// else is refused with the closed reasons the real service uses.
func (FixtureOperator) Request(_ context.Context, _ *auth.Principal, ns, name string, req viewmodel.OperationRequest) (viewmodel.OperationAccepted, error) {
	ref, err := refFor(ns, name)
	if err != nil {
		return viewmodel.OperationAccepted{}, err
	}
	if err := operations.CheckConfirmation(name, req); err != nil {
		return viewmodel.OperationAccepted{}, err
	}
	if req.Kind == viewmodel.OperationPromote {
		return viewmodel.OperationAccepted{}, operations.Rejection(http.StatusConflict, "promote refused: orders-3 is fenced")
	}
	op := fixtures.Operation()
	id := operations.ID{Kind: req.Kind, Namespace: ns, Cluster: name, AcceptedAt: fixtures.Now.Unix()}
	switch req.Kind {
	case viewmodel.OperationBackup:
		id.Created = op.Created
	case viewmodel.OperationRestoreDrill:
		id.Created, id.CreatedNamespace = "drill-"+name+"-"+fixtures.Now.Format("0601021504"), ns
	case viewmodel.OperationRestart, viewmodel.OperationReload, viewmodel.OperationPromote:
		// These act on existing objects: nothing is created.
	}
	return viewmodel.OperationAccepted{
		OperationID: id.Encode(), Kind: req.Kind, Target: ref, AcceptedAt: fixtures.Now, ObservedGeneration: 3, Created: id.Created,
		CreatedNamespace: id.CreatedNamespace,
	}, nil
}

// Lookup implements Operator.
func (FixtureOperator) Lookup(_ context.Context, id operations.ID) (viewmodel.Operation, error) {
	ref, err := refFor(id.Namespace, id.Cluster)
	if err != nil {
		return viewmodel.Operation{}, err
	}
	op := fixtures.Operation()
	op.OperationID, op.Target, op.Kind = id.Encode(), ref, id.Kind
	op.AcceptedAt = id.Accepted()
	op.Deadline = op.AcceptedAt.Add(30 * time.Minute)
	op.Actor = ""
	return op, nil
}

// fixtureOperationID is the handle of the fixture backup.
func fixtureOperationID() string {
	return operations.ID{Kind: viewmodel.OperationBackup, Namespace: "team-a", Cluster: "orders", Created: fixtures.Operation().Created, AcceptedAt: fixtures.Now.Add(-5 * time.Minute).Unix()}.Encode()
}

// Insight implements InsightSource: orders runs the pack, billing has
// not opted in.
func (FixtureSource) Insight(_ context.Context, ns, name string) (viewmodel.Insight, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Insight()
	if name == "billing" {
		v = fixtures.InsightNotEnabled()
	}
	v.Ref = ref
	return v, err
}

// InsightBlocking implements InsightSource.
func (FixtureSource) InsightBlocking(_ context.Context, ns, name string, _ bool) (viewmodel.InsightBlocking, error) {
	_, err := refFor(ns, name)
	if name == "billing" {
		return fixtures.InsightNotEnabled().Blocking, err
	}
	return fixtures.InsightBlocking(), err
}

// FleetStatements implements InsightSource.
func (FixtureSource) FleetStatements(_ context.Context, visible func([]viewmodel.ClusterRef) []viewmodel.ClusterRef) (viewmodel.FleetStatements, error) {
	return fixtures.FleetStatementsOf(visible), nil
}
