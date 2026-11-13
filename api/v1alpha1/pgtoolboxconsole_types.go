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
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PgToolBoxConsole-specific reasons of the frozen catalogue, registered from
// pgtoolboxconsole_funcs.go.
const (
	// ReasonConsoleImageRequired: spec.image is unset and the operator has
	// no default console image.
	ReasonConsoleImageRequired Reason = "ConsoleImageRequired"
	// ReasonConsoleRoleMissing: the ClusterRole the console's RoleBinding
	// references (pgtoolbox-console, shipped with the operator) does not
	// exist, so the console could read nothing.
	ReasonConsoleRoleMissing Reason = "ConsoleRoleMissing"
	// ReasonInstanceCAMissing: InstanceCABundleComplete=False, at least
	// one selected Cluster's server CA is not in the mirrored bundle;
	// status.instanceCABundle.missing says which and why.
	ReasonInstanceCAMissing Reason = "InstanceCAMissing"
	// ReasonInstanceCAInvalid: a Cluster's server CA Secret has a ca.crt
	// that holds no PEM certificate.
	ReasonInstanceCAInvalid Reason = "InstanceCAInvalid"
	// ReasonInstanceCABundleFull: the mirrored bundle reached the size a
	// ConfigMap can hold; the Clusters after it (by name) are left out.
	ReasonInstanceCABundleFull Reason = "InstanceCABundleFull"
	// ReasonExposureOpen: ExposureConfined=False, the console is exposed
	// without its ingress NetworkPolicy, so any Pod can reach its port.
	ReasonExposureOpen Reason = "ExposureOpen"
	// ReasonExposurePeersBroad: ExposureConfined=False, a networkPolicy.from
	// peer admits (nearly) every namespace (a namespaceSelector that pins
	// no label value), every Pod of the console's namespace (such a
	// podSelector alone), or the Pod network or every address (an ipBlock
	// wider than /8 or /32, holding a whole private range, a console
	// Pod's address, or overlapping a Node's podCIDRs): its peers are not the proxies, so the console trusts
	// none of them with X-Forwarded-For.
	ReasonExposurePeersBroad Reason = "ExposurePeersBroad"
	// ReasonExposurePolicyBypassed: ExposureConfined=False, another
	// NetworkPolicy of the namespace admits peers the exposure does not
	// list to the console port (NetworkPolicies add up), or there were too
	// many to check: the console trusts none of the peers with
	// X-Forwarded-For.
	ReasonExposurePolicyBypassed Reason = "ExposurePolicyBypassed"
	// ReasonExposureTrustUnverified: ExposureConfined=False, the
	// networkPolicy.from peers could not be checked yet, or are held back:
	// an ipBlock peer while no console Pod has an address to check it
	// against and no earlier one verified the same blocks, more console Pods than one bounded list returns, or a
	// widened from that waits until no console Pod trusts every peer any
	// more. The console trusts none of the peers with X-Forwarded-For
	// meanwhile.
	ReasonExposureTrustUnverified Reason = "ExposureTrustUnverified"
	// ReasonExposurePolicyKept: ExposureConfined=False, the ingress
	// NetworkPolicy is no longer rendered (the exposure or the policy was
	// turned off) and is kept while a console Pod may still trust every
	// peer it admits on the operator's grant: until the rollout without
	// the grant is through. The message names a Pod stuck terminating
	// that holds it. Also while the console is stalled on
	// HostnameNotAllowed: its Ingress, Route or HTTPRoute is withdrawn,
	// and the workload and the policy stay as they were.
	ReasonExposurePolicyKept Reason = "ExposurePolicyKept"
	// ReasonExposurePolicyInvalid: ConfigurationValid=False and
	// ExposureConfined=False, the API server refused the ingress
	// NetworkPolicy rendered from spec.exposure.networkPolicy (a selector
	// or block the CRD could not check), or an admission policy or webhook
	// denied it. The reconcile stalls before anything new is published.
	// Looked at again every five minutes and on every spec change.
	ReasonExposurePolicyInvalid Reason = "ExposurePolicyInvalid"
)

// ConditionExposureConfined is advisory and gates nothing (it never enters
// Ready): present only while an exposure object is published. True when
// the operator renders the ingress NetworkPolicy that admits only the
// router, ingress controller or gateway to the console port and nothing
// else in the namespace admits more; False with ExposureOpen when it is
// not rendered (ingress or gateway exposure without networkPolicy.from, or
// networkPolicy.enabled=false), ExposurePeersBroad when a peer of from
// admits (nearly) everyone, ExposurePolicyBypassed when another
// NetworkPolicy does, ExposureTrustUnverified while the peers could not be
// checked yet or a widened from is held back, ExposurePolicyKept while a
// policy no longer rendered is kept for a Pod that may trust every peer,
// ExposurePolicyInvalid while the API server refuses the policy. Removed when nothing is published and no policy is kept, the hostname
// stall included. The
// message says which peers the console believes about X-Forwarded-For.
// Whether the CNI enforces NetworkPolicies cannot be observed: True says
// what is declared, not what the network does.
const ConditionExposureConfined = "ExposureConfined"

// ConditionInstanceCABundleComplete is advisory and gates nothing (it never
// enters Ready): present only while the operator mirrors the instance CA
// bundle (spec.instanceCABundleRef omitted). True when the server CA of
// every selected Cluster is in the bundle; False with InstanceCAMissing
// when one is not, or with ResourceNotOwned when a ConfigMap that is not
// this console's already has the bundle's name. The console shows a
// Cluster missing from the bundle as "not verifiable", never reads it
// unverified.
const ConditionInstanceCABundleComplete = "InstanceCABundleComplete"

