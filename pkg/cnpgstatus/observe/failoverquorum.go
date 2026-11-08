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

package observe

import (
	"time"
)

// MaxQuorumStandbys bounds the rendered synchronous-standby list.
const MaxQuorumStandbys = 32

// FailoverQuorumFacts is the operator-reported failover quorum of the
// target cluster.
//
// The resource is written by the primary's instance manager and reset by
// the operator, so every value here is a report about synchronous
// replication rather than a claim the console verifies. It exists only
// when the cluster runs synchronous replication with a quorum, which is
// why absence is a first-class state rather than an error.
type FailoverQuorumFacts struct {
	// Present reports that the API server confirmed the resource
	// exists. False is a successful observation of absence: the cluster
	// is not running failover quorum.
	Present bool
	// Method is the latest reported synchronous-replication method.
	Method string
	// Primary is the instance that last updated the quorum.
	Primary string
	// StandbyNumber is how many synchronous standbys a transaction
	// waits for.
	StandbyNumber int
	// Standbys is the bounded list of potentially synchronous instances.
	Standbys []string
	// StandbysTruncated reports that more standbys were reported than
	// the display bound.
	StandbysTruncated bool
}

// FailoverQuorumState is one complete observation. It carries no
// resource version for the same reason ClusterState does not: a
// singleton get yields no watch-safe cursor.
type FailoverQuorumState struct {
	// Facts is the observed quorum, or an absence.
	Facts FailoverQuorumFacts
}

// FailoverQuorumSnapshot is the rendered quorum, immutable and carrying
// its own staleness.
type FailoverQuorumSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports lost contact; the facts below are the retained
	// last-good observation.
	Stale bool
	// Quorum is the last observed state.
	Quorum FailoverQuorumFacts
}
