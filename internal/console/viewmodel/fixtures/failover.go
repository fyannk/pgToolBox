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

package fixtures

import (
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

var failoverBlindSpots = []viewmodel.BlindSpot{
	{ID: "failover.zones", Panel: "failover", Text: "The console reads no Node, so it cannot tell whether replicas share the primary's zone (topology.kubernetes.io/zone); only same-node placement is shown."},
	{ID: "failover.synchronousCommit", Panel: "failover", Text: "synchronous_commit is read from the Cluster's parameters only: a role, a database, a session or a transaction can lower it (local, off), and those commits do not wait for any standby whatever sync_state says."},
	{ID: "failover.consoleSweep", Panel: "failover", Text: "Positions are the console's last sweep of /pg/status; at a real failure the operator reads the instances itself, and the distance will have moved."},
}

func rehearsalProv(stale bool, reason string) viewmodel.Provenance {
	return viewmodel.Provenance{
		Origin: viewmodel.OriginConsole, Source: "CloudNativePG's election rule over /pg/status, Cluster.spec, FailoverQuorum and Pods",
		ObservedAt: ago(8 * time.Second), Stale: stale, StaleReason: reason,
	}
}

// OrdersFailover is orders' rehearsal: orders-2 is caught up and in the
// quorum, so it would be promoted with no acknowledged commit lost; the
// console could not read orders-3, so whether the failover quorum holds
// is not certain, and orders-3 shares the primary's node.
func OrdersFailover() viewmodel.FailoverRehearsal {
	return viewmodel.FailoverRehearsal{
		Outcome: "promote", Health: viewmodel.HealthWarning,
		Text:      "orders-2 would be promoted, with no acknowledged commit lost (synchronous, for transactions using the cluster default synchronous_commit).",
		Candidate: "orders-2", BehindBytes: new(int64(0)), DataLoss: "none",
		DataLossText: "Quorum synchronous replication (sync_state quorum) and enough promotable members of the quorum: the most advanced of them holds every acknowledged commit. This holds for transactions using the cluster default: a role, a database, a session or a transaction can lower synchronous_commit, which the console cannot see.",
		Certain:      false,
		Ranking: []viewmodel.RehearsalRow{
			{
				Name: "orders-2", Eligible: true, ReceivedLSN: "0/9000148", ReplayLSN: "0/9000148", BehindBytes: new(int64(0)),
				SyncState: "quorum", PodReady: new(true), Node: "node-b", Provenance: instanceProv(),
			},
			{
				Name: "orders-3", Reason: "its status could not be read by the console", SyncState: "potential", PodReady: new(true),
				Node: "node-a", SameNodeAsPrimary: true,
				Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", Stale: true, StaleReason: "status port not verifiable: CA not in the console's bundle"},
			},
		},
		Quorum: &viewmodel.RehearsalQuorum{
			Required: 1, Set: []string{"orders-2", "orders-3"}, Promotable: []string{"orders-2"},
			Text:       "cannot tell: 1 promotable of 2, 1 required, and orders-3 not read by the console (R + W > N needs 2)",
			Provenance: operatorProv("FailoverQuorum.status"),
		},
		Synchronous: "explicit", Fencing: viewmodel.FencingNone, PrimaryNode: "node-a", SameNodeAsPrimary: []string{"orders-3"},
		Notes:      []string{"Not read by the console: orders-3. The operator reads the instances itself and would rank one of them first if it is further ahead."},
		Provenance: rehearsalProv(false, ""),
		BlindSpots: failoverBlindSpots,
	}
}

// BillingFailover is billing's rehearsal mid-failover: no replica's
// status could be read, so the console cannot tell.
func BillingFailover() viewmodel.FailoverRehearsal {
	return viewmodel.FailoverRehearsal{
		Outcome: "unknown", Health: viewmodel.HealthUnknown,
		Text:     "No replica's status could be read, so the console cannot tell which one the operator would promote.",
		DataLoss: "unknown", DataLossText: "not known",
		Ranking: []viewmodel.RehearsalRow{
			{
				Name: "billing-2", Reason: "its status could not be read by the console", PodReady: new(true), Node: "node-2",
				Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", Stale: true, StaleReason: "status port not verifiable: CA not in the console's bundle"},
			},
		},
		Synchronous: "none", Fencing: viewmodel.FencingNone, PrimaryNode: "node-1",
		Notes:      []string{"Not read by the console: billing-2. The operator reads the instances itself and would rank one of them first if it is further ahead."},
		Provenance: rehearsalProv(true, "billing-1: not reachable from this console"),
		BlindSpots: failoverBlindSpots,
	}
}