// PgToolBoxConsoleExposureSpec is the console's exposure: the common
// ExposureSpec, plus who the console believes about the client's address
// and who may reach its port.
// +kubebuilder:validation:XValidation:rule="!has(self.networkPolicy) || !has(self.networkPolicy.enabled) || !self.networkPolicy.enabled || (has(self.type) && self.type == 'route') || (has(self.networkPolicy.from) && size(self.networkPolicy.from) > 0)",message="networkPolicy.enabled needs networkPolicy.from for ingress and gateway exposure: the controller's namespace is not known",fieldPath=".networkPolicy.from",reason=FieldValueRequired
type PgToolBoxConsoleExposureSpec struct {
	ExposureSpec `json:",inline"`

	// The reverse proxies in front of the console: how many append to
	// X-Forwarded-For and which peers they are (TRUSTED_PROXY_HOPS,
	// TRUSTED_PROXY_CIDRS). The console reads the header only from a peer
	// it trusts, so the per-address sign-in budgets and the audit trail's
	// client_ip follow each client. Omitted: one hop when an exposure
	// object is published; trusted peers per networkPolicy.
	// +optional
	TrustedProxy *PgToolBoxConsoleTrustedProxySpec `json:"trustedProxy,omitempty"`

	// The ingress NetworkPolicy around the console Pods: only the router,
	// ingress controller or gateway reaches the console port. Omitted: on
	// for route exposure (the OpenShift router namespaces carry
	// policy-group.network.openshift.io/ingress), and for ingress and
	// gateway exposure only once from names the controller's namespace;
	// ExposureConfined says which. Never rendered for clusterIP. Every
	// in-cluster client of the Service's http port (a debug Pod, an
	// in-cluster MCP client) is then refused unless from lists it, and is
	// trusted with X-Forwarded-For once listed.
	// +optional
	NetworkPolicy *PgToolBoxConsoleExposurePolicySpec `json:"networkPolicy,omitempty"`
}

// PgToolBoxConsoleTrustedProxySpec says which X-Forwarded-For entries the
// console believes.
type PgToolBoxConsoleTrustedProxySpec struct {
	// How many proxies append to X-Forwarded-For: 2 for a load balancer
	// that appends in front of the router. 0 ignores the header. Omitted:
	// 1 when an exposure object is published, else 0.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8
	// +optional
	Hops *int32 `json:"hops,omitempty"`

	// The proxies' own addresses, as the console sees them (the router or
	// ingress controller Pods; the nodes for a host-network router). The
	// header is read only from a peer inside one. Omitted: with the
	// NetworkPolicy rendered, any peer it admits (every other Pod is
	// refused by the network); without it, none, and the header is
	// ignored: every client behind the proxy then shares its address, and
	// no per-address sign-in budget applies (only the per-flow, per-token
	// and per-replica bounds).
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=49
	// +kubebuilder:validation:items:XValidation:rule="isCIDR(self)",message="trustedProxy.cidrs entries must be CIDRs",reason=FieldValueInvalid
	// +listType=set
	// +optional
	CIDRs []string `json:"cidrs,omitempty"`
}

// PgToolBoxConsoleExposurePolicySpec configures the console's ingress
// NetworkPolicy.
type PgToolBoxConsoleExposurePolicySpec struct {
	// Whether the operator renders it. Omitted: true for route exposure,
	// and for ingress and gateway exposure when from is set.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Who may reach the console port: the router, ingress controller or
	// gateway Pods. For route exposure the namespaces labelled
	// policy-group.network.openshift.io/ingress (the OpenShift routers,
	// host-network ones included) are always admitted, and from adds to
	// them. Required for ingress and gateway. Every peer listed is trusted
	// to write X-Forwarded-For unless trustedProxy.cidrs narrows it; a peer
	// that admits (nearly) everyone (a namespaceSelector, or a podSelector
	// alone, that pins no label value; a namespaceSelector that pins only
	// labels many namespaces share, pod-security.kubernetes.io/*; an
	// ipBlock wider than /8 or /32, holding a whole private IPv4 range, or
	// a unique-local IPv6 block wider than /64) turns that trust off, and
	// an ipBlock peer is trusted only once a console Pod has an address to
	// check it against. A wider from is applied only once no console Pod
	// trusts every peer any more. The probe
	// port (health and /metrics) stays open to scrapers; it never reads the
	// header.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	From []PgToolBoxConsolePolicyPeer `json:"from,omitempty"`
}

// PgToolBoxConsolePolicyPeer is a NetworkPolicyPeer, bounded so that the
// API server checks it here rather than refuse the policy the operator
// renders from it.
// +kubebuilder:validation:XValidation:rule="has(self.podSelector) || has(self.namespaceSelector) || has(self.ipBlock)",message="each networkPolicy.from peer needs podSelector, namespaceSelector or ipBlock",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="!has(self.ipBlock) || (!has(self.podSelector) && !has(self.namespaceSelector))",message="a networkPolicy.from peer with ipBlock may not also have podSelector or namespaceSelector",reason=FieldValueForbidden
type PgToolBoxConsolePolicyPeer struct {
	// Pods (of this namespace, or of the namespaces namespaceSelector
	// selects). Pinning no label value (no matchLabels, no In) with no
	// namespaceSelector: (nearly) every Pod of this namespace, which
	// ExposureConfined reports as broad.
	// +optional
	PodSelector *PgToolBoxConsoleLabelSelector `json:"podSelector,omitempty"`

	// Namespaces. Pinning no label value (no matchLabels, no In), or only
	// labels many namespaces share (pod-security.kubernetes.io/*,
	// security.openshift.io/*, openshift.io/run-level): (nearly) every
	// namespace, which ExposureConfined reports as broad. A label any
	// namespace owner may set on their own namespace is not told apart:
	// pin one only an installer can set (kubernetes.io/metadata.name).
	// +optional
	NamespaceSelector *PgToolBoxConsoleLabelSelector `json:"namespaceSelector,omitempty"`

	// Addresses: a CIDR, less the except CIDRs inside it. Alone in its
	// peer. Wider than /8 (IPv4) or /32 (IPv6), holding a whole private
	// IPv4 range, a unique-local (fc00::/7) block wider than /64, or one
	// holding a console Pod's address, is broad.
	// +optional
	IPBlock *PgToolBoxConsoleIPBlock `json:"ipBlock,omitempty"`
}

// PgToolBoxConsoleLabelSelector is a label selector, bounded and checked
// here so that the API server never refuses the NetworkPolicy rendered from
// it: label keys and values in their syntax, operators from the four the
// API knows, values only (and always) for In and NotIn.
// +kubebuilder:validation:XValidation:rule="!has(self.matchLabels) || self.matchLabels.all(k, k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$'))",message="matchLabels keys must be label keys ([prefix/]name, name at most 63 characters)",fieldPath=".matchLabels",reason=FieldValueInvalid
type PgToolBoxConsoleLabelSelector struct {
	// Labels the object must carry, with these values.
	// +kubebuilder:validation:MaxProperties=16
	// +optional
	MatchLabels map[string]PgToolBoxConsoleLabelValue `json:"matchLabels,omitempty"`

	// Requirements the object's labels must meet, all of them.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	MatchExpressions []PgToolBoxConsoleLabelRequirement `json:"matchExpressions,omitempty"`
}

