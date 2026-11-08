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

// PgAdmin label key and condition types.
const (
	// PgAdminLabelKey labels every object generated for a PgAdmin with its
	// name; the controller maps Pods and Jobs back to their owner by it.
	PgAdminLabelKey = "pgtoolbox.fyannk.dev/pgadmin"

	// ConditionServersSynced reports the last registration Job: True when
	// pgAdmin's shared servers match the offered set, False with the Job's
	// fact when it failed, Unknown while it runs or waits for pgAdmin.
	ConditionServersSynced = "ServersSynced"

	// ConditionPgAdminEgressConfined is advisory and gates nothing (it never
	// enters Ready): True when this PgAdmin's NetworkPolicy confines
	// pgAdmin's egress to named destinations, False (PgAdminEgressOpen)
	// naming every rule that does not, Unknown while pgAdmin is not
	// running. Whoever opens pgAdmin holds the offered credentials either
	// way; open egress only means they, or the data those credentials
	// reach, can also leave the Pod directly.
	ConditionPgAdminEgressConfined = "PgAdminEgressConfined"
)

// PgAdmin-specific reasons. They join the frozen catalogue in
// pgadmin_funcs.go.
const (
	// ClusterNotFound (a clusterRefs entry names no Cluster) is the shared
	// ReasonClusterNotFound, registered with the EvidenceAgent reasons.
	// ReasonNoClusterSelected: clusterSelector matches no Cluster.
	ReasonNoClusterSelected Reason = "NoClusterSelected"
	// ReasonPoolerNotFound: a pooler:<name> endpoint names no Pooler of a
	// registered Cluster.
	ReasonPoolerNotFound Reason = "PoolerNotFound"
	// ReasonTooManyServers: the offered set exceeds the bound the
	// credential files and the registration accept.
	ReasonTooManyServers Reason = "TooManyServers"
	// ReasonPgAdminNotRunning: pgAdmin has no Ready Pod yet; registration
	// and the egress statement wait for it.
	ReasonPgAdminNotRunning Reason = "PgAdminNotRunning"
	// ReasonRegistrationRunning: the registration Job for the current
	// offered set is running.
	ReasonRegistrationRunning Reason = "RegistrationRunning"
	// ReasonRegistrationFailed: the registration Job failed; the message
	// carries its fact.
	ReasonRegistrationFailed Reason = "RegistrationFailed"
	// ReasonServersRegistered is the Event of a successful registration.
	ReasonServersRegistered Reason = "ServersRegistered"
	// ReasonRegistrationStarted is the Event of a new registration Job.
	ReasonRegistrationStarted Reason = "RegistrationStarted"
	// ReasonUpgradeBackupRunning: the pre-upgrade backup of pgAdmin's
	// settings database runs before the new image rolls out.
	ReasonUpgradeBackupRunning Reason = "UpgradeBackupRunning"
	// ReasonUpgradeBackupFailed: the pre-upgrade backup failed; the old
	// image keeps running.
	ReasonUpgradeBackupFailed Reason = "UpgradeBackupFailed"
	// ReasonUpgradeBackupCompleted is the Event of a completed backup.
	ReasonUpgradeBackupCompleted Reason = "UpgradeBackupCompleted"
	// ReasonUpgradeBackupSkipped is the Warning Event of a pre-upgrade
	// backup skipped by AnnotationSkipUpgradeBackup.
	ReasonUpgradeBackupSkipped Reason = "UpgradeBackupSkipped"
	// ReasonRolloutStarted is the Event of a changed workload hash.
	ReasonRolloutStarted Reason = "RolloutStarted"
	// ReasonPgAdminEgressOpen: PgAdminEgressConfined=False.
	ReasonPgAdminEgressOpen Reason = "PgAdminEgressOpen"
	// ReasonDatabaseRoleAPIUnavailable: the CNPG DatabaseRole API is not
	// readable (a refused List), or not served while spec.configDatabase
	// needs it; only application and superuser credentials are offered.
	// CloudNativePG 1.30 and later serve the kind; an install whose CRDs
	// lack it is not a problem for the offered credentials: none is
	// offered, and the PgAdmin is Ready.
	ReasonDatabaseRoleAPIUnavailable Reason = "DatabaseRoleAPIUnavailable"
	// ReasonPoolerAPIUnavailable: the CNPG Pooler API is not readable.
	ReasonPoolerAPIUnavailable Reason = "PoolerAPIUnavailable"
	// ReasonGrantMissing: a crossNamespaceClusterRefs entry names a Cluster
	// no PgAdminAccess of its namespace grants this PgAdmin.
	ReasonGrantMissing Reason = "GrantMissing"
	// ReasonConfigDatabaseNotReady: the configDatabase's DatabaseRole or
	// Database is not applied by CloudNativePG yet, or its generated
	// password Secret is not recorded; pgAdmin waits for it.
	ReasonConfigDatabaseNotReady Reason = "ConfigDatabaseNotReady"
	// ReasonPasswordGenerationUnavailable: the CloudNativePG DatabaseRole API
	// of this cluster cannot generate a password (CloudNativePG 1.31 and
	// later can), which configDatabase needs: the operator never holds one.
	ReasonPasswordGenerationUnavailable Reason = "PasswordGenerationUnavailable"
	// ReasonDatabaseAPIUnavailable: the CloudNativePG Database API, which
	// configDatabase needs, is not served or not readable.
	ReasonDatabaseAPIUnavailable Reason = "DatabaseAPIUnavailable"
)

