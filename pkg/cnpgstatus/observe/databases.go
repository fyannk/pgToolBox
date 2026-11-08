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

// Declared is the spine every declarative object shares: what the
// operator did with the declaration, not what the database contains.
//
// The console never connects to PostgreSQL. Everything here is the
// operator's report about reconciling a Kubernetes resource, which is
// why a database's size, a role's actual privileges in the cluster, or a
// publication's replicated rows appear nowhere: those are questions for
// pgAdmin, and answering them here would be doing a sibling's job.
type Declared struct {
	// Applied is the operator's reconciliation verdict. Nil means it has
	// not reported one yet, which is distinct from a reported failure.
	Applied *bool
	// Message is the operator's reconciliation output, bounded.
	Message string
	// ObservedGeneration is the spec generation the operator last
	// synchronized.
	ObservedGeneration int64
}

// DatabaseFacts is one declared PostgreSQL database.
type DatabaseFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Database is the declared database name inside the cluster.
	Database string
	// Owner is the declared owning role.
	Owner string
	// Encoding is the declared encoding, empty when unset.
	Encoding string
	// Ensure is the declared reconciliation intent, present or absent.
	Ensure string
	// CreatedAt is the resource creation time.
	CreatedAt time.Time
	// Declared is the operator's reconciliation report.
	Declared
}

// DatabaseRoleFacts is one declared PostgreSQL role.
type DatabaseRoleFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Role is the declared role name inside the cluster.
	Role string
	// Superuser, CreateDB and CreateRole are the declared privilege
	// attributes. They are the declaration, never a reading of the
	// cluster's catalogs.
	Superuser  bool
	CreateDB   bool
	CreateRole bool
	// ConnectionLimit is the declared limit; negative means unlimited.
	ConnectionLimit int64
	// InRoles are the declared memberships, bounded.
	InRoles []string
	// HasPasswordSecret reports that the declaration references a
	// password Secret. Only the fact of the reference crosses this
	// boundary — never the Secret's name and never its content. The
	// console holds no Secret permission and displays nothing that would
	// need one.
	HasPasswordSecret bool
	// ValidUntil is the declared expiry of the role's password.
	ValidUntil *time.Time
	// CreatedAt is the resource creation time.
	CreatedAt time.Time
	// Declared is the operator's reconciliation report.
	Declared
}

// PublicationFacts is one declared logical-replication publication.
type PublicationFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Publication is the declared publication name.
	Publication string
	// Database is the database the publication lives in.
	Database string
	// AllTables reports the declared target as the whole database rather
	// than an enumerated object list. The objects themselves are not
	// rendered: a table list is database content.
	AllTables bool
	// CreatedAt is the resource creation time.
	CreatedAt time.Time
	// Declared is the operator's reconciliation report.
	Declared
}

// SubscriptionFacts is one declared logical-replication subscription.
type SubscriptionFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Subscription is the declared subscription name.
	Subscription string
	// Database is the database the subscription lives in.
	Database string
	// Publication is the publication it subscribes to.
	Publication string
	// ExternalCluster is the declared source cluster name.
	ExternalCluster string
	// CreatedAt is the resource creation time.
	CreatedAt time.Time
	// Declared is the operator's reconciliation report.
	Declared
}

// DatabaseObjectsSnapshot is the rendered declarative set: one section,
// one freshness, four lists.
type DatabaseObjectsSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports lost contact.
	Stale bool
	// Truncated reports that any kind was cut at its bound.
	Truncated bool
	// Each list is name-sorted; the producer bounds it.
	Databases     []DatabaseFacts
	Roles         []DatabaseRoleFacts
	Publications  []PublicationFacts
	Subscriptions []SubscriptionFacts
}