// PgToolBoxConsoleLabelValue is a label value: at most 63 characters,
// alphanumeric at both ends, '-', '_' and '.' inside.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type PgToolBoxConsoleLabelValue string

// PgToolBoxConsoleLabelRequirement is one matchExpressions entry.
// +kubebuilder:validation:XValidation:rule="self.operator in ['In', 'NotIn'] ? (has(self.values) && size(self.values) > 0) : (!has(self.values) || size(self.values) == 0)",message="values are required for In and NotIn and forbidden for Exists and DoesNotExist",reason=FieldValueInvalid
type PgToolBoxConsoleLabelRequirement struct {
	// The label key ([prefix/]name).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`
	Key string `json:"key"`

	// In, NotIn, Exists or DoesNotExist.
	// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist
	Operator metav1.LabelSelectorOperator `json:"operator"`

	// The values, for In and NotIn.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Values []PgToolBoxConsoleLabelValue `json:"values,omitempty"`
}

// PgToolBoxConsoleIPBlock is a NetworkPolicy ipBlock.
// +kubebuilder:validation:XValidation:rule="!has(self.except) || (isCIDR(self.cidr) && self.except.all(e, isCIDR(e) && cidr(self.cidr).containsCIDR(e) && cidr(e).prefixLength() > cidr(self.cidr).prefixLength()))",message="networkPolicy.from ipBlock.except entries must be CIDRs strictly inside ipBlock.cidr",fieldPath=".except",reason=FieldValueInvalid
type PgToolBoxConsoleIPBlock struct {
	// The block, as a CIDR (10.0.0.0/8, fd00::/64).
	// +kubebuilder:validation:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="networkPolicy.from ipBlock.cidr must be a CIDR",reason=FieldValueInvalid
	CIDR string `json:"cidr"`

	// CIDRs inside cidr that are not admitted.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=49
	// +kubebuilder:validation:items:XValidation:rule="isCIDR(self)",message="networkPolicy.from ipBlock.except entries must be CIDRs",reason=FieldValueInvalid
	// +listType=atomic
	// +optional
	Except []string `json:"except,omitempty"`
}

// PgToolBoxConsoleAuthProvider selects how people sign in to the console.
// +kubebuilder:validation:Enum=token;oidc;openshift
type PgToolBoxConsoleAuthProvider string

const (
	// PgToolBoxConsoleAuthToken signs in with an API-server token the user pastes
	// (break-glass; works everywhere).
	PgToolBoxConsoleAuthToken PgToolBoxConsoleAuthProvider = "token"
	// PgToolBoxConsoleAuthOIDC signs in through the code flow of an IdP the API
	// server also trusts, so the token is an API-server token.
	PgToolBoxConsoleAuthOIDC PgToolBoxConsoleAuthProvider = "oidc"
	// PgToolBoxConsoleAuthOpenShift signs in through the OpenShift OAuth code flow,
	// with the console's ServiceAccount as the OAuth client.
	PgToolBoxConsoleAuthOpenShift PgToolBoxConsoleAuthProvider = "openshift"
)

