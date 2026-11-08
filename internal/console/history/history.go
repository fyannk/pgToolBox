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

// Package history is the console's revision history of the watched
// object definitions: the pkg/cnpgstatus/history store fed
// by the informer cache's tap, in memory by default, mirrored into one
// bbolt file per console when HISTORY_PATH is set. A persistent history
// is the one piece of state the console owns, which is why the chart
// forces one replica with Recreate when it is on (history.persistent);
// this package only honours the path it is given.
package history

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/kube"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history/bolt"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// recorded are the sources whose definitions are kept: the CNPG kinds a
// DBA asks "who changed what" about, and the member pods, whose
// incarnations the diagnostics timeline counts (a pod replaced over and
// over is invisible to every snapshot). Events, leases and PVCs churn
// without a definition change worth a revision. Pod status transitions
// are coalesced by the store's window and the whole is bounded.
var recorded = map[string]bool{
	cache.SourceClusters: true, cache.SourcePods: true, cache.SourceBackups: true, cache.SourceScheduledBackups: true,
	cache.SourcePoolers: true, cache.SourceDatabases: true, cache.SourceDatabaseRoles: true,
	cache.SourcePublications: true, cache.SourceSubscriptions: true, cache.SourceImageCatalogs: true,
}

// History is the store and, when persistent, its journal.
type History struct {
	store   *history.Store
	journal *bolt.Journal
	logger  *slog.Logger

	// owners maps an object incarnation (UID) to the Cluster it belongs
	// to, captured when it was observed, so a revision of a since-deleted
	// child still files under its Cluster.
	mu     sync.RWMutex
	owners map[string]Owner
	limit  int
}

// Owner is the Cluster an observed object belongs to.
type Owner struct{ Namespace, Cluster string }

// Open builds the history. A disabled configuration returns nil: no tap,
// no store. A persistent one opens the journal and primes the store from
// it; a journal that cannot be opened or read fails the start, because a
// deployment that mounted a volume for history expects it to survive.
func Open(cfg config.History, logger *slog.Logger) (*History, error) {
	if !cfg.Enabled {
		return nil, nil //nolint:nilnil // disabled is a valid, explicit outcome
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	limits := history.Limits{
		PerObject:      max(cfg.MaxRevisions/50, 20),
		MaxRevisions:   cfg.MaxRevisions,
		MaxBytes:       cfg.MaxBytes,
		CoalesceWindow: 30 * time.Second,
	}
	clock := observe.RealClock{}
	if !cfg.Persistent() {
		return &History{store: history.NewStore(limits, clock), logger: logger, owners: map[string]Owner{}, limit: ownerLimit(cfg)}, nil
	}
	journal, err := bolt.Open(cfg.Path, clock, logger)
	if err != nil {
		return nil, err
	}
	store, err := history.NewPersistedStore(limits, clock, journal)
	if err != nil {
		// The journal has no Close of its own (Run closes it on exit); a
		// failed start exits the process, which releases the file lock.
		return nil, err
	}
	h := &History{store: store, journal: journal, logger: logger, owners: map[string]Owner{}, limit: ownerLimit(cfg)}
	h.primeOwners()
	return h, nil
}

// primeOwners rebuilds the owners of a persisted store's retained
// revisions from their manifests. The UID → Cluster map is not journaled:
// without this, a child deleted before a restart (its last revision a
// manifest-less deletion, no live object to tap) would lose its Cluster
// and drop out of every per-Cluster timeline. Newest revisions first; one
// manifest decoded per incarnation.
func (h *History) primeOwners() {
	snap, _ := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range snap.Entries {
		if !entry.HasManifest || entry.UID == "" || len(h.owners) >= h.limit {
			continue
		}
		if _, known := h.owners[entry.UID]; known {
			continue
		}
		revision, ok := h.store.Revision(entry.Seq)
		if !ok {
			continue
		}
		if cluster := clusterOfManifest(revision.Group, revision.Kind, revision.Manifest); cluster != "" {
			h.owners[entry.UID] = Owner{Namespace: revision.Namespace, Cluster: cluster}
		}
	}
}

// Tap is the cache.Options.Tap that feeds the store.
func (h *History) Tap(kind cache.Kind, obj client.Object, deleted bool) {
	if h == nil || !recorded[kind.Source] {
		return
	}
	obs, err := kube.Observation(kind.Source, kind.GVK, obj, deleted)
	if err != nil {
		h.logger.Debug("history capture skipped", slog.String("source", kind.Source), slog.String("error", err.Error()))
		return
	}
	if cluster := clusterOf(obj); cluster != "" {
		h.remember(string(obj.GetUID()), Owner{Namespace: obj.GetNamespace(), Cluster: cluster})
	}
	h.store.Observe(obs)
}

// Seed is the cache.Options.Seed that feeds the store: the complete
// first listing of one source in one cache epoch, over the namespaces it
// covered. It is what classifies against the previous process life (an
// edit made while the console was down is an after-gap revision) and
// accounts for what that life knew and the listing no longer holds (an
// unobserved deletion), which a stream of watch adds cannot.
func (h *History) Seed(kind cache.Kind, namespaces []string, objs []client.Object) {
	if h == nil || !recorded[kind.Source] {
		return
	}
	observations := make([]history.Observation, 0, len(objs))
	for _, obj := range objs {
		obs, err := kube.Observation(kind.Source, kind.GVK, obj, false)
		if err != nil {
			// Skipping one object would read as its deletion; the seed is
			// not complete, so it is not a seed.
			h.logger.Warn("history seed skipped", slog.String("source", kind.Source), slog.String("error", err.Error()))
			for _, o := range objs {
				h.Tap(kind, o, false)
			}
			return
		}
		if cluster := clusterOf(obj); cluster != "" {
			h.remember(string(obj.GetUID()), Owner{Namespace: obj.GetNamespace(), Cluster: cluster})
		}
		observations = append(observations, obs)
	}
	h.store.SeedIn(kind.Source, namespaces, observations)
}

func ownerLimit(cfg config.History) int { return max(cfg.MaxRevisions*2, 1000) }

// clusterOf is the Cluster an object of a recorded source belongs to.
func clusterOf(obj client.Object) string {
	switch o := obj.(type) {
	case *cnpgv1.Cluster:
		return o.Name
	case *corev1.Pod:
		return o.Labels["cnpg.io/cluster"]
	case *cnpgv1.Backup:
		return o.Spec.Cluster.Name
	case *cnpgv1.ScheduledBackup:
		return o.Spec.Cluster.Name
	case *cnpgv1.Pooler:
		return o.Spec.Cluster.Name
	case *cnpgv1.Database:
		return o.Spec.ClusterRef.Name
	case *cnpgv1.DatabaseRole:
		return o.Spec.ClusterRef.Name
	case *cnpgv1.Publication:
		return o.Spec.ClusterRef.Name
	case *cnpgv1.Subscription:
		return o.Spec.ClusterRef.Name
	default:
		return ""
	}
}

// clusterOfManifest is clusterOf for a stored manifest (the normalized
// object as canonical JSON); a test holds the two equal for every
// recorded kind.
func clusterOfManifest(group, kind string, manifest []byte) string {
	var object struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		// Every recorded CNPG child names its Cluster at spec.cluster
		// (ClusterRef is serialized as "cluster" on Database,
		// DatabaseRole, Publication and Subscription too).
		Spec struct {
			Cluster struct {
				Name string `json:"name"`
			} `json:"cluster"`
		} `json:"spec"`
	}
	if len(manifest) == 0 || json.Unmarshal(manifest, &object) != nil {
		return ""
	}
	cnpg := group == cnpgv1.SchemeGroupVersion.Group
	switch {
	case cnpg && kind == "Cluster":
		return object.Metadata.Name
	case group == "" && kind == "Pod":
		return object.Metadata.Labels["cnpg.io/cluster"]
	case cnpg && (kind == "Backup" || kind == "ScheduledBackup" || kind == "Pooler" ||
		kind == "Database" || kind == "DatabaseRole" || kind == "Publication" || kind == "Subscription"):
		return object.Spec.Cluster.Name
	default:
		return ""
	}
}

