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

// AccessRequestState is the decision state of a PgToolBoxAccessRequest.
// +kubebuilder:validation:Enum=pending;approved;denied
type AccessRequestState string

const (
	// AccessRequestStatePending awaits review by a dba-level user.
	AccessRequestStatePending AccessRequestState = "pending"
	// AccessRequestStateApproved was granted; the operator creates the
	// corresponding PgToolBoxUser.
	AccessRequestStateApproved AccessRequestState = "approved"
	// AccessRequestStateDenied was refused; the object stays as an audit
	// record.
	AccessRequestStateDenied AccessRequestState = "denied"
)

// PgToolBoxAccessRequestSpec is the desired state of an access request,
// created by the proxy when an authenticated identity is unknown to the
// console. The proxy only ever creates these objects; reading and deciding
// them is the console's job (its operate Role grants read + status update).
type PgToolBoxAccessRequestSpec struct {
	// The PgConsole the requester wants access to, in the same namespace.
	PgConsoleRef LocalObjectReference `json:"pgConsoleRef"`

	// The identity the authentication provider vouched for, set by the
	// proxy. Immutable: a request belongs to exactly one identity.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="subject is immutable"
	Subject string `json:"subject"`

	// Free-form justification from the requester, shown to the reviewer.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`
}

// PgToolBoxAccessRequestStatus is the observed state of an access request.
// The review decision — state, requestedLevel, decidedBy, decidedAt — is
// written by a dba-level user through the console, and only the status
// subresource, so the create-only proxy cannot self-approve.
type PgToolBoxAccessRequestStatus struct {
	// The generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// The decision state.
	// +kubebuilder:default=pending
	// +optional
	State AccessRequestState `json:"state,omitempty"`

	// The level the reviewer granted, required when state is approved.
	RequestedLevel RoleLevel `json:"requestedLevel,omitempty"`

	// The identity of the reviewer who took the decision.
	// +optional
	DecidedBy string `json:"decidedBy,omitempty"`

	// When the decision was taken.
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`

	// Standard conditions: Decided, UserReady.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgreq
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Console",type=string,JSONPath=`.spec.pgConsoleRef.name`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.status.requestedLevel`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PgToolBoxAccessRequest describes one request for access to a PgConsole.
type PgToolBoxAccessRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PgToolBoxAccessRequestSpec `json:"spec"`
	// +optional
	Status PgToolBoxAccessRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PgToolBoxAccessRequestList contains a list of PgToolBoxAccessRequest
// resources.
type PgToolBoxAccessRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PgToolBoxAccessRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PgToolBoxAccessRequest{}, &PgToolBoxAccessRequestList{})
}
