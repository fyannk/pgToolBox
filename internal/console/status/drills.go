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
	"math"
	"strconv"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/opstate"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// Restore drills as the read model sees them: the drill Clusters are
// ordinary Clusters of a watched namespace (the configuration requires
// it), so their phase, their instance's /pg/status and their exporter
// come from the same cache and sweep as every other Cluster's.

// DrillsView is the restore-drill record and configuration the Backups
// screen shows.
type DrillsView struct {
	Records *drills.Store
	// Namespace is RESTORE_DRILL_NAMESPACE (empty: the source's).
	Namespace string
	TimeLimit time.Duration
}

// SetDrills wires the restore-drill record after construction (the
// record's store is opened beside the history journal).
func (s *Service) SetDrills(d *DrillsView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Drills = d
}

// databaseSizeKey is the metrics catalog key of cnpg_pg_database_size_bytes.
const databaseSizeKey = "database-size"

// DrillClusters lists the Clusters labelled as drills of owner in the
// watched namespaces. Verification of the seal is the reaper's.
func (s *Service) DrillClusters(ctx context.Context, owner string) ([]*cnpgv1.Cluster, error) {
	reader := s.cache.Reader()
	var out []*cnpgv1.Cluster
	for _, ns := range s.cache.Namespaces() {
		if !s.cache.Readable(ns, cache.SourceClusters) {
			continue
		}
		list := &cnpgv1.ClusterList{}
		if err := reader.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{ops.LabelDrillOwner: owner}); err != nil {
			return nil, err
		}
		for i := range list.Items {
			out = append(out, &list.Items[i])
		}
	}
	return out, nil
}

// ErrDrillNamespaceUnreadable is DrillCluster's answer for a namespace
// whose Clusters the console cannot read: nothing is known there, which
// is not the same as gone.
var ErrDrillNamespaceUnreadable = errors.New("the console cannot read Clusters in this namespace")

// DrillCluster reads one Cluster by name from the cache: nil when it does
// not exist, ErrDrillNamespaceUnreadable when the namespace is not
// readable.
func (s *Service) DrillCluster(ctx context.Context, namespace, name string) (*cnpgv1.Cluster, error) {
	if !s.cache.Readable(namespace, cache.SourceClusters) {
		return nil, ErrDrillNamespaceUnreadable
	}
	c := &cnpgv1.Cluster{}
	err := s.cache.Reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, c)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return c, nil
}

// DrillObservation reads one drill Cluster.
func (s *Service) DrillObservation(ctx context.Context, namespace, name string) (drills.Observation, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return drills.Observation{}, err
	}
	o := drills.Observation{Phase: g.cluster.Status.Phase, ReadyInstances: g.cluster.Status.ReadyInstances}
	instance := g.cluster.Status.CurrentPrimary
	if g.hasSweep {
		inst, ok := g.swept.Instances[instance]
		if !ok && len(g.swept.Instances) == 1 {
			for _, only := range g.swept.Instances {
				inst, ok = only, true
			}
		}
		switch {
		case ok && inst.Reading != nil:
			o.Reading = &drills.Reading{IsPrimary: inst.Reading.IsPrimary, TimelineID: inst.Reading.TimelineID, ObservedAt: inst.Reading.ObservedAt}
		case ok && inst.State != viewmodel.StatusReadOK && inst.State != viewmodel.StatusReadPending:
			// A drill whose CA the bundle lacks would otherwise wait,
			// silent, until it times out.
			o.ReadProblem = inst.Reason
		}
	}
	o.DatabaseBytes = s.instanceDatabaseBytes(g.sweepKey(), instance)
	return o, nil
}

// DatabaseBytes is the database size the exporter of the Cluster's
// primary (the caller's status.currentPrimary) last reported, nil when
// it reported none. It reads nothing from the cache, so it cannot take a
// failed read for a missing size.
func (s *Service) DatabaseBytes(namespace, name, primary string) *int64 {
	return s.instanceDatabaseBytes(sweep.Key{Namespace: namespace, Name: name}, primary)
}

func (s *Service) instanceDatabaseBytes(key sweep.Key, instance string) *int64 {
	sw := s.sweeper()
	if sw == nil || instance == "" {
		return nil
	}
	store := sw.Metrics(key)
	if store == nil {
		return nil
	}
	st, ok := store.SeriesStats(databaseSizeKey)[instance]
	if !ok || st.Latest == nil || math.IsNaN(*st.Latest) || *st.Latest < 0 {
		return nil
	}
	v := int64(*st.Latest)
	return &v
}

// ObjectStore reads a barman-cloud ObjectStore from the cache: its
// destination and credential kind, whether it exists, and whether the
// console watches ObjectStores there. A configuration that does not parse
// is reported as existing with empty facts (which never match).
func (s *Service) ObjectStore(ctx context.Context, namespace, name string) (facts ops.StoreFacts, exists, readable bool) {
	if !s.cache.Readable(namespace, cache.SourceObjectStores) {
		return ops.StoreFacts{}, false, false
	}
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(cache.ObjectStoreGVK)
	if err := s.cache.Reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, store); err != nil {
		// Only NotFound says the ObjectStore does not exist; a failed
		// read says nothing either way.
		return ops.StoreFacts{}, false, apierrors.IsNotFound(err)
	}
	facts, _ = ops.ObjectStoreFacts(store.Object)
	return facts, true, true
}

