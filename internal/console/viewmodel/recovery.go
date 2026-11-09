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

import "time"

// RecoveryTimeline is the recovery-window bar of the backups screen:
// from the earliest to the latest restore point, with the holes marked.
// With repository evidence it is the agent's observed recovery paths,
// judged from segment names only: "covered" means structurally usable,
// never that a restore was run or the WAL bytes were read.
type RecoveryTimeline struct {
	// Basis: evidence (the agent's recovery paths), reported (the
	// operator's or plugin's own window, not verified), none.
	Basis string `json:"basis" enum:"evidence,reported,none"`
	// State is the evidence vocabulary: healthy (one structurally usable
	// span up to the newest archived WAL), warning (holes or candidate
	// gaps inside), unhealthy (the recent end is not restorable), unknown.
	State  string `json:"state" enum:"healthy,warning,unhealthy,unknown"`
	Reason string `json:"reason,omitempty"`
	// Earliest and Latest bound the restorable span; absent when unknown.
	Earliest *time.Time `json:"earliest,omitempty"`
	Latest   *time.Time `json:"latest,omitempty"`
	// TargetTimeline is the timeline the paths recover to (the newest).
	TargetTimeline uint64            `json:"targetTimeline,omitempty"`
	Segments       []RecoverySegment `json:"segments" doc:"Time-ordered spans: covered ones and the holes between them."`
	Markers        []RecoveryMarker  `json:"markers" doc:"Base backups on the bar: completed, failed or incomplete."`
	// Assumptions are what the evidence did not verify.
	Assumptions []string `json:"assumptions"`
	// Complete is false when a collection was cut at a bound: what is
	// shown is then a lower bound of the coverage.
	Complete bool `json:"complete"`
	// RestoreTested: a restore drill of this Cluster passed within the
	// drill window. False, every covered span is "not restore-tested".
	RestoreTested bool       `json:"restoreTested,omitempty"`
	Provenance    Provenance `json:"provenance"`
}

// RecoverySegment is one span of the bar.
type RecoverySegment struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Kind: covered (structurally restorable), missingWAL (a confirmed
	// hole in the archive), candidateGap (a hole seen once, not yet
	// confirmed), unknown (the evidence cannot tell), reported (the
	// operator's window, not verified).
	Kind string `json:"kind" enum:"covered,missingWAL,candidateGap,unknown,reported"`
	// BackupID is the base backup a restore into a covered span starts
	// from (the newest one that reaches it).
	BackupID string `json:"backupId,omitempty"`
	Text     string `json:"text,omitempty"`
}

// RecoveryMarker is one base backup on the bar.
type RecoveryMarker struct {
	At       time.Time `json:"at"`
	BackupID string    `json:"backupId,omitempty"`
	Name     string    `json:"name,omitempty" doc:"The Backup object, when the marker comes from one." untrusted:"true"`
	Kind     string    `json:"kind" enum:"completed,failed,incomplete"`
	Text     string    `json:"text,omitempty"`
	// Source: repository (the evidence agent) or backupObject.
	Source string `json:"source" enum:"repository,backupObject"`
}

// RestoreCheck answers "can I restore to this time?".
type RestoreCheck struct {
	Ref         ClusterRef `json:"ref"`
	GeneratedAt time.Time  `json:"generatedAt"`
	At          time.Time  `json:"at"`
	// Answer: covered (structurally: a base backup and contiguous
	// archived WAL span the time), notCovered (before the earliest or
	// after the latest restore point), gap (inside a hole), reported
	// (only the operator's unverified window covers it), unknown.
	Answer   string `json:"answer" enum:"covered,notCovered,gap,reported,unknown"`
	Text     string `json:"text"`
	BackupID string `json:"backupId,omitempty" doc:"The base backup to restore from, when covered."`
	Basis    string `json:"basis" enum:"evidence,reported,none"`
	// Caveats are what the answer does not prove.
	Caveats    []string   `json:"caveats"`
	Provenance Provenance `json:"provenance"`
}