// remember records an owner; past the bound it forgets the incarnations
// the store no longer retains.
func (h *History) remember(uid string, o Owner) {
	if uid == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.owners[uid] = o
	if len(h.owners) <= h.limit {
		return
	}
	snap, _ := h.store.Snapshot()
	live := make(map[string]bool, len(snap.Entries))
	for _, e := range snap.Entries {
		live[e.UID] = true
	}
	for k := range h.owners {
		if !live[k] && k != uid {
			delete(h.owners, k)
		}
	}
}

// OwnerOf returns the Cluster a revision's object belongs to. A Cluster
// revision recorded before the console knew its owner (journal replay)
// still files under its own name.
func (h *History) OwnerOf(uid, kind, namespace, name string) (Owner, bool) {
	if h == nil {
		return Owner{}, false
	}
	h.mu.RLock()
	o, ok := h.owners[uid]
	h.mu.RUnlock()
	if !ok && kind == "Cluster" {
		return Owner{Namespace: namespace, Cluster: name}, true
	}
	return o, ok
}

// Diff compares a revision with the previous retained definition.
func (h *History) Diff(seq uint64) (history.Diff, bool) {
	if h == nil {
		return history.Diff{}, false
	}
	return h.store.Diff(seq)
}

// Run flushes the journal until ctx is done; an in-memory history has
// nothing to run.
func (h *History) Run(ctx context.Context) error {
	if h == nil || h.journal == nil {
		<-ctx.Done()
		return nil
	}
	// Run flushes every interval and, when ctx ends, flushes once more
	// and closes the file.
	return h.journal.Run(ctx)
}

// Persistent reports whether the history survives restarts.
func (h *History) Persistent() bool { return h != nil && h.journal != nil }

// Snapshot is the retained timeline, newest first.
func (h *History) Snapshot() (history.Snapshot, bool) {
	if h == nil {
		return history.Snapshot{}, false
	}
	return h.store.Snapshot()
}

// Revision returns one full revision.
func (h *History) Revision(seq uint64) (history.Revision, bool) {
	if h == nil {
		return history.Revision{}, false
	}
	return h.store.Revision(seq)
}

// DrillJournal is the journal the restore-drill records are kept in when
// history is persistent; nil otherwise (drills.Store then keeps them in
// memory). The returned value is a *bolt.Journal, which implements
// drills.Persister.
func (h *History) DrillJournal() *bolt.Journal {
	if h == nil {
		return nil
	}
	return h.journal
}
