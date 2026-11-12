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
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/kube"
	"github.com/fyannk/pgToolBox/internal/console/logfollow"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// Input assembles one Cluster's diagnostics input from the published
// snapshots (diag.InputSource). Each snapshot carries its Has flag from
// the source's state, never from emptiness: an unreadable source makes
// its checks "could not run", an empty readable one makes them clear.
//
// Fed from the informer cache: the Cluster, member pods, events, backups
// and schedules, poolers, failover quorum, primary lease, declarative
// objects and image catalogs. Fed from the console's runners: the
// instance status and metrics sweep, the log matcher (ALLOW_LOGS), the
// API server version (discovery), repository evidence (the agent
// channel) and the revision history (HISTORY_ENABLED), filtered to the
// Cluster's own objects.
//
// Not fed, by construction, so their checks say they could not run:
// infrastructure (Services, Jobs and VolumeSnapshots are outside the
// console's Role; PVCs alone would make the Service checks
// read "no read-write Service"), quotas (ResourceQuotas are outside the
// Role), and the pooler pods and pooler metrics (pooler membership needs
// the ReplicaSet and Deployment chain, also outside the Role).
func (s *Service) Input(ctx context.Context, key cache.ClusterKey) (diagnose.Input, bool) {
	g, err := s.gather(ctx, key, true)
	if err != nil {
		return diagnose.Input{}, false
	}
	now := s.now().UTC()
	facts := kube.NewFacts(key.Namespace, key.Name, now)
	// The one CNPG version detector gates every version-pinned rule.
	in := diagnose.Input{Now: now, CNPGVersion: g.version, HasCNPGVersion: true}

	stale := func(source string) bool {
		return s.cache.Source(key.Namespace, source).State == viewmodel.SourceStale
	}

	clusterFacts, err := facts.Cluster(g.cluster)
	if err == nil {
		clusterFacts.Present = true
		in.Cluster = observe.Snapshot{ObservedAt: now, Stale: stale(cache.SourceClusters), Cluster: clusterFacts}
		in.HasCluster = true
	}

	in.Volumes, in.HasVolumes = s.diagnoseVolumes(ctx, g)

	if g.listed(cache.SourcePods) {
		in.Pods = observe.PodsSnapshot{ObservedAt: now, Stale: stale(cache.SourcePods)}
		for _, pod := range g.pods {
			if pf, member, err := facts.Pod(pod); err == nil && member {
				in.Pods.Pods = append(in.Pods.Pods, pf)
			}
		}
		in.HasPods = true
	}

	if g.listed(cache.SourceEvents) {
		in.Events = observe.EventsSnapshot{ObservedAt: now, Stale: stale(cache.SourceEvents)}
		for _, event := range g.events {
			if ef, ok := facts.Event(event); ok {
				in.Events.Events = append(in.Events.Events, ef)
			}
		}
		in.HasEvents = true
	}

	if g.listed(cache.SourceBackups) {
		in.Backups = observe.BackupsSnapshot{ObservedAt: now, Stale: stale(cache.SourceBackups)}
		for _, b := range g.backups {
			if bf, ok, err := facts.Backup(b); err == nil && ok {
				in.Backups.Backups = append(in.Backups.Backups, bf)
			}
		}
		for _, sb := range g.scheduled {
			if sf, ok, err := facts.ScheduledBackup(sb); err == nil && ok {
				in.Backups.ScheduledBackups = append(in.Backups.ScheduledBackups, sf)
			}
		}
		in.Backups.ObjectStore = facts.ObjectStoreReference(g.cluster, g.store, g.listed(cache.SourceObjectStores))
		// Without the scheduled-backup source the catalog would read as
		// "no schedule": only claim the snapshot when both are readable.
		in.HasBackups = g.listed(cache.SourceScheduledBackups)
	}

	if g.listed(cache.SourcePoolers) {
		in.Poolers = observe.PoolersSnapshot{ObservedAt: now, Stale: stale(cache.SourcePoolers)}
		for _, p := range g.poolers {
			if pf, ok, err := facts.Pooler(p); err == nil && ok {
				in.Poolers.Poolers = append(in.Poolers.Poolers, pf)
			}
		}
		in.HasPoolers = true
	}

	if g.listed(cache.SourceFailoverQuorums) {
		in.FailoverQuorum = observe.FailoverQuorumSnapshot{ObservedAt: now, Stale: stale(cache.SourceFailoverQuorums)}
		if g.quorum != nil {
			if q, err := facts.FailoverQuorum(g.quorum); err == nil {
				in.FailoverQuorum.Quorum = q
			}
		}
		in.HasFailoverQuorum = true
	}

	if g.listed(cache.SourceLeases) {
		in.PrimaryLease = observe.PrimaryLeaseSnapshot{ObservedAt: now, Stale: stale(cache.SourceLeases)}
		if g.lease != nil {
			if l, err := facts.PrimaryLease(g.lease); err == nil {
				in.PrimaryLease.Lease = l
			}
		}
		in.HasPrimaryLease = true
	}

	s.declaredInput(ctx, g, facts, &in, now, stale)
	s.catalogInput(ctx, g, facts, &in, now, stale)
	if f := s.opts.KubeVersion; f != nil {
		in.KubeVersion, in.HasKubeVersion = f()
	}
	if f := s.opts.Logs; f != nil {
		if obs := f(key.Namespace, key.Name); obs != nil {
			in.Logs = obs
		}
	}
	if h := s.historyReader(); h != nil {
		in.History, in.HasHistory = s.clusterHistory(h, key), true
	}
	if s.opts.Evidence != nil {
		in.Evidence, in.HasEvidence = evidenceStatus(s.opts.Evidence.ForCluster(key.Namespace, key.Name))
	}
	in.Drills, in.HasDrills = s.diagnoseDrills(g.cluster)

	if sw := s.sweeper(); sw != nil {
		in.InstanceStatus = sw.InstanceStatus(g.sweepKey())
		if store := sw.Metrics(g.sweepKey()); store != nil {
			in.Metrics = store
			// With metrics on, the insight pack is read from the same
			// exposition; a Cluster not swept yet has nothing to show,
			// which the insight checks report as such.
			in.Insight, _ = sw.Insight(g.sweepKey())
			in.HasInsight = true
		}
	}
	if g.cluster != nil {
		in.Parameters = g.cluster.Spec.PostgresConfiguration.Parameters
	}
	return in, true
}