// AnnotationSkipUpgradeBackup on a PgAdmin skips the pre-upgrade backup of
// one image change: its value names the target, the image's digest
// (sha256:...) or its full resolved reference, so it cannot skip a later
// upgrade. For an old image that can no longer run its backup (it cannot
// be pulled any more); the skip is recorded in status.upgradeBackup and
// as a Warning Event.
const AnnotationSkipUpgradeBackup = ReservedKeyPrefix + "skip-upgrade-backup"

// Metadata of the per-person DatabaseRoles the console creates: a
// DatabaseRole labelled LabelPersonalRole=true and annotated with
// AnnotationPgAdminUser is registered as a personal (non-shared) server
// of that pgAdmin user, in every PgAdmin that registers its Cluster,
// instead of a shared one. It is API surface: the console writes it, the
// operator reads it.
const (
	LabelPersonalRole      = ReservedKeyPrefix + "personal-role"
	PersonalRoleLabelValue = "true"
	AnnotationPgAdminUser  = ReservedKeyPrefix + "pgadmin-user"
)

// Endpoint names which Service of a Cluster a server connects through:
// rw (the primary), ro (the replicas), r (any instance) or
// pooler:<Pooler name> (a CNPG Pooler of that Cluster; its Service name
// must be among the Cluster's spec.certificates.serverAltDNSNames for
// verify-full to succeed).
// +kubebuilder:validation:MaxLength=70
// +kubebuilder:validation:Pattern=`^(rw|ro|r|pooler:[a-z0-9]([-a-z0-9]*[a-z0-9])?)$`
type Endpoint string

// Endpoints of every Cluster.
const (
	EndpointRW Endpoint = "rw"
	EndpointRO Endpoint = "ro"
	EndpointR  Endpoint = "r"
	// EndpointPoolerPrefix starts a pooler endpoint.
	EndpointPoolerPrefix = "pooler:"
)

