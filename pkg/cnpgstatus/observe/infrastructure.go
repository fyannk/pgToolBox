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

// The infrastructure snapshot is what a DBA reaches for when the
// question is physical rather than logical: which addresses clients
// actually connect to, what disk each instance actually holds, and
// which volume snapshots actually exist. The console read none of this
// before — the wiring diagram named endpoints by string-building from
// the cluster name and said nothing about storage at all, which reads
// as a claim about a cluster that was never observed.
//
// The three kinds travel together because they are read together and
// answer one question between them. One freshness line covers the set,
// exactly as the declarative-objects snapshot does for its four kinds.
//
// Volume snapshots are optional in a way the others are not: the API is
// a separate CRD that many clusters do not install. Its absence is a
// successful observation — SnapshotsObservable false — and never an
// error, because "no snapshot API here" and "no snapshots here" are
// different claims.

// ServiceFacts is one observed Service that fronts the cluster.
type ServiceFacts struct {
	// Name is the service name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Role is the CloudNativePG service role — rw, ro or r — taken from
	// the operator's own label, empty when it carries none.
	Role string
	// Type is the service type, such as ClusterIP.
	Type string
	// ClusterIP is the allocated address; empty for a headless service,
	// which is a fact rather than an absence.
	ClusterIP string
	// Headless reports the explicit None address.
	Headless bool
	// Port is the first reported port; nil when none is reported.
	Port *int32
	// TargetSelector is the label selector the service routes on, as
	// reported, sorted for a stable rendering.
	TargetSelector []string
}

// VolumeFacts is one observed PersistentVolumeClaim of the cluster.
type VolumeFacts struct {
	// Name is the claim name, which for CloudNativePG matches the
	// instance it belongs to (or carries a role suffix, such as -wal).
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Instance is the instance the claim serves, from the operator's
	// label; empty when unlabelled.
	Instance string
	// Role is the claim's purpose as the operator labels it, such as
	// PG_DATA or PG_WAL; empty when unlabelled.
	Role string
	// Phase is the claim phase, such as Bound.
	Phase string
	// Capacity is the reported capacity, empty when unreported.
	Capacity string
	// StorageClass is the class the claim was provisioned from; empty
	// when the cluster uses the default and reports none.
	StorageClass string
	// VolumeName is the bound PersistentVolume; empty until bound.
	VolumeName string
}

// SnapshotFacts is one observed VolumeSnapshot.
type SnapshotFacts struct {
	// Name is the snapshot name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// SourceClaim is the claim the snapshot was taken from.
	SourceClaim string
	// Ready is the reported readiness; nil when unreported, which is
	// not the same as not ready.
	Ready *bool
	// RestoreSize is the reported restore size, empty when unreported.
	RestoreSize string
	// CreatedAt is the reported creation time; nil when unreported.
	CreatedAt *time.Time
}

// ChildFacts is one further object the Cluster owns: a Secret, a
// ConfigMap, a PodDisruptionBudget, a ServiceAccount, a Role, a
// RoleBinding or a Job. These kinds share one facts shape because each
// contributes a name and at most two observed details; the per-kind
// fields below are nil or empty for every other kind.
//
// For a Secret only the metadata is retained: its name, its type and
// how many keys it holds. No key name and no value ever leaves the
// adapter, and none of these objects is recorded into history.
type ChildFacts struct {
	// Kind is the Kubernetes kind: Secret, ConfigMap,
	// PodDisruptionBudget, ServiceAccount, Role, RoleBinding, Job.
	Kind string
	// Name is the object name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// CreatedAt is the object's creation time; nil when unreported.
	CreatedAt *time.Time
	// SecretType is the Secret's reported type.
	SecretType string
	// Keys counts a Secret's or ConfigMap's entries; nil elsewhere.
	Keys *int
	// MinAvailable and MaxUnavailable are the PodDisruptionBudget's
	// constraint exactly as declared; empty when the side is unset.
	MinAvailable   string
	MaxUnavailable string
	// DisruptionsAllowed is the PodDisruptionBudget's reported headroom.
	DisruptionsAllowed *int32
	// Rules counts a Role's rules; Subjects a RoleBinding's subjects.
	Rules    *int
	Subjects *int
	// RoleRef is the role a RoleBinding grants.
	RoleRef string
	// Active, Succeeded and Failed are a Job's reported pod counts.
	Active    *int32
	Succeeded *int32
	Failed    *int32
	// BootstrapImage is the operator's own image on a Job's
	// bootstrap-controller init container, empty elsewhere. It exists
	// for the cluster that breaks before its first instance pod: the
	// bootstrap Jobs are then the only place the operator's version is
	// observable at all.
	BootstrapImage string
}

// InfrastructureSnapshot is the immutable published view.
type InfrastructureSnapshot struct {
	Generation          uint64
	ObservedAt          time.Time
	Stale               bool
	Services            []ServiceFacts
	Volumes             []VolumeFacts
	Snapshots           []SnapshotFacts
	Children            []ChildFacts
	SnapshotsObservable bool
	ChildrenUnobserved  []string
	Truncated           bool
}