// PgToolBoxConsoleSpec declares one console in this namespace: the console the
// chart installs, run and kept converged by the operator. It watches the
// CloudNativePG Clusters of its own namespace only; every reader still sees
// only what their own RBAC allows (SubjectAccessReview).
// +kubebuilder:validation:XValidation:rule="self.auth.provider != 'oidc' || has(self.auth.oidc)",message="auth.oidc is required with auth.provider=oidc",fieldPath=".auth.oidc",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="self.auth.provider != 'openshift' || (has(self.publicURL) && size(self.publicURL) > 0) || (has(self.exposure.hostname) && size(self.exposure.hostname) > 0)",message="auth.provider=openshift needs publicURL or exposure.hostname: OpenShift redirects only to the callback the ServiceAccount lists",fieldPath=".publicURL",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="!has(self.mcp) || !has(self.mcp.enabled) || !self.mcp.enabled || (has(self.publicURL) && size(self.publicURL) > 0) || (has(self.exposure.hostname) && size(self.exposure.hostname) > 0)",message="mcp needs publicURL or exposure.hostname: the MCP endpoint advertises only its configured origin",fieldPath=".mcp",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="!has(self.restoreDrills) || !has(self.restoreDrills.enabled) || !self.restoreDrills.enabled || !has(self.operations) || self.operations",message="restoreDrills needs operations: a drill is started through the operations endpoints",fieldPath=".restoreDrills",reason=FieldValueInvalid
// +kubebuilder:validation:XValidation:rule="!has(self.prometheus) || !has(self.prometheus.tokenSecretRef) || !has(self.prometheus.projectedToken) || !self.prometheus.projectedToken",message="prometheus: set tokenSecretRef or projectedToken, not both",fieldPath=".prometheus",reason=FieldValueInvalid
// +kubebuilder:validation:XValidation:rule="!has(self.digest) || !has(self.digest.delivery) || !has(self.history) || !has(self.history.persistent) || self.history.persistent",message="digest.delivery needs history.persistent: a delivered digest links to its page, which in-memory history loses at the next restart",fieldPath=".digest.delivery",reason=FieldValueInvalid
// +kubebuilder:validation:XValidation:rule="!has(self.digest) || !has(self.digest.delivery) || !has(self.digest.enabled) || self.digest.enabled",message="digest.delivery needs digest.enabled",fieldPath=".digest.enabled",reason=FieldValueInvalid
type PgToolBoxConsoleSpec struct {
	// The console image. Omitted, the operator's default applies (the
	// console released with the operator). The repository must be in the
	// operator's allow-list (--console-image-repositories; by default the
	// default image's repository only).
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// Replicas of the console, with or without persistent history. Each
	// replica keeps its own history; the exposure keeps a person on one
	// (cookie affinity).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +kubebuilder:default=2
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Narrows the Clusters shown to those this selector matches, on top of
	// what each user's own RBAC allows. Omitted: every Cluster of the
	// namespace.
	// +optional
	ClusterSelector *metav1.LabelSelector `json:"clusterSelector,omitempty"`

	// How people sign in.
	// +kubebuilder:default={provider: token}
	// +optional
	Auth PgToolBoxConsoleAuthSpec `json:"auth,omitempty"`

	// The URL people type, https://host. Used for cookies, redirects and the
	// OpenShift callback. Defaults to https://<exposure.hostname>.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]{1,5})?/?$`
	// +optional
	PublicURL string `json:"publicURL,omitempty"`

	// Exposure of the console. The console streams events over SSE; the
	// operator sets the long edge timeouts it knows for each type.
	// +kubebuilder:default={type: clusterIP}
	// +optional
	Exposure PgToolBoxConsoleExposureSpec `json:"exposure,omitempty"`

	// The CA bundle the console verifies each instance's /pg/status with:
	// a ConfigMap with the key ca.crt holding the PEM server CAs of the
	// namespace's Clusters. Omitted (recommended): the operator keeps
	// <name>-instance-ca itself, copying only the public ca.crt of each
	// selected Cluster's server CA Secret, and follows CloudNativePG's CA
	// renewals and Clusters coming and going; the console still reads no
	// Secret. Set: the console mounts the named ConfigMap, which you keep
	// current yourself (CloudNativePG renews its self-signed CA every 90
	// days, so a one-off copy expires). A Cluster missing from the bundle
	// is shown as "not verifiable", never read unverified.
	// +optional
	InstanceCABundleRef *LocalObjectReference `json:"instanceCABundleRef,omitempty"`

	// The end-of-life table the diagnostics read. Omitted, the operator
	// creates <console>-lifecycle with its built-in table and keeps it at
	// the operator's table until someone edits it (or annotates it
	// pgtoolbox.fyannk.dev/user-managed=true); from then on it is left
	// alone. Set, the console reads that ConfigMap (key lifecycle.json) and
	// the operator creates none.
	// +optional
	LifecycleConfigMapRef *LocalObjectReference `json:"lifecycleConfigMapRef,omitempty"`

	// Repository evidence from the namespace's EvidenceAgents.
	// +optional
	Evidence PgToolBoxConsoleEvidenceSpec `json:"evidence,omitempty"`

	// The operations endpoints (backup, reload, restart, promote). Every
	// write is made with the caller's own token and authorized by the API
	// server. Default true.
	// +optional
	Operations *bool `json:"operations,omitempty"`

	// Pod logs: the bounded log tail and the log-based diagnostics, which
	// need `get pods/log`. Default true.
	// +optional
	PodLogs *bool `json:"podLogs,omitempty"`

	// Restore drills: the "Run a restore drill" operation of the Backups
	// screen. A person starts a drill with their own token; the console
	// restores the latest backup into a throwaway one-instance Cluster in
	// this namespace (no WAL archiver, no backup configuration), checks it
	// without SQL and deletes it after its check or the time limit. Enabled,
	// the operator also binds the console's ServiceAccount to
	// pgtoolbox-console-restore-drills (get, list, watch and delete
	// clusters, in this namespace only). Needs operations.
	// +optional
	RestoreDrills *PgToolBoxConsoleRestoreDrillsSpec `json:"restoreDrills,omitempty"`

	// The read-only MCP endpoint at /mcp, for AI agents and IDE
	// assistants. Off by default.
	// +optional
	MCP *PgToolBoxConsoleMCPSpec `json:"mcp,omitempty"`

	// OpenTelemetry tracing: spans for HTTP requests, the Kubernetes API
	// calls they make and MCP tool calls, exported over OTLP/HTTP. Off
	// when omitted.
	// +optional
	Tracing *PgToolBoxConsoleTracingSpec `json:"tracing,omitempty"`

	// The link-out to the site's monitoring system: https, or
	// root-relative.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self.startsWith('https://') || (self.startsWith('/') && !self.startsWith('//'))",message="monitoringURL must be https or root-relative",reason=FieldValueInvalid
	// +optional
	MonitoringURL string `json:"monitoringURL,omitempty"`

	// The PromQL source of the metrics charts. Omitted, the charts come
	// from the console's own ring.
	// +optional
	Prometheus *PgToolBoxConsolePrometheusSpec `json:"prometheus,omitempty"`

	// The history journal, and with it the state snapshots, the timeline
	// and the digests. Persistent by default: the console is then a
	// StatefulSet with one volume per replica.
	// +kubebuilder:default={persistent: true}
	// +optional
	History PgToolBoxConsoleHistorySpec `json:"history,omitempty"`

	// The daily digest: a summary page per day, kept with the history, and
	// optional delivery to chat, a webhook or mail. Delivery needs
	// history.persistent (the message links to the digest's page); only the
	// replica holding the Lease <console>-digest delivers.
	// +optional
	Digest PgToolBoxConsoleDigestSpec `json:"digest,omitempty"`

	// Compute resources of the console container. Omitted: requests 50m and
	// 128Mi, limits 1 CPU and 512Mi (the chart's).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Labels and annotations copied onto the generated objects.
	// +kubebuilder:validation:MaxItems=8
	// +optional
	MetadataPropagation []MetadataPropagation `json:"metadataPropagation,omitempty"`
}

// PgToolBoxConsoleAuthSpec configures sign-in.
// +kubebuilder:validation:XValidation:rule="!has(self.sessionIdleTimeout) || !has(self.sessionMaxAge) || duration(self.sessionIdleTimeout) <= duration(self.sessionMaxAge)",message="sessionIdleTimeout must not exceed sessionMaxAge",fieldPath=".sessionIdleTimeout",reason=FieldValueInvalid
type PgToolBoxConsoleAuthSpec struct {
	// The sign-in provider.
	// +kubebuilder:default=token
	// +optional
	Provider PgToolBoxConsoleAuthProvider `json:"provider,omitempty"`

	// OpenID Connect, for provider oidc.
	// +optional
	OIDC *PgToolBoxConsoleOIDCSpec `json:"oidc,omitempty"`

	// OpenShift OAuth, for provider openshift.
	// +optional
	OpenShift *PgToolBoxConsoleOpenShiftSpec `json:"openshift,omitempty"`

	// Shows the break-glass form (paste an API-server token) next to SSO.
	// Default true.
	// +optional
	TokenLogin *bool `json:"tokenLogin,omitempty"`

	// Idle timeout of a session. Default 1h.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('12h')",message="sessionIdleTimeout must be between 1m and 12h",reason=FieldValueInvalid
	// +optional
	SessionIdleTimeout *metav1.Duration `json:"sessionIdleTimeout,omitempty"`

	// Maximum age of a session whatever its activity. Default 12h.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('5m') && duration(self) <= duration('168h')",message="sessionMaxAge must be between 5m and 168h",reason=FieldValueInvalid
	// +optional
	SessionMaxAge *metav1.Duration `json:"sessionMaxAge,omitempty"`
}