// PgAdminSpec declares one pgAdmin for a team namespace: the
// Clusters of that namespace it registers, the credentials it offers, how
// people sign in, and how it is published.
//
// Who may open it: dba only. Whoever reaches pgAdmin holds the credentials
// of every role it offers (pgAdmin hides the passwords but a connected role
// can change its own, and a server a user registers can name a managed
// libpq service), so opening it is equivalent to reading those Secrets.
// +kubebuilder:validation:XValidation:rule="!(has(self.clusterRefs) && has(self.clusterSelector))",message="clusterRefs and clusterSelector are mutually exclusive",reason=FieldValueInvalid
// +kubebuilder:validation:XValidation:rule="has(self.clusterRefs) || has(self.clusterSelector) || has(self.crossNamespaceClusterRefs)",message="one of clusterRefs, clusterSelector and crossNamespaceClusterRefs is required",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="!has(self.replicas) || self.replicas == 1 || has(self.configDatabase)",message="replicas above 1 need configDatabase: SQLite on a ReadWriteOnce volume serves one pgAdmin",reason=FieldValueForbidden
// +kubebuilder:validation:XValidation:rule="has(self.configDatabase) == has(oldSelf.configDatabase)",message="configDatabase cannot be added to or removed from an existing PgAdmin: pgAdmin's settings do not move between SQLite and PostgreSQL automatically (see the manual move in the documentation)",reason=FieldValueForbidden
type PgAdminSpec struct {
	// The CloudNativePG Clusters to register, by name, in this namespace.
	// Exactly one of clusterRefs and clusterSelector.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	ClusterRefs []LocalObjectReference `json:"clusterRefs,omitempty"`

	// A label selector over the Clusters of this namespace only; Clusters
	// of other namespaces are never selected, only named in
	// crossNamespaceClusterRefs. At most one of clusterRefs and
	// clusterSelector.
	// +optional
	ClusterSelector *metav1.LabelSelector `json:"clusterSelector,omitempty"`

	// Clusters of other namespaces, by namespace and name. Each is
	// registered only while a PgAdminAccess in its namespace grants this
	// PgAdmin that Cluster (ConfigurationValid=False, GrantMissing,
	// otherwise); the grant is the target namespace owner's decision.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=namespace
	// +listMapKey=name
	// +optional
	CrossNamespaceClusterRefs []PgAdminClusterReference `json:"crossNamespaceClusterRefs,omitempty"`

	// Which Services of every registered Cluster get a server.
	// +kubebuilder:default={"rw"}
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	// +optional
	Endpoints []Endpoint `json:"endpoints,omitempty"`

	// Which credentials each registered Cluster offers.
	// +kubebuilder:default={}
	// +optional
	Credentials PgAdminCredentialsSpec `json:"credentials,omitempty"`

	// The pgAdmin image. Omitted, the operator's digest-pinned default.
	// Changing the image backs up pgAdmin's settings database before the
	// new image rolls out (status.conditions: UpgradeBackup*).
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// The settings volume (pgAdmin's SQLite database and STORAGE_DIR).
	// Unused with configDatabase: STORAGE_DIR is then per Pod and
	// ephemeral.
	// +kubebuilder:default={}
	// +optional
	Storage PgAdminStorageSpec `json:"storage"`

	// pgAdmin's settings database on a CloudNativePG Cluster of this
	// namespace instead of SQLite on the settings volume (pgAdmin's
	// CONFIG_DATABASE_URI). The operator owns a DatabaseRole whose password
	// CloudNativePG generates (1.31 and later) and a Database it owns; the
	// password reaches the pgAdmin Pod only. Set at creation only.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="configDatabase is immutable",reason=FieldValueForbidden
	// +optional
	ConfigDatabase *PgAdminConfigDatabaseSpec `json:"configDatabase,omitempty"`

	// pgAdmin Pods. Above 1 only with configDatabase; the Service then keeps
	// a client on one Pod (sessionAffinity ClientIP), and the exposure must
	// be sticky too.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// How people sign in. pgAdmin has no local password login here.
	Auth PgAdminAuthSpec `json:"auth"`

	// Usernames (as the identity provider presents them) that get pgAdmin's
	// Administrator role; everyone else who signs in is a User.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=254
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:Pattern=`^[^-\s'"\\][^\s'"\\]*$`
	// +listType=set
	// +optional
	Admins []string `json:"admins,omitempty"`

	// How pgAdmin is published.
	// +kubebuilder:default={}
	// +optional
	Exposure ExposureSpec `json:"exposure"`

	// An optional NetworkPolicy around the pgAdmin Pod. Off by default: a
	// NetworkPolicy is the installing admin's choice, never a product
	// precondition. Whether it confines egress is reported by the advisory
	// PgAdminEgressConfined condition.
	// +kubebuilder:default={}
	// +optional
	NetworkPolicy PgAdminNetworkPolicySpec `json:"networkPolicy"`

	// The pgAdmin container's resources. Defaults to a request of 100m CPU
	// and 256Mi memory and a 1Gi memory limit.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Labels and annotations copied onto generated objects.
	// +kubebuilder:validation:MaxItems=8
	// +optional
	MetadataPropagation []MetadataPropagation `json:"metadataPropagation,omitempty"`
}

