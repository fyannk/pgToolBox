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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EvidenceAgent-specific condition types (the common ones are in
// conditions.go). Frozen API surface: extend, never rename.
const (
	// ConditionServingCertificateReady is True when the agent's serving
	// certificate exists, is valid for the Service name and its CA is
	// published in status.caBundle.
	ConditionServingCertificateReady = "ServingCertificateReady"
)

// EvidenceAgent-specific reasons of the frozen catalogue, registered from
// evidenceagent_funcs.go.
const (
	// ReasonBarmanPluginNotConfigured: the Cluster declares neither an
	// enabled barman-cloud plugin with a barmanObjectName nor an in-tree
	// spec.backup.barmanObjectStore, and spec.objectStoreRef is not set.
	ReasonBarmanPluginNotConfigured Reason = "BarmanPluginNotConfigured"
	// ReasonObjectStoreNotFound: the resolved ObjectStore, or the pgBackRest
	// plugin's Stanza or Archive, does not exist.
	ReasonObjectStoreNotFound Reason = "ObjectStoreNotFound"
	// ReasonObjectStoreAPIUnavailable: barmancloud.cnpg.io ObjectStore (or,
	// for a Cluster backed up with pgBackRest, pgbackrest.dalibo.com Stanza
	// or pgbackrest.cnpg.opera.com Archive) is not served or not readable by
	// the operator.
	ReasonObjectStoreAPIUnavailable Reason = "ObjectStoreAPIUnavailable"
	// ReasonUnsupportedObjectStore: the backup configuration (an
	// ObjectStore, the in-tree spec.backup.barmanObjectStore, or a pgBackRest
	// plugin's Stanza or Archive) is outside the agent's profile, or the
	// Cluster backs up through a plugin the agent does not read; the message
	// names the shape and the field.
	ReasonUnsupportedObjectStore Reason = "UnsupportedObjectStore"
	// ReasonDestinationInvalid: destinationPath, endpointURL, the server
	// name (or a pgBackRest bucket, path, endpoint or stanza) is rejected by
	// the shared evidence contract parser.
	ReasonDestinationInvalid Reason = "DestinationInvalid"
	// ReasonRegionRequired: AWS S3 (no endpointURL) without s3Credentials.region.
	ReasonRegionRequired Reason = "RegionRequired"
	// ReasonImageRequired: spec.image is unset and the operator has no
	// default evidence agent image.
	ReasonImageRequired Reason = "ImageRequired"
	// ReasonServiceCAPending: the OpenShift service CA has not yet issued
	// the serving certificate or injected the CA bundle.
	ReasonServiceCAPending Reason = "ServiceCAPending"
	// ReasonCertificateIssued is the Event of a newly issued or renewed
	// operator-signed serving certificate.
	ReasonCertificateIssued Reason = "CertificateIssued"
)

// EvidenceAgentCertificateIssuer selects who signs the agent's serving
// certificate.
// +kubebuilder:validation:Enum=Operator;OpenShiftServiceCA
type EvidenceAgentCertificateIssuer string

const (
	// CertificateIssuerOperator means the operator keeps a CA per EvidenceAgent
	// (a Secret the agent Pod never mounts) and signs a renewable serving
	// certificate with it.
	CertificateIssuerOperator EvidenceAgentCertificateIssuer = "Operator"
	// CertificateIssuerOpenShiftServiceCA means the OpenShift service CA issues
	// the certificate through the Service annotation and injects its CA
	// bundle into a ConfigMap the operator creates.
	CertificateIssuerOpenShiftServiceCA EvidenceAgentCertificateIssuer = "OpenShiftServiceCA"
)

// EvidenceAgentTLSSpec configures the agent's serving certificate.
type EvidenceAgentTLSSpec struct {
	// Who issues the serving certificate.
	// +kubebuilder:default=Operator
	// +optional
	Issuer EvidenceAgentCertificateIssuer `json:"issuer,omitempty"`
}

// EvidenceAgentSpec declares one evidence agent: a read-only reader of the
// backup repository of one CloudNativePG Cluster in this namespace, serving
// evidence/v1alpha1 to the console over in-cluster HTTPS.
// +kubebuilder:validation:XValidation:rule="!has(self.exposure) || self.exposure.type == 'clusterIP'",message="the evidence agent is reachable in-cluster only: exposure.type must be clusterIP",fieldPath=".exposure.type",reason=FieldValueForbidden
// +kubebuilder:validation:XValidation:rule="!has(self.pgbackrest) || (!has(self.objectStoreRef) && !has(self.serverName))",message="pgbackrest reads a Cluster backed up with a pgBackRest plugin: objectStoreRef and serverName are barman-cloud settings",fieldPath=".pgbackrest",reason=FieldValueForbidden
type EvidenceAgentSpec struct {
	// The CloudNativePG Cluster, in this namespace, whose backups the agent
	// reads. Its barman-cloud plugin stanza names the ObjectStore and the
	// server name unless the fields below override them.
	ClusterRef LocalObjectReference `json:"clusterRef"`

	// The barman-cloud ObjectStore (barmancloud.cnpg.io/v1), in this
	// namespace, to read. Defaults to the Cluster's barmanObjectName
	// plugin parameter. The agent uses the same credentials the plugin
	// uses; they never leave this namespace.
	// +optional
	ObjectStoreRef *LocalObjectReference `json:"objectStoreRef,omitempty"`

	// The Secrets, in this namespace, the agent may read: whoever may edit
	// the Cluster or the ObjectStore chooses which Secrets the backup
	// configuration names, and the agent reads only those also listed
	// here. A configuration naming another is refused by name
	// (ConfigurationValid=False, UnsupportedObjectStore). Empty is the
	// deprecated behaviour, kept for an ObjectStore of the barman-cloud
	// plugin only: every Secret that ObjectStore names; the Cluster's own
	// spec.backup.barmanObjectStore is then refused.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +listType=set
	// +optional
	AllowedSecrets []string `json:"allowedSecrets,omitempty"`

	// Admit a plain-http object-store endpoint, over which the credentials
	// cross the network in clear text. Off, such an endpoint is refused
	// (UnsupportedObjectStore), except for the deprecated behaviour of an
	// empty allowedSecrets.
	// +optional
	AllowInsecureEndpoint bool `json:"allowInsecureEndpoint,omitempty"`

	// The Barman server name (the first key prefix under the destination).
	// Defaults to the Cluster's serverName plugin parameter, else the
	// Cluster's name — the same rule the barman-cloud plugin applies.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="self != '.' && self != '..' && !self.contains('/') && !self.contains('\\\\')",message="serverName must not contain '/' or '\\' and must not be '.' or '..'",reason=FieldValueInvalid
	// +optional
	ServerName string `json:"serverName,omitempty"`

	// The agent image. Omitted, the operator's default applies. The
	// repository must be in the operator's allow-list
	// (--evidence-agent-image-repositories; by default the default image's
	// repository only): the agent Pod mounts the object-store credentials.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// Compute resources of the agent container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// How often the agent re-reads the repository.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('30s') && duration(self) <= duration('24h')",message="refreshInterval must be between 30s and 24h",reason=FieldValueInvalid
	// +optional
	RefreshInterval *metav1.Duration `json:"refreshInterval,omitempty"`

	// Exposure of the agent. Only clusterIP is accepted: the console in the
	// cluster is the agent's only client.
	// +optional
	Exposure *ExposureSpec `json:"exposure,omitempty"`

	// Serving certificate.
	// +optional
	TLS *EvidenceAgentTLSSpec `json:"tls,omitempty"`

	// Workload identity of the agent, for a repository read with the Pod's
	// own cloud identity (s3Credentials.inheritFromIAMRole,
	// azureCredentials.inheritFromAzureAD or useDefaultAzureCredentials,
	// googleCredentials.gkeEnvironment, a pgBackRest key type auto or
	// web-id): the agent's ServiceAccount and Pod carry what the platform's
	// identity webhook or metadata server needs to grant the agent a role
	// that can read the repository.
	// +optional
	WorkloadIdentity *WorkloadIdentitySpec `json:"workloadIdentity,omitempty"`

	// pgBackRest settings, for a Cluster backed up with a pgBackRest plugin
	// (pgbackrest.dalibo.com or pgbackrest.cnpg.opera.com). Omitted, such a
	// Cluster is read from repo1 with the system's trusted roots.
	// +optional
	PgBackRest *EvidenceAgentPgBackRestSpec `json:"pgbackrest,omitempty"`
}

// EvidenceAgentPgBackRestSpec selects the pgBackRest repository the agent
// reads. The stanza, the bucket or container, the path, the credentials and
// the cipher pass come from the plugin's Stanza or Archive; every Secret
// they name, and endpointCA, must be listed in spec.allowedSecrets.
type EvidenceAgentPgBackRestSpec struct {
	// The pgBackRest repository number (repo<n>) to read. Dalibo numbers
	// the Stanza's S3 repositories first, then its Azure ones; Opera
	// numbers the Archive's repositories in order. 1 is where both plugins
	// send a backup that names no repository.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=256
	// +kubebuilder:default=1
	// +optional
	Repository int32 `json:"repository,omitempty"`

	// A CA bundle (a key of a Secret in this namespace) the agent trusts for
	// the repository endpoint, beside the system roots. It wins over the
	// Archive's endpointCA. TLS is always verified: Dalibo's verifyTLS and
	// Opera's disableVerifyTLS are not followed.
	// +optional
	EndpointCA *SecretKeyReference `json:"endpointCA,omitempty"`

	// Pins the repository's endpoint: a host[:port] (a DNS name, an IPv4
	// address or a bracketed IPv6 address) that the host the agent sends
	// its requests and credentials to must match, or the Cluster is refused
	// (UnsupportedObjectStore). That host is the S3 endpoint the plugin's
	// object names, or the Azure account's Blob host
	// (<account>.blob.core.windows.net host-style, the endpoint path-style).
	// Without a port, the scheme's default port. Whoever may edit the
	// Stanza or the Archive chooses where the credentials go; the pin keeps
	// that choice with this object's author. It is also the opt-in that
	// lets an Azure storage key or SAS token go to a host outside the Azure
	// Blob domains (Azurite).
	// +kubebuilder:validation:MaxLength=261
	// +kubebuilder:validation:Pattern=`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?([.][A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?)*)(:[0-9]{1,5})?$`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// WorkloadIdentitySpec binds the agent to a cloud identity. Setting it (even
// empty, as GKE workload identity federation needs nothing more) is the
// opt-in: without it a configuration using the Pod's cloud identity is
// refused, so it never falls back to the node's.
type WorkloadIdentitySpec struct {
	// Annotations added to the agent's ServiceAccount, among the identity
	// webhooks' own: eks.amazonaws.com/role-arn,
	// eks.amazonaws.com/sts-regional-endpoints,
	// eks.amazonaws.com/token-expiration (IRSA),
	// azure.workload.identity/client-id, azure.workload.identity/tenant-id
	// (Azure workload identity) and iam.gke.io/gcp-service-account (GKE).
	// Any other ServiceAccount annotation can change what the account is
	// (an OpenShift OAuth client, for one) and is refused.
	// +kubebuilder:validation:MaxProperties=6
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['eks.amazonaws.com/role-arn', 'eks.amazonaws.com/sts-regional-endpoints', 'eks.amazonaws.com/token-expiration', 'azure.workload.identity/client-id', 'azure.workload.identity/tenant-id', 'iam.gke.io/gcp-service-account'])",message="serviceAccountAnnotations accepts only the workload-identity annotations eks.amazonaws.com/role-arn, eks.amazonaws.com/sts-regional-endpoints, eks.amazonaws.com/token-expiration, azure.workload.identity/client-id, azure.workload.identity/tenant-id and iam.gke.io/gcp-service-account",reason=FieldValueForbidden
	// +optional
	ServiceAccountAnnotations map[string]string `json:"serviceAccountAnnotations,omitempty"`

	// Labels added to the agent's Pod: azure.workload.identity/use only
	// (Azure workload identity). Any other label could select the agent
	// into a Service or a policy (a CloudNativePG -rw Service, for one).
	// +kubebuilder:validation:MaxProperties=1
	// +kubebuilder:validation:XValidation:rule="self.all(k, k == 'azure.workload.identity/use')",message="podLabels accepts only azure.workload.identity/use",reason=FieldValueForbidden
	// +optional
	PodLabels map[string]LabelValue `json:"podLabels,omitempty"`
}

// LabelValue is a Kubernetes label value (the keys are allow-listed by the
// map's own rule).
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`
type LabelValue string

// EvidenceAgentStatus is the observed state of an EvidenceAgent. A console
// reads endpoint, caBundle, fingerprint and producerEpoch to call the agent
// and to judge its answers (docs: the evidence agent channel).
type EvidenceAgentStatus struct {
	// The generation the status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// The in-cluster HTTPS URL of the agent's Service.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// PEM CA bundle that verifies the agent's serving certificate for the
	// endpoint's host. Public material, never a key.
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`

	// The repository identity the agent reads (sha256: domain-separated,
	// region-free fingerprint of endpoint, bucket, prefix, format and
	// server); a snapshot whose destination_fingerprint differs is not this
	// repository's.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +optional
	Fingerprint string `json:"fingerprint,omitempty"`

	// The configuration epoch of the running agent: the generation its
	// Deployment was rendered from, published once that rollout completed.
	// Every snapshot carries it as producer_epoch; a snapshot with an older
	// epoch comes from a superseded configuration.
	// +optional
	ProducerEpoch int64 `json:"producerEpoch,omitempty"`

	// The resolved ObjectStore and Barman server name.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ObjectStoreName string `json:"objectStoreName,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	// +optional
	ServerName string `json:"serverName,omitempty"`

	// The resolved pgBackRest repository, for a Cluster backed up with a
	// pgBackRest plugin.
	// +optional
	PgBackRest *EvidenceAgentPgBackRestStatus `json:"pgbackrest,omitempty"`

	// The image reference the agent runs.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`

	// Conditions: Ready (summary), ConfigurationValid, WorkloadReady,
	// ServingCertificateReady, Progressing and Stalled.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EvidenceAgentPgBackRestStatus is the pgBackRest repository an agent
// resolved: the plugin and its object as soon as the Cluster names them,
// the rest once the object was read.
type EvidenceAgentPgBackRestStatus struct {
	// The plugin: pgbackrest.dalibo.com or pgbackrest.cnpg.opera.com.
	// +kubebuilder:validation:MaxLength=253
	Plugin string `json:"plugin"`

	// The plugin's object in this namespace: a Stanza (Dalibo) or an
	// Archive (Opera).
	// +kubebuilder:validation:Enum=Stanza;Archive
	Kind string `json:"kind"`
	// +kubebuilder:validation:MaxLength=253
	ObjectName string `json:"objectName"`

	// The stanza the backups are read under.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Stanza string `json:"stanza,omitempty"`

	// The stanza the WAL is read under, when it differs from stanza
	// (Opera's configuration.stanza): a restore must name it.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	ArchiveStanza string `json:"archiveStanza,omitempty"`

	// The repository number read (repo<n>).
	// +optional
	Repository int32 `json:"repository,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgea,categories=pgtoolbox
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40 && self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name must be a DNS-1035 label of at most 40 characters (it prefixes generated object names)",reason=FieldValueInvalid

// EvidenceAgent deploys a read-only evidence agent for the backup repository
// of one CloudNativePG Cluster in its namespace. Whoever may get an
// EvidenceAgent may read the evidence it serves.
type EvidenceAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EvidenceAgentSpec `json:"spec"`
	// +optional
	Status EvidenceAgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EvidenceAgentList contains a list of EvidenceAgent.
type EvidenceAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EvidenceAgent `json:"items"`
}
