/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoleLevel is the coarse authorization level a PgToolBoxRole grants,
// forwarded by the proxy as the X-PgToolBox-Level header.
// +kubebuilder:validation:Enum=view;poweruser;dba
type RoleLevel string

const (
	// RoleLevelView grants read-only access to every console panel.
	RoleLevelView RoleLevel = "view"
	// RoleLevelPowerUser additionally enables console operations.
	RoleLevelPowerUser RoleLevel = "poweruser"
	// RoleLevelDBA is the highest level, and the default pgAdmin gate.
	RoleLevelDBA RoleLevel = "dba"
)

// PgToolBoxRoleSpec is the desired state of a role: one level of access to
// one PgConsole, backed by one postgres role.
type PgToolBoxRoleSpec struct {
	// The PgConsole this role is attached to, in the same namespace.
	PgConsoleRef LocalObjectReference `json:"pgConsoleRef"`

	// The console authorization level granted by this role.
	Level RoleLevel `json:"level"`

	// The postgres backing of the role.
	PostgresRole PostgresRoleSpec `json:"postgresRole"`
}

// PostgresRoleSpec selects the postgres role backing a PgToolBoxRole as an
// exactly-one union: either a profile the operator materializes as a CNPG
// DatabaseRole, or a reference to an existing DatabaseRole.
// +kubebuilder:validation:XValidation:rule="has(self.profile) != has(self.databaseRoleRef)",message="exactly one of profile or databaseRoleRef must be set"
type PostgresRoleSpec struct {
	// The operator-managed profile to materialize as a CNPG DatabaseRole
	// plus password Secret.
	// +optional
	Profile PostgresRoleProfile `json:"profile,omitempty"`

	// Reference to an existing CNPG DatabaseRole (bring-your-own), in the
	// same namespace.
	// +optional
	DatabaseRoleRef *LocalObjectReference `json:"databaseRoleRef,omitempty"`
}

// PostgresRoleProfile selects a predefined DatabaseRole shape.
// +kubebuilder:validation:Enum=monitor;database-readonly;database-owner
type PostgresRoleProfile string

const (
	// PostgresRoleProfileMonitor is a read-only monitoring role.
	PostgresRoleProfileMonitor PostgresRoleProfile = "monitor"
	// PostgresRoleProfileDatabaseReadonly is a read-only role on the
	// application database.
	PostgresRoleProfileDatabaseReadonly PostgresRoleProfile = "database-readonly"
	// PostgresRoleProfileDatabaseOwner owns the application database.
	PostgresRoleProfileDatabaseOwner PostgresRoleProfile = "database-owner"
)

// PgToolBoxRoleStatus is the observed state of a PgToolBoxRole.
type PgToolBoxRoleStatus struct {
	// The generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// The name of the CNPG DatabaseRole backing this role, whether
	// materialized from a profile or resolved from databaseRoleRef.
	// +optional
	DatabaseRoleName string `json:"databaseRoleName,omitempty"`

	// Standard conditions: Ready, PgConsoleReady, DatabaseRoleReady,
	// CredentialReady.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgrole
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Console",type=string,JSONPath=`.spec.pgConsoleRef.name`
// +kubebuilder:printcolumn:name="Level",type=string,JSONPath=`.spec.level`
// +kubebuilder:printcolumn:name="DatabaseRole",type=string,JSONPath=`.status.databaseRoleName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PgToolBoxRole describes one role on one PgConsole.
type PgToolBoxRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PgToolBoxRoleSpec `json:"spec"`
	// +optional
	Status PgToolBoxRoleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PgToolBoxRoleList contains a list of PgToolBoxRole resources.
type PgToolBoxRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PgToolBoxRole `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PgToolBoxRole{}, &PgToolBoxRoleList{})
}
