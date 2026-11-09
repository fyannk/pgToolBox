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

package cnpgstatus

import (
	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

// BlindSpot is something the console cannot see from its sources, stated
// on the panel it would otherwise be mistaken as absent from. IDs are
// stable API: the UI keys translations and tests on them.
type BlindSpot struct {
	ID    string `json:"id"`
	Panel string `json:"panel"`
	Text  string `json:"text"`
}

// Blind-spot IDs.
const (
	BlindSpotStreamingReplicaOnly    = "replication.streamingReplicaOnly"
	BlindSpotSyncStandbyNames        = "replication.syncStandbyNamesNotReported"
	BlindSpotPluginNotReported       = "backups.pluginNotReported"
	BlindSpotIsFencedNotReported     = "instances.isFencedNotReported"
	BlindSpotNoObservedGeneration    = "operator.noObservedGeneration"
	BlindSpotVolumeUsageNotReported  = "storage.volumeUsageNotReported"
	BlindSpotNoSyncTopologyCondition = "topology.noSyncTopologyCondition"
	BlindSpotBootstrapJobsGone       = "bootstrap.noJobs"
)

// BlindSpots lists the blind spots that hold on every supported minor,
// plus those the Cluster's own configuration creates. A nil Cluster
// gets the unconditional ones. Use BlindSpotsFor when the minor is
// known.
func BlindSpots(c *cnpgv1.Cluster, backups BackupConfig) []BlindSpot {
	spots := []BlindSpot{
		{
			// pkg/management/postgres/probes.go fillWalStatusFromConnection
			// v1.30.0:465-490: WHERE application_name ~ '<cluster>-[0-9]+$'
			// AND usename = 'streaming_replica'; same filter at main.
			ID:    BlindSpotStreamingReplicaOnly,
			Panel: "replication",
			Text: "Only this cluster's own standbys (<cluster>-N connected as streaming_replica) are listed. " +
				"Replica clusters, logical replication walsenders and external base backups are not reported by the instance. " +
				"The \"received\" LSN of each standby is the primary's sent_lsn.",
		},
		{
			// /pg/status carries no synchronous_standby_names
			// (pkg/postgres/status.go PostgresqlStatus v1.30.0:36-106).
			ID:    BlindSpotSyncStandbyNames,
			Panel: "replication",
			Text: "The synchronous_standby_names PostgreSQL actually applies is not reported by the instance; " +
				"sync_state per standby and FailoverQuorum.status (when quorum failover is enabled) are the evidence.",
		},
		{
			// pkg/postgres/status.go: no isFenced at v1.30.0; v1.30.1:107-111
			// (and main) add it, "never populated in the instance
			// manager": the operator fills it from the annotation
			// (pkg/management/postgres/webserver/client/remote/instance.go
			// v1.30.1:139), so /pg/status never carries it in either.
			ID:    BlindSpotIsFencedNotReported,
			Panel: "instances",
			Text: "Whether an instance is fenced comes from the cnpg.io/fencedInstances annotation (intent); " +
				"the instance's own status does not report it.",
		},
		{
			// api/v1/cluster_types.go ClusterStatus has no observedGeneration
			// at v1.30.0 or main.
			ID:    BlindSpotNoObservedGeneration,
			Panel: "operator",
			Text: "The Cluster status has no observedGeneration: the console cannot prove the operator has processed the latest spec. " +
				"Condition and event ages are the only liveness evidence.",
		},
		{
			// pkg/postgres/status.go has no volume usage upstream (v1.30.0,
			// main); the fork's autoGrow branch adds volumeUsages[].
			ID:    BlindSpotVolumeUsageNotReported,
			Panel: "storage",
			Text:  "Disk usage per volume is not reported by upstream CloudNativePG instances; PVC classes and metrics are the evidence.",
		},
	}
	if backups.PluginBased {
		name := backups.PluginName
		if name == "" {
			name = "a backup plugin"
		}
		source := "Recoverability comes from the ObjectStore's serverRecoveryWindow, the Backup objects and repository evidence."
		if backups.PgBackRest != nil {
			// Neither pgBackRest plugin writes a recovery window to the
			// Cluster or its own object's status in a form that covers
			// WAL.
			name = backups.PgBackRest.Display
			source = "Recoverability comes from the repository evidence of an EvidenceAgent and the Backup objects only."
		}
		spots = append(spots, BlindSpot{
			// api/v1/cluster_types.go v1.30.0:1026-1056 "Deprecated: the
			// field is not set for backup plugins".
			ID:    BlindSpotPluginNotReported,
			Panel: "backups",
			Text: "Backups go through " + name + ": the Cluster's first recoverability point and last backup fields " +
				"(and their cnpg_collector metrics) are not reported by plugin-based backups. " + source,
		})
	}
	return spots
}

// BlindSpotsFor is BlindSpots plus the blind spots of one minor. An
// unsupported minor adds none.
func BlindSpotsFor(minor Minor, c *cnpgv1.Cluster, backups BackupConfig) []BlindSpot {
	spots := BlindSpots(c, backups)
	switch minor {
	case Minor130:
		spots = append(spots, BlindSpot{
			// SyncReplicationTopologySatisfied is main-only (planned 1.31).
			ID:    BlindSpotNoSyncTopologyCondition,
			Panel: "topology",
			Text:  "CloudNativePG 1.30 does not report whether synchronous standbys sit in another failure domain.",
		})
	case Minor131:
		spots = append(spots, BlindSpot{
			// PLANNED: main 8179beb05 replaces the bootstrap Job by the
			// bootstrap-instance init container (pkg/specs/pods.go main:86, :620-627).
			ID:    BlindSpotBootstrapJobsGone,
			Panel: "instances",
			Text:  "CloudNativePG 1.31 bootstraps new instances in an init container, not a Job: bootstrap progress and errors are in that Pod's init container status and logs.",
		})
	}
	return spots
}
