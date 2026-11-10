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

// Package wiring holds the adapters cmd/console puts between the read model
// and the API: names
// and not-found mapping, the cache's change feed to the event stream, pod
// logs, and the operation service.
package wiring

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/operations"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/podlogs"
	"github.com/fyannk/pgToolBox/internal/console/server"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/status"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// ReadService is the slice of status.Service the API serves.
type ReadService interface {
	ClusterList(ctx context.Context) (viewmodel.ClusterList, error)
	Overview(ctx context.Context, namespace, name string) (viewmodel.ClusterOverview, error)
	Instances(ctx context.Context, namespace, name string) (viewmodel.InstanceList, error)
	Events(ctx context.Context, namespace, name string) (viewmodel.EventList, error)
	Diagnostics(ctx context.Context, namespace, name string) (viewmodel.Diagnostics, error)
	Triage(ctx context.Context, namespace, name string) (viewmodel.Triage, error)
	Timeline(ctx context.Context, namespace, name string, q status.TimelineQuery) (viewmodel.Timeline, error)
	screenSource
}

// Source adapts the read model to api.Source: names, and the not-found
// error the API maps to 404.
type Source struct {
	s       ReadService
	digests DigestArchive
}

// DigestArchive is the daily digest's store (digest.Service).
type DigestArchive interface {
	Archive() ([]viewmodel.Digest, viewmodel.DigestSchedule)
}

// NewSource wraps the read model.
func NewSource(s ReadService) Source { return Source{s: s} }

// WithDigests serves the daily digests from d; without it the API says
// the digest is disabled.
func (a Source) WithDigests(d DigestArchive) Source {
	a.digests = d
	return a
}

// Timeline implements api.Source.
func (a Source) Timeline(ctx context.Context, ns, name string, q api.TimelineQuery) (viewmodel.Timeline, error) {
	v, err := a.s.Timeline(ctx, ns, name, status.TimelineQuery{From: q.From, To: q.To, Kinds: q.Kinds, Limit: q.Limit})
	return v, notFound(err)
}

// Digests implements api.Source.
func (a Source) Digests(context.Context) (api.DigestArchive, error) {
	now := time.Now().UTC()
	if a.digests == nil {
		return api.DigestArchive{GeneratedAt: now}, api.ErrDigestDisabled
	}
	items, schedule := a.digests.Archive()
	return api.DigestArchive{GeneratedAt: now, Schedule: schedule, Items: items}, nil
}

// screenSource is the read model's half behind api.ScreenSource; its
// errors are mapped like the rest.
type screenSource interface {
	Replication(ctx context.Context, namespace, name string) (viewmodel.Replication, error)
	Backups(ctx context.Context, namespace, name string) (viewmodel.Backups, error)
	RestoreCheck(ctx context.Context, namespace, name string, at time.Time) (viewmodel.RestoreCheck, error)
	Storage(ctx context.Context, namespace, name string) (viewmodel.Storage, error)
	Objects(ctx context.Context, namespace, name string) (viewmodel.Objects, error)
	History(ctx context.Context, namespace, name string) (viewmodel.History, error)
	Revision(ctx context.Context, namespace, name string, seq int64) (viewmodel.Revision, error)
	Metrics(ctx context.Context, namespace, name string) (viewmodel.Metrics, error)
	Topology(ctx context.Context, namespace, name string, fold ...string) (viewmodel.Topology, error)
	PgAdminLinks(ctx context.Context, namespace, name string) ([]viewmodel.PgAdminLink, error)
	Admin(ctx context.Context) (viewmodel.Admin, error)
	Insight(ctx context.Context, namespace, name string) (viewmodel.Insight, error)
	InsightBlocking(ctx context.Context, namespace, name string, refresh bool) (viewmodel.InsightBlocking, error)
	FleetStatements(ctx context.Context, visible func([]viewmodel.ClusterRef) []viewmodel.ClusterRef) (viewmodel.FleetStatements, error)
	Compare(ctx context.Context, leftNamespace, leftName, rightNamespace, rightName string) (viewmodel.Comparison, error)
}

