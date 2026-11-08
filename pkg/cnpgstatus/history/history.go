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

// Package history retains a bounded, in-memory revision timeline of the
// watched Kubernetes object definitions: what changed, when it was
// observed, and by which field manager. It is pure domain, like observe:
// it imports neither client-go nor the CloudNativePG API types. The kube
// boundary hands it source-neutral observations whose manifests are
// already normalized and scrubbed; this package classifies, deduplicates,
// coalesces and bounds them.
//
// Everything here is Kubernetes-reported and says so: a revision records
// what the API server delivered and when this process observed it, never
// when the change happened. Windows in which the process was not watching
// are explicit records, not silent continuity.
package history

import "time"

// Clock supplies time to the store. It is the read-only half of
// observe.Clock, so observe.RealClock satisfies it.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// Actor is the field manager that last touched an object, read from
// metadata.managedFields before the boundary strips it. It is
// Kubernetes-reported attribution: the manager's self-declared name and
// the API operation, never an authenticated identity.
type Actor struct {
	// Manager is the manager name, such as "cloudnative-pg" or
	// "kubectl-client-side-apply". Empty when the object carried no
	// managed fields.
	Manager string
	// Operation is the reported operation, "Apply" or "Update". Empty
	// alongside an empty Manager.
	Operation string
}

// FieldOwner maps one field manager to the paths it owned at one
// observation, parsed from metadata.managedFields before the boundary
// strips it. It is what turns "this manager touched the object" into
// "this manager owned the field that changed". Paths use the same
// encoding the differ produces, so attribution compares like with like.
type FieldOwner struct {
	// Manager is the manager's self-declared name.
	Manager string
	// Paths are the owned field paths, sorted and bounded at capture.
	Paths []string
}

// Observation is one capture handed over by the kube boundary. The
// manifest is opaque, already-normalized, already-scrubbed canonical
// JSON; nothing in this package parses it until a diff is requested.
type Observation struct {
	// Scope names the seed-and-watch unit this observation came through,
	// such as "pods" or "scheduled backups". A complete seed of one scope
	// accounts for every live object of that scope and nothing else, so
	// gap detection is keyed by scope rather than by kind: instance pods
	// and pooler pods share a kind but are listed by different scopes.
	Scope string
	// Group, Version and Kind identify the object's API type. The core
	// group is the empty string.
	Group, Version, Kind string
	// Namespace, Name and UID identify the object. The UID is the
	// incarnation identity: a recreated object with the same name is a
	// different object.
	Namespace, Name, UID string
	// Generation is metadata.generation, the server's spec revision
	// counter. Zero for kinds that do not maintain one.
	Generation int64
	// Manifest is the normalized, scrubbed object definition as canonical
	// JSON. Nil on a deletion: the last definition is the previous
	// revision's word.
	Manifest []byte
	// SpecHash digests the manifest minus its status subtree.
	SpecHash string
	// StatusHash digests the status subtree alone; empty when absent.
	StatusHash string
	// Actor is the last-touching field manager.
	Actor Actor
	// Owners are the per-manager owned field paths at this observation,
	// bounded at capture. Empty attribution is honest: a change whose
	// owner is unknown stays unattributed.
	Owners []FieldOwner
	// Deleted reports that the watch delivered a deletion.
	Deleted bool
}

// Change is the closed classification of one revision.
type Change string

// The revision classifications. Creation and first observation are
// distinct on purpose: a watch add proves the object appeared while this
// process was looking, a seed only proves it existed by the time the
// list ran.
const (
	// ChangeCreated is an object that appeared through the watch.
	ChangeCreated Change = "created"
	// ChangeFirstObserved is an object first seen through a complete
	// seed: it existed by then, but its creation was not observed.
	ChangeFirstObserved Change = "first-observed"
	// ChangeSpec is a change to the definition outside status.
	ChangeSpec Change = "spec-changed"
	// ChangeStatus is a change confined to the status subtree.
	ChangeStatus Change = "status-transition"
	// ChangeDeleted is a deletion delivered by the watch.
	ChangeDeleted Change = "deleted"
	// ChangeDeletedUnobserved is an object present before a contact loss
	// and absent from the re-seed: it was deleted at some unobserved
	// point between the two.
	ChangeDeletedUnobserved Change = "deleted-unobserved"
)

// Revision is one retained timeline record.
type Revision struct {
	// Seq is the store-wide monotonic sequence of this revision. It is
	// the stable handle the Revision accessor resolves.
	Seq uint64
	// Scope names the seed-and-watch unit that observed this revision.
	Scope string
	// Group, Version and Kind identify the object's API type.
	Group, Version, Kind string
	// Namespace, Name and UID identify the object incarnation.
	Namespace, Name, UID string
	// Generation is metadata.generation at this observation.
	Generation int64
	// Change classifies what this revision records.
	Change Change
	// AfterGap reports that the change was discovered by a re-seed after
	// contact loss: it happened at some unobserved point between
	// PrevObservedAt and ObservedAt, not at ObservedAt.
	AfterGap bool
	// Actor is the last-touching field manager at this observation.
	Actor Actor
	// ObservedAt is when this process observed the revision — an
	// application-side clock reading, never a server timestamp.
	ObservedAt time.Time
	// PrevObservedAt is when the same object was last observed before
	// this revision; zero for a first observation.
	PrevObservedAt time.Time
	// Coalesced counts the additional status transitions folded into
	// this revision inside the coalescing window.
	Coalesced int
	// SpecHash and StatusHash are the observation's digests, retained so
	// a later backend can deduplicate without reparsing manifests.
	SpecHash, StatusHash string
	// Owners are the per-manager owned field paths at this revision,
	// consulted when a diff attributes its changes.
	Owners []FieldOwner
	// Manifest is the object definition at this revision; nil for
	// deletions and unobserved deletions.
	Manifest []byte
}

