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

// PgConsole condition types.
const (
	PgConsoleConditionReady              = "Ready"
	PgConsoleConditionProgressing        = "Progressing"
	PgConsoleConditionConfigurationValid = "ConfigurationValid"
	PgConsoleConditionRouteReady         = "RouteReady"
	// PgConsoleConditionRepositoryEvidenceReady reports the evidence
	// sidecar composition. False never blocks console readiness: evidence
	// degrades to unknown, it does not take the console down.
	PgConsoleConditionRepositoryEvidenceReady = "RepositoryEvidenceReady"
	// PgConsoleConditionProxyConfigReady reports the rendered proxy
	// configuration Secret from the PgToolBoxUser/PgToolBoxRole set.
	PgConsoleConditionProxyConfigReady = "ProxyConfigReady"
	// PgConsoleConditionClusterReady reports the resolution of the CNPG
	// Cluster the console is attached to. False never fails the object
	// hard: the controller keeps retrying until the Cluster exists.
	PgConsoleConditionClusterReady = "ClusterReady"
	// PgConsoleConditionPgAdminSynced reports whether pgAdmin accounts and
	// shared server definitions are synced for the console's users.
	PgConsoleConditionPgAdminSynced = "PgAdminSynced"
)

// PgToolBoxRole condition types.
const (
	RoleConditionReady          = "Ready"
	RoleConditionPgConsoleReady = "PgConsoleReady"
)

// PgToolBoxUser condition types.
const (
	UserConditionReady       = "Ready"
	UserConditionRoleReady   = "RoleReady"
	UserConditionProxySynced = "ProxySynced"
)

// PgToolBoxAccessRequest condition types.
const (
	AccessRequestConditionDecided   = "Decided"
	AccessRequestConditionUserReady = "UserReady"
)

// Shared condition reasons. Reasons are part of the API contract: they must
// be stable, and messages concise enough for automation.
const (
	ReasonAsExpected            = "AsExpected"
	ReasonReconciling           = "Reconciling"
	ReasonReconciliationSkipped = "ReconciliationSkipped"
	ReasonRolloutInProgress     = "RolloutInProgress"
	ReasonRolloutStuck          = "RolloutStuck"
	ReasonConfigurationInvalid  = "ConfigurationInvalid"
	ReasonSecretNotFound        = "SecretNotFound"
	ReasonSecretKeyMissing      = "SecretKeyMissing"
	ReasonSecretFormatInvalid   = "SecretFormatInvalid"
	ReasonRenderFailed          = "RenderFailed"
	ReasonNotAdmitted           = "NotAdmitted"
	ReasonNotAccepted           = "NotAccepted"
	ReasonGatewayNotFound       = "GatewayNotFound"
	ReasonHostnameConflict      = "HostnameConflict"
	ReasonNotRequested          = "NotRequested"
	ReasonAllApplied            = "AllApplied"
	ReasonSomeDegraded          = "SomeDegraded"
	ReasonNoneConfigured        = "NoneConfigured"

	ReasonPgConsoleNotFound = "PgConsoleNotFound"
	ReasonClusterNotFound   = "ClusterNotFound"
	ReasonClusterNotReady   = "ClusterNotReady"
	ReasonRoleNotFound      = "RoleNotFound"
	// #nosec G101 -- condition reason identifier; no credentials.
	ReasonCredentialLost      = "CredentialLost"
	ReasonDatabaseRolePending = "DatabaseRolePending"
	ReasonDatabaseRoleFailed  = "DatabaseRoleFailed"
	ReasonPendingRollout      = "PendingRollout"
	ReasonSyncFailed          = "SyncFailed"

	// ReasonCNPGAPIMissing reports that the CloudNativePG API group is not
	// installed on the cluster; discovery runs at operator startup.
	ReasonCNPGAPIMissing = "CNPGAPIMissing"
	// ReasonDatabaseRoleAPIMissing reports that the CloudNativePG
	// DatabaseRole API (CNPG >= 1.30) is not installed.
	ReasonDatabaseRoleAPIMissing = "DatabaseRoleAPIMissing"

	// Access-request decision reasons.
	ReasonPending  = "Pending"
	ReasonApproved = "Approved"
	ReasonDenied   = "Denied"

	// ReasonEvidenceDisabled reports that the evidence sidecar is turned
	// off by spec.evidence.enabled.
	ReasonEvidenceDisabled = "EvidenceDisabled"
	// ReasonImageRequired reports that a repository is discoverable but no
	// viewer image is configured to compose the evidence sidecar from.
	ReasonImageRequired = "ImageRequired"
	// ReasonObjectStoreNotFound covers the whole missing chain: no Barman
	// object store on the cluster, no served ObjectStore API, or a named
	// ObjectStore that does not exist.
	ReasonObjectStoreNotFound = "ObjectStoreNotFound"
	// ReasonUnsupportedCredentialMode reports a repository whose credential
	// or provider shape is outside the evidence contract's initial profile.
	ReasonUnsupportedCredentialMode = "UnsupportedCredentialMode"
	// ReasonUnsupportedAuthMode reports a proxy authentication mode this
	// build of the proxy does not implement yet (openshift).
	ReasonUnsupportedAuthMode = "UnsupportedAuthMode"
)