func (a Source) screens() screenSource { return a.s }

// Insight implements api.Source.
func (a Source) Insight(ctx context.Context, ns, name string) (viewmodel.Insight, error) {
	v, err := a.screens().Insight(ctx, ns, name)
	return v, notFound(err)
}

// InsightBlocking implements api.Source.
func (a Source) InsightBlocking(ctx context.Context, ns, name string, refresh bool) (viewmodel.InsightBlocking, error) {
	v, err := a.screens().InsightBlocking(ctx, ns, name, refresh)
	return v, notFound(err)
}

// FleetStatements implements api.Source.
func (a Source) FleetStatements(ctx context.Context, visible func([]viewmodel.ClusterRef) []viewmodel.ClusterRef) (viewmodel.FleetStatements, error) {
	return a.screens().FleetStatements(ctx, visible)
}

// Compare implements api.Source.
func (a Source) Compare(ctx context.Context, lns, ln, rns, rn string) (viewmodel.Comparison, error) {
	v, err := a.screens().Compare(ctx, lns, ln, rns, rn)
	return v, notFound(err)
}

// Replication implements api.Source.
func (a Source) Replication(ctx context.Context, ns, name string) (viewmodel.Replication, error) {
	v, err := a.screens().Replication(ctx, ns, name)
	return v, notFound(err)
}

// Backups implements api.Source.
func (a Source) Backups(ctx context.Context, ns, name string) (viewmodel.Backups, error) {
	v, err := a.screens().Backups(ctx, ns, name)
	return v, notFound(err)
}

// Storage implements api.Source.
func (a Source) Storage(ctx context.Context, ns, name string) (viewmodel.Storage, error) {
	v, err := a.screens().Storage(ctx, ns, name)
	return v, notFound(err)
}

// Objects implements api.Source.
func (a Source) Objects(ctx context.Context, ns, name string) (viewmodel.Objects, error) {
	v, err := a.screens().Objects(ctx, ns, name)
	return v, notFound(err)
}

// RestoreCheck implements api.Source.
func (a Source) RestoreCheck(ctx context.Context, ns, name string, at time.Time) (viewmodel.RestoreCheck, error) {
	v, err := a.screens().RestoreCheck(ctx, ns, name, at)
	return v, notFound(err)
}

// History implements api.Source.
func (a Source) History(ctx context.Context, ns, name string) (viewmodel.History, error) {
	v, err := a.screens().History(ctx, ns, name)
	return v, notFound(err)
}

// Revision implements api.Source.
func (a Source) Revision(ctx context.Context, ns, name string, seq int64) (viewmodel.Revision, error) {
	v, err := a.screens().Revision(ctx, ns, name, seq)
	return v, notFound(err)
}

// Metrics implements api.Source.
func (a Source) Metrics(ctx context.Context, ns, name string) (viewmodel.Metrics, error) {
	v, err := a.screens().Metrics(ctx, ns, name)
	return v, notFound(err)
}

// Topology implements api.Source.
func (a Source) Topology(ctx context.Context, ns, name string, fold ...string) (viewmodel.Topology, error) {
	v, err := a.screens().Topology(ctx, ns, name, fold...)
	return v, notFound(err)
}

// PgAdminLinks implements api.Source.
func (a Source) PgAdminLinks(ctx context.Context, ns, name string) ([]viewmodel.PgAdminLink, error) {
	v, err := a.screens().PgAdminLinks(ctx, ns, name)
	return v, notFound(err)
}

// Admin implements api.Source.
func (a Source) Admin(ctx context.Context) (viewmodel.Admin, error) { return a.screens().Admin(ctx) }

func notFound(err error) error {
	if errors.Is(err, status.ErrNotFound) {
		return api.ErrNotFound
	}
	return err
}

