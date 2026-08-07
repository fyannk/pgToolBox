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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PgConsoleSpec is the desired state of a pgToolBox console: one access
// stack — authentication proxy, observation console, embedded pgAdmin, and
// an optional evidence sidecar — attached to exactly one CloudNativePG
// Cluster in the same namespace.
type PgConsoleSpec struct {
	// The CloudNativePG Cluster this console serves, in the same namespace.
	// Immutable: a console is dedicated to one cluster for its whole life.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cnpgClusterRef is immutable"
	CNPGClusterRef LocalObjectReference `json:"cnpgClusterRef"`

	// The pgconsole container image to run. Defaults to the operator's
	// configured image (--default-pgconsole-image).
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// The pgtoolbox-proxy authentication proxy, the single authentication
	// and coarse authorization boundary of the console.
	Proxy ProxySpec `json:"proxy"`

	// The embedded pgAdmin, dedicated to this cluster.
	// +optional
	PgAdmin PgAdminSpec `json:"pgAdmin,omitempty"`

	// The ObjectStoreViewer evidence sidecar, publishing repository
	// evidence to the console over a pod-private Unix socket.
	// +optional
	Evidence EvidenceSpec `json:"evidence,omitempty"`

	// How the console is reached from outside the cluster.
	// +optional
	Exposure ExposureSpec `json:"exposure,omitempty"`

	// +optional
	NetworkPolicy NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Resource budget shared by the pgconsole container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ProxySpec configures the pgtoolbox-proxy container.
type ProxySpec struct {
	// The proxy container image. Defaults to the operator's configured
	// image (--default-pgtoolbox-proxy-image).
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// How users authenticate.
	Authentication ProxyAuthenticationSpec `json:"authentication"`

	// Proxy container resources.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ProxyAuthenticationMode selects how humans authenticate to the proxy.
// +kubebuilder:validation:Enum=openshift;oidc;local
type ProxyAuthenticationMode string

const (
	// ProxyAuthenticationModeOpenShift uses the integrated OpenShift OAuth
	// stack, auto-discovered from the cluster.
	ProxyAuthenticationModeOpenShift ProxyAuthenticationMode = "openshift"

	// ProxyAuthenticationModeOIDC speaks plain OIDC to an external identity
	// provider.
	ProxyAuthenticationModeOIDC ProxyAuthenticationMode = "oidc"

	// ProxyAuthenticationModeLocal authenticates against bcrypt credentials
	// rendered by the operator from the console's PgToolBoxUser set.
	ProxyAuthenticationModeLocal ProxyAuthenticationMode = "local"
)

// ProxyAuthenticationSpec configures proxy authentication as a discriminated
// union on mode: the oidc block is required exactly when mode is oidc.
// The openshift and local modes take no configuration: openshift is
// auto-discovered from the cluster, and local users come only from the
// console's PgToolBoxUser resources.
// +kubebuilder:validation:XValidation:rule="self.mode == 'oidc' ? has(self.oidc) : true",message="oidc is required when mode is oidc"
// +kubebuilder:validation:XValidation:rule="self.mode == 'oidc' || !has(self.oidc)",message="oidc may only be set when mode is oidc"
type ProxyAuthenticationSpec struct {
	// The authentication mode.
	Mode ProxyAuthenticationMode `json:"mode"`

	// Configuration for the oidc mode.
	// +optional
	OIDC *ProxyOIDCSpec `json:"oidc,omitempty"`
}

// ProxyOIDCSpec configures the oidc authentication mode.
type ProxyOIDCSpec struct {
	// The OIDC issuer URL. Must use the https scheme.
	// +kubebuilder:validation:Pattern=`^https://.+$`
	IssuerURL string `json:"issuerURL"`

	// The OAuth2 client ID.
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`

	// Reference to the Secret holding the client secret. Default key:
	// "clientSecret".
	ClientSecretRef SecretKeyReference `json:"clientSecretRef"`
}

// PgAdminSpec configures the embedded pgAdmin container.
type PgAdminSpec struct {
	// Whether pgAdmin is composed into the console Pod.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// The pgAdmin container image. Defaults to the operator's configured
	// image (--default-pgadmin-image).
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// The minimum role level allowed to reach pgAdmin through the proxy.
	// +kubebuilder:validation:Enum=dba;poweruser
	// +kubebuilder:default=dba
	// +optional
	AccessMinLevel string `json:"accessMinLevel,omitempty"`

	// The settings database storage.
	// +optional
	Storage PgAdminStorageSpec `json:"storage,omitempty"`

	// pgAdmin container resources.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PgAdminStorageSpec configures the PVC backing pgAdmin's settings
// database. The settings DB is written only through the operator's sync
// mechanism, never rendered-and-replaced wholesale.
type PgAdminStorageSpec struct {
	// Requested PVC size. Expansion-only: shrinking is rejected.
	// Defaults to the operator's configured default settings size.
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// StorageClass for the PVC; cluster default when empty.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
}

// EvidenceSpec configures the ObjectStoreViewer evidence sidecar.
type EvidenceSpec struct {
	// Whether the sidecar is composed into the console Pod.
	// +kubebuilder:default=false
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// The viewer container image. A pointer so that an omitted image is
	// genuinely absent rather than an empty object failing ImageSpec's
	// required fields. Defaults to the operator's configured image
	// (--default-objectstoreviewer-image); without either, no sidecar is
	// composed.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// Viewer container resources.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PgConsoleStatus is the observed state of a PgConsole.
type PgConsoleStatus struct {
	// The generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// The external URL of the console, when exposed.
	// +optional
	URL string `json:"url,omitempty"`

	// The checksum of the rendered proxy configuration, reported so a
	// pending rollout is visible without reading the configuration Secret.
	// +optional
	ConfigRevision string `json:"configRevision,omitempty"`

	// User provisioning counters across the PgToolBoxUser set.
	// +optional
	UserSync UserSyncStatus `json:"userSync,omitempty"`

	// The evidence sidecar's resolved state.
	// +optional
	Evidence EvidenceStatus `json:"evidence,omitempty"`

	// Standard conditions: Ready, Progressing, ConfigurationValid,
	// RouteReady, ProxyConfigReady, RepositoryEvidenceReady.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// UserSyncStatus counts PgToolBoxUser provisioning outcomes.
type UserSyncStatus struct {
	// +optional
	Desired int32 `json:"desired,omitempty"`
	// +optional
	Synced int32 `json:"synced,omitempty"`
	// +optional
	Degraded int32 `json:"degraded,omitempty"`
}

// EvidenceStatus is the observed state of the evidence sidecar composition.
type EvidenceStatus struct {
	// Whether the sidecar is composed into the console Pod.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// The active pod-local token Secret. Rotation creates a successor
	// under a new name and updates this field.
	// +optional
	TokenSecretName string `json:"tokenSecretName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgc
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cnpgClusterRef.name`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.configRevision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PgConsole describes one pgToolBox console managed by the operator.
type PgConsole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PgConsoleSpec `json:"spec"`
	// +optional
	Status PgConsoleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PgConsoleList contains a list of PgConsole resources.
type PgConsoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PgConsole `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PgConsole{}, &PgConsoleList{})
}