// PgAdminClusterReference names a Cluster of another namespace.
type PgAdminClusterReference struct {
	// The namespace of the Cluster.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`

	// The name of the Cluster.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

// PgAdminConfigDatabaseSpec puts pgAdmin's settings database on a
// CloudNativePG Cluster.
type PgAdminConfigDatabaseSpec struct {
	// The Cluster, in this namespace.
	ClusterRef LocalObjectReference `json:"clusterRef"`

	// The database, and the role that owns it. Defaults to
	// pgadmin_<PgAdmin name, dashes as underscores>.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +optional
	Database string `json:"database,omitempty"`
}

// PgAdminCredentialsSpec selects the credentials every registered Cluster
// offers. Each offered credential becomes one shared server per endpoint.
type PgAdminCredentialsSpec struct {
	// The application user (the Cluster's app Secret, or the Secret its
	// bootstrap names).
	// +kubebuilder:default=true
	// +optional
	Application *bool `json:"application,omitempty"`

	// The privileged credentials: the superuser (when the Cluster enables
	// superuser access) and DatabaseRoles that are superuser, createrole,
	// bypassrls or replication, or members, directly or through roles the
	// Cluster declares, of a pg_* role, the superuser, the application
	// owner or another privileged role. Off by default.
	// +kubebuilder:default=false
	// +optional
	Superuser *bool `json:"superuser,omitempty"`

	// DatabaseRoles of the Cluster that CloudNativePG reports applied, with
	// the password Secret at the version it applied. Privileged ones need
	// superuser as well. CloudNativePG 1.30 and later serve the
	// DatabaseRole kind; where an install does not (its CRDs lack it), none
	// is offered and the PgAdmin is still Ready: the ConfigurationValid
	// message says so. Where the operator may not list
	// DatabaseRoles, ConfigurationValid is False (DatabaseRoleAPIUnavailable).
	// +kubebuilder:default=true
	// +optional
	DatabaseRoles *bool `json:"databaseRoles,omitempty"`
}

// PgAdminRetainPolicy says what happens to the settings volume when the
// PgAdmin is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type PgAdminRetainPolicy string

const (
	// PgAdminRetainPolicyRetain keeps the settings volume (no owner
	// reference); a PgAdmin of the same name re-adopts it.
	PgAdminRetainPolicyRetain PgAdminRetainPolicy = "Retain"
	// PgAdminRetainPolicyDelete lets garbage collection delete it.
	PgAdminRetainPolicyDelete PgAdminRetainPolicy = "Delete"
)

// PgAdminStorageSpec configures the settings volume.
type PgAdminStorageSpec struct {
	// Requested size; may grow, never shrink. Defaults to 1Gi.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// The StorageClass; the cluster default when omitted. Immutable.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="storageClassName is immutable",reason=FieldValueForbidden
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// What happens to the volume when the PgAdmin is deleted.
	// +kubebuilder:default=Retain
	// +optional
	RetainPolicy PgAdminRetainPolicy `json:"retainPolicy,omitempty"`
}

// PgAdminAuthSpec selects the single sign-on mode: pgAdmin's native OAuth2
// against the platform's identity provider (Kubernetes), or the OpenShift
// oauth-proxy in front of pgAdmin, which admits only users that may get
// this PgAdmin (OpenShift).
// +kubebuilder:validation:XValidation:rule="has(self.oauth2) != has(self.openshiftOAuthProxy)",message="exactly one of oauth2 and openshiftOAuthProxy is required",reason=FieldValueInvalid
type PgAdminAuthSpec struct {
	// pgAdmin's native OAuth2 / OpenID Connect sign-in.
	// +optional
	OAuth2 *PgAdminOAuth2Spec `json:"oauth2,omitempty"`

	// The OpenShift oauth-proxy, authorizing with a SubjectAccessReview
	// "get pgadmins/<name>" (granted by the pgtoolbox-console-operator role).
	// +optional
	OpenShiftOAuthProxy *PgAdminOpenShiftOAuthProxySpec `json:"openshiftOAuthProxy,omitempty"`
}

// PgAdminOAuth2Spec configures pgAdmin's OAuth2 provider from an OpenID
// Connect issuer.
type PgAdminOAuth2Spec struct {
	// The issuer; pgAdmin reads <issuerURL>/.well-known/openid-configuration.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://[^\s'"\\]+$`
	IssuerURL string `json:"issuerURL"`

	// The OAuth2 client ID.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\s'"\\]+$`
	ClientID string `json:"clientID"`

	// The Secret (this namespace) holding the client secret. pgAdmin reads
	// it from a mounted file; it never enters the Pod spec or the operator.
	ClientSecretRef SecretKeySelector `json:"clientSecretRef"`

	// The claims a user must carry to sign in, e.g. {"groups": ["dba"]}.
	// Required: on Kubernetes the identity provider, not RBAC, decides who
	// reaches pgAdmin, so this must name the same people the namespace's
	// pgtoolbox-console-operator RoleBinding names.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=8
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[A-Za-z0-9_.:-]{1,64}$'))",message="claim names are 1-64 characters of letters, digits and _.:-",reason=FieldValueInvalid
	RequiredClaims map[string]ClaimValues `json:"requiredClaims"`

	// The claim that becomes the pgAdmin username.
	// +kubebuilder:default=email
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.:-]+$`
	// +optional
	UsernameClaim string `json:"usernameClaim,omitempty"`

	// Scopes requested.
	// +kubebuilder:default={"openid","email","profile"}
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_.:/-]{1,128}$`
	// +listType=set
	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// The button label on pgAdmin's login page.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[^'"\\\r\n]*$`
	// +optional
	DisplayName string `json:"displayName,omitempty"`
}

