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

// Package observe owns the collector and the immutable snapshot the
// console renders. Snapshots carry a monotonic generation, the time of
// the last successful contact, and an explicit staleness mark; a broken
// watch retains the last complete snapshot and marks it stale, so the
// page can never present a broken watch as a healthy, current cluster.
// The package is pure domain: it imports neither client-go nor the
// CloudNativePG API types.
package observe

import (
	"context"
	"fmt"
	"time"
)

// Condition is one operator-reported cluster condition.
type Condition struct {
	// Type is the condition type, such as "Ready".
	Type string
	// Status is the reported status string, such as "True".
	Status string
	// Reason is the operator's machine reason, possibly empty.
	Reason string
	// Message is the operator's human message, possibly empty. It is
	// rendered as escaped, length-bounded text only.
	Message string
	// LastTransition is when the operator reports the condition last
	// changed status; nil when unreported or unparseable. It is the
	// operator's clock, not the console's, which is what lets a check
	// say a condition has held for an hour without having watched it.
	LastTransition *time.Time
}

// ClusterFacts is the operator-reported state of the one target
// cluster, converted to a source-neutral shape. A string field that the
// operator did not report is empty; a numeric field that was not
// reported is nil. Both render as "unknown", never as a value.
type ClusterFacts struct {
	// Present is false when the API server confirmed the cluster does
	// not exist. The other fields are meaningful only when true.
	Present bool
	// UID is the immutable Kubernetes UID of the observed Cluster. It
	// is the console's own evidence of cluster identity: correlation
	// with external sources compares against this observed value, never
	// against injected configuration. A recreated cluster with the same
	// name carries a new UID and must never inherit correlations.
	UID string
	// Phase is the operator-reported phase.
	Phase string
	// PhaseReason is the operator-reported phase reason.
	PhaseReason string
	// CurrentPrimary is the instance currently acting as primary.
	CurrentPrimary string
	// TargetPrimary is the instance the operator is moving primary to.
	// The operator writes the sentinel "pending" while a failover is
	// decided but no candidate is chosen yet.
	TargetPrimary string
	// TargetPrimaryTimestamp is when the operator requested the current
	// target primary; nil when unreported or unparseable. It is the
	// clock on an unfinished primary move.
	TargetPrimaryTimestamp *time.Time
	// DesiredInstances is the declared instance count.
	DesiredInstances *int
	// ReadyInstances is the operator-reported ready instance count.
	ReadyInstances *int
	// TimelineID is the operator-reported PostgreSQL timeline.
	TimelineID *int
	// Image is the container image the operator reports for the pods.
	Image string
	// ImageCatalogRef is the catalog the cluster draws its image from,
	// nil when the image is named directly. It is a reference, not the
	// catalog: resolving it is the rendering layer's job, because the
	// reference can change without the catalog changing and vice versa.
	ImageCatalogRef *ImageCatalogRef
	// PostgresMajorVersion is the reported PostgreSQL major version.
	PostgresMajorVersion *int
	// Conditions are the operator-reported conditions, bounded by the
	// source.
	Conditions []Condition
	// ReportedInstances is the operator's count of instances it knows
	// of — PVC groups, which can exceed the pods that exist.
	ReportedInstances *int
	// FailedInstances are the instances the operator lists under its
	// failed status: pods that will not be scheduled again.
	FailedInstances []string
	// UnusablePVCs, DanglingPVCs, ResizingPVCs and InitializingPVCs are
	// the operator's own classification of the cluster's claims: part
	// of an incomplete group, attached to no pod, carrying a resize
	// condition, or created by a job that has not yet produced a pod.
	UnusablePVCs     []string
	DanglingPVCs     []string
	ResizingPVCs     []string
	InitializingPVCs []string
	// CertificateExpirations are the expiry instants the operator
	// reports for the certificates it manages, by Secret name.
	CertificateExpirations []CertificateExpiry
	// UnreconcilableRoles are the managed roles the operator reports it
	// cannot reconcile, with its own bounded error text.
	UnreconcilableRoles []RoleProblem
	// Tablespaces are the declared tablespaces and their reconciliation
	// state.
	Tablespaces []TablespaceFacts
	// PrimaryFailingSince is when the operator detected the current
	// primary as unhealthy; nil while it is not.
	PrimaryFailingSince *time.Time
	// InstanceTimelines are the PostgreSQL timelines each instance
	// reported to the operator, sorted by instance name.
	InstanceTimelines []InstanceTimeline
	// ReplicaSwitchInProgress reports the operator mid-way through
	// turning this cluster into a replica cluster.
	ReplicaSwitchInProgress bool
	// FencedInstances is the Cluster's cnpg.io/fencedInstances
	// annotation: the instances the operator keeps shut down. "*" fences
	// every instance. Sorted and bounded; nil when nothing is fenced.
	// The annotation is the source of truth for fencing — the instance
	// manager's own report never carries it.
	FencedInstances []string
	// FencingUnreadable reports a fencing annotation that is not a JSON
	// array of names. What it fences is then unknown, not "nothing".
	FencingUnreadable bool
	// Sizing is the declared PostgreSQL memory configuration and the
	// postgres container's resources, which the configuration advisor
	// weighs against each other. Sizing.Read is false when the facts
	// were not converted from a Cluster spec.
	Sizing PostgresSizing
	// BackupArchiverPlugin reports that WAL archiving and backups go
	// through a CNPG-I plugin: an enabled plugin in spec.plugins declares
	// isWALArchiver, or is the barman-cloud plugin. The deprecated in-tree
	// backup status fields (firstRecoverabilityPoint, lastSuccessfulBackup
	// and their kin) are never set for plugin backups, so their absence
	// then means "not reported", never "no backup".
	BackupArchiverPlugin bool
	// ObservedSince records, for each of the states the console tracks
	// across publications, when the console first saw the state it now
	// sees. The operator reports no clock for its phase or its PVC
	// lists, so this is the console's own: a floor, since the console
	// may have started watching after the state began. Keys are the
	// state's name and value, such as "phase=Creating a new replica";
	// see HeldKeys. Nil until the store has published once.
	ObservedSince map[string]time.Time
}