// PgBackRestTarget is the backup a pgBackRest drill restores: the newest
// backup of the evidence agent's recovery window, read from a live
// snapshot only (a stale one may name a backup expired since). A live read
// of a snapshot the agent itself marks stale (its last refresh failed and
// it serves the previous one) is stale too.
func (s *Service) PgBackRestTarget(namespace, name string) (opstate.PgBackRestTarget, string) {
	reader := s.opts.Evidence
	if reader == nil {
		return opstate.PgBackRestTarget{}, "the console reads no repository evidence"
	}
	r := reader.ForCluster(namespace, name)
	switch {
	case r.State != viewmodel.EvidenceLive:
		return opstate.PgBackRestTarget{}, "the evidence is " + string(r.State)
	case r.Snapshot != nil && r.Snapshot.Stale:
		return opstate.PgBackRestTarget{}, "the agent's last refresh failed: its snapshot is stale"
	case r.Snapshot == nil || r.Snapshot.Details.PGBackRest == nil:
		return opstate.PgBackRestTarget{}, "the agent reports no pgBackRest stanza"
	case r.Snapshot.Details.PGBackRest.RecoveryWindow == nil:
		return opstate.PgBackRestTarget{}, "no backup reaches the WAL frontier"
	}
	p := r.Snapshot.Details.PGBackRest
	t := p.RecoveryWindow.NewestStopAt.UTC()
	return opstate.PgBackRestTarget{
		Label: p.RecoveryWindow.NewestBackup, Stanza: p.Stanza, StoppedAt: &t,
		Plugin: r.PgBackRestPlugin, Object: r.PgBackRestObject,
	}, ""
}

// drillsBlock is the restore-drill block of a Cluster's Backups screen.
func (s *Service) drillsBlock(namespace, name string) (out viewmodel.RestoreDrills) {
	s.mu.RLock()
	d := s.opts.Drills
	s.mu.RUnlock()
	out = viewmodel.RestoreDrills{Items: []viewmodel.RestoreDrill{}, NotChecked: []string{}, WindowDays: int(diagnose.DrillWindow / (24 * time.Hour))}
	defer func() {
		// The same window as the restore-drill-overdue check.
		out.RestoreTested = out.LastPassedAt != nil && s.now().Sub(*out.LastPassedAt) <= diagnose.DrillWindow
	}()
	if d == nil || d.Records == nil {
		out.Scope = "restore drills are not enabled on this console (RESTORE_DRILL_ENABLED)"
		return out
	}
	out.Enabled = true
	out.Namespace = d.Namespace
	if out.Namespace == "" {
		out.Namespace = namespace
	}
	out.TimeLimitSeconds = int64(d.TimeLimit / time.Second)
	out.SizeTolerancePercent = drills.SizeTolerancePercent
	out.NotChecked = append(out.NotChecked, drills.NotChecked...)
	for _, r := range d.Records.Source(drills.SourceKey{Namespace: namespace, Name: name}) {
		out.Items = append(out.Items, drills.View(r))
		if r.Result == viewmodel.DrillPassed && out.LastPassedAt == nil {
			t := r.FinishedAt.UTC()
			out.LastPassedAt = &t
		}
	}
	switch {
	case d.Records.Kept():
		out.Scope = "outcomes kept in this console's shared-state ConfigMap (bounded to the " + strconv.Itoa(drills.MaxPerSource) +
			" newest drills per Cluster); the checks of a drill that finished before a restart are not kept: enable persistent history to keep them"
	case d.Records.Persistent():
		out.Scope = "kept in this console's persistent history (bounded to the " + strconv.Itoa(drills.MaxPerSource) + " newest drills per Cluster)"
	default:
		out.LostAtRestart = true
		out.Scope = "drill history lost at restart: kept in this console replica's memory since it started; " +
			"enable persistent history or the shared-state ConfigMap to keep drill results across restarts"
	}
	return out
}

// diagnoseDrills is the restore-drill record of one Cluster for the
// diagnostics: false when drills are not enabled on this console.
func (s *Service) diagnoseDrills(c *cnpgv1.Cluster) (diagnose.DrillsSnapshot, bool) {
	s.mu.RLock()
	d := s.opts.Drills
	s.mu.RUnlock()
	if d == nil || d.Records == nil || c == nil {
		return diagnose.DrillsSnapshot{}, false
	}
	out := diagnose.DrillsSnapshot{RecordedSince: d.Records.RecordedSince(), Persistent: d.Records.Persistent()}
	if _, isDrill := c.Labels[ops.LabelRestoreDrillOf]; !isDrill {
		_, err := ops.DrillRecoverySource(c)
		out.Drillable = err == nil
	}
	src := drills.SourceKey{Namespace: c.Namespace, Name: c.Name}
	out.Recorded = len(d.Records.Source(src))
	if at, ok := d.Records.LastPassed(src); ok {
		out.LastPassedAt = at.UTC()
	}
	return out, true
}
