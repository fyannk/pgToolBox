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

package status

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// ErrNotFound is returned for a Cluster the console does not watch or
// that does not exist; the API maps it to 404 (and must answer the same
// for a Cluster the caller may not get).
var ErrNotFound = errors.New("cluster not found")

// Cache is the informer layer as the read model uses it.
type Cache interface {
	Reader() client.Reader
	Namespaces() []string
	Source(namespace, source string) viewmodel.SourceState
	Sources(namespace string) []viewmodel.SourceState
	Readable(namespace, source string) bool
	Served(source string) *bool
}

// Sweep is the /pg/status sweep as the read model uses it.
type Sweep interface {
	Snapshot(sweep.Key) (sweep.Snapshot, bool)
	Series(key sweep.Key, instance string) []viewmodel.Series
	Metrics(sweep.Key) *metrics.Store
	InstanceStatus(sweep.Key) sweep.StatusSource
	// Insight is the Cluster's insight-pack state; false when instance
	// metrics are not scraped or the Cluster has not been swept.
	Insight(sweep.Key) (insight.Snapshot, bool)
	// RefreshInsight re-reads the primary's exporter, rate-limited.
	RefreshInsight(context.Context, sweep.Key) (time.Time, error)
	State() viewmodel.SourceState
}

// Diagnostics is the background runner as the read model uses it.
type Diagnostics interface {
	Latest(cache.ClusterKey) (diag.Run, bool)
	State() viewmodel.SourceState
}

// Options wires a Service.
type Options struct {
	Cache Cache
	// Sweep and Diagnostics may be set after New (SetSweep,
	// SetDiagnostics), because both read the Service in turn.
	Sweep       Sweep
	Diagnostics Diagnostics
	// EventsMaxAge and EventsMaxPerCluster bound the event lists.
	EventsMaxAge        time.Duration
	EventsMaxPerCluster int
	// FenceCluster labels a fork build of the operator; nothing proves
	// it, so it is a deployment assertion (default false).
	ForkConditions bool
	// AllowClusterCatalogs mirrors ALLOW_CLUSTER_CATALOGS: the opt-in
	// ClusterImageCatalog read is configured.
	AllowClusterCatalogs bool
	// InsightPackConfigMap is the insight pack's ConfigMap name
	// (INSIGHT_PACK_CONFIGMAP); empty is insight.ConfigMapName.
	InsightPackConfigMap string
	// Evidence is the repository-evidence reader; nil reads none.
	Evidence evidence.Reader
	// KubeVersion is the API server version poller's latest snapshot;
	// nil feeds none.
	KubeVersion func() (observe.KubeVersionSnapshot, bool)
	// Logs is the log matcher of a Cluster (logfollow.Follower); nil
	// when log following is off.
	Logs func(namespace, name string) diagnose.LogObservations
	// History is the revision history; nil when HISTORY_ENABLED is false.
	History HistoryReader
	// Timeline is the journal of observed transitions; nil
	// when HISTORY_ENABLED is false.
	Timeline *timeline.Journal
	// Digest reports the daily digest on the admin page; nil when off.
	Digest func() *viewmodel.DigestStatus
	// Monitoring is the link-out to the site's monitoring system.
	Monitoring viewmodel.Monitoring
	// Prometheus, when set, serves the metrics charts from PromQL
	// (PROMETHEUS_URL); PrometheusRange is their window (default 1 h).
	Prometheus      PromRanger
	PrometheusRange time.Duration
	// Trends is the long volume trend behind the storage forecasts; nil
	// serves none.
	Trends *volumetrend.Store
	// TrendsPersistent: the trend is mirrored to disk (HISTORY_PATH).
	TrendsPersistent bool
	// Drills is the restore-drill record; nil when drills are off.
	Drills *DrillsView
	// Console describes the console itself for the admin page.
	Console ConsoleInfo
	// Shared, when set, merges the phase clocks with what the other
	// replicas observed (the console's state ConfigMap).
	Shared SharedClocks
	// Persistence reports the state snapshots and the shared state on
	// the admin page; nil reports neither.
	Persistence func() []viewmodel.SourceState
	Now         func() time.Time
	Logger      *slog.Logger
}