// ClusterList implements api.Source.
func (a Source) ClusterList(ctx context.Context) (viewmodel.ClusterList, error) {
	return a.s.ClusterList(ctx)
}

// ClusterOverview implements api.Source.
func (a Source) ClusterOverview(ctx context.Context, ns, name string) (viewmodel.ClusterOverview, error) {
	v, err := a.s.Overview(ctx, ns, name)
	return v, notFound(err)
}

// Instances implements api.Source.
func (a Source) Instances(ctx context.Context, ns, name string) (viewmodel.InstanceList, error) {
	v, err := a.s.Instances(ctx, ns, name)
	return v, notFound(err)
}

// Events implements api.Source.
func (a Source) Events(ctx context.Context, ns, name string) (viewmodel.EventList, error) {
	v, err := a.s.Events(ctx, ns, name)
	return v, notFound(err)
}

// Diagnostics implements api.Source.
func (a Source) Diagnostics(ctx context.Context, ns, name string) (viewmodel.Diagnostics, error) {
	v, err := a.s.Diagnostics(ctx, ns, name)
	return v, notFound(err)
}

// Triage implements api.Source.
func (a Source) Triage(ctx context.Context, ns, name string) (viewmodel.Triage, error) {
	v, err := a.s.Triage(ctx, ns, name)
	return v, notFound(err)
}

// KeyFeed is one coalescing subscription to the cache's changes.
type KeyFeed interface {
	Ready() <-chan struct{}
	Drain() []cache.ClusterKey
}

// SubscribeCache adapts the cache manager's change feed.
func SubscribeCache(m interface {
	Subscribe() *cache.Subscription
	Unsubscribe(*cache.Subscription)
},
) func() (KeyFeed, func()) {
	return func() (KeyFeed, func()) {
		s := m.Subscribe()
		return s, func() { m.Unsubscribe(s) }
	}
}

// Invalidations turns the cache's change feed into stream events: one
// "clusters" event per batch (rows of the list may have changed), and one
// "cluster" event per named Cluster. A namespace-wide change (an object
// the cache could not attribute, a source changing state) has no name and
// is covered by the "clusters" event, which the UI treats as "refetch
// every cluster view": naming the namespace to everyone would tell users
// about namespaces they may not see.
func Invalidations(subscribe func() (KeyFeed, func()), broker *sse.Broker) func(context.Context) error {
	return func(ctx context.Context) error {
		sub, unsubscribe := subscribe()
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-sub.Ready():
			}
			keys := sub.Drain()
			if len(keys) == 0 {
				continue
			}
			broker.Publish(sse.Invalidate{Topic: sse.TopicClusters})
			for _, k := range keys {
				if k.Name != "" {
					broker.Publish(sse.Invalidate{Topic: sse.TopicCluster, Namespace: k.Namespace, Name: k.Name})
				}
			}
		}
	}
}

// Logs adapts podlogs.Reader to api.LogReader.
type Logs struct{ r *podlogs.Reader }

// NewLogs wraps r.
func NewLogs(r *podlogs.Reader) Logs { return Logs{r: r} }

func logErr(err error) error {
	if errors.Is(err, podlogs.ErrNotFound) {
		return api.ErrNotFound
	}
	return err
}

// Tail implements api.LogReader.
func (l Logs) Tail(ctx context.Context, ns, cluster, pod string, o api.LogOptions) (viewmodel.LogTail, error) {
	v, err := l.r.Tail(ctx, ns, cluster, pod, podlogs.Options{Container: o.Container, Previous: o.Previous, TailLines: o.TailLines})
	return v, logErr(err)
}

// Follow implements api.LogReader.
func (l Logs) Follow(ctx context.Context, ns, cluster, pod string, o api.LogOptions, emit func(any) error) error {
	return logErr(l.r.Follow(ctx, ns, cluster, pod, podlogs.Options{Container: o.Container, TailLines: o.TailLines}, emit))
}

// PodLogOpener opens container logs with the console's identity (its
// ServiceAccount in a cluster, the kubeconfig in local mode).
type PodLogOpener struct{ c kubernetes.Interface }