// ClaimValues are the accepted values of one claim; any one matches.
// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=16
// +kubebuilder:validation:items:MaxLength=256
// +kubebuilder:validation:items:Pattern=`^[^'"\\\r\n]+$`
type ClaimValues []string

// SecretKeySelector names a key of a Secret in the referencing object's
// namespace.
type SecretKeySelector struct {
	// The Secret name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// The key.
	// +kubebuilder:default=client-secret
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	Key string `json:"key,omitempty"`
}

// PgAdminOpenShiftOAuthProxySpec configures the OpenShift oauth-proxy.
type PgAdminOpenShiftOAuthProxySpec struct {
	// The oauth-proxy image. Omitted, the operator's default.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`
}

// PgAdminNetworkPolicySpec configures the optional NetworkPolicy.
type PgAdminNetworkPolicySpec struct {
	// Whether the operator renders a NetworkPolicy around the pgAdmin Pod
	// (ingress and egress). Off by default.
	// +kubebuilder:default=false
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Who may reach pgAdmin's port. Empty: any source (the router or
	// ingress controller of the exposure lives in a namespace only the
	// admin knows).
	// +kubebuilder:validation:MaxItems=16
	// +optional
	IngressFrom []networkingv1.NetworkPolicyPeer `json:"ingressFrom,omitempty"`

	// The identity provider's addresses (CIDRs), for pgAdmin's server-side
	// OAuth2 calls. Without them the generated rule allows port 443 to any
	// address, and PgAdminEgressConfined says so.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=49
	// +kubebuilder:validation:items:Pattern=`^[0-9a-fA-F:.]+/[0-9]{1,3}$`
	// +optional
	IdentityProviderCIDRs []string `json:"identityProviderCIDRs,omitempty"`

	// Additional egress rules, appended to the generated ones.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	ExtraEgress []networkingv1.NetworkPolicyEgressRule `json:"extraEgress,omitempty"`
}

// ServerState is the registration state of one offered server.
// +kubebuilder:validation:Enum=Registered;Pending;NotOffered
type ServerState string