// SharedClocks is the slice of the shared state the phase clocks need.
type SharedClocks interface {
	// PhaseSince returns when the Cluster was first seen in phase: the
	// earliest of local and what is shared for the same phase.
	PhaseSince(clusterUID, phase string, local time.Time) time.Time
}

// Service is the read model.
type Service struct {
	opts   Options
	cache  Cache
	now    func() time.Time
	logger *slog.Logger
	prom   *promCharts

	mu    sync.RWMutex
	sweep Sweep
	diag  Diagnostics
	// clocks holds the console's own "since when" for states the
	// operator reports no time for (phase), per Cluster.
	clocks map[cache.ClusterKey]phaseClock
	// runners reports the supervised runners (admin page).
	runners func() []viewmodel.RunnerState
	// rows memoizes the cluster list's rows.
	rows rowMemo
	// lastKeys is each namespace's last successful Cluster list, what
	// Keys answers with while a list fails.
	lastKeys map[string][]cache.ClusterKey
	// listFailing is the namespaces whose last Cluster list failed, so a
	// failure is logged when it starts and when it ends, not on every read.
	listFailing map[string]bool
}

type phaseClock struct {
	uid, phase string
	since      time.Time
}

// New returns the read model.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.EventsMaxAge <= 0 {
		opts.EventsMaxAge = time.Hour
	}
	if opts.EventsMaxPerCluster <= 0 {
		opts.EventsMaxPerCluster = 200
	}
	svc := &Service{
		opts: opts, cache: opts.Cache, now: opts.Now, logger: opts.Logger,
		sweep: opts.Sweep, diag: opts.Diagnostics, clocks: map[cache.ClusterKey]phaseClock{},
		lastKeys: map[string][]cache.ClusterKey{}, listFailing: map[string]bool{},
	}
	if opts.Prometheus != nil {
		svc.prom = newPromCharts(opts.Prometheus, opts.PrometheusRange, opts.Now)
	}
	return svc
}

// SetSweep wires the sweep after construction (it reads Targets).
func (s *Service) SetSweep(sw Sweep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep = sw
}

// SetDiagnostics wires the runner after construction (it reads Input).
func (s *Service) SetDiagnostics(d Diagnostics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diag = d
}

func (s *Service) sweeper() Sweep {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sweep
}

func (s *Service) diagnostics() Diagnostics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.diag
}

// Run keeps the console's own clocks current between reads, so "since"
// is when the console first saw a state, not when someone first looked.
func (s *Service) Run(ctx context.Context, changes diag.Changes) error {
	sub := changes.Subscribe()
	defer changes.Unsubscribe(sub)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sub.Ready():
			for _, key := range sub.Drain() {
				for _, k := range s.expand(ctx, key) {
					if g, err := s.gather(ctx, k, false); err == nil {
						s.phaseSince(g)
					}
				}
			}
		}
	}
}

func (s *Service) expand(ctx context.Context, key cache.ClusterKey) []cache.ClusterKey {
	if key.Name != "" {
		return []cache.ClusterKey{key}
	}
	var out []cache.ClusterKey
	for _, k := range s.Keys(ctx) {
		if k.Namespace == key.Namespace {
			out = append(out, k)
		}
	}
	return out
}

// phaseSince returns when the console first saw the Cluster's current
// phase, recording it on first sight. A recreated Cluster (new UID)
// restarts the clock.
func (s *Service) phaseSince(g *gathered) *time.Time {
	if g.cluster == nil || g.cluster.Status.Phase == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clock, ok := s.clocks[g.key]
	if !ok || clock.uid != string(g.cluster.UID) || clock.phase != g.cluster.Status.Phase {
		clock = phaseClock{uid: string(g.cluster.UID), phase: g.cluster.Status.Phase, since: s.now().UTC()}
	}
	if s.opts.Shared != nil {
		// Another replica, or this console before a restart, may have
		// seen the phase begin earlier.
		clock.since = s.opts.Shared.PhaseSince(clock.uid, clock.phase, clock.since)
	}
	s.clocks[g.key] = clock
	since := clock.since
	return &since
}