// NewPodLogOpener wraps c.
func NewPodLogOpener(c kubernetes.Interface) PodLogOpener { return PodLogOpener{c: c} }

// Open implements podlogs.Opener.
func (o PodLogOpener) Open(ctx context.Context, ns, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	return o.c.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
}

// OperatorFactory builds the operation service: live reads and writes as
// the caller (token passthrough), offers and trackers from the read model.
// drill is nil unless restore drills are enabled (NewDrills).
func OperatorFactory(state operations.State, drill *Drills, log *slog.Logger) server.OperatorFactory {
	return func(restConfig *rest.Config, az *authz.Authorizer) (api.Operator, error) {
		var d *operations.DrillOptions
		if drill != nil {
			d = drill.options(az)
		}
		return NewOperator(state, auth.UserConfig(restConfig), d, log)
	}
}

// NewOperator builds the operation service over userConfig, a
// configuration that acts as the caller of each request (auth.UserConfig).
func NewOperator(state operations.State, userConfig *rest.Config, drill *operations.DrillOptions, log *slog.Logger) (api.Operator, error) {
	c, err := ops.NewUserClient(userConfig)
	if err != nil {
		return nil, err
	}
	return operations.New(operations.Options{
		State: state, Writer: ops.NewUserWriter(c, 0),
		IsNotFound: func(err error) bool { return errors.Is(err, status.ErrNotFound) },
		Logger:     log.With("component", "operations"),
		Drills:     drill,
	}), nil
}

// Drills is the restore-drill assembly: the record, the
// supervisor that judges and tears drills down with the console's own
// ServiceAccount, and what the operation service needs to start one as
// the user.
type Drills struct {
	Store      *drills.Store
	Supervisor *drills.Supervisor
	cfg        config.RestoreDrill
	selector   labels.Selector
	owner      string
	keys       [][]byte
	read       *status.Service
}

var (
	_ drills.Observer       = (*status.Service)(nil)
	_ operations.DrillState = (*status.Service)(nil)
)

// NewDrills assembles restore drills, or returns nil when they are off.
// owner is the console's identity (its namespace); secrets are what the
// drill seal's keys derive from (HKDF, ops.DrillKeyLabel): the seal's own
// key file (RESTORE_DRILL_SEAL_KEY_FILE) when one is configured, else the
// session secrets; persist is the history journal when history is
// persistent; keeper keeps finished drills' outcomes otherwise (the
// shared-state ConfigMap; nil keeps them in memory).
func NewDrills(cfg config.Console, read *status.Service, consoleConfig *rest.Config, owner string, secrets [][]byte,
	persist drills.Persister, keeper drills.Keeper, log *slog.Logger,
) (*Drills, error) {
	if !cfg.RestoreDrill.Enabled {
		return nil, nil //nolint:nilnil // disabled is a valid, explicit outcome
	}
	if owner == "" {
		return nil, errors.New("restore drills need the console's namespace (POD_NAMESPACE) to label the drills it creates")
	}
	keys, err := auth.PurposeKeys(secrets, ops.DrillKeyLabel)
	if err != nil {
		return nil, fmt.Errorf("restore drill seal keys: %w", err)
	}
	c, err := ops.NewDrillReaperClient(consoleConfig)
	if err != nil {
		return nil, err
	}
	log = log.With("component", "restore-drills")
	store := drills.NewStore(persist, log, time.Now())
	if persist == nil && keeper != nil {
		store.SetKeeper(keeper)
	}
	reaper := ops.NewDrillReaper(c, owner, keys, cfg.APIRequestTimeout)
	read.SetDrills(&status.DrillsView{Records: store, Namespace: cfg.RestoreDrill.Namespace, TimeLimit: cfg.RestoreDrill.TimeLimit})
	return &Drills{
		Store: store, Supervisor: drills.NewSupervisor(store, read, reaper, nil, log),
		cfg: cfg.RestoreDrill, selector: cfg.ClusterSelector, owner: owner, keys: keys, read: read,
	}, nil
}

