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

// The failover rehearsal: pkg/cnpgstatus.Rehearse fed
// from what the read model already gathered, with provenance attached.

import (
	"sort"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

const rehearsalSource = "CloudNativePG's election rule over /pg/status, Cluster.spec, FailoverQuorum and Pods"

// failover rehearses the loss of the current primary.
func (s *Service) failover(g *gathered, now time.Time) viewmodel.FailoverRehearsal {
	in := cnpgstatus.RehearsalInput{
		Primary:           g.cluster.Status.CurrentPrimary,
		Fencing:           cnpgstatus.Fencing(g.cluster),
		Sync:              cnpgstatus.SyncConfig(g.minor, g.cluster),
		Quorum:            g.quorum,
		QuorumReadable:    g.listed(cache.SourceFailoverQuorums),
		ReplicaCluster:    g.cluster.Spec.ReplicaCluster != nil && g.cluster.Spec.ReplicaCluster.Enabled != nil && *g.cluster.Spec.ReplicaCluster.Enabled,
		FailoverDelay:     g.cluster.Spec.FailoverDelay,
		Hibernated:        cnpgstatus.Hibernation(g.cluster).State == cnpgstatus.HibernationHibernated,
		SynchronousCommit: declaredParameter(g.cluster.Spec.PostgresConfiguration.Parameters, "synchronous_commit"),
	}
	for _, name := range members(g) {
		inst := cnpgstatus.RehearsalInstance{Name: name}
		if swept, ok := g.swept.Instances[name]; ok {
			inst.Reading, inst.Fresh = swept.Reading, swept.State == viewmodel.StatusReadOK
		}
		for _, pod := range g.pods {
			if pod.Name == name {
				ready := podReady(pod)
				inst.PodReady, inst.Node = &ready, pod.Spec.NodeName
			}
		}
		in.Instances = append(in.Instances, inst)
	}
	r := cnpgstatus.Rehearse(in)

	out := viewmodel.FailoverRehearsal{
		Outcome: r.Outcome, Health: health(r.Class), Text: r.Text, Candidate: r.Candidate, BehindBytes: r.BehindBytes,
		DataLoss: r.DataLoss, DataLossText: r.DataLossText, Certain: r.Certain, Ranking: []viewmodel.RehearsalRow{},
		Synchronous: in.Sync.Mode, Fencing: viewmodel.FencingState(in.Fencing.State), FailoverDelay: in.FailoverDelay,
		PrimaryNode: r.PrimaryNode, SameNodeAsPrimary: r.SameNodeAsPrimary, Notes: r.Notes,
		Provenance: viewmodel.Provenance{Origin: viewmodel.OriginConsole, Source: rehearsalSource},
		BlindSpots: []viewmodel.BlindSpot{
			{ID: "failover.zones", Panel: "failover", Text: "The console reads no Node, so it cannot tell whether replicas share the primary's zone (topology.kubernetes.io/zone); only same-node placement is shown."},
			{ID: "failover.synchronousCommit", Panel: "failover", Text: "synchronous_commit is read from the Cluster's parameters only: a role, a database, a session or a transaction can lower it (local, off), and those commits do not wait for any standby whatever sync_state says."},
			{ID: "failover.consoleSweep", Panel: "failover", Text: "Positions are the console's last sweep of /pg/status; at a real failure the operator reads the instances itself, and the distance will have moved."},
		},
	}
	if !g.listed(cache.SourcePods) {
		out.BlindSpots = append(out.BlindSpots, viewmodel.BlindSpot{ID: "failover.pods", Panel: "failover", Text: "Pods are not readable, so node placement and pod readiness are not known."})
	}
	if g.hasSweep && !g.swept.SweptAt.IsZero() {
		at := g.swept.SweptAt.UTC()
		out.Provenance.ObservedAt = &at
	}
	// The inference is stale when the primary's or the candidate's own
	// report is: the distance then describes the past.
	for _, name := range []string{in.Primary, r.Candidate} {
		if inst, ok := g.swept.Instances[name]; name != "" && ok && inst.State != viewmodel.StatusReadOK {
			out.Provenance.Stale, out.Provenance.StaleReason = true, name+": "+inst.Reason
		}
	}
	for _, c := range r.Ranking {
		row := viewmodel.RehearsalRow{
			Name: c.Name, Eligible: c.Eligible, Reason: c.Reason, ReceivedLSN: c.ReceivedLSN, ReplayLSN: c.ReplayLSN,
			BehindBytes: c.BehindBytes, SyncState: c.SyncState, PodReady: c.PodReady, Node: c.Node, SameNodeAsPrimary: c.SameNodeAsPrimary,
			Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status"},
		}
		if inst, ok := g.swept.Instances[c.Name]; ok {
			row.Provenance = instanceProvenance(inst, "/pg/status")
		}
		out.Ranking = append(out.Ranking, row)
	}
	if q := r.Quorum; q != nil {
		out.Quorum = &viewmodel.RehearsalQuorum{
			Required: q.Required, Set: q.Set, Promotable: q.Promotable, Met: q.Met, Text: q.Text,
			Provenance: s.provenance(g.key.Namespace, cache.SourceFailoverQuorums, viewmodel.OriginOperator, "FailoverQuorum.status", now),
		}
	}
	return out
}

// declaredParameter is a parameter as declared, matched the way
// PostgreSQL matches names (case-insensitively); when two spellings are
// declared, the later in sorted order wins, as the later line of
// postgresql.conf does. Empty when not declared.
func declaredParameter(params map[string]string, name string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if strings.EqualFold(k, name) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return params[keys[len(keys)-1]]
}
