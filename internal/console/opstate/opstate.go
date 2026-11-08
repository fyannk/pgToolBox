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

// Package opstate holds the read model's answers that the operations read:
// a Cluster as the informer cache sees it and the backup a pgBackRest drill
// restores. The read model (status) builds them and the operations
// (operations) consume them, so neither the read side imports the write
// orchestrator nor the other way round.
package opstate

import (
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// InstanceState is one instance as the offers show it.
type InstanceState struct {
	Name     string
	Health   viewmodel.Health
	LagBytes *int64
}

// ClusterState is the informer cache's view of a Cluster, for the offers
// and the trackers (never for the precondition check of a write).
type ClusterState struct {
	Cluster   *cnpgv1.Cluster
	Pods      []corev1.Pod
	Backups   []*cnpgv1.Backup
	Minor     cnpgstatus.Minor
	Instances []InstanceState
}

// PgBackRestTarget is the newest backup of an evidence agent's recovery
// window: its label and stop time, the stanza the agent read, and the
// plugin and object (Stanza or Archive) the agent reports it resolved
// (EvidenceAgent status.pgbackrest; empty for an agent the console reads
// without an EvidenceAgent, which resolves the Cluster's own object).
type PgBackRestTarget struct {
	Label, Stanza  string
	StoppedAt      *time.Time
	Plugin, Object string
}