// LiveUIDs lists the UIDs of the Clusters and instance pods the cache
// holds, for the shared state to forget what is gone. False when a
// watched namespace cannot be read: nothing may be pruned on a partial
// view, or a passing list failure would erase every shared clock.
func (s *Service) LiveUIDs(ctx context.Context) (map[string]bool, bool) {
	live := map[string]bool{}
	reader := s.cache.Reader()
	for _, ns := range s.cache.Namespaces() {
		if !s.cache.Readable(ns, cache.SourceClusters) || !s.cache.Readable(ns, cache.SourcePods) {
			return nil, false
		}
		clusters := &cnpgv1.ClusterList{}
		if err := reader.List(ctx, clusters, client.InNamespace(ns), client.UnsafeDisableDeepCopy); err != nil {
			return nil, false
		}
		for i := range clusters.Items {
			live[string(clusters.Items[i].UID)] = true
		}
		pods := &corev1.PodList{}
		if err := reader.List(ctx, pods, client.InNamespace(ns), client.UnsafeDisableDeepCopy); err != nil {
			return nil, false
		}
		for i := range pods.Items {
			live[string(pods.Items[i].UID)] = true
		}
	}
	return live, true
}

// Keys lists every watched Cluster the cache holds. A namespace whose
// Cluster list fails keeps the Clusters its last successful list named:
// the roster's consumers (sweep, diagnostics, timeline, log follower)
// would otherwise take a failed read for every Cluster being deleted.
func (s *Service) Keys(ctx context.Context) []cache.ClusterKey {
	keys, _ := s.keysRead(ctx)
	return keys
}

// keysRead is Keys and the namespaces whose Cluster list failed this
// time, each with the last-known keys it answered with.
func (s *Service) keysRead(ctx context.Context) ([]cache.ClusterKey, map[string]bool) {
	var keys []cache.ClusterKey
	var failed map[string]bool
	watched := map[string]bool{}
	for _, ns := range s.cache.Namespaces() {
		watched[ns] = true
		if !s.cache.Readable(ns, cache.SourceClusters) {
			s.setLastKeys(ns, nil, false)
			continue
		}
		list := &cnpgv1.ClusterList{}
		// Only names are read, so the cache's own objects are listed
		// without the per-object deep copy.
		if err := s.cache.Reader().List(ctx, list, client.InNamespace(ns), client.UnsafeDisableDeepCopy); err != nil {
			if failed == nil {
				failed = map[string]bool{}
			}
			failed[ns] = true
			if s.setListFailing(ns, true) {
				s.logger.LogAttrs(ctx, slog.LevelWarn, "listing Clusters failed; keeping the last known ones until a list succeeds",
					slog.String("namespace", ns), slog.String("error", err.Error()))
			}
			keys = append(keys, s.lastKeysOf(ns)...)
			continue
		}
		nsKeys := make([]cache.ClusterKey, 0, len(list.Items))
		for i := range list.Items {
			nsKeys = append(nsKeys, cache.ClusterKey{Namespace: ns, Name: list.Items[i].Name})
		}
		s.setLastKeys(ns, nsKeys, true)
		if s.setListFailing(ns, false) {
			s.logger.LogAttrs(ctx, slog.LevelInfo, "listing Clusters works again", slog.String("namespace", ns))
		}
		keys = append(keys, nsKeys...)
	}
	s.pruneLastKeys(watched)
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})
	return keys, failed
}

func (s *Service) lastKeysOf(ns string) []cache.ClusterKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.lastKeys[ns])
}

func (s *Service) setLastKeys(ns string, keys []cache.ClusterKey, keep bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !keep {
		delete(s.lastKeys, ns)
		delete(s.listFailing, ns)
		return
	}
	s.lastKeys[ns] = keys
}

