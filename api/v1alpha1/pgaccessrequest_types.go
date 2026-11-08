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

// PgAccessRequest-specific condition types (the common ones are in
// conditions.go). Frozen API surface: extend, never rename.
const (
	// ConditionRoleBindingSynced is True when the RBAC state of the
	// namespace matches the request's phase: an Approved request's
	// RoleBinding exists and binds exactly the requested subject to the
	// requested ClusterRole; a request in any other phase has none.
	ConditionRoleBindingSynced = "RoleBindingSynced"
)

// PgAccessRequest-specific reasons of the frozen catalogue, registered from
// pgaccessrequest_funcs.go.
const (
	// ReasonAwaitingDecision: the request is Pending; nothing is bound.
	ReasonAwaitingDecision Reason = "AwaitingDecision"
	// ReasonAccessGranted: the request is Approved and its RoleBinding is
	// in place.
	ReasonAccessGranted Reason = "AccessGranted"
	// ReasonAccessDenied: the request was denied; nothing is bound.
	ReasonAccessDenied Reason = "AccessDenied"
	// ReasonAccessExpired: spec.expiresAt passed; the RoleBinding, if any,
	// was deleted.
	ReasonAccessExpired Reason = "AccessExpired"
	// ReasonAccessRevoked: the grant was revoked; the RoleBinding was
	// deleted.
	ReasonAccessRevoked Reason = "AccessRevoked"
	// ReasonRoleBindingMissing: the request is Approved and its RoleBinding
	// does not exist (deleted by hand, or the approval's create never
	// landed). The operator never recreates it: revoke and request again.
	ReasonRoleBindingMissing Reason = "RoleBindingMissing"
	// ReasonRoleBindingDrifted: the request's RoleBinding binds another
	// subject or ClusterRole than the request names; the operator deleted
	// it (fail closed).
	ReasonRoleBindingDrifted Reason = "RoleBindingDrifted"
	// ReasonRoleBindingOrphaned: a RoleBinding of a request that is not
	// Approved (an approval whose status write failed) outlived the grace
	// period and was deleted.
	ReasonRoleBindingOrphaned Reason = "RoleBindingOrphaned"
	// ReasonSelfApprovalRefused: the recorded approver is the subject
	// itself; the operator revoked the grant (four eyes).
	ReasonSelfApprovalRefused Reason = "SelfApprovalRefused"
	// ReasonRoleBindingDeleted is the Normal Event of a RoleBinding the
	// operator deleted (expiry, revocation, drift, orphan).
	ReasonRoleBindingDeleted Reason = "RoleBindingDeleted"
)

// AccessSubjectKind is the kind of the subject access is requested for.
// +kubebuilder:validation:Enum=User;Group;ServiceAccount
type AccessSubjectKind string

const (
	// AccessSubjectUser is a user as the API server names them (the
	// username a token review returns, with any OIDC prefix).
	AccessSubjectUser AccessSubjectKind = "User"
	// AccessSubjectGroup is a group as the API server names it.
	AccessSubjectGroup AccessSubjectKind = "Group"
	// AccessSubjectServiceAccount is a ServiceAccount of the request's own
	// namespace (same-namespace references only).
	AccessSubjectServiceAccount AccessSubjectKind = "ServiceAccount"
)

// AccessSubject is who the access is for.
// +kubebuilder:validation:XValidation:rule="self.kind != 'ServiceAccount' || (size(self.name) <= 253 && self.name.matches('^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$'))",message="a ServiceAccount subject is the name of a ServiceAccount in the request's namespace (a DNS subdomain)",fieldPath=".name",reason=FieldValueInvalid
// +kubebuilder:validation:XValidation:rule="self.kind == 'ServiceAccount' || !self.name.startsWith('system:')",message="system: users and groups cannot be granted access through a request (system:authenticated would grant everyone); name a person, a group or a ServiceAccount",fieldPath=".name",reason=FieldValueForbidden
type AccessSubject struct {
	// The subject's kind.
	Kind AccessSubjectKind `json:"kind"`

	// The subject's name as the API server sees it: a username (with its
	// OIDC prefix, if the API server adds one), a group, or a
	// ServiceAccount name in this namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1f\x7f]+$`
	Name string `json:"name"`
}

// AccessCapability is what is requested: one of the two ClusterRoles of
// the console. viewer includes pod logs; operator includes
// the console's operations and opening pgAdmin.
// +kubebuilder:validation:Enum=viewer;operator
type AccessCapability string

const (
	// AccessCapabilityViewer binds the pgtoolbox-console-viewer ClusterRole.
	AccessCapabilityViewer AccessCapability = "viewer"
	// AccessCapabilityOperator binds the pgtoolbox-console-operator ClusterRole.
	AccessCapabilityOperator AccessCapability = "operator"
)