// PgToolBoxConsoleOIDCSpec is the OpenID Connect code flow. Groups come from the
// API server (SelfSubjectReview), never from a claim.
// +kubebuilder:validation:XValidation:rule="!has(self.grantClient) || self.grantClient.clientID != self.clientID",message="oidc.grantClient.clientID must differ from oidc.clientID",fieldPath=".grantClient.clientID",reason=FieldValueInvalid
type PgToolBoxConsoleOIDCSpec struct {
	// The issuer, https only.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://`
	IssuerURL string `json:"issuerURL"`

	// The client id.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ClientID string `json:"clientID"`

	// The Secret key holding the client secret, mounted as a file.
	ClientSecretRef SecretKeySelector `json:"clientSecretRef"`

	// The callback URL. Defaults to <publicURL>/auth/oidc/callback.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://`
	// +optional
	RedirectURL string `json:"redirectURL,omitempty"`

	// Scopes requested. Default: openid, email, profile, offline_access.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=128
	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// A ConfigMap with ca.crt trusting a private IdP. Omitted: the system
	// roots.
	// +optional
	CAConfigMapRef *LocalObjectReference `json:"caConfigMapRef,omitempty"`

	// Refuse a login whose ID token lacks email_verified=true. Default true;
	// set false only for an IdP that omits the claim.
	// +optional
	RequireEmailVerified *bool `json:"requireEmailVerified,omitempty"`

	// A second client of the same issuer for the identity provider sign-ins
	// of console grants (MCP clients), registered with the same redirect
	// URI. An identity provider that keeps one refresh token per person and
	// client (dex) otherwise ends the browser session's renewal each time an
	// assistant connects. The API server's JWT authenticator must accept
	// this client's audience too. Omitted: grants sign in as clientID.
	// +optional
	GrantClient *PgToolBoxConsoleOIDCClient `json:"grantClient,omitempty"`
}

// PgToolBoxConsoleOIDCClient is one more OIDC client of the console's issuer.
type PgToolBoxConsoleOIDCClient struct {
	// The client id.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ClientID string `json:"clientID"`

	// The Secret key holding the client secret, mounted as a file.
	ClientSecretRef SecretKeySelector `json:"clientSecretRef"`
}

// PgToolBoxConsoleOpenShiftSpec is the OpenShift OAuth code flow. The console's
// ServiceAccount is the OAuth client; its redirect annotation is rendered
// from the public URL.
type PgToolBoxConsoleOpenShiftSpec struct {
	// The Secret key holding a long-lived ServiceAccount token to present as
	// the client secret. Omitted: the console presents its projected token.
	// +optional
	ClientSecretRef *SecretKeyReference `json:"clientSecretRef,omitempty"`

	// A ConfigMap with ca.crt trusting the OAuth server. Omitted: the
	// service CA OpenShift publishes in every namespace.
	// +optional
	CAConfigMapRef *LocalObjectReference `json:"caConfigMapRef,omitempty"`
}

// SecretKeyReference names a key of a Secret in the referencing object's
// namespace; unlike SecretKeySelector, the key has no default.
type SecretKeyReference struct {
	// The Secret name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// The key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
}

// PgToolBoxConsoleEvidenceSpec configures the EvidenceAgent channel.
type PgToolBoxConsoleEvidenceSpec struct {
	// Read repository evidence from the namespace's EvidenceAgents. Default
	// true. Off, the backups screen says "disabled".
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// How often each agent is asked again. Omitted: the console's default.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('30s') && duration(self) <= duration('1h')",message="evidence.interval must be between 30s and 1h",reason=FieldValueInvalid
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
}

// PgToolBoxConsoleMCPSpec configures the MCP endpoint. A client authenticates
// with `Authorization: Bearer <token>`, never the session cookie: a console
// grant, which the console issues as the clients' OAuth authorization server
// once the person signs in and approves the client, or (while
// auth.tokenLogin is on) a raw API-server token. Every tool is authorized as
// its owner exactly like the screen it mirrors.
// +kubebuilder:validation:XValidation:rule="!has(self.writeTools) || !self.writeTools || (has(self.enabled) && self.enabled)",message="mcp.writeTools requires mcp.enabled",fieldPath=".writeTools",reason=FieldValueInvalid
type PgToolBoxConsoleMCPSpec struct {
	// Serve the read-only MCP endpoint. Default false.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Add the MCP write tools (backup, reload, restart, promote) when the
	// console serves operations. Each runs the console's own operation as
	// the person, once, after a confirmation (MCP elicitation): backup and
	// reload in the client's form, which the client relays (it proves the
	// client asked); restart and promote on a console page in the person's
	// own browser session (it proves the person approved). A client that
	// cannot ask is refused. A console grant acts only if consented for
	// "read and operate". Requires enabled. Default false.
	// +optional
	WriteTools bool `json:"writeTools,omitempty"`

	// The cap on one tool result's JSON, in bytes (MCP_MAX_RESULT_BYTES);
	// a larger result is shortened and says so. Omitted: the console's
	// default, 262144 (256 KiB). Used only with enabled.
	// +kubebuilder:validation:Minimum=16384
	// +kubebuilder:validation:Maximum=4194304
	// +optional
	MaxResultBytes *int32 `json:"maxResultBytes,omitempty"`

	// The time limit of one tool call (MCP_CALL_TIMEOUT). Omitted: the
	// console's default, 20s. Used only with enabled.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('50s')",message="mcp.callTimeout must be between 1s and 50s",reason=FieldValueInvalid
	// +optional
	CallTimeout *metav1.Duration `json:"callTimeout,omitempty"`

	// The lifetime of an access token the console issues to an MCP client
	// (MCP_OAUTH_ACCESS_TTL); the client renews it with its refresh token.
	// Omitted: the console's default, 1h. Used only with enabled.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s') && duration(self) <= duration('1h')",message="mcp.oauthAccessTTL must be between 10s and 1h",reason=FieldValueInvalid
	// +optional
	OAuthAccessTTL *metav1.Duration `json:"oauthAccessTTL,omitempty"`

	// Accept OAuth client ID metadata documents
	// (MCP_OAUTH_CLIENT_METADATA_DOCUMENTS): a client may name itself with
	// an HTTPS URL, and the console fetches the document at that URL, only
	// from a public address and, when oauthClientMetadataHosts is set, only
	// from those hosts. The console then needs egress to the Internet on
	// TCP 443; the operator renders no NetworkPolicy around the console, so
	// where egress is policed the site's policy must allow it. Dynamic
	// registration, always accepted, needs no egress. Used only with
	// enabled. Default false.
	// +optional
	OAuthClientMetadataDocuments bool `json:"oauthClientMetadataDocuments,omitempty"`

	// The only hosts (lower-case DNS names) the console fetches client ID
	// metadata documents from (MCP_OAUTH_CLIENT_METADATA_HOSTS). It takes
	// effect with oauthClientMetadataDocuments and is ignored without it.
	// Omitted: any public address. Used only with enabled.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$`
	// +listType=set
	// +optional
	OAuthClientMetadataHosts []string `json:"oauthClientMetadataHosts,omitempty"`
}