// setListFailing records whether ns's Cluster list failed and reports
// whether that changed, so exactly one concurrent reader logs each
// transition.
func (s *Service) setListFailing(ns string, failing bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listFailing[ns] == failing {
		return false
	}
	if failing {
		s.listFailing[ns] = true
	} else {
		delete(s.listFailing, ns)
	}
	return true
}

func (s *Service) pruneLastKeys(watched map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ns := range s.lastKeys {
		if !watched[ns] {
			delete(s.lastKeys, ns)
		}
	}
	for ns := range s.listFailing {
		if !watched[ns] {
			delete(s.listFailing, ns)
		}
	}
}

// gathered is everything the cache holds about one Cluster.
type gathered struct {
	key     cache.ClusterKey
	cluster *cnpgv1.Cluster
	pods    []*corev1.Pod
	events  []*corev1.Event
	backups []*cnpgv1.Backup
	// priorBackups name this Cluster but predate it (created before
	// it, or owned by a Cluster with another UID): a recreated Cluster
	// must not inherit them as its recency, topology or timeline.
	priorBackups []*cnpgv1.Backup
	scheduled    []*cnpgv1.ScheduledBackup
	poolers      []*cnpgv1.Pooler
	lease        *coordinationv1.Lease
	quorum       *cnpgv1.FailoverQuorum
	store        *unstructured.Unstructured
	swept        sweep.Snapshot
	hasSweep     bool
	// volumesReported: an instance reported its volumes recently (the
	// fork's volumeUsages or cnpg_disk_* metrics).
	volumesReported bool
	// volumesRefused: the trend store, at its cap, turned away a volume
	// of this Cluster recently: its forecasts are missing, not flat.
	volumesRefused bool
	version        cnpgstatus.Version
	// upgradeInFlight names an instance manager / operator annotation
	// version mismatch (cnpgstatus.UpgradeInFlight), or is empty.
	upgradeInFlight string
	minor           cnpgstatus.Minor
	readable        map[string]bool
	// failed names the readable sources whose read failed this time:
	// their lists are empty because nothing was read, not because
	// nothing exists.
	failed map[string]bool
}

// listed reports that a source was read: readable, and its read did not
// fail.
func (g *gathered) listed(source string) bool {
	return g.readable[source] && !g.failed[source]
}

// readFailed records a failed read of a readable source; a NotFound
// answer to a Get is an answer, not a failure.
func (g *gathered) readFailed(source string, err error) {
	if err == nil || apierrors.IsNotFound(err) {
		return
	}
	if g.failed == nil {
		g.failed = map[string]bool{}
	}
	g.failed[source] = true
}

// markFailed marks, on a view's source states, every source whose read
// failed while gathering: the view rests on a read that did not happen.
func (g *gathered) markFailed(states []viewmodel.SourceState) []viewmodel.SourceState {
	for _, source := range slices.Sorted(maps.Keys(g.failed)) {
		states = unread(states, source)
	}
	return states
}

func (g *gathered) sweepKey() sweep.Key {
	return sweep.Key{Namespace: g.key.Namespace, Name: g.key.Name}
}