const (
	// ServerStateRegistered means the last successful registration carries it.
	ServerStateRegistered ServerState = "Registered"
	// ServerStatePending means offered, not registered yet.
	ServerStatePending ServerState = "Pending"
	// ServerStateNotOffered marks a credential the Cluster has that this
	// PgAdmin does not offer; the message says why.
	ServerStateNotOffered ServerState = "NotOffered"
)

// PgAdminServerStatus is one server pgAdmin offers, or a credential it
// does not. It names the role, never a secret.
type PgAdminServerStatus struct {
	// The Cluster.
	// +kubebuilder:validation:MaxLength=253
	Cluster string `json:"cluster"`
	// The endpoint (rw, ro, r, pooler:<name>).
	// +kubebuilder:validation:MaxLength=70
	Endpoint string `json:"endpoint"`
	// The PostgreSQL role.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	User string `json:"user,omitempty"`
	// The pgAdmin user a personal server belongs to; empty for a shared
	// server.
	// +kubebuilder:validation:MaxLength=254
	// +optional
	PgAdminUser string `json:"pgAdminUser,omitempty"`
	// The registration state.
	State ServerState `json:"state"`
	// Why a credential is not offered.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Message string `json:"message,omitempty"`
}

// PgAdminStatus is the observed state of a PgAdmin.
type PgAdminStatus struct {
	// The generation the status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Where pgAdmin is reached: the exposure URL, or the in-cluster
	// Service URL for clusterIP exposure.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	URL string `json:"url,omitempty"`

	// The image the Ready workload runs (the resolved reference, with its
	// digest when pinned).
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`

	// The last image change the pre-upgrade backup released. It outlives
	// the backup Job (deleted an hour after it finishes), so that a new
	// image slow to become Ready is neither rolled back nor backed up again.
	// +optional
	UpgradeBackup *PgAdminUpgradeBackupStatus `json:"upgradeBackup,omitempty"`

	// The pgAdmin version the last registration Job reported.
	// +kubebuilder:validation:MaxLength=32
	// +optional
	PgAdminVersion string `json:"pgAdminVersion,omitempty"`

	// The hash of the offered set the last successful registration
	// applied.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	RegisteredHash string `json:"registeredHash,omitempty"`

	// The pgAdmin users the last successful registration gave personal
	// servers (per-person DatabaseRoles); the next registration removes
	// the managed personal servers of those no longer among them.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=254
	// +listType=set
	// +optional
	PersonalServerOwners []string `json:"personalServerOwners,omitempty"`

	// The servers pgAdmin offers and the credentials it does not.
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	// +optional
	Servers []PgAdminServerStatus `json:"servers,omitempty"`

	// Conditions: Ready (summary of ConfigurationValid, WorkloadReady,
	// ExposureReady and ServersSynced), Progressing, Stalled, and the
	// advisory PgAdminEgressConfined, which never enters Ready.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PgAdminUpgradeBackupStatus is one image change the pre-upgrade backup
// released.
type PgAdminUpgradeBackupStatus struct {
	// The image whose settings database was backed up: the image the Ready
	// workload ran before the change.
	// +kubebuilder:validation:MaxLength=512
	From string `json:"from"`

	// The image the backup released.
	// +kubebuilder:validation:MaxLength=512
	To string `json:"to"`

	// The hash of the change: it names the backup Job and labels the
	// backup file.
	// +kubebuilder:validation:MaxLength=64
	Hash string `json:"hash"`

	// The backup was not taken: the skip-upgrade-backup annotation named
	// this target.
	// +optional
	Skipped bool `json:"skipped,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pga,categories=pgtoolbox
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.pgAdminVersion`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40 && self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name must be a DNS-1035 label of at most 40 characters (it prefixes generated object names)",reason=FieldValueInvalid

// PgAdmin is one pgAdmin for a team namespace, registered against the
// CloudNativePG Clusters of that namespace. Only dba may open it: whoever
// reaches it holds the credentials of every role it offers.
type PgAdmin struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PgAdminSpec `json:"spec"`
	// +optional
	Status PgAdminStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PgAdminList contains a list of PgAdmin.
type PgAdminList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PgAdmin `json:"items"`
}