// PgToolBoxConsoleTracingSpec configures OpenTelemetry tracing (the console's
// OTEL_* variables). Spans carry no payload unless includePayloads is set;
// the egress to the collector is the site's NetworkPolicy decision.
// +kubebuilder:validation:XValidation:rule="!has(self.samplerArg) || (has(self.sampler) && self.sampler.endsWith('traceidratio'))",message="tracing.samplerArg needs a traceidratio sampler",fieldPath=".samplerArg",reason=FieldValueInvalid
type PgToolBoxConsoleTracingSpec struct {
	// The OTLP/HTTP base URL (OTEL_EXPORTER_OTLP_ENDPOINT); /v1/traces is
	// appended. For example http://jaeger-collector.monitoring:4318 or
	// https://api.smith.langchain.com/otel.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://[^\s/?#@]+(/[^\s?#]*)?$`
	Endpoint string `json:"endpoint"`

	// A Secret key holding the exporter's headers, `key=value,...` with
	// percent-encoded values (an API key, a project name), mounted as a
	// file and read at start: the console reads no Secret through the API.
	// +optional
	HeadersSecretRef *SecretKeyReference `json:"headersSecretRef,omitempty"`

	// The service name (OTEL_SERVICE_NAME). Default pgtoolbox-console.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][-._A-Za-z0-9]*$`
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// The sampler (OTEL_TRACES_SAMPLER). Default parentbased_always_on: a
	// request the caller traces is traced.
	// +kubebuilder:validation:Enum=always_on;always_off;traceidratio;parentbased_always_on;parentbased_always_off;parentbased_traceidratio
	// +optional
	Sampler string `json:"sampler,omitempty"`

	// The ratio of the traceidratio samplers (OTEL_TRACES_SAMPLER_ARG),
	// from "0" to "1".
	// +kubebuilder:validation:Pattern=`^(0(\.[0-9]{1,6})?|1(\.0{1,6})?)$`
	// +optional
	SamplerArg string `json:"samplerArg,omitempty"`

	// Add MCP tool arguments and results to the tool spans, redacted and
	// capped (TRACING_INCLUDE_PAYLOADS). Default false; tokens never.
	// +optional
	IncludePayloads bool `json:"includePayloads,omitempty"`
}

// PgToolBoxConsoleRestoreDrillsSpec configures restore drills. Drill Clusters run
// in the console's namespace, the only one it watches.
type PgToolBoxConsoleRestoreDrillsSpec struct {
	// Offer the drill. Default false.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// The drill Clusters' name prefix. Omitted: drill-.
	// +kubebuilder:validation:MaxLength=20
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*)?$`
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// When an unfinished drill is recorded as timed out and deleted.
	// Omitted: 2h.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10m') && duration(self) <= duration('24h')",message="restoreDrills.timeLimit must be between 10m and 24h",reason=FieldValueInvalid
	// +optional
	TimeLimit *metav1.Duration `json:"timeLimit,omitempty"`

	// The drill instance's CPU and memory. Omitted: the source Cluster's.
	// +optional
	Resources *PgToolBoxConsoleDrillResources `json:"resources,omitempty"`

	// The drill instance's storage. Omitted: the source Cluster's.
	// +optional
	Storage PgToolBoxConsoleDrillStorage `json:"storage,omitempty"`

	// The drills of this console running at once (one per source in any
	// case). Omitted: 3.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +optional
	MaxConcurrent *int32 `json:"maxConcurrent,omitempty"`

	// The drill seal's own keys (RESTORE_DRILL_SEAL_KEY_FILE): a key of a
	// Secret in the console's namespace holding one key per line, at least
	// 32 bytes each (the first seals, all verify), mounted read-only. Keep
	// the Secret readable by the console alone. Omitted: the keys derive
	// from the session secret, so whoever can read the session Secret can
	// forge a seal.
	// +optional
	SealKeySecretRef *SecretKeyReference `json:"sealKeySecretRef,omitempty"`

	// The NetworkPolicy around the drill Pods. A promoted drill is a live
	// copy of a production database: the console turns off the background
	// workers it knows (logical replication, pg_cron, TimescaleDB), and this
	// policy holds the rest to DNS, the API server and the object store.
	// +optional
	NetworkPolicy PgToolBoxConsoleDrillNetworkPolicy `json:"networkPolicy,omitempty"`
}

// PgToolBoxConsoleDrillNetworkPolicy is the drill Pods' NetworkPolicy: egress to
// DNS, the API server and the object store; ingress from the CloudNativePG
// operator and the console on the instance's status and metrics ports.
type PgToolBoxConsoleDrillNetworkPolicy struct {
	// Whether the operator renders it. Default true: unlike the pgAdmin
	// policy, it fences a copy of production data the console created.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// The CloudNativePG operator's namespace, which reaches every
	// instance's status port. Omitted: cnpg-system.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	OperatorNamespace string `json:"operatorNamespace,omitempty"`

	// The API server's addresses (CIDRs). Empty: ports 443 and 6443 to any
	// address.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=49
	// +kubebuilder:validation:items:Pattern=`^[0-9a-fA-F:.]+/[0-9]{1,3}$`
	// +optional
	APIServerCIDRs []string `json:"apiServerCIDRs,omitempty"`

	// The object store the drills restore from: an ipBlock, or a
	// namespaceSelector and podSelector for an in-cluster store. Empty:
	// objectStorePorts to any address.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	ObjectStore []networkingv1.NetworkPolicyPeer `json:"objectStore,omitempty"`

	// The object store's ports. Omitted: 443.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	// +optional
	ObjectStorePorts []int32 `json:"objectStorePorts,omitempty"`
}

// PgToolBoxConsoleDrillResources are a drill instance's requests and limits. The
// console refuses to start on a quantity that is not positive or a limit
// below its request (CEL quantity functions are not available on every
// supported API server).
type PgToolBoxConsoleDrillResources struct {
	// +optional
	Requests *PgToolBoxConsoleDrillQuantities `json:"requests,omitempty"`
	// +optional
	Limits *PgToolBoxConsoleDrillQuantities `json:"limits,omitempty"`
}

// PgToolBoxConsoleDrillQuantities are a CPU and a memory quantity.
type PgToolBoxConsoleDrillQuantities struct {
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// PgToolBoxConsoleDrillStorage is a drill instance's volume.
type PgToolBoxConsoleDrillStorage struct {
	// The volume size. Omitted: the source Cluster's.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// The StorageClass. Omitted: the source Cluster's.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
}

// PgToolBoxConsolePrometheusSpec is the Prometheus HTTP API the charts query.
// +kubebuilder:validation:XValidation:rule="!has(self.tokenAudience) || (has(self.projectedToken) && self.projectedToken)",message="prometheus.tokenAudience applies to projectedToken only",fieldPath=".tokenAudience",reason=FieldValueInvalid
// +kubebuilder:validation:XValidation:rule="(!has(self.tokenSecretRef) && (!has(self.projectedToken) || !self.projectedToken)) || self.url.startsWith('https://')",message="prometheus.url must be https when a token is configured: the token is never sent in clear",fieldPath=".url",reason=FieldValueInvalid
type PgToolBoxConsolePrometheusSpec struct {
	// The Prometheus (or Thanos querier) URL.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// A Secret key holding a bearer token, mounted as a file.
	// +optional
	TokenSecretRef *SecretKeyReference `json:"tokenSecretRef,omitempty"`

	// Present the console's own ServiceAccount token, projected and rotated
	// by the kubelet (what the OpenShift thanos-querier accepts once the
	// ServiceAccount may view metrics).
	// +optional
	ProjectedToken bool `json:"projectedToken,omitempty"`

	// The audience of the projected token. Omitted: the API server's own
	// audience, which a TokenReview-based proxy (OpenShift's thanos-querier
	// behind kube-rbac-proxy) expects; such a token is also accepted by the
	// API server itself, so only a Prometheus you trust may receive it. Set
	// it when the server checks a dedicated audience: the token is then
	// useless against the API server.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\s]+$`
	// +optional
	TokenAudience string `json:"tokenAudience,omitempty"`

	// A ConfigMap with ca.crt that signed the server certificate. Omitted:
	// the system roots.
	// +optional
	CAConfigMapRef *LocalObjectReference `json:"caConfigMapRef,omitempty"`

	// Add namespace=<ns> to each query, as OpenShift's tenancy port requires.
	// +optional
	Namespaced bool `json:"namespaced,omitempty"`

	// The charts' window. Omitted: 1h.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('5m') && duration(self) <= duration('24h')",message="prometheus.range must be between 5m and 24h",reason=FieldValueInvalid
	// +optional
	Range *metav1.Duration `json:"range,omitempty"`
}