// gather reads one Cluster and its children from the cache. withEvents
// skips the event list where a view does not need it (the cluster list).
func (s *Service) gather(ctx context.Context, key cache.ClusterKey, withEvents bool) (*gathered, error) {
	reader := s.cache.Reader()
	g := &gathered{key: key, readable: map[string]bool{}}
	for _, source := range []string{
		cache.SourceClusters, cache.SourcePods, cache.SourceEvents, cache.SourceBackups, cache.SourceScheduledBackups,
		cache.SourcePoolers, cache.SourceLeases, cache.SourceFailoverQuorums, cache.SourceObjectStores,
	} {
		g.readable[source] = s.cache.Readable(key.Namespace, source)
	}
	if !g.readable[cache.SourceClusters] {
		return nil, ErrNotFound
	}
	cluster := &cnpgv1.Cluster{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	g.cluster = cluster
	inNS := client.InNamespace(key.Namespace)
	byCluster := client.MatchingFields{cache.IndexCluster: key.Name}

	if g.readable[cache.SourcePods] {
		pods := &corev1.PodList{}
		err := reader.List(ctx, pods, inNS, client.MatchingFields{cache.IndexCluster: key.Name})
		g.readFailed(cache.SourcePods, err)
		if err == nil {
			for i := range pods.Items {
				if isMember(&pods.Items[i], cluster) {
					g.pods = append(g.pods, &pods.Items[i])
				}
			}
			sort.Slice(g.pods, func(i, j int) bool { return g.pods[i].Name < g.pods[j].Name })
		}
	}
	if g.readable[cache.SourceBackups] {
		list := &cnpgv1.BackupList{}
		err := reader.List(ctx, list, inNS, byCluster)
		g.readFailed(cache.SourceBackups, err)
		if err == nil {
			for i := range list.Items {
				if predatesCluster(&list.Items[i], cluster) {
					g.priorBackups = append(g.priorBackups, &list.Items[i])
					continue
				}
				g.backups = append(g.backups, &list.Items[i])
			}
		}
	}
	if g.readable[cache.SourceScheduledBackups] {
		list := &cnpgv1.ScheduledBackupList{}
		err := reader.List(ctx, list, inNS, byCluster)
		g.readFailed(cache.SourceScheduledBackups, err)
		if err == nil {
			for i := range list.Items {
				g.scheduled = append(g.scheduled, &list.Items[i])
			}
		}
	}
	if g.readable[cache.SourcePoolers] {
		list := &cnpgv1.PoolerList{}
		err := reader.List(ctx, list, inNS, byCluster)
		g.readFailed(cache.SourcePoolers, err)
		if err == nil {
			for i := range list.Items {
				g.poolers = append(g.poolers, &list.Items[i])
			}
		}
	}
	if g.readable[cache.SourceLeases] {
		// NotFound is an answer (no lease); any other error is not.
		lease := &coordinationv1.Lease{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, lease)
		g.readFailed(cache.SourceLeases, err)
		if err == nil && metav1.IsControlledBy(lease, cluster) {
			g.lease = lease
		}
	}
	if g.readable[cache.SourceFailoverQuorums] {
		quorum := &cnpgv1.FailoverQuorum{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, quorum)
		g.readFailed(cache.SourceFailoverQuorums, err)
		if err == nil {
			g.quorum = quorum
		}
	}
	if g.readable[cache.SourceObjectStores] {
		if name := objectStoreName(cluster); name != "" {
			store := &unstructured.Unstructured{}
			store.SetGroupVersionKind(cache.ObjectStoreGVK)
			err := reader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: name}, store)
			g.readFailed(cache.SourceObjectStores, err)
			if err == nil {
				g.store = store
			}
		}
	}
	if withEvents && g.readable[cache.SourceEvents] {
		g.events = s.clusterEvents(ctx, reader, g)
	}
	if sw := s.sweeper(); sw != nil {
		g.swept, g.hasSweep = sw.Snapshot(g.sweepKey())
		if s.opts.Trends != nil {
			since := s.now().Add(-max(3*g.swept.Interval, 5*time.Minute))
			g.volumesReported = s.opts.Trends.Reported(key.Namespace, key.Name, since)
			g.volumesRefused = s.opts.Trends.RefusedFor(key.Namespace, key.Name, since)
		}
	}
	versionEvidence := cnpgstatus.VersionEvidence{
		InstanceManagerVersions: managerVersions(g),
		InstancePods:            g.pods,
		Cluster:                 cluster,
		LeaseServed:             leaseObserved(g),
		DatabaseRolesServed:     s.cache.Served(cache.SourceDatabaseRoles),
		FailoverQuorumsServed:   s.cache.Served(cache.SourceFailoverQuorums),
	}
	g.version = cnpgstatus.DetectVersion(versionEvidence)
	g.upgradeInFlight = cnpgstatus.UpgradeInFlight(versionEvidence)
	g.minor = g.version.Minor
	return g, nil
}

