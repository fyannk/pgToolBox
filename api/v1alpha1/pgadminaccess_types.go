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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PgAdminNamespaceLabelKey labels the Role and RoleBinding a PgAdminAccess
// grant gets in its namespace with the namespace of the PgAdmin they serve
// (PgAdminLabelKey carries its name): the controller maps them back to it.
const PgAdminNamespaceLabelKey = "pgtoolbox.fyannk.dev/pgadmin-namespace"

// PgAdminAccessSpec grants one PgAdmin of another namespace the
// registration of named Clusters of this namespace.
type PgAdminAccessSpec struct {
	// The PgAdmin granted access.
	PgAdmin PgAdminReference `json:"pgAdmin"`

	// The Clusters of this namespace it may register. Their credentials
	// (the Secrets its spec.credentials offers and each Cluster's CA) become
	// readable by that PgAdmin's ServiceAccount through a name-scoped Role
	// the operator maintains here while this grant exists.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Clusters []LocalObjectReference `json:"clusters"`

	// Whether the privileged credentials of these Clusters (the superuser,
	// and DatabaseRoles that are superuser, createrole, bypassrls or
	// replication or members of a privileged role) may be offered, when the
	// PgAdmin asks for them (spec.credentials.superuser). Off by default:
	// the PgAdmin's own spec cannot widen what this grant allows.
	// +kubebuilder:default=false
	// +optional
	AllowPrivileged *bool `json:"allowPrivileged,omitempty"`
}

// PgAdminReference names a PgAdmin, in any namespace.
type PgAdminReference struct {
	// The namespace of the PgAdmin.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`

	// The name of the PgAdmin.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=pgaa,categories=pgtoolbox
// +kubebuilder:printcolumn:name="PgAdmin Namespace",type=string,JSONPath=`.spec.pgAdmin.namespace`
// +kubebuilder:printcolumn:name="PgAdmin",type=string,JSONPath=`.spec.pgAdmin.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PgAdminAccess is a grant, in the namespace of the Clusters it names: the
// PgAdmin it names, in another namespace, may register those Clusters and
// read their offered credentials. Creating one is the decision of whoever
// may read this namespace's Secrets; who may create PgAdminAccess objects
// here is therefore an RBAC decision of the namespace owner. Like the
// Gateway API's ReferenceGrant it has no status: the PgAdmin reports
// whether it is granted (ConfigurationValid, GrantMissing). Deleting it
// revokes the access (the Role and RoleBinding it owns are garbage
// collected).
type PgAdminAccess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PgAdminAccessSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// PgAdminAccessList contains a list of PgAdminAccess.
type PgAdminAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PgAdminAccess `json:"items"`
}
