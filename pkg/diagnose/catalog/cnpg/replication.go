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

package cnpg

import (
	"time"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// The replication thresholds, declared here rather than inline so the
// numbers this console applies can be read in one place. Every one is
// console-pinned knowledge, not something a source reports, and each
// rule's check row renders the number it is applying so a reader never
// has to trust this file.
//
// The holding windows matter as much as the values. Replication lag,
// slot retention and transaction age all spike in normal operation — a
// write burst, a backup, a long report — and a console that reported
// the spike would be teaching its reader to scroll past the screen. A
// value held across every retained sample of a quarter hour is a state,
// not an event.
const (
	// lagSeconds is the replication lag a replica must be behind by.
	// Five minutes is far past a write burst and is the recovery window
	// a failover to that replica would discard.
	lagSeconds = 300
	// lagHeld is how long the lag must hold.
	lagHeld = 15 * time.Minute
	// slotRetainedBytes is the WAL one replication slot must be holding
	// back. Ten gibibytes is more than a healthy slot ever retains: a
	// slot this far behind has no live consumer, and every byte of it
	// sits on the data volume until the slot advances or is dropped.
	slotRetainedBytes = 10 * 1024 * 1024 * 1024
	// slotHeld is how long the retention must hold. A slot that clears
	// within the window is a consumer catching up.
	slotHeld = 15 * time.Minute
)

// replicationRules are the claims about streaming replication and the
// WAL it retains — the incidents the operator's own status reports
// nothing about, because from its side nothing has failed yet.
func replicationRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			// The corroborating rule: three readings of one instance,
			// none of which means much alone. In recovery is every
			// replica. No WAL receiver is normal for a replica replaying
			// the archive on its way up. Lag is a number that spikes.
			// Together, on the same instance, they are a replica that
			// stopped streaming and is not catching up either.
			ID:        "cnpg-replica-not-receiving",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Pinned:    []string{"in_recovery", "is_wal_receiver_up", "END AS lag,"},
			Severity:  diagnose.SeverityHigh,
			Describes: "a replica in recovery with no WAL receiver and a lag that is not closing",
			Summary:   "A replica has stopped streaming and is not catching up.",
			Detail: "The instance reports itself in recovery with no WAL receiver running, " +
				"and its lag has stayed past the threshold across the whole retained " +
				"window. A replica replaying the archive on its way up looks the same " +
				"for a while, which is why all three readings are required and why the " +
				"lag has to hold. Until it streams again it is not a failover " +
				"candidate, and any synchronous commit waiting on it waits forever.",
			When: diagnose.AllOf{Of: []diagnose.Condition{
				diagnose.InstantNonZero{Key: "in-recovery"},
				diagnose.InstantZero{Key: "wal-receiver-up"},
				diagnose.SeriesAbove{Key: "replication-lag", Threshold: lagSeconds, For: lagHeld},
			}},
			NextSteps: "Read that instance's log: a replica that cannot connect says so " +
				"every retry. The usual causes are a primary that no longer has the " +
				"WAL it needs, credentials the replica cannot use, or a NetworkPolicy " +
				"between them.",
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
		{
			ID:        "cnpg-replication-lag-high",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Pinned:    []string{"END AS lag,", "Replication lag behind primary in seconds"},
			Severity:  diagnose.SeverityWarning,
			Describes: "a replica whose lag has stayed past the threshold",
			Summary:   "A replica's lag has stayed past the threshold across the retained window.",
			Detail: "Lag is how far behind the primary a replica's replay has fallen, in " +
				"seconds of transaction time. Held rather than spiked, it is the " +
				"data a failover to this replica would discard, and it means the " +
				"replica cannot apply WAL as fast as the primary produces it.",
			When: diagnose.SeriesAbove{Key: "replication-lag", Threshold: lagSeconds, For: lagHeld},
			// The same instance's stopped receiver explains its lag; a
			// lag on another instance is its own story.
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-replica-not-receiving", Scope: diagnose.ScopePod},
			},
			NextSteps: "Check the replica's IO and CPU against the primary's write rate. " +
				"A replica on slower storage than its primary cannot keep up by " +
				"design, and no amount of waiting closes that gap.",
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
		{
			ID:        "cnpg-sync-replicas-short",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Pinned:    []string{"sync_replicas"},
			Severity:  diagnose.SeverityCritical,
			Describes: "an instance reporting fewer synchronous replicas than it expects",
			Summary:   "An instance has fewer synchronous replicas than it expects.",
			Detail: "The instance manager publishes both numbers: how many synchronous " +
				"standbys the configuration asks for and how many it has. While the " +
				"second is smaller, every commit that requires synchronous " +
				"confirmation waits for a standby that is not there. Applications " +
				"see this as writes that hang rather than writes that fail.",
			When: diagnose.InstantShortfall{
				Expected: "sync-replicas-expected", Observed: "sync-replicas-observed",
				Noun: "synchronous replicas",
			},
			NextSteps: "Get the missing standbys streaming — the replication checks above " +
				"name which are down — or lower what the cluster's synchronous " +
				"configuration requires if the topology has shrunk on purpose.",
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
		{
			ID:        "cnpg-slot-retaining-wal",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Pinned:    []string{"pg_wal_lsn_diff"},
			Severity:  diagnose.SeverityWarning,
			Describes: "a replication slot holding back WAL past the threshold",
			Summary:   "A replication slot is holding back more WAL than the threshold, and not releasing it.",
			Detail: "A slot pins every WAL segment its consumer has not confirmed, and " +
				"PostgreSQL will not remove a pinned segment however full the volume " +
				"gets. A slot whose backlog does not shrink across the window has no " +
				"live consumer — a replica that will not return, or a subscription " +
				"nobody dropped — and it fills the data volume on its own schedule.",
			When: diagnose.SeriesAbove{
				Key: "slot-retained-bytes", Threshold: slotRetainedBytes, For: slotHeld,
			},
			NextSteps: "Find the slot's consumer on the Metrics screen. If it is a replica " +
				"that is coming back, nothing needs doing; if it is one that is not, " +
				"dropping the slot releases the WAL. Nothing else reclaims that space.",
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
	}
}
