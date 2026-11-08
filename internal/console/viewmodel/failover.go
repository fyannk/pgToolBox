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

package viewmodel

// FailoverRehearsal answers "if the primary died now":
// which instance CloudNativePG would promote, by the operator's own
// election rule, how far behind it is, what that costs under the
// replication mode actually in force, whether the failover quorum would
// let the operator proceed, and the placement risks the console can see.
// It is a console inference over instance self-reports and operator
// claims; each input keeps its provenance, and what the console cannot
// see is a blind spot, never a guess.
type FailoverRehearsal struct {
	Outcome string `json:"outcome" enum:"promote,blocked,noCandidate,unknown,notApplicable" doc:"promote: a replica would be promoted; blocked: the operator would promote none (fencing, quorum); noCandidate: no replica; unknown: the console cannot tell."`
	Health  Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Text is the one sentence, such as "orders-2 would be promoted,
	// losing up to 3.0 MiB of WAL it had not received (asynchronous)."
	Text      string `json:"text"`
	Candidate string `json:"candidate,omitempty" doc:"The instance the operator would promote."`
	// BehindBytes is the primary's current LSN minus the candidate's
	// received LSN, both as last read.
	BehindBytes  *int64 `json:"behindBytes,omitempty" doc:"Primary current LSN minus the candidate's received LSN, as last read."`
	DataLoss     string `json:"dataLoss" enum:"none,upTo,unknown" doc:"none: synchronous replication was satisfied for the candidate (no acknowledged commit lost); upTo: up to behindBytes of WAL is lost; unknown: positions not read in good standing."`
	DataLossText string `json:"dataLossText"`
	// Certain is false when an instance the operator might rank first
	// could not be read, or the quorum object could not be read.
	Certain bool `json:"certain" doc:"False when an input the operator would use is not readable by the console."`
	// Ranking is every replica in the operator's election order.
	Ranking []RehearsalRow   `json:"ranking"`
	Quorum  *RehearsalQuorum `json:"quorum,omitempty" doc:"The failover quorum check (R + W > N), when spec.postgresql.synchronous.failoverQuorum is set."`
	// Synchronous is the declared mode: none, legacy or explicit.
	Synchronous   string       `json:"synchronous" enum:"none,legacy,explicit"`
	Fencing       FencingState `json:"fencing" enum:"none,some,all,unreadable"`
	FailoverDelay int32        `json:"failoverDelaySeconds,omitempty" doc:"spec.failoverDelay: seconds the operator waits before promoting."`
	PrimaryNode   string       `json:"primaryNode,omitempty"`
	// SameNodeAsPrimary are the replicas scheduled on the primary's node:
	// one node failure takes both.
	SameNodeAsPrimary []string `json:"sameNodeAsPrimary,omitempty"`
	Notes             []string `json:"notes,omitempty"`
	// Provenance is the inference's: the sweep that read the positions.
	Provenance Provenance  `json:"provenance"`
	BlindSpots []BlindSpot `json:"blindSpots"`
}

// RehearsalRow is one replica in the operator's election order.
type RehearsalRow struct {
	Name     string `json:"name"`
	Eligible bool   `json:"eligible" doc:"The operator could promote it, as far as the console can tell."`
	Reason   string `json:"reason,omitempty" doc:"Why it is not eligible."`
	// ReceivedLSN and ReplayLSN are its own report; the order is by
	// received, then replay, then name.
	ReceivedLSN       string     `json:"receivedLsn,omitempty"`
	ReplayLSN         string     `json:"replayLsn,omitempty"`
	BehindBytes       *int64     `json:"behindBytes,omitempty"`
	SyncState         string     `json:"syncState,omitempty" doc:"pg_stat_replication.sync_state on the primary, verbatim."`
	PodReady          *bool      `json:"podReady,omitempty"`
	Node              string     `json:"node,omitempty"`
	SameNodeAsPrimary bool       `json:"sameNodeAsPrimary,omitempty"`
	Provenance        Provenance `json:"provenance"`
}

// RehearsalQuorum is the failover quorum check as the operator would
// make it now.
type RehearsalQuorum struct {
	Required   int      `json:"required" doc:"W: FailoverQuorum.status.standbyNumber."`
	Set        []string `json:"set,omitempty" doc:"N: FailoverQuorum.status.standbyNames."`
	Promotable []string `json:"promotable,omitempty" doc:"R: members of the set read in good standing with a ready pod."`
	// Met is absent when the console cannot read the FailoverQuorum.
	Met        *bool      `json:"met,omitempty"`
	Text       string     `json:"text"`
	Provenance Provenance `json:"provenance"`
}