// isMember is the membership rule: the label selected the pod, but
// labels are freely settable, so the controller owner reference must name
// this Cluster (a pooler pod is controlled by its Deployment and is not
// an instance).
// predatesCluster reports a Backup that names this Cluster
// (spec.cluster.name, the index) but belongs to an earlier Cluster of the
// same name: it was created before this Cluster, or a Cluster owner
// reference carries another UID. CNPG sets no owner on a manual Backup
// and the ScheduledBackup (or the Cluster, backupOwnerReference: cluster)
// on a scheduled one, so the UID test applies only when that owner is a
// Cluster.
func predatesCluster(b *cnpgv1.Backup, cluster *cnpgv1.Cluster) bool {
	if !cluster.CreationTimestamp.IsZero() && b.CreationTimestamp.Before(&cluster.CreationTimestamp) {
		return true
	}
	for _, ref := range b.OwnerReferences {
		if ref.Kind == "Cluster" && ref.UID != "" && cluster.UID != "" && ref.UID != cluster.UID {
			return true
		}
	}
	return false
}

func isMember(pod *corev1.Pod, cluster *cnpgv1.Cluster) bool {
	ref := metav1.GetControllerOf(pod)
	return ref != nil && ref.Kind == "Cluster" && ref.Name == cluster.Name &&
		(ref.UID == "" || ref.UID == cluster.UID)
}

func managerVersions(g *gathered) []string {
	var out []string
	for _, inst := range g.swept.Instances {
		if inst.Reading != nil && inst.Reading.InstanceManagerVersion != "" {
			out = append(out, inst.Reading.InstanceManagerVersion)
		}
	}
	sort.Strings(out)
	return out
}

func leaseObserved(g *gathered) *bool {
	if !g.listed(cache.SourceLeases) {
		return nil
	}
	observed := g.lease != nil
	return &observed
}

// objectStoreName is the barman-cloud plugin's ObjectStore of the
// Cluster's WAL archiver, from spec.plugins[].parameters.barmanObjectName.
func objectStoreName(cluster *cnpgv1.Cluster) string {
	for _, plugin := range cluster.Spec.Plugins {
		if plugin.Enabled != nil && !*plugin.Enabled {
			continue
		}
		if name := plugin.Parameters["barmanObjectName"]; name != "" && (plugin.Name == cnpgstatus.BarmanCloudPlugin || (plugin.IsWALArchiver != nil && *plugin.IsWALArchiver)) {
			return name
		}
	}
	return ""
}

// clusterEvents lists the events about the Cluster and its children,
// newest first, within the age and count bounds.
func (s *Service) clusterEvents(ctx context.Context, reader client.Reader, g *gathered) []*corev1.Event {
	involved := make([]string, 0, 1+len(g.pods)+len(g.backups)+len(g.scheduled)+len(g.poolers))
	involved = append(involved, cache.InvolvedKey("Cluster", g.key.Name))
	for _, pod := range g.pods {
		involved = append(involved, cache.InvolvedKey("Pod", pod.Name))
	}
	for _, b := range g.backups {
		involved = append(involved, cache.InvolvedKey("Backup", b.Name))
	}
	for _, b := range g.scheduled {
		involved = append(involved, cache.InvolvedKey("ScheduledBackup", b.Name))
	}
	for _, p := range g.poolers {
		involved = append(involved, cache.InvolvedKey("Pooler", p.Name))
	}
	cutoff := s.now().Add(-s.opts.EventsMaxAge)
	seen := map[types.UID]bool{}
	var out []*corev1.Event
	for _, key := range involved {
		list := &corev1.EventList{}
		if err := reader.List(ctx, list, client.InNamespace(g.key.Namespace), client.MatchingFields{cache.IndexInvolved: key}); err != nil {
			// Some events may be missing: the source is marked failed,
			// so nothing reads the partial list as complete.
			g.readFailed(cache.SourceEvents, err)
			continue
		}
		for i := range list.Items {
			event := &list.Items[i]
			if seen[event.UID] || eventTime(event).Before(cutoff) {
				continue
			}
			seen[event.UID] = true
			out = append(out, event)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return eventTime(out[i]).After(eventTime(out[j])) })
	return out
}

// eventTime is the event's last occurrence across the core and
// events.k8s.io fields.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}
