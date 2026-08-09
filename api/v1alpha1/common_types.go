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
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// Label and annotation keys used by the operator. Kept in the API package so
// that controllers, webhooks, and external tooling share one definition.
const (
	// ManagedByLabelValue is the value of app.kubernetes.io/managed-by on
	// every generated resource, and the selector for the label-filtered
	// informers of owned kinds.
	ManagedByLabelValue = "pgtoolbox"

	// PgConsoleLabelKey labels every generated resource with the owning
	// PgConsole name.
	PgConsoleLabelKey = "pgtoolbox.fyannk.dev/pgconsole"

	// PgToolBoxRoleLabelKey labels every generated resource with the owning
	// PgToolBoxRole name.
	PgToolBoxRoleLabelKey = "pgtoolbox.fyannk.dev/pgtoolboxrole"

	// PgToolBoxUserLabelKey labels every generated resource with the owning
	// PgToolBoxUser name.
	PgToolBoxUserLabelKey = "pgtoolbox.fyannk.dev/pgtoolboxuser"

	// ConfigChecksumAnnotation carries the rendered-configuration checksum
	// on the PgConsole Pod template; a change triggers a rollout.
	ConfigChecksumAnnotation = "pgtoolbox.fyannk.dev/config-checksum"

	// AdminSyncSecretVersionAnnotation carries the admin-sync TLS/token
	// Secret resourceVersion on the Pod template; rotation rolls the Pod.
	// #nosec G101 -- annotation key; no credential material.
	AdminSyncSecretVersionAnnotation = "pgtoolbox.fyannk.dev/admin-sync-secret-version"

	// PgAdminSyncRevisionAnnotation records the sha256 revision of the last
	// successfully applied pgAdmin sync payload on the Deployment.
	PgAdminSyncRevisionAnnotation = "pgtoolbox.fyannk.dev/pgadmin-sync-revision"

	// EvidenceTokenLabelKey marks generated evidence-token Secrets with the
	// owning PgConsole name, so superseded generations can be selected for
	// garbage collection without reading their content.
	// #nosec G101 -- label key; no credential material.
	EvidenceTokenLabelKey = "pgtoolbox.fyannk.dev/evidence-token"

	// RotateEvidenceTokenAnnotation set to "now" on a PgConsole rotates the
	// pod-local evidence token: a successor Secret is minted under a new
	// name and rolled out as one Pod-template revision.
	// #nosec G101 -- annotation key; no credential material.
	RotateEvidenceTokenAnnotation = "pgtoolbox.fyannk.dev/rotate-evidence-token"

	// ReconcileAnnotation set to "skip" pauses reconciliation of a resource.
	ReconcileAnnotation = "pgtoolbox.fyannk.dev/reconcile"

	// PgConsoleFinalizer marks the controller responsible for tearing a
	// PgConsole instance down. Finalizers are per-kind by design, so each
	// kind in the family gets its own.
	PgConsoleFinalizer = "pgtoolbox.fyannk.dev/pgconsole"

	// PgToolBoxRoleFinalizer marks the controller responsible for tearing a
	// PgToolBoxRole down: it owns a managed DatabaseRole and credential
	// Secret.
	PgToolBoxRoleFinalizer = "pgtoolbox.fyannk.dev/pgtoolboxrole"

	// PgToolBoxUserFinalizer marks the controller responsible for
	// de-provisioning a PgToolBoxUser from proxy configuration and pgAdmin.
	PgToolBoxUserFinalizer = "pgtoolbox.fyannk.dev/pgtoolboxuser"
)