// PostgresSizing is what the Cluster declares about PostgreSQL's memory:
// the memory-relevant entries of spec.postgresql.parameters, verbatim,
// and spec.resources, which CloudNativePG applies to the postgres
// container of every instance. It is intent, not what PostgreSQL
// applied: /pg/status reports no setting, and a parameter that needs a
// restart is pending until the instance restarts.
type PostgresSizing struct {
	// Read is true when the converter read the Cluster's spec.
	Read bool
	// Parameters are the entries of spec.postgresql.parameters whose
	// name (case-insensitively) is one of pgconf.Parameters, values
	// bounded. Absent names take PostgreSQL's defaults.
	Parameters map[string]string
	// TooLong names the parameters whose declared value is longer than
	// the console keeps: not read, so never parsed from a prefix.
	TooLong []string
	// MemoryLimit and MemoryRequest are the postgres container's, in
	// bytes; nil when not set.
	MemoryLimit   *int64
	MemoryRequest *int64
	// HugePages are the hugepages-<size> limits, in bytes, by resource
	// name; nil when none is set. Kubernetes requires the request to
	// equal the limit for huge pages.
	HugePages map[string]int64
	// WALSegmentBytes is spec.bootstrap.initdb.walSegmentSize in bytes;
	// 0 for PostgreSQL's 16 MB default.
	WALSegmentBytes int64
}

// CertificateExpiry is one certificate the operator manages and when it
// reports the certificate expires.
type CertificateExpiry struct {
	// Secret is the Secret the certificate lives in. Only its name is
	// retained.
	Secret string
	// ExpiresAt is the reported expiry instant.
	ExpiresAt time.Time
}

// RoleProblem is a managed role the operator could not reconcile.
type RoleProblem struct {
	// Role is the PostgreSQL role name.
	Role string
	// Errors are the operator's bounded error messages, at most a few.
	Errors []string
}

// TablespaceFacts is one declared tablespace and its reconciliation
// state as the operator reports it.
type TablespaceFacts struct {
	// Name is the tablespace name.
	Name string
	// State is the operator's word: reconciled or pending.
	State string
	// Error is the reconciliation error, empty when none.
	Error string
}

// InstanceTimeline is one instance's self-reported PostgreSQL timeline.
type InstanceTimeline struct {
	// Instance is the instance name.
	Instance string
	// TimelineID is the timeline the instance reported; zero when it
	// reported none.
	TimelineID int
	// IsPrimary is the instance's own claim to the primary role.
	IsPrimary bool
}

