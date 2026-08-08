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

	// The pgconsole application's own behaviour: which screens and day-2
	// operations it serves, what it retains, and the link-outs it offers.
	// +optional
	Console ConsoleSpec `json:"console,omitempty"`

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

// ConsoleSpec configures the pgconsole container itself. Every field maps
// to one of the application's configuration variables, and every default
// here reproduces the application's own, so an omitted block deploys the
// console the application ships.
//
// Two properties are load-bearing. First, the application validates its
// configuration at startup and refuses to serve on a value it rejects, so
// the bounds this API accepts are exactly the bounds the application
// accepts — a PgConsole that admission accepts cannot produce a Pod that
// crash-loops on its own configuration. Second, enabling a capability
// grants the matching rules in the generated Roles and nothing more: the
// console never widens its own authority, and a disabled capability is
// denied by RBAC as well as by the flag.
//
// Deliberately absent: the application's HISTORY_PATH and METRICS_PATH
// journals. Both imply a PersistentVolumeClaim and pin the console to a
// single replica, which is a storage decision this API does not yet make;
// history and metrics are retained in memory and live with the process.
// +kubebuilder:validation:XValidation:rule="!has(self.monitoringURL) || self.monitoringURL.startsWith('https://') || (has(self.allowInsecureLinks) && self.allowInsecureLinks)",message="monitoringURL must use https unless allowInsecureLinks is true"
type ConsoleSpec struct {
	// Whether the four enumerated day-2 operations — request backup,
	// reload, rolling restart and promote — are served. Disabling both
	// removes the write surface from the application and drops the
	// operate Role from the deployment, so RBAC denies the mutation
	// independently of the flag.
	// +kubebuilder:default=true
	// +optional
	AllowOperations *bool `json:"allowOperations,omitempty"`

	// Whether the bounded instance log tail is served. Instance logs can
	// contain query text; disabling removes the screen and the pods/log
	// rule from the generated read Role.
	// +kubebuilder:default=true
	// +optional
	AllowLogs *bool `json:"allowLogs,omitempty"`

	// Whether the dba access-request review panel is served. With it
	// disabled the PgToolBoxAccessRequests the proxy's 403 page creates
	// have no reviewer UI, and the console holds no authority over them.
	// +kubebuilder:default=true
	// +optional
	AllowAccessReview *bool `json:"allowAccessReview,omitempty"`

	// Whether the console may read the one cluster-scoped
	// ClusterImageCatalog its Cluster references. This is the only
	// authority a console holds outside its own namespace, so it is
	// opt-in and generates a separate ClusterRole granting exactly get —
	// never list, never watch. Declining costs nothing but a panel that
	// reports the catalog content as unread, never as absent.
	// +kubebuilder:default=false
	// +optional
	AllowClusterCatalogs *bool `json:"allowClusterCatalogs,omitempty"`

	// Whether http link-out URLs are accepted. For lab use: the console
	// rejects an http monitoringURL without it, and a console with no
	// exposure hostname has no https base URL from which to build the
	// pgAdmin link-out.
	// +kubebuilder:default=false
	// +optional
	AllowInsecureLinks *bool `json:"allowInsecureLinks,omitempty"`

	// Link-out base URL to the monitoring dashboard for this cluster.
	// Must use https unless allowInsecureLinks is true.
	// +kubebuilder:validation:Pattern=`^https?://.+$`
	// +optional
	MonitoringURL string `json:"monitoringURL,omitempty"`

	// Timeout applied to every Kubernetes API request the console makes.
	// Between 1s and 1m; defaults to 10s.
	// +optional
	APIRequestTimeout *metav1.Duration `json:"apiRequestTimeout,omitempty"`

	// How far back the cluster's Events are shown. Between 1m and 24h;
	// defaults to 1h.
	// +optional
	EventsMaxAge *metav1.Duration `json:"eventsMaxAge,omitempty"`

	// Bounds applied to a single log tail request.
	// +optional
	LogTail ConsoleLogTailSpec `json:"logTail,omitempty"`

	// The bounded in-memory metrics window behind the metrics screens.
	// +optional
	Metrics ConsoleMetricsSpec `json:"metrics,omitempty"`

	// The bounded in-memory revision history of the watched object
	// definitions.
	// +optional
	History ConsoleHistorySpec `json:"history,omitempty"`
}

// ConsoleLogTailSpec bounds one log tail request. Tails are fetched on
// demand and never persisted; these bounds cap a single response.
type ConsoleLogTailSpec struct {
	// Maximum lines returned per request. Defaults to 200.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2000
	// +optional
	Lines *int32 `json:"lines,omitempty"`

	// Maximum bytes returned per request. Defaults to 1048576 (1Mi).
	// +kubebuilder:validation:Minimum=4096
	// +kubebuilder:validation:Maximum=8388608
	// +optional
	MaxBytes *int32 `json:"maxBytes,omitempty"`
}

// ConsoleMetricsSpec configures the console's in-memory metrics window.
type ConsoleMetricsSpec struct {
	// Whether the metrics screens are served.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Sampling interval. Between 5s and 5m; defaults to 10s.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// How long samples are retained. Between 1h and 720h (30d); defaults
	// to 168h (7d).
	// +optional
	Retention *metav1.Duration `json:"retention,omitempty"`
}

// ConsoleHistorySpec configures the console's in-memory object definition
// history. Retention is bounded on three axes so a busy namespace cannot
// grow the console's memory without limit.
type ConsoleHistorySpec struct {
	// Whether history is recorded at all.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Global cap on retained revisions. Defaults to 2000.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=20000
	// +optional
	MaxRevisions *int32 `json:"maxRevisions,omitempty"`

	// Global cap on retained manifest bytes. Defaults to 16777216 (16Mi).
	// +kubebuilder:validation:Minimum=1048576
	// +kubebuilder:validation:Maximum=67108864
	// +optional
	MaxBytes *int32 `json:"maxBytes,omitempty"`

	// Cap on retained revisions of any one object. Defaults to 20.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=200
	// +optional
	PerObjectRevisions *int32 `json:"perObjectRevisions,omitempty"`

	// Window within which repeated changes to one object collapse into a
	// single revision. Between 1s and 1h; defaults to 1m.
	// +optional
	CoalesceWindow *metav1.Duration `json:"coalesceWindow,omitempty"`
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
