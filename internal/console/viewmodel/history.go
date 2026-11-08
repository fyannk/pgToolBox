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

// History is the /history screen: the revisions the console observed of
// the Cluster and its declarative children, newest first. Times are the
// console's own observation clock, never server timestamps; attribution
// is the self-declared field manager Kubernetes reports, never an
// authenticated identity (the API server's audit log has that).
type History struct {
	Ref         ClusterRef `json:"ref"`
	GeneratedAt time.Time  `json:"generatedAt"`
	// Source is "history": live, or disabled by configuration.
	Source SourceState `json:"source"`
	// Persistent reports that the history survives restarts (bbolt);
	// otherwise it begins at this replica's start.
	Persistent bool `json:"persistent"`
	// Evicted reports that retention bounds dropped older revisions: the
	// first entry is a budget, not the beginning of events.
	Evicted bool           `json:"evicted,omitempty"`
	Items   []HistoryEntry `json:"items"`
}

// HistoryEntry is one revision without its manifest.
type HistoryEntry struct {
	Seq  int64  `json:"seq" doc:"Stable handle of the revision (GET .../history/{seq})."`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Change classifies the revision.
	Change string `json:"change" enum:"created,first-observed,spec-changed,status-transition,deleted,deleted-unobserved"`
	// AfterGap: the change happened at some unobserved point between
	// PrevObservedAt and ObservedAt (found by a re-list after contact
	// loss).
	AfterGap bool `json:"afterGap,omitempty"`
	// Manager and Operation are the last field manager Kubernetes
	// reports (self-declared), when known.
	Manager        string     `json:"manager,omitempty"`
	Operation      string     `json:"operation,omitempty"`
	ObservedAt     time.Time  `json:"observedAt"`
	PrevObservedAt *time.Time `json:"prevObservedAt,omitempty"`
	// Coalesced counts the status transitions folded into this entry.
	Coalesced   int  `json:"coalesced,omitempty"`
	HasManifest bool `json:"hasManifest"`
}

// Revision is one revision with its definition and the structural diff
// against the previous retained definition of the same object.
type Revision struct {
	Ref   ClusterRef   `json:"ref"`
	Entry HistoryEntry `json:"entry"`
	// Manifest is the scrubbed definition at this revision, indented
	// JSON; absent for a deletion.
	Manifest string `json:"manifest,omitempty"`
	// BaseManifest is the definition it is compared with, so a line
	// diff can be drawn; absent when there is no base.
	BaseManifest string       `json:"baseManifest,omitempty"`
	Diff         RevisionDiff `json:"diff"`
	// Preview is what this Cluster revision set in motion compared with
	// its base; absent for other kinds and for a revision
	// with no base definition.
	Preview *ChangePreview `json:"preview,omitempty"`
}

// RevisionDiff is the structural comparison.
type RevisionDiff struct {
	// HasBase is false for a first observation, a deletion or a base
	// already evicted: there is honestly nothing to compare against.
	HasBase        bool        `json:"hasBase"`
	BaseSeq        *int64      `json:"baseSeq,omitempty"`
	BaseObservedAt *time.Time  `json:"baseObservedAt,omitempty"`
	Entries        []DiffEntry `json:"entries"`
	Truncated      bool        `json:"truncated,omitempty"`
}

// DiffEntry is one changed path.
type DiffEntry struct {
	Path   string `json:"path"`
	Op     string `json:"op" enum:"added,removed,changed"`
	Before string `json:"before,omitempty" doc:"Bounded rendered JSON value."`
	After  string `json:"after,omitempty"`
	// Manager owned the path at the newer revision; absent when unknown
	// or ambiguous.
	Manager string `json:"manager,omitempty"`
}