// HeldKeys names the states of these facts whose duration the store
// tracks: one key per state and value currently present. A key that
// stays present across publications keeps its first-seen instant; a
// key that disappears is forgotten, so a state that recurs starts a
// new clock. The set is closed and enumerated here so the store need
// know nothing about what the fields mean.
func (f ClusterFacts) HeldKeys() []string {
	var keys []string
	if f.Phase != "" {
		keys = append(keys, "phase="+f.Phase)
	}
	for _, name := range f.FailedInstances {
		keys = append(keys, "failedInstance="+name)
	}
	for _, name := range f.UnusablePVCs {
		keys = append(keys, "unusablePVC="+name)
	}
	for _, name := range f.DanglingPVCs {
		keys = append(keys, "danglingPVC="+name)
	}
	for _, name := range f.ResizingPVCs {
		keys = append(keys, "resizingPVC="+name)
	}
	for _, name := range f.InitializingPVCs {
		keys = append(keys, "initializingPVC="+name)
	}
	for _, timeline := range f.InstanceTimelines {
		if timeline.TimelineID > 0 {
			keys = append(keys, fmt.Sprintf("instanceTimeline=%s:%d", timeline.Instance, timeline.TimelineID))
		}
	}
	if f.ReplicaSwitchInProgress {
		keys = append(keys, "replicaSwitch=inProgress")
	}
	if f.DesiredInstances != nil && f.ReadyInstances != nil && *f.ReadyInstances < *f.DesiredInstances {
		keys = append(keys, fmt.Sprintf("readyShort=%d/%d", *f.ReadyInstances, *f.DesiredInstances))
	}
	return keys
}

// Since reports when the console first saw the named state, and whether
// it is being tracked at all. A state the store has not published is
// not tracked, which a reader must distinguish from a state held since
// the beginning of time.
func (f ClusterFacts) Since(key string) (time.Time, bool) {
	at, ok := f.ObservedSince[key]
	return at, ok
}

// ClusterState is one complete observation delivered by a Source. An
// absent cluster has Facts.Present false.
//
// It carries no resource version on purpose. The seed is a pinned get of
// one object, and a single object's resource version is its last
// modification, not the collection's current position: on a cluster idle
// longer than the server's watch window it is expired before the watch
// even starts, and every watch resumed from it dies instantly with 410
// Expired — permanently, because the next get returns the same version.
type ClusterState struct {
	// Facts is the converted operator-reported state.
	Facts ClusterFacts
}

// Watch is a running watch on the target cluster. Results is closed
// when the watch ends for any reason; the collector then re-seeds with
// a fresh Fetch. Stop releases the watch and must be safe to call after
// Results closed.
type Watch interface {
	// Results streams observations until the watch ends.
	Results() <-chan ClusterState
	// Stop releases the watch.
	Stop()
}

// Source produces observations of the one target cluster. The concrete
// implementation performs the pinned get and the name-scoped watch; the
// fake drives tests.
type Source interface {
	// Fetch returns the current state through the pinned get. An absent
	// cluster is a successful observation, not an error.
	Fetch(ctx context.Context) (ClusterState, error)
	// Watch streams observations from the server's current state: the
	// present object is re-delivered as the first result, then changes
	// follow. There is no cursor to resume from — see ClusterState.
	Watch(ctx context.Context) (Watch, error)
}

// Snapshot is what the console renders. It is immutable: the store
// publishes replacement values, never mutations of shared state.
type Snapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports that the collector has lost contact and the data
	// below is the retained last-good observation.
	Stale bool
	// Cluster is the last observed state.
	Cluster ClusterFacts
}

// ImageCatalogRef is the cluster's spec.imageCatalogRef: which catalog
// it draws its image from, and for which PostgreSQL major version.
type ImageCatalogRef struct {
	// Kind is ImageCatalog or ClusterImageCatalog. The latter is
	// cluster-scoped and outside this console's namespaced authority,
	// so it is reported as referenced-but-not-observable.
	Kind string
	// Name is the referenced catalog's name.
	Name string
	// Major is the PostgreSQL major version taken from the catalog.
	Major int
}

// LogTail is one bounded, on-demand log fetch. It is never cached and
// never persisted: the content lives for one response.
type LogTail struct {
	// Content is the escaped-at-render tail content.
	Content string
	// TruncatedByBytes reports the byte ceiling cut the tail.
	TruncatedByBytes bool
	// LineLimit is the applied line bound.
	LineLimit int
	// ByteLimit is the applied byte bound.
	ByteLimit int64
}

// Clock supplies time and interruptible waiting so the collector is
// deterministic under test.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// Wait sleeps for d or until ctx is done, returning ctx.Err in that
	// case.
	Wait(ctx context.Context, d time.Duration) error
}

// RealClock is the production Clock.
type RealClock struct{}

// Now returns the wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Wait sleeps for d or until ctx is done.
func (RealClock) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