// declaredInput feeds the declarative objects when the kinds that exist
// are readable. DatabaseRole is new in CNPG 1.30: a kind the API server
// does not serve has no objects, which is true, not unknown.
func (s *Service) declaredInput(ctx context.Context, g *gathered, facts kube.Facts, in *diagnose.Input, now time.Time, stale func(string) bool) {
	ns := g.key.Namespace
	for _, source := range []string{cache.SourceDatabases, cache.SourcePublications, cache.SourceSubscriptions} {
		if !s.cache.Readable(ns, source) {
			return
		}
	}
	rolesServed := s.cache.Source(ns, cache.SourceDatabaseRoles).State != viewmodel.SourceNotInstalled
	if rolesServed && !s.cache.Readable(ns, cache.SourceDatabaseRoles) {
		return
	}
	reader := s.cache.Reader()
	inNS, byCluster := client.InNamespace(ns), client.MatchingFields{cache.IndexCluster: g.key.Name}
	snap := observe.DatabaseObjectsSnapshot{
		ObservedAt: now,
		Stale:      stale(cache.SourceDatabases) || stale(cache.SourcePublications) || stale(cache.SourceSubscriptions) || stale(cache.SourceDatabaseRoles),
	}
	dbs := &cnpgv1.DatabaseList{}
	pubs := &cnpgv1.PublicationList{}
	subs := &cnpgv1.SubscriptionList{}
	if reader.List(ctx, dbs, inNS, byCluster) != nil || reader.List(ctx, pubs, inNS, byCluster) != nil || reader.List(ctx, subs, inNS, byCluster) != nil {
		return
	}
	for i := range dbs.Items {
		if f, ok, err := facts.Database(&dbs.Items[i]); err == nil && ok {
			snap.Databases = append(snap.Databases, f)
		}
	}
	for i := range pubs.Items {
		if f, ok, err := facts.Publication(&pubs.Items[i]); err == nil && ok {
			snap.Publications = append(snap.Publications, f)
		}
	}
	for i := range subs.Items {
		if f, ok, err := facts.Subscription(&subs.Items[i]); err == nil && ok {
			snap.Subscriptions = append(snap.Subscriptions, f)
		}
	}
	if rolesServed {
		roles := &cnpgv1.DatabaseRoleList{}
		if reader.List(ctx, roles, inNS, byCluster) != nil {
			return
		}
		for i := range roles.Items {
			if f, ok, err := facts.DatabaseRole(&roles.Items[i]); err == nil && ok {
				snap.Roles = append(snap.Roles, f)
			}
		}
	}
	in.DatabaseObjects, in.HasDatabaseObjects = snap, true
}

