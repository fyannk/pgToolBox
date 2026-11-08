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
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/insightview"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
)

// The insight screens read what the Cluster's exporters report through
// the insight pack; the conversion is insightview's. No SQL, no Secret,
// no call beyond the exporter reads the sweep already makes (and the
// rate-limited on-demand read of the primary for the blocking tree).

// Insight is the /insight screens of one Cluster.
func (s *Service) Insight(ctx context.Context, namespace, name string) (viewmodel.Insight, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.Insight{}, err
	}
	snap, has, interval := s.insightSnapshot(g.sweepKey())
	out := insightview.Build(g.cluster, snap, has, interval, s.now().UTC())
	out.Sources = []viewmodel.SourceState{s.insightSource(has)}
	return out, nil
}

// InsightBlocking is the lock graph, read again from the primary's
// exporter when refresh is set (at most once a second per Cluster,
// whoever asks). A failed refresh still answers with the sweep's last
// reading, stating why it is not fresher.
func (s *Service) InsightBlocking(ctx context.Context, namespace, name string, refresh bool) (viewmodel.InsightBlocking, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.InsightBlocking{}, err
	}
	var refreshedAt *time.Time
	var refreshErr error
	if sw := s.sweeper(); refresh && sw != nil {
		at, err := sw.RefreshInsight(ctx, g.sweepKey())
		if err == nil {
			at = at.UTC()
			refreshedAt = &at
		}
		refreshErr = err
	}
	snap, has, interval := s.insightSnapshot(g.sweepKey())
	now := s.now().UTC()
	out := insightview.Build(g.cluster, snap, has, interval, now).Blocking
	out.RefreshedAt = refreshedAt
	if refreshErr != nil && out.Available {
		reason := refreshErr.Error()
		switch {
		case errors.Is(refreshErr, sweep.ErrNoPrimary):
			reason = "no primary with a readable exporter is known"
		case errors.Is(refreshErr, sweep.ErrReadsBusy):
			reason = "the console's exporter reads are all in use; try again in a moment"
		}
		out.Text = "The on-demand read failed (" + reason + "); this is the sweep's last reading."
	}
	return out, nil
}

// FleetStatements is the query insight of the watched Clusters that
// visible keeps (the API's authorization), built over those only.
func (s *Service) FleetStatements(ctx context.Context, visible func([]viewmodel.ClusterRef) []viewmodel.ClusterRef) (viewmodel.FleetStatements, error) {
	keys := s.Keys(ctx)
	refs := make([]viewmodel.ClusterRef, len(keys))
	for i, key := range keys {
		refs[i] = viewmodel.ClusterRef{Namespace: key.Namespace, Name: key.Name}
	}
	kept := visible(refs)
	clusters := make([]insightview.FleetCluster, 0, len(kept))
	for _, ref := range kept {
		snap, has, interval := s.insightSnapshot(sweep.Key{Namespace: ref.Namespace, Name: ref.Name})
		clusters = append(clusters, insightview.FleetCluster{Ref: ref, Snapshot: snap, Has: has, Interval: interval})
	}
	return insightview.Fleet(clusters, s.now().UTC()), nil
}

// insightSnapshot is the sweep's insight state of one Cluster: has is
// false when exporters are not scraped; interval is the sweep cadence.
func (s *Service) insightSnapshot(key sweep.Key) (insight.Snapshot, bool, time.Duration) {
	sw := s.sweeper()
	if sw == nil {
		return insight.Snapshot{}, false, 0
	}
	store := sw.Metrics(key)
	if store == nil {
		return insight.Snapshot{}, false, 0
	}
	snap, _ := sw.Insight(key)
	return snap, true, store.Interval()
}

func (s *Service) insightSource(has bool) viewmodel.SourceState {
	if !has {
		st := viewmodel.SourceState{
			Source: "insight", State: viewmodel.SourceDisabled,
			Reason: "METRICS_ENABLED is false, or the Cluster was not swept yet",
		}
		if sw := s.sweeper(); sw != nil && sw.State().State == viewmodel.SourceLive {
			st.State, st.Reason = viewmodel.SourcePending, "the Cluster was not swept yet"
		}
		return st
	}
	st := s.sweeper().State()
	st.Source = "insight"
	return st
}