// PgAccessRequestSpec is a request for access to the CloudNativePG
// Clusters of this namespace through the console. A request is immutable:
// to change it, make a new one.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a PgAccessRequest cannot be changed once made; create a new one",reason=FieldValueForbidden
type PgAccessRequestSpec struct {
	// Who the access is for.
	Subject AccessSubject `json:"subject"`

	// The Cluster, in this namespace, the access is wanted for. Empty: every
	// Cluster of the namespace. Informative only: a RoleBinding cannot be
	// narrowed to one Cluster, so an approved request grants the
	// capability on every Cluster of the namespace, and its status says so.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +optional
	Cluster string `json:"cluster,omitempty"`

	// The capability requested.
	Capability AccessCapability `json:"capability"`

	// Why the access is needed; shown to whoever decides.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Justification string `json:"justification"`

	// When the access ends. The operator deletes the RoleBinding at this
	// time and marks the request Expired; a Pending request past it can no
	// longer be approved. Omitted: until revoked.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// PgAccessRequestPhase is where a request stands.
// +kubebuilder:validation:Enum=Pending;Approved;Denied;Expired;Revoked
type PgAccessRequestPhase string

const (
	// AccessPhasePending is waiting for a decision.
	AccessPhasePending PgAccessRequestPhase = "Pending"
	// AccessPhaseApproved means a RoleBinding grants the capability.
	AccessPhaseApproved PgAccessRequestPhase = "Approved"
	// AccessPhaseDenied is a refusal; terminal.
	AccessPhaseDenied PgAccessRequestPhase = "Denied"
	// AccessPhaseExpired means expiresAt passed; terminal.
	AccessPhaseExpired PgAccessRequestPhase = "Expired"
	// AccessPhaseRevoked means the grant was withdrawn; terminal.
	AccessPhaseRevoked PgAccessRequestPhase = "Revoked"
)

// AccessGrantScope is how far an approved request reaches.
// +kubebuilder:validation:Enum=Namespace
type AccessGrantScope string

// AccessGrantScopeNamespace means the RoleBinding covers every Cluster of the
// namespace, whatever spec.cluster names.
const AccessGrantScopeNamespace AccessGrantScope = "Namespace"

// PgAccessRequestStatus is the decision and its effect. The decision
// fields are written by the deciding person (through the console, with
// their own token: the API server records who wrote them); the conditions
// by the operator.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.phase) || oldSelf.phase == self.phase || (oldSelf.phase == 'Pending' && self.phase in ['Approved', 'Denied', 'Expired']) || (oldSelf.phase == 'Approved' && self.phase in ['Revoked', 'Expired'])",message="phase moves only Pending to Approved, Denied or Expired, and Approved to Revoked or Expired",fieldPath=".phase",reason=FieldValueForbidden
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.decidedBy) || (has(self.decidedBy) && self.decidedBy == oldSelf.decidedBy)",message="decidedBy cannot change once recorded",fieldPath=".decidedBy",reason=FieldValueForbidden
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.roleBinding) || (has(self.roleBinding) && self.roleBinding == oldSelf.roleBinding)",message="roleBinding cannot change once recorded",fieldPath=".roleBinding",reason=FieldValueForbidden
// +kubebuilder:validation:XValidation:rule="!has(self.phase) || !(self.phase in ['Approved', 'Denied']) || (has(self.decidedBy) && has(self.decidedAt))",message="an Approved or Denied request records decidedBy and decidedAt",fieldPath=".decidedBy",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="!has(self.phase) || self.phase != 'Approved' || (has(self.roleBinding) && has(self.clusterRole))",message="an Approved request records its roleBinding and clusterRole",fieldPath=".roleBinding",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="!has(self.phase) || self.phase != 'Denied' || (has(self.note) && size(self.note) > 0)",message="a Denied request records why in note",fieldPath=".note",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="!has(self.phase) || self.phase != 'Revoked' || (has(self.revokedBy) && has(self.revokedAt))",message="a Revoked request records revokedBy and revokedAt",fieldPath=".revokedBy",reason=FieldValueRequired
type PgAccessRequestStatus struct {
	// The generation the conditions were computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Where the request stands. Empty until the operator or a decision
	// first writes it.
	// +optional
	Phase PgAccessRequestPhase `json:"phase,omitempty"`

	// The username of the person who approved or denied the request, as
	// the API server named them when the console asked.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	DecidedBy string `json:"decidedBy,omitempty"`

	// When the request was approved or denied.
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`

	// The username of the person who revoked the grant, or
	// "pgtoolbox-operator" when the operator revoked a self-approval.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	RevokedBy string `json:"revokedBy,omitempty"`

	// When the grant was revoked.
	// +optional
	RevokedAt *metav1.Time `json:"revokedAt,omitempty"`

	// The decider's note: why it was denied (required), approved or
	// revoked; or the operator's explanation of a revocation.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Note string `json:"note,omitempty"`

	// The RoleBinding of an approved request, in this namespace.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RoleBinding string `json:"roleBinding,omitempty"`

	// The ClusterRole the RoleBinding references.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ClusterRole string `json:"clusterRole,omitempty"`

	// How far the grant reaches: always Namespace, since a RoleBinding
	// cannot be narrowed to one Cluster, even when spec.cluster names one.
	// +optional
	GrantedScope AccessGrantScope `json:"grantedScope,omitempty"`

	// Conditions: Ready (summary), ConfigurationValid, RoleBindingSynced,
	// Progressing and Stalled.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgar,categories=pgtoolbox
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject.name`
// +kubebuilder:printcolumn:name="Capability",type=string,JSONPath=`.spec.capability`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cluster`,priority=1
// +kubebuilder:printcolumn:name="Decided-By",type=string,JSONPath=`.status.decidedBy`,priority=1
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,priority=1
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name must be a DNS-1123 label of at most 63 characters (it names the RoleBinding and labels it)",reason=FieldValueInvalid

// PgAccessRequest asks for one of the console's two ClusterRoles
// (pgtoolbox-console-viewer, pgtoolbox-console-operator) in this namespace. Whoever may
// create RoleBindings here and update the request's status approves it
// through the console, which creates the RoleBinding with the approver's own
// token; the operator deletes it when the request expires, is revoked or is
// deleted.
type PgAccessRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PgAccessRequestSpec `json:"spec"`
	// +optional
	Status PgAccessRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PgAccessRequestList contains a list of PgAccessRequest.
type PgAccessRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PgAccessRequest `json:"items"`
}