// catalogInput feeds the namespace's image catalogs and the one
// ClusterImageCatalog the Cluster references, in five states: disabled
// without the opt-in read, not-referenced, present, absent only on a
// not-found, unknown otherwise.
func (s *Service) catalogInput(ctx context.Context, g *gathered, facts kube.Facts, in *diagnose.Input, now time.Time, stale func(string) bool) {
	ns := g.key.Namespace
	if !s.cache.Readable(ns, cache.SourceImageCatalogs) {
		return
	}
	list := &cnpgv1.ImageCatalogList{}
	if s.cache.Reader().List(ctx, list, client.InNamespace(ns)) != nil {
		return
	}
	snap := observe.ImageCatalogsSnapshot{ObservedAt: now, Stale: stale(cache.SourceImageCatalogs)}
	for i := range list.Items {
		if f, err := facts.ImageCatalog(&list.Items[i]); err == nil {
			snap.Catalogs = append(snap.Catalogs, f)
		}
	}
	if len(snap.Catalogs) > observe.MaxImageCatalogs {
		snap.Catalogs, snap.Truncated = snap.Catalogs[:observe.MaxImageCatalogs], true
	}
	ref := g.cluster.Spec.ImageCatalogRef
	switch {
	case !s.opts.AllowClusterCatalogs:
		snap.ClusterCatalogState = observe.ClusterCatalogDisabled
	case ref == nil || ref.Kind != "ClusterImageCatalog":
		snap.ClusterCatalogState = observe.ClusterCatalogNotReferenced
	case !s.cache.Readable("", cache.SourceClusterImageCatalogs):
		snap.ClusterCatalog, snap.ClusterCatalogState = observe.ImageCatalogFacts{Name: ref.Name}, observe.ClusterCatalogUnknown
	default:
		c := &cnpgv1.ClusterImageCatalog{}
		err := s.cache.Reader().Get(ctx, types.NamespacedName{Name: ref.Name}, c)
		switch {
		case apierrors.IsNotFound(err):
			snap.ClusterCatalog, snap.ClusterCatalogState = observe.ImageCatalogFacts{Name: ref.Name}, observe.ClusterCatalogAbsent
		case err != nil:
			snap.ClusterCatalog, snap.ClusterCatalogState = observe.ImageCatalogFacts{Name: ref.Name}, observe.ClusterCatalogUnknown
		default:
			if f, err := facts.ImageCatalog(c); err == nil {
				snap.ClusterCatalog, snap.ClusterCatalogState = f, observe.ClusterCatalogPresent
			} else {
				snap.ClusterCatalog, snap.ClusterCatalogState = observe.ImageCatalogFacts{Name: ref.Name}, observe.ClusterCatalogUnknown
			}
		}
	}
	in.ImageCatalogs, in.HasImageCatalogs = snap, true
}

// clusterHistory is the retained timeline restricted to the Cluster and
// the objects that belong to it.
func (s *Service) clusterHistory(h HistoryReader, key cache.ClusterKey) history.Snapshot {
	snap, _ := h.Snapshot()
	out := history.Snapshot{Generation: snap.Generation, Evicted: snap.Evicted}
	for _, e := range snap.Entries {
		if e.Namespace == key.Namespace && s.belongs(h, e.UID, e.Kind, e.Namespace, e.Name, key) {
			out.Entries = append(out.Entries, e)
		}
	}
	return out
}

// LogRoster is what the log follower follows: every watched Cluster and
// the running containers of its member pods.
func (s *Service) LogRoster() logfollow.Roster { return logRoster{s} }

type logRoster struct{ s *Service }

func (r logRoster) Keys(ctx context.Context) []logfollow.Key {
	keys := r.s.Keys(ctx)
	out := make([]logfollow.Key, 0, len(keys))
	for _, k := range keys {
		out = append(out, logfollow.Key{Namespace: k.Namespace, Name: k.Name})
	}
	return out
}

// UID is the Cluster's UID, empty when the cache does not hold it.
func (r logRoster) UID(ctx context.Context, key logfollow.Key) string {
	g, err := r.s.gather(ctx, cache.ClusterKey{Namespace: key.Namespace, Name: key.Name}, false)
	if err != nil || g.cluster == nil {
		return ""
	}
	return string(g.cluster.UID)
}

// Members are the running containers of the member pods: a terminated
// init container has nothing more to say, and a waiting one has no
// stream yet.
func (r logRoster) Members(ctx context.Context, key logfollow.Key) []logstream.Member {
	g, err := r.s.gather(ctx, cache.ClusterKey{Namespace: key.Namespace, Name: key.Name}, false)
	if err != nil || !g.listed(cache.SourcePods) {
		return nil
	}
	var out []logstream.Member
	for _, pod := range g.pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		for _, cs := range append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			if cs.State.Running != nil {
				out = append(out, logstream.Member{Pod: pod.Name, Container: cs.Name})
			}
		}
	}
	return out
}