// PgToolBoxConsoleHistorySpec configures the history journal.
type PgToolBoxConsoleHistorySpec struct {
	// Keep the history on a volume per replica, across restarts: the
	// console is a StatefulSet whose claim template makes
	// history-<console>-<ordinal> for each replica. false: history in
	// memory, a Deployment. Default true.
	// +kubebuilder:default=true
	// +optional
	Persistent *bool `json:"persistent,omitempty"`

	// Size of each replica's history volume. Default 1Gi, which holds the
	// default retention of a console of a few hundred instances. A change
	// replaces the StatefulSet (its Pods keep running) and applies to
	// volumes made later; grow an existing one by editing its claim.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// StorageClass of the history volumes. Omitted: the cluster default.
	// Like size, a change applies to volumes made later.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// PgToolBoxConsoleDigestSpec configures the daily digest (DIGEST_*).
type PgToolBoxConsoleDigestSpec struct {
	// Make a digest every day. Default: on with persistent history; with
	// in-memory history each replica would make its own, so it is on only
	// when set.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// The UTC hour at which each digest closes its 24 hours. Default 7.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=23
	// +optional
	Hour *int32 `json:"hour,omitempty"`

	// How long digests are kept. Default 720h (30 days).
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('24h') && duration(self) <= duration('8784h')",message="digest.retention must be between 24h and 8784h",reason=FieldValueInvalid
	// +optional
	Retention *metav1.Duration `json:"retention,omitempty"`

	// Where each digest is sent: a summary and a link, never the details.
	// Omitted: nowhere (the page only).
	// +optional
	Delivery *PgToolBoxConsoleDigestDeliverySpec `json:"delivery,omitempty"`
}

// PgToolBoxConsoleDigestDeliverySpec names the channels. Each credential is a
// Secret key the operator mounts as a file; the console reads no Secret
// through the API.
// +kubebuilder:validation:XValidation:rule="has(self.chatWebhookSecretRef) || has(self.webhookSecretRef) || has(self.smtp)",message="digest.delivery names no channel",reason=FieldValueRequired
type PgToolBoxConsoleDigestDeliverySpec struct {
	// A Slack or Mattermost incoming-webhook URL (https), which receives
	// POST {"text": ...}.
	// +optional
	ChatWebhookSecretRef *SecretKeyReference `json:"chatWebhookSecretRef,omitempty"`

	// A generic webhook URL (https), which receives the digest's JSON
	// summary.
	// +optional
	WebhookSecretRef *SecretKeyReference `json:"webhookSecretRef,omitempty"`

	// Mail over TLS.
	// +optional
	SMTP *PgToolBoxConsoleDigestSMTPSpec `json:"smtp,omitempty"`

	// Bound of one delivery attempt. Default 10s.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('1m')",message="digest.delivery.timeout must be between 1s and 1m",reason=FieldValueInvalid
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// PgToolBoxConsoleDigestSMTPTLS is how the mail connection is secured; there is
// no plaintext mode.
// +kubebuilder:validation:Enum=starttls;tls
type PgToolBoxConsoleDigestSMTPTLS string

// PgToolBoxConsoleDigestSMTPSpec is the mail channel.
// +kubebuilder:validation:XValidation:rule="has(self.username) == has(self.passwordSecretRef)",message="smtp.username and smtp.passwordSecretRef are set together",reason=FieldValueInvalid
type PgToolBoxConsoleDigestSMTPSpec struct {
	// The mail server, host:port.
	// +kubebuilder:validation:MaxLength=261
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?:[0-9]{1,5}$`
	Address string `json:"address"`

	// starttls (required, never optional) or tls (implicit, port 465).
	// Default starttls.
	// +kubebuilder:default=starttls
	// +optional
	TLS PgToolBoxConsoleDigestSMTPTLS `json:"tls,omitempty"`

	// The user for AUTH PLAIN (over TLS only).
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Username string `json:"username,omitempty"`

	// The password.
	// +optional
	PasswordSecretRef *SecretKeyReference `json:"passwordSecretRef,omitempty"`

	// The sender, one bare address.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=254
	From string `json:"from"`

	// The recipients, bare addresses.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MaxLength=254
	To []string `json:"to"`
}

// PgToolBoxConsoleStatus is the observed state of a PgToolBoxConsole.
type PgToolBoxConsoleStatus struct {
	// The generation the status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Where the console is reached.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	URL string `json:"url,omitempty"`

	// The image reference the console runs.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`

	// The ConfigMap the console reads its end-of-life table from, and who
	// keeps it: Operator (the operator's table, updated with the operator),
	// User (edited or annotated user-managed, left alone) or Referenced
	// (spec.lifecycleConfigMapRef).
	// +optional
	Lifecycle *PgToolBoxConsoleLifecycleStatus `json:"lifecycle,omitempty"`

	// The instance CA bundle the console mounts, who keeps it and, when the
	// operator does, how many Clusters it covers and which are missing.
	// +optional
	InstanceCABundle *PgToolBoxConsoleInstanceCABundleStatus `json:"instanceCABundle,omitempty"`

	// Conditions: Ready (summary), ConfigurationValid, WorkloadReady,
	// ExposureReady, Progressing, Stalled and the advisory
	// InstanceCABundleComplete and ExposureConfined.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PgToolBoxConsoleLifecycleKeeper says who keeps the lifecycle ConfigMap.
// +kubebuilder:validation:Enum=Operator;User;Referenced
type PgToolBoxConsoleLifecycleKeeper string

const (
	// LifecycleKeptByOperator means the ConfigMap holds the operator's
	// built-in table, rewritten when the operator brings a newer one.
	LifecycleKeptByOperator PgToolBoxConsoleLifecycleKeeper = "Operator"
	// LifecycleKeptByUser means someone edited the ConfigMap the operator
	// created, or annotated it user-managed; the operator no longer writes it.
	LifecycleKeptByUser PgToolBoxConsoleLifecycleKeeper = "User"
	// LifecycleReferenced means spec.lifecycleConfigMapRef names a ConfigMap
	// the operator never writes.
	LifecycleReferenced PgToolBoxConsoleLifecycleKeeper = "Referenced"
)

// PgToolBoxConsoleLifecycleStatus reports the lifecycle ConfigMap.
type PgToolBoxConsoleLifecycleStatus struct {
	// The ConfigMap's name.
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
	// Who keeps it.
	KeptBy PgToolBoxConsoleLifecycleKeeper `json:"keptBy"`
	// The day the operator's table was last reviewed, when the operator
	// keeps it.
	// +kubebuilder:validation:MaxLength=10
	// +optional
	Reviewed string `json:"reviewed,omitempty"`
}

// PgToolBoxConsoleInstanceCAKeeper says who keeps the instance CA bundle.
// +kubebuilder:validation:Enum=Operator;Referenced
type PgToolBoxConsoleInstanceCAKeeper string

const (
	// InstanceCAKeptByOperator means the operator mirrors the selected
	// Clusters' server CAs into <name>-instance-ca.
	InstanceCAKeptByOperator PgToolBoxConsoleInstanceCAKeeper = "Operator"
	// InstanceCAReferenced means spec.instanceCABundleRef names a ConfigMap
	// the operator never reads or writes.
	InstanceCAReferenced PgToolBoxConsoleInstanceCAKeeper = "Referenced"
)

// MaxInstanceCAMissing bounds status.instanceCABundle.missing.
const MaxInstanceCAMissing = 32

// PgToolBoxConsoleInstanceCABundleStatus reports the instance CA bundle.
type PgToolBoxConsoleInstanceCABundleStatus struct {
	// The ConfigMap's name.
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
	// Who keeps it.
	KeptBy PgToolBoxConsoleInstanceCAKeeper `json:"keptBy"`
	// How many selected Clusters have their server CA in the bundle, when
	// the operator keeps it (Clusters sharing one CA each count).
	// +optional
	Clusters int32 `json:"clusters,omitempty"`
	// The selected Clusters whose server CA is not in the bundle, and why;
	// at most 32, by Cluster name (the condition message has the count).
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Missing []PgToolBoxConsoleInstanceCAMissing `json:"missing,omitempty"`
}

// PgToolBoxConsoleInstanceCAMissing is a Cluster whose server CA the operator
// could not put in the bundle.
type PgToolBoxConsoleInstanceCAMissing struct {
	// The Cluster's name.
	// +kubebuilder:validation:MaxLength=253
	Cluster string `json:"cluster"`
	// The server CA Secret read: status.certificates.serverCASecret, else
	// spec.certificates.serverCASecret, else <cluster>-ca.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Secret string `json:"secret,omitempty"`
	// Why: SecretNotFound, SecretKeyMissing (no ca.crt), InstanceCAInvalid
	// (ca.crt holds no PEM certificate) or InstanceCABundleFull.
	// +kubebuilder:validation:MaxLength=64
	Reason Reason `json:"reason"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgc,categories=pgtoolbox
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40 && self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name must be a DNS-1035 label of at most 40 characters (it prefixes generated object names)",reason=FieldValueInvalid

// PgToolBoxConsole runs the pgToolBox console for the CloudNativePG Clusters of
// its namespace: the same console the chart installs, kept converged by the
// operator. The console's own authority is read-only in this namespace;
// whoever may create a PgToolBoxConsole may create a Deployment here anyway.
type PgToolBoxConsole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PgToolBoxConsoleSpec `json:"spec,omitempty"`
	// +optional
	Status PgToolBoxConsoleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PgToolBoxConsoleList contains a list of PgToolBoxConsole.
type PgToolBoxConsoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PgToolBoxConsole `json:"items"`
}
