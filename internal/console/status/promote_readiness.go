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
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// promoteReadiness is what a person weighs before confirming a
// switchover to the replica r: its Pod, whether it streams, how far its
// replay is behind, its synchronous state on the primary and fencing.
// Each fact keeps its own source and age; a fact the console did not
// read is unknown, never ok. It is not a score and refuses nothing: the
// offer's blockers (cnpgstatus.PromotePreconditions) do.
func (s *Service) promoteReadiness(g *gathered, r viewmodel.InstanceRow, now time.Time) []viewmodel.ReadinessFact {
	stale := sweepStale(g, now)
	return []viewmodel.ReadinessFact{
		s.readinessPod(g, r, now),
		readinessStreaming(g, r, stale),
		readinessLag(g, r, stale),
		readinessSync(g, r.Name, stale),
		s.readinessFencing(g, r, now),
	}
}

func (s *Service) readinessPod(g *gathered, r viewmodel.InstanceRow, now time.Time) viewmodel.ReadinessFact {
	f := viewmodel.ReadinessFact{Key: viewmodel.ReadinessPod}
	if r.Pod == nil {
		f.Health, f.Text = viewmodel.HealthCritical, "no Pod: the operator reports the instance but no Pod exists"
		f.Provenance = s.provenance(g.key.Namespace, cache.SourcePods, viewmodel.OriginKubernetes, "Pod", now)
		return f
	}
	f.Provenance = r.Pod.Provenance
	f.Provenance.Source = "Pod.status.conditions[Ready]"
	switch {
	case r.Pod.Deleting:
		f.Health, f.Text = viewmodel.HealthProgressing, "the Pod is being deleted"
	case !r.Pod.Ready && r.Pod.Waiting != "":
		f.Health, f.Text = viewmodel.HealthWarning, "not Ready ("+r.Pod.Waiting+")"
	case !r.Pod.Ready:
		f.Health, f.Text = viewmodel.HealthWarning, "not Ready"
	default:
		f.Health, f.Text = viewmodel.HealthOK, "Ready"
	}
	return f
}

// instanceRead is the provenance of the instance's own report and
// whether that report is a current reading of PostgreSQL.
func instanceRead(g *gathered, r viewmodel.InstanceRow, field string, stale bool) (viewmodel.Provenance, bool) {
	inst, swept := g.swept.Instances[r.Name]
	if !swept {
		return viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: field}, false
	}
	p := instanceProvenance(inst, field, stale)
	return p, r.Live != nil && r.StatusRead.State == viewmodel.StatusReadOK && r.Live.PostgresUnread == nil && !p.Stale
}

// notRead says why an instance fact is not a current reading: an old
// reading is not shown as the current one.
func notRead(r viewmodel.InstanceRow, p viewmodel.Provenance) string {
	switch {
	case r.Live != nil && r.Live.PostgresUnread != nil:
		return "not read: the instance manager answered without reaching PostgreSQL"
	case r.StatusRead.Reason != "":
		return "not read: " + r.StatusRead.Reason
	case p.StaleReason != "":
		return "not read now: " + p.StaleReason
	default:
		return "not read"
	}
}

func readinessStreaming(g *gathered, r viewmodel.InstanceRow, stale bool) viewmodel.ReadinessFact {
	f := viewmodel.ReadinessFact{Key: viewmodel.ReadinessStreaming, Health: viewmodel.HealthUnknown}
	p, ok := instanceRead(g, r, "/pg/status walReceiverActive, replayPaused", stale)
	f.Provenance = p
	switch {
	case !ok:
		f.Text = notRead(r, p)
	case r.Live.IsPrimary:
		f.Health, f.Text = viewmodel.HealthCritical, "reports itself primary"
	case !r.Live.WALReceiverActive:
		f.Health, f.Text = viewmodel.HealthWarning, "not streaming: no active WAL receiver"
	case r.Live.ReplayPaused:
		f.Health, f.Text = viewmodel.HealthWarning, "streaming, but WAL replay is paused"
	default:
		f.Health, f.Text = viewmodel.HealthOK, "streaming from the primary"
	}
	return f
}

func readinessLag(g *gathered, r viewmodel.InstanceRow, stale bool) viewmodel.ReadinessFact {
	f := viewmodel.ReadinessFact{Key: viewmodel.ReadinessReplayLag, Health: viewmodel.HealthUnknown}
	p, ok := instanceRead(g, r, "/pg/status replayLsn against the primary's currentLsn", stale)
	f.Provenance = p
	switch {
	case !ok:
		f.Text = notRead(r, p)
	case r.Live.LagBytes == nil:
		f.Text = "not measured: the primary's position was not read"
	case *r.Live.LagBytes <= 0:
		f.Health, f.Text = viewmodel.HealthOK, "replayed up to the primary's position as last read"
	default:
		// No threshold is invented: the switchover waits for the replay,
		// so lag lengthens it; it is a fact, not a verdict.
		f.Health, f.Text = viewmodel.HealthInfo, cnpgstatus.ByteWords(*r.Live.LagBytes)+" behind the primary; the switchover waits for it to replay"
	}
	return f
}

func readinessSync(g *gathered, name string, stale bool) viewmodel.ReadinessFact {
	f := viewmodel.ReadinessFact{Key: viewmodel.ReadinessSyncState, Health: viewmodel.HealthUnknown}
	primaryName, primary, ok := primaryInstance(g)
	if !ok {
		f.Text = "not read: no primary report"
		f.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status replicationInfo of the primary"}
		return f
	}
	f.Provenance = instanceProvenance(primary, "/pg/status replicationInfo of "+primaryName, stale)
	if !primary.ReadingOK() || f.Provenance.Stale {
		f.Text = "not read: the primary's walsenders were not read now"
		return f
	}
	for _, sb := range primary.Reading.Standbys {
		if sb.ApplicationName != name {
			continue
		}
		reading := cnpgstatus.ClassifySyncState(sb.SyncState)
		f.Health, f.Text = health(reading.Class), bound(sb.SyncState)+": "+reading.Text
		return f
	}
	f.Health, f.Text = viewmodel.HealthWarning, "the primary reports no walsender for it"
	return f
}

func (s *Service) readinessFencing(g *gathered, r viewmodel.InstanceRow, now time.Time) viewmodel.ReadinessFact {
	f := viewmodel.ReadinessFact{
		Key:        viewmodel.ReadinessFencing,
		Provenance: s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginKubernetes, "Cluster.metadata.annotations["+cnpgstatus.AnnotationFencedInstances+"]", now),
	}
	switch {
	case cnpgstatus.Fencing(g.cluster).State == cnpgstatus.FencingUnreadable:
		f.Health, f.Text = viewmodel.HealthUnknown, "the fencing annotation could not be read"
	case r.Fenced:
		f.Health, f.Text = viewmodel.HealthCritical, "fenced: CloudNativePG would not promote it"
	default:
		f.Health, f.Text = viewmodel.HealthOK, "not fenced"
	}
	return f
}
