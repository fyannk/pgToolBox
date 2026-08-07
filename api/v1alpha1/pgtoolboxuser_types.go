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

// PgToolBoxUserSpec is the desired state of a user: one identity on one
// console. Deleting a PgToolBoxUser de-provisions it from the proxy
// configuration and from pgAdmin.
type PgToolBoxUserSpec struct {
	// The PgConsole this user belongs to, in the same namespace.
	PgConsoleRef LocalObjectReference `json:"pgConsoleRef"`

	// The identity the authentication provider vouches for: email, sub or
	// username depending on the proxy authentication mode.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Subject string `json:"subject"`

	// The PgToolBoxRole granting this user its level and postgres role,
	// in the same namespace.
	RoleRef LocalObjectReference `json:"roleRef"`

	// Reference to the Secret holding this user's local-mode password
	// (bcrypt). Only meaningful when the console's proxy authentication
	// mode is local. Default key: "password".
	// +optional
	LocalPasswordSecretRef *SecretKeyReference `json:"localPasswordSecretRef,omitempty"`
}

// PgToolBoxUserStatus is the observed state of a PgToolBoxUser.
type PgToolBoxUserStatus struct {
	// The generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Whether the rendered proxy configuration includes this user.
	// +optional
	ProxySynced bool `json:"proxySynced,omitempty"`

	// Whether the pgAdmin account and shared server definition are
	// provisioned for this user.
	// +optional
	PgAdminSynced bool `json:"pgAdminSynced,omitempty"`

	// Standard conditions: Ready, RoleReady, ProxySynced, PgAdminSynced.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pguser
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Console",type=string,JSONPath=`.spec.pgConsoleRef.name`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.roleRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PgToolBoxUser describes one identity on one PgConsole.
type PgToolBoxUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PgToolBoxUserSpec `json:"spec"`
	// +optional
	Status PgToolBoxUserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PgToolBoxUserList contains a list of PgToolBoxUser resources.
type PgToolBoxUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PgToolBoxUser `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PgToolBoxUser{}, &PgToolBoxUserList{})
}
