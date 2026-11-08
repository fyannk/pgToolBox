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

// Objects is the /objects screen: the declarative objects that target
// the Cluster (Database, DatabaseRole, Publication, Subscription), the
// managed roles and tablespaces of Cluster.status, the Poolers and the
// image catalog. A declarative object is ok only when applied and its
// observedGeneration equals its generation; its message is
// the PostgreSQL error. A list whose source is not served or not
// readable is empty with the reason in Sources, never "none".
type Objects struct {
	Ref           ClusterRef       `json:"ref"`
	GeneratedAt   time.Time        `json:"generatedAt"`
	Databases     []DeclaredObject `json:"databases"`
	Roles         []DeclaredObject `json:"roles" doc:"DatabaseRole objects (CloudNativePG 1.30 and later)."`
	ManagedRoles  []ManagedRole    `json:"managedRoles" doc:"spec.managed.roles merged with status.managedRolesStatus."`
	Tablespaces   []TablespaceView `json:"tablespaces" doc:"spec.tablespaces merged with status.tablespacesStatus."`
	Publications  []DeclaredObject `json:"publications"`
	Subscriptions []DeclaredObject `json:"subscriptions"`
	Poolers       []PoolerRow      `json:"poolers"`
	// ImageCatalog is the catalog the Cluster's image comes from; absent
	// when the Cluster names no catalog.
	ImageCatalog *CatalogView  `json:"imageCatalog,omitempty"`
	Sources      []SourceState `json:"sources"`
	BlindSpots   []BlindSpot   `json:"blindSpots"`
}

// DeclaredObject is one Database, DatabaseRole, Publication or
// Subscription.
type DeclaredObject struct {
	Kind string `json:"kind" enum:"Database,DatabaseRole,Publication,Subscription"`
	Name string `json:"name" doc:"Kubernetes object name." untrusted:"true"`
	// PGName is the PostgreSQL object name (spec.name).
	PGName string `json:"pgName,omitempty" untrusted:"true"`
	// Database is the database a publication or subscription lives in.
	Database string `json:"database,omitempty" untrusted:"true"`
	Ensure   string `json:"ensure,omitempty" doc:"present or absent, verbatim."`
	// State is the reading: applied, pendingGeneration, failed,
	// waitingForPrimary, notReconciled, notApplicable or unknown.
	State   string `json:"state" enum:"applied,pendingGeneration,failed,waitingForPrimary,notReconciled,notApplicable,unknown"`
	Health  Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Message string `json:"message,omitempty" doc:"status.message: the PostgreSQL error, bounded, rendered escaped."`
	// FailedObjects names sub-objects that failed (schemas, extensions,
	// foreign data wrappers, servers of a Database).
	FailedObjects      []string   `json:"failedObjects,omitempty" untrusted:"true"`
	Generation         *int64     `json:"generation,omitempty"`
	ObservedGeneration *int64     `json:"observedGeneration,omitempty"`
	Provenance         Provenance `json:"provenance"`
}

// ManagedRole is one role of spec.managed.roles or status.
type ManagedRole struct {
	Name   string `json:"name" untrusted:"true"`
	Status string `json:"status" doc:"The status bucket the operator files it under (reconciled, not-managed, pending-reconciliation, reserved), verbatim, or unreported."`
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Declared is false for a role the status reports but the spec no
	// longer lists.
	Declared   bool       `json:"declared"`
	Errors     []string   `json:"errors,omitempty" untrusted:"true"`
	Provenance Provenance `json:"provenance"`
}

// TablespaceView is one tablespace.
type TablespaceView struct {
	Name       string     `json:"name" untrusted:"true"`
	Owner      string     `json:"owner,omitempty" untrusted:"true"`
	State      string     `json:"state" doc:"The status state (reconciled, pending, ...), verbatim, or unreported."`
	Health     Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Error      string     `json:"error,omitempty" untrusted:"true"`
	Declared   bool       `json:"declared"`
	Temporary  bool       `json:"temporary,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// PoolerRow is one Pooler of the Cluster.
type PoolerRow struct {
	Name     string `json:"name" untrusted:"true"`
	Type     string `json:"type,omitempty" doc:"rw, ro or r."`
	PoolMode string `json:"poolMode,omitempty" doc:"session or transaction."`
	Paused   bool   `json:"paused,omitempty"`
	// DesiredInstances is spec.instances; Instances is status.instances,
	// which is the Deployment's replica count, not ready pods.
	DesiredInstances *int `json:"desiredInstances,omitempty"`
	Instances        *int `json:"instances,omitempty"`
	// PodsReady counts the pooler's Ready pods per Kubernetes.
	PodsReady   *int       `json:"podsReady,omitempty"`
	Phase       string     `json:"phase,omitempty" doc:"status.phase (CloudNativePG 1.30 and later), verbatim."`
	PhaseReason string     `json:"phaseReason,omitempty" untrusted:"true"`
	Image       string     `json:"image,omitempty"`
	Error       string     `json:"error,omitempty" untrusted:"true"`
	Text        string     `json:"text,omitempty" untrusted:"true"`
	Health      Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Provenance  Provenance `json:"provenance"`
}

// CatalogView is the image catalog a Cluster's spec.imageCatalogRef
// names.
type CatalogView struct {
	Kind  string `json:"kind" enum:"ImageCatalog,ClusterImageCatalog"`
	Name  string `json:"name"`
	Major *int   `json:"major,omitempty" doc:"The major the Cluster selects."`
	// State is readable, notFound or notReadable (a ClusterImageCatalog
	// without the opt-in ClusterRole: never an empty list).
	State  string         `json:"state" enum:"readable,notFound,notReadable"`
	Reason string         `json:"reason,omitempty"`
	Image  string         `json:"image,omitempty" doc:"The image the catalog gives for the selected major."`
	Images []CatalogImage `json:"images,omitempty"`
	Health Health         `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Provenance is the catalog object's.
	Provenance Provenance `json:"provenance"`
}

// CatalogImage is one entry of a catalog.
type CatalogImage struct {
	Major int    `json:"major"`
	Image string `json:"image"`
}