// LocalObjectReference is a reference to a resource in the same namespace.
type LocalObjectReference struct {
	// The name of the referenced resource.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// SecretKeyReference is a reference to a key of a Secret in the same
// namespace. When Key is empty, a type-specific default applies.
type SecretKeyReference struct {
	// The name of the referenced Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// The key inside the Secret.
	// +optional
	Key string `json:"key,omitempty"`
}

// ImageSpec selects a container image.
type ImageSpec struct {
	// Image repository, without tag or digest.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Image tag.
	// +kubebuilder:validation:MinLength=1
	Tag string `json:"tag"`

	// Optional image digest; when set it takes precedence over Tag.
	// +kubebuilder:validation:Pattern=`^(sha256:[a-f0-9]{64})?$`
	// +optional
	Digest string `json:"digest,omitempty"`

	// Image pull policy.
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`

	// Pull secrets for the image registry.
	// +optional
	PullSecrets []LocalObjectReference `json:"pullSecrets,omitempty"`
}

// ExposureType selects how the console is published.
// +kubebuilder:validation:Enum=clusterIP;route;ingress;gateway
type ExposureType string

const (
	// ExposureTypeClusterIP exposes the console only inside the cluster.
	ExposureTypeClusterIP ExposureType = "clusterIP"
	// ExposureTypeRoute publishes an OpenShift Route.
	ExposureTypeRoute ExposureType = "route"
	// ExposureTypeIngress publishes a Kubernetes Ingress.
	ExposureTypeIngress ExposureType = "ingress"
	// ExposureTypeGateway publishes a Gateway API HTTPRoute.
	ExposureTypeGateway ExposureType = "gateway"
)

// ExposureSpec configures external access to the console.
// +kubebuilder:validation:XValidation:rule="self.type == 'clusterIP' || size(self.hostname) > 0",message="hostname is required for route, ingress and gateway exposure"
// +kubebuilder:validation:XValidation:rule="self.type == 'gateway' ? has(self.gateway) : !has(self.gateway)",message="gateway configuration is required exactly when type is gateway"
type ExposureSpec struct {
	// The exposure mechanism.
	// +kubebuilder:default=clusterIP
	// +optional
	Type ExposureType `json:"type,omitempty"`

	// The external hostname. Required for route, ingress and gateway.
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// TLS settings for route/ingress exposure. For gateway exposure TLS
	// terminates at the Gateway listener and this section must be empty.
	// +optional
	TLS *ExposureTLSSpec `json:"tls,omitempty"`

	// IngressClass name, for ingress exposure only.
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`

	// Gateway API attachment, for gateway exposure only.
	// +optional
	Gateway *GatewayExposureSpec `json:"gateway,omitempty"`

	// Annotations copied onto the exposure resource after allowlist
	// filtering.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ExposureTLSSpec configures TLS for Route/Ingress exposure.
type ExposureTLSSpec struct {
	// Route termination mode.
	// +kubebuilder:validation:Enum=edge;reencrypt
	// +optional
	Termination string `json:"termination,omitempty"`

	// TLS certificate Secret (kubernetes.io/tls), for ingress exposure.
	// +optional
	CertificateSecretRef *LocalObjectReference `json:"certificateSecretRef,omitempty"`
}

// GatewayExposureSpec configures the HTTPRoute parent reference.
type GatewayExposureSpec struct {
	// The Gateway to attach to.
	ParentRef GatewayParentRef `json:"parentRef"`
}

// GatewayParentRef identifies a Gateway listener.
type GatewayParentRef struct {
	// Gateway name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Gateway namespace; defaults to the PgConsole namespace. Cross-namespace
	// attachment requires a ReferenceGrant in the Gateway namespace, owned
	// by the platform.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Listener (section) name on the Gateway.
	// +optional
	SectionName string `json:"sectionName,omitempty"`
}

// NetworkPolicy directional modes.
const (
	// NetworkPolicyTypesFull restricts both ingress and egress.
	NetworkPolicyTypesFull = "full"
	// NetworkPolicyTypesIngress restricts ingress only.
	NetworkPolicyTypesIngress = "ingress"
)

// NetworkPolicySpec configures NetworkPolicy generation.
// +kubebuilder:validation:XValidation:rule="self.policyTypes == 'ingress' ? !has(self.extraEgress) || size(self.extraEgress) == 0 : true",message="extraEgress requires policyTypes full"
type NetworkPolicySpec struct {
	// Whether the operator generates a NetworkPolicy. Identity-provider
	// egress is added automatically when enabled.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Which directions the generated policy restricts. "full" (the
	// default) generates ingress and egress rules; "ingress" restricts
	// only incoming traffic and leaves egress unconstrained, for clusters
	// whose CNI or platform policy does not support egress rules.
	// +kubebuilder:validation:Enum=full;ingress
	// +kubebuilder:default=full
	// +optional
	PolicyTypes string `json:"policyTypes,omitempty"`

	// Additional egress rules appended to the generated policy. Generated
	// rules cannot be removed. Only accepted with policyTypes "full".
	// +optional
	ExtraEgress []networkingv1.NetworkPolicyEgressRule `json:"extraEgress,omitempty"`
}

// RoleLevel is the coarse authorization level the proxy asserts in the
// X-PgToolBox-Level header, and the console maps onto its own ladder.
//
// The set is closed and hardcoded on both sides — there is nothing an
// operator could add — so a level is chosen on a PgToolBoxUser rather than
// declared as an object of its own.
// +kubebuilder:validation:Enum=view;poweruser;dba
type RoleLevel string

const (
	// RoleLevelView grants the overviews and the metrics screens.
	RoleLevelView RoleLevel = "view"
	// RoleLevelPowerUser adds the remaining read screens and the log tails.
	RoleLevelPowerUser RoleLevel = "poweruser"
	// RoleLevelDBA adds the day-2 operations, the access-request review
	// panel and pgAdmin. It is the default pgAdmin gate.
	RoleLevelDBA RoleLevel = "dba"
)