// Entry is the manifest-free projection of one revision, what a timeline
// renders. Fetching a manifest is the Revision accessor's job, so a
// snapshot never copies megabytes.
type Entry struct {
	// Seq resolves the full revision through the Revision accessor.
	Seq uint64
	// Scope names the seed-and-watch unit that observed this revision.
	Scope string
	// Group, Version and Kind identify the object's API type.
	Group, Version, Kind string
	// Namespace, Name and UID identify the object incarnation.
	Namespace, Name, UID string
	// Change classifies what this revision records.
	Change Change
	// AfterGap marks a change discovered across an unobserved window.
	AfterGap bool
	// Actor is the last-touching field manager.
	Actor Actor
	// ObservedAt is when this process observed the revision.
	ObservedAt time.Time
	// PrevObservedAt bounds the unobserved window of an AfterGap or
	// deleted-unobserved revision; zero for a first observation.
	PrevObservedAt time.Time
	// Coalesced counts status transitions folded into this entry.
	Coalesced int
	// HasManifest reports that the full revision carries a definition.
	HasManifest bool
}

// Snapshot is the timeline a consumer renders, newest first. It is
// immutable: the store returns fresh copies, never shared state.
type Snapshot struct {
	// Generation increases by one on every retained change.
	Generation uint64
	// Entries are the retained revisions, newest first.
	Entries []Entry
	// Evicted reports that retention bounds have dropped older history:
	// the timeline's beginning is a budget, not the beginning of events.
	Evicted bool
}

// Limits bound the store. A zero field is unbounded — the configuration
// layer is expected to supply real bounds; zero exists so tests can
// exercise one axis at a time.
type Limits struct {
	// PerObject bounds the retained revisions of one object incarnation.
	PerObject int
	// MaxRevisions bounds the retained revisions across all objects.
	MaxRevisions int
	// MaxBytes bounds the summed manifest bytes across all revisions.
	MaxBytes int
	// CoalesceWindow folds consecutive status transitions of one object
	// closer together than this into a single revision. Zero disables
	// coalescing.
	CoalesceWindow time.Duration
}

// Recorder receives captures from the kube boundary. The store is the
// one implementation; the interface exists so the boundary depends on
// the capture contract rather than on the retention machinery.
type Recorder interface {
	// Observe records one watch delivery.
	Observe(obs Observation)
	// Seed records one complete listing of a scope. Completeness is the
	// contract: every live object of the scope is present, so an absent
	// previously-known object was deleted while unobserved. A truncated
	// listing must go through Observe instead, item by item.
	Seed(scope string, obs []Observation)
}

// ObjectRecord is the persisted classification state of one live object
// incarnation: what the store must know to fold the next observation
// even when every retained revision of the object was evicted, and to
// name an object a later seed reports gone.
type ObjectRecord struct {
	// Scope names the seed-and-watch unit that observes the object.
	Scope string
	// Group, Version and Kind identify the object's API type.
	Group, Version, Kind string
	// Namespace and Name identify the object alongside its UID key.
	Namespace, Name string
	// SpecHash and StatusHash are the digests of the last observation.
	SpecHash, StatusHash string
	// LastObservedAt is when the object was last observed.
	LastObservedAt time.Time
}

// Contents is everything a persister reloads at boot: the retained
// revisions oldest first, the live objects' classification state, and
// the counters the store resumes from.
type Contents struct {
	// Revisions are the retained revisions, oldest first.
	Revisions []Revision
	// Objects is the live incarnations' classification state by UID.
	Objects map[string]ObjectRecord
	// Seq is the highest sequence ever assigned.
	Seq uint64
	// Evicted reports that retention had already dropped history.
	Evicted bool
}

// Persister mirrors store mutations to a durable medium and reloads
// them at boot. Mutation methods are called under the store's lock and
// must only enqueue — durable writes belong to the persister's own
// flush loop, so capture latency never includes disk latency. The store
// with no persister is the default and mirrors nothing.
type Persister interface {
	// Load returns everything persisted so far. It is called once,
	// before any mutation.
	Load() (Contents, error)
	// Append mirrors one new revision.
	Append(rev Revision)
	// Update mirrors the rewrite of an existing revision, which
	// coalescing performs; the sequence identifies it.
	Update(rev Revision)
	// Evict mirrors the removal of one revision.
	Evict(seq uint64)
	// PutObject mirrors one live object's classification state.
	PutObject(uid string, rec ObjectRecord)
	// DeleteObject mirrors the end of one live incarnation.
	DeleteObject(uid string)
	// MarkEvicted mirrors the fact that retention has dropped history.
	MarkEvicted()
}