// SharedDrillKeeper keeps finished drills' outcomes in the console's
// shared-state ConfigMap (drills.Keeper).
type SharedDrillKeeper struct{ Store *sharedstate.Store }

// NewSharedDrillKeeper returns a keeper over store, or nil when no
// ConfigMap is configured. It starts the record of drills now when the
// ConfigMap holds none yet.
func NewSharedDrillKeeper(store *sharedstate.Store, now time.Time) drills.Keeper {
	if !store.Enabled() {
		return nil
	}
	store.StartKeepingDrills(now)
	return SharedDrillKeeper{Store: store}
}

// KeepDrill implements drills.Keeper.
func (k SharedDrillKeeper) KeepDrill(r drills.Kept) {
	unix := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.Unix()
	}
	k.Store.KeepDrill(sharedstate.DrillResult{
		Namespace: r.Drill.Namespace, Name: r.Drill.Name, SourceNamespace: r.Source.Namespace, Source: r.Source.Name, SourceUID: r.SourceUID,
		Backup: r.Backup, BackupLabel: r.BackupLabel, Result: string(r.Result), Reason: r.Reason,
		StartedAt: unix(r.StartedAt), Deadline: unix(r.Deadline), HealthyAt: unix(r.HealthyAt), FinishedAt: unix(r.FinishedAt),
	})
}

// KeptDrills implements drills.Keeper.
func (k SharedDrillKeeper) KeptDrills() []drills.Kept {
	at := func(u int64) time.Time {
		if u == 0 {
			return time.Time{}
		}
		return time.Unix(u, 0).UTC()
	}
	results := k.Store.KeptDrills()
	out := make([]drills.Kept, 0, len(results))
	for _, r := range results {
		out = append(out, drills.Kept{
			Drill: drills.Key{Namespace: r.Namespace, Name: r.Name}, Source: drills.SourceKey{Namespace: r.SourceNamespace, Name: r.Source}, SourceUID: r.SourceUID,
			Backup: r.Backup, BackupLabel: r.BackupLabel, Result: viewmodel.DrillResult(r.Result), Reason: r.Reason,
			StartedAt: at(r.StartedAt), Deadline: at(r.Deadline), HealthyAt: at(r.HealthyAt), FinishedAt: at(r.FinishedAt),
		})
	}
	return out
}

// KeepsDrills implements drills.Keeper.
func (k SharedDrillKeeper) KeepsDrills() bool { return k.Store.KeepsDrills() }

// KeptSince implements drills.Keeper.
func (k SharedDrillKeeper) KeptSince() time.Time { return k.Store.DrillsKeptSince() }

func (d *Drills) options(az *authz.Authorizer) *operations.DrillOptions {
	r := d.cfg
	return &operations.DrillOptions{
		Namespace: r.Namespace, Prefix: r.Prefix, TimeLimit: r.TimeLimit,
		Resources: ops.DrillResources{
			CPURequest: r.CPURequest, MemoryRequest: r.MemoryRequest, CPULimit: r.CPULimit, MemoryLimit: r.MemoryLimit,
			StorageSize: r.StorageSize, StorageClass: r.StorageClass,
		},
		ServerCA: ops.DrillServerCA{Mode: r.ServerCA, Secret: r.ServerCASecret},
		Owner:    d.owner, Keys: d.keys, Selector: d.selector, State: d.read, Records: d.Store, MaxConcurrent: r.MaxConcurrent,
		CanCreate: func(ctx context.Context, p *auth.Principal, namespace string) (bool, bool) {
			if az == nil {
				return false, true
			}
			ds := az.Check(p.Context(ctx), p.User, []authz.Attributes{{Namespace: namespace, Verb: "create", Group: authz.CNPGGroup, Resource: "clusters"}})
			return ds[0].Allowed, ds[0].Err != nil
		},
	}
}
