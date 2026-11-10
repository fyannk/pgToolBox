---
sidebar_position: 1
title: API reference (v1alpha1)
---

:::note Generated
This page is generated from `api/v1alpha1` by `make crd-ref-docs` (crd-ref-docs) and
checked for drift by `make docs`. Do not edit it. Condition types and reasons
are on [Conditions and reasons](../operator/conditions.md).
:::

# API Reference

## Packages
- [pgtoolbox.fyannk.dev/v1alpha1](#pgtoolboxfyannkdevv1alpha1)


## pgtoolbox.fyannk.dev/v1alpha1

The kinds of the operator, `PgAdmin`, `EvidenceAgent` and `PgAccessRequest`. Field descriptions are
the Go doc comments of `api/v1alpha1`; the Validation column lists the
`kubebuilder` markers, and the cross-field CEL rules are collected at the end of
the page.


### Resource Types
- [EvidenceAgent](#evidenceagent)
- [EvidenceAgentList](#evidenceagentlist)
- [PgAccessRequest](#pgaccessrequest)
- [PgAccessRequestList](#pgaccessrequestlist)
- [PgAdmin](#pgadmin)
- [PgAdminAccess](#pgadminaccess)
- [PgAdminAccessList](#pgadminaccesslist)
- [PgAdminList](#pgadminlist)
- [PgToolBoxConsole](#pgtoolboxconsole)
- [PgToolBoxConsoleList](#pgtoolboxconsolelist)



#### AccessCapability

_Underlying type:_ _string_

AccessCapability is what is requested: one of the two ClusterRoles of
the console. viewer includes pod logs; operator includes
the console's operations and opening pgAdmin.

_Validation:_
- Enum: [viewer operator]

_Appears in:_
- [PgAccessRequestSpec](#pgaccessrequestspec)

| Field | Description |
| --- | --- |
| `viewer` | AccessCapabilityViewer binds the pgtoolbox-console-viewer ClusterRole.<br /> |
| `operator` | AccessCapabilityOperator binds the pgtoolbox-console-operator ClusterRole.<br /> |


#### AccessGrantScope

_Underlying type:_ _string_

AccessGrantScope is how far an approved request reaches.

_Validation:_
- Enum: [Namespace]

_Appears in:_
- [PgAccessRequestStatus](#pgaccessrequeststatus)

| Field | Description |
| --- | --- |
| `Namespace` |  |


#### AccessSubject



AccessSubject is who the access is for.



_Appears in:_
- [PgAccessRequestSpec](#pgaccessrequestspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _[AccessSubjectKind](#accesssubjectkind)_ | The subject's kind. |  | Enum: [User Group ServiceAccount] <br /> |
| `name` _string_ | The subject's name as the API server sees it: a username (with its<br />OIDC prefix, if the API server adds one), a group, or a<br />ServiceAccount name in this namespace. |  | MaxLength: 512 <br />MinLength: 1 <br />Pattern: `^[^\x00-\x1f\x7f]+$` <br /> |


#### AccessSubjectKind

_Underlying type:_ _string_

AccessSubjectKind is the kind of the subject access is requested for.

_Validation:_
- Enum: [User Group ServiceAccount]

_Appears in:_
- [AccessSubject](#accesssubject)

| Field | Description |
| --- | --- |
| `User` | AccessSubjectUser is a user as the API server names them (the<br />username a token review returns, with any OIDC prefix).<br /> |
| `Group` | AccessSubjectGroup is a group as the API server names it.<br /> |
| `ServiceAccount` | AccessSubjectServiceAccount is a ServiceAccount of the request's own<br />namespace (same-namespace references only).<br /> |




#### ClaimValues

_Underlying type:_ _string array_

ClaimValues are the accepted values of one claim; any one matches.

_Validation:_
- MaxItems: 16
- MinItems: 1
- items:MaxLength: 256
- items:Pattern: `^[^'"\\\r\n]+$`

_Appears in:_
- [PgAdminOAuth2Spec](#pgadminoauth2spec)



#### Endpoint

_Underlying type:_ _string_

Endpoint names which Service of a Cluster a server connects through:
rw (the primary), ro (the replicas), r (any instance) or
pooler:&lt;Pooler name> (a CNPG Pooler of that Cluster; its Service name
must be among the Cluster's spec.certificates.serverAltDNSNames for
verify-full to succeed).

_Validation:_
- MaxLength: 70
- Pattern: `^(rw|ro|r|pooler:[a-z0-9]([-a-z0-9]*[a-z0-9])?)$`

_Appears in:_
- [PgAdminSpec](#pgadminspec)

| Field | Description |
| --- | --- |
| `rw` |  |
| `ro` |  |
| `r` |  |


#### EvidenceAgent



EvidenceAgent deploys a read-only evidence agent for the backup repository
of one CloudNativePG Cluster in its namespace. Whoever may get an
EvidenceAgent may read the evidence it serves.



_Appears in:_
- [EvidenceAgentList](#evidenceagentlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `EvidenceAgent` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EvidenceAgentSpec](#evidenceagentspec)_ |  |  |  |
| `status` _[EvidenceAgentStatus](#evidenceagentstatus)_ |  |  | Optional: \{\} <br /> |


#### EvidenceAgentCertificateIssuer

_Underlying type:_ _string_

EvidenceAgentCertificateIssuer selects who signs the agent's serving
certificate.

_Validation:_
- Enum: [Operator OpenShiftServiceCA]

_Appears in:_
- [EvidenceAgentTLSSpec](#evidenceagenttlsspec)

| Field | Description |
| --- | --- |
| `Operator` | CertificateIssuerOperator means the operator keeps a CA per EvidenceAgent<br />(a Secret the agent Pod never mounts) and signs a renewable serving<br />certificate with it.<br /> |
| `OpenShiftServiceCA` | CertificateIssuerOpenShiftServiceCA means the OpenShift service CA issues<br />the certificate through the Service annotation and injects its CA<br />bundle into a ConfigMap the operator creates.<br /> |


#### EvidenceAgentList



EvidenceAgentList contains a list of EvidenceAgent.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `EvidenceAgentList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[EvidenceAgent](#evidenceagent) array_ |  |  |  |


#### EvidenceAgentPgBackRestSpec



EvidenceAgentPgBackRestSpec selects the pgBackRest repository the agent
reads. The stanza, the bucket or container, the path, the credentials and
the cipher pass come from the plugin's Stanza or Archive; every Secret
they name, and endpointCA, must be listed in spec.allowedSecrets.



_Appears in:_
- [EvidenceAgentSpec](#evidenceagentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `repository` _integer_ | The pgBackRest repository number (repo&lt;n>) to read. Dalibo numbers<br />the Stanza's S3 repositories first, then its Azure ones; Opera<br />numbers the Archive's repositories in order. 1 is where both plugins<br />send a backup that names no repository. | 1 | Maximum: 256 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `endpointCA` _[SecretKeyReference](#secretkeyreference)_ | A CA bundle (a key of a Secret in this namespace) the agent trusts for<br />the repository endpoint, beside the system roots. It wins over the<br />Archive's endpointCA. TLS is always verified: Dalibo's verifyTLS and<br />Opera's disableVerifyTLS are not followed. |  | Optional: \{\} <br /> |
| `endpoint` _string_ | Pins the repository's endpoint: a host[:port] (a DNS name, an IPv4<br />address or a bracketed IPv6 address) that the host the agent sends<br />its requests and credentials to must match, or the Cluster is refused<br />(UnsupportedObjectStore). That host is the S3 endpoint the plugin's<br />object names, or the Azure account's Blob host<br />(&lt;account>.blob.core.windows.net host-style, the endpoint path-style).<br />Without a port, the scheme's default port. Whoever may edit the<br />Stanza or the Archive chooses where the credentials go; the pin keeps<br />that choice with this object's author. It is also the opt-in that<br />lets an Azure storage key or SAS token go to a host outside the Azure<br />Blob domains (Azurite). |  | MaxLength: 261 <br />Pattern: `^(\[[0-9A-Fa-f:.]+\]\|[A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?([.][A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?)*)(:[0-9]\{1,5\})?$` <br />Optional: \{\} <br /> |


#### EvidenceAgentPgBackRestStatus



EvidenceAgentPgBackRestStatus is the pgBackRest repository an agent
resolved: the plugin and its object as soon as the Cluster names them,
the rest once the object was read.



_Appears in:_
- [EvidenceAgentStatus](#evidenceagentstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `plugin` _string_ | The plugin: pgbackrest.dalibo.com or pgbackrest.cnpg.opera.com. |  | MaxLength: 253 <br /> |
| `kind` _string_ | The plugin's object in this namespace: a Stanza (Dalibo) or an<br />Archive (Opera). |  | Enum: [Stanza Archive] <br /> |
| `objectName` _string_ |  |  | MaxLength: 253 <br /> |
| `stanza` _string_ | The stanza the backups are read under. |  | MaxLength: 256 <br />Optional: \{\} <br /> |
| `archiveStanza` _string_ | The stanza the WAL is read under, when it differs from stanza<br />(Opera's configuration.stanza): a restore must name it. |  | MaxLength: 256 <br />Optional: \{\} <br /> |
| `repository` _integer_ | The repository number read (repo&lt;n>). |  | Optional: \{\} <br /> |


#### EvidenceAgentSpec



EvidenceAgentSpec declares one evidence agent: a read-only reader of the
backup repository of one CloudNativePG Cluster in this namespace, serving
evidence/v1alpha1 to the console over in-cluster HTTPS.



_Appears in:_
- [EvidenceAgent](#evidenceagent)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterRef` _[LocalObjectReference](#localobjectreference)_ | The CloudNativePG Cluster, in this namespace, whose backups the agent<br />reads. Its barman-cloud plugin stanza names the ObjectStore and the<br />server name unless the fields below override them. |  |  |
| `objectStoreRef` _[LocalObjectReference](#localobjectreference)_ | The barman-cloud ObjectStore (barmancloud.cnpg.io/v1), in this<br />namespace, to read. Defaults to the Cluster's barmanObjectName<br />plugin parameter. The agent uses the same credentials the plugin<br />uses; they never leave this namespace. |  | Optional: \{\} <br /> |
| `allowedSecrets` _string array_ | The Secrets, in this namespace, the agent may read: whoever may edit<br />the Cluster or the ObjectStore chooses which Secrets the backup<br />configuration names, and the agent reads only those also listed<br />here. A configuration naming another is refused by name<br />(ConfigurationValid=False, UnsupportedObjectStore). Empty is the<br />deprecated behaviour, kept for an ObjectStore of the barman-cloud<br />plugin only: every Secret that ObjectStore names; the Cluster's own<br />spec.backup.barmanObjectStore is then refused. |  | MaxItems: 16 <br />items:MaxLength: 253 <br />items:Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$` <br />Optional: \{\} <br /> |
| `allowInsecureEndpoint` _boolean_ | Admit a plain-http object-store endpoint, over which the credentials<br />cross the network in clear text. Off, such an endpoint is refused<br />(UnsupportedObjectStore), except for the deprecated behaviour of an<br />empty allowedSecrets. |  | Optional: \{\} <br /> |
| `serverName` _string_ | The Barman server name (the first key prefix under the destination).<br />Defaults to the Cluster's serverName plugin parameter, else the<br />Cluster's name — the same rule the barman-cloud plugin applies. |  | MaxLength: 256 <br />MinLength: 1 <br />Optional: \{\} <br /> |
| `image` _[ImageSpec](#imagespec)_ | The agent image. Omitted, the operator's default applies. The<br />repository must be in the operator's allow-list<br />(--evidence-agent-image-repositories; by default the default image's<br />repository only): the agent Pod mounts the object-store credentials. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#resourcerequirements-v1-core)_ | Compute resources of the agent container. |  | Optional: \{\} <br /> |
| `refreshInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | How often the agent re-reads the repository. |  | Optional: \{\} <br /> |
| `exposure` _[ExposureSpec](#exposurespec)_ | Exposure of the agent. Only clusterIP is accepted: the console in the<br />cluster is the agent's only client. |  | Optional: \{\} <br /> |
| `tls` _[EvidenceAgentTLSSpec](#evidenceagenttlsspec)_ | Serving certificate. |  | Optional: \{\} <br /> |
| `workloadIdentity` _[WorkloadIdentitySpec](#workloadidentityspec)_ | Workload identity of the agent, for a repository read with the Pod's<br />own cloud identity (s3Credentials.inheritFromIAMRole,<br />azureCredentials.inheritFromAzureAD or useDefaultAzureCredentials,<br />googleCredentials.gkeEnvironment, a pgBackRest key type auto or<br />web-id): the agent's ServiceAccount and Pod carry what the platform's<br />identity webhook or metadata server needs to grant the agent a role<br />that can read the repository. |  | Optional: \{\} <br /> |
| `pgbackrest` _[EvidenceAgentPgBackRestSpec](#evidenceagentpgbackrestspec)_ | pgBackRest settings, for a Cluster backed up with a pgBackRest plugin<br />(pgbackrest.dalibo.com or pgbackrest.cnpg.opera.com). Omitted, such a<br />Cluster is read from repo1 with the system's trusted roots. |  | Optional: \{\} <br /> |


#### EvidenceAgentStatus



EvidenceAgentStatus is the observed state of an EvidenceAgent. A console
reads endpoint, caBundle, fingerprint and producerEpoch to call the agent
and to judge its answers (docs: the evidence agent channel).



_Appears in:_
- [EvidenceAgent](#evidenceagent)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | The generation the status was computed from. |  | Optional: \{\} <br /> |
| `endpoint` _string_ | The in-cluster HTTPS URL of the agent's Service. |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `caBundle` _integer array_ | PEM CA bundle that verifies the agent's serving certificate for the<br />endpoint's host. Public material, never a key. |  | MaxLength: 65536 <br />Optional: \{\} <br /> |
| `fingerprint` _string_ | The repository identity the agent reads (sha256: domain-separated,<br />region-free fingerprint of endpoint, bucket, prefix, format and<br />server); a snapshot whose destination_fingerprint differs is not this<br />repository's. |  | Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Optional: \{\} <br /> |
| `producerEpoch` _integer_ | The configuration epoch of the running agent: the generation its<br />Deployment was rendered from, published once that rollout completed.<br />Every snapshot carries it as producer_epoch; a snapshot with an older<br />epoch comes from a superseded configuration. |  | Optional: \{\} <br /> |
| `objectStoreName` _string_ | The resolved ObjectStore and Barman server name. |  | MaxLength: 253 <br />Optional: \{\} <br /> |
| `serverName` _string_ |  |  | MaxLength: 256 <br />Optional: \{\} <br /> |
| `pgbackrest` _[EvidenceAgentPgBackRestStatus](#evidenceagentpgbackreststatus)_ | The resolved pgBackRest repository, for a Cluster backed up with a<br />pgBackRest plugin. |  | Optional: \{\} <br /> |
| `image` _string_ | The image reference the agent runs. |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#condition-v1-meta) array_ | Conditions: Ready (summary), ConfigurationValid, WorkloadReady,<br />ServingCertificateReady, Progressing and Stalled. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### EvidenceAgentTLSSpec



EvidenceAgentTLSSpec configures the agent's serving certificate.



_Appears in:_
- [EvidenceAgentSpec](#evidenceagentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `issuer` _[EvidenceAgentCertificateIssuer](#evidenceagentcertificateissuer)_ | Who issues the serving certificate. | Operator | Enum: [Operator OpenShiftServiceCA] <br />Optional: \{\} <br /> |


#### ExposureSpec



ExposureSpec configures access to a workload from outside its Service.



_Appears in:_
- [EvidenceAgentSpec](#evidenceagentspec)
- [PgAdminSpec](#pgadminspec)
- [PgToolBoxConsoleExposureSpec](#pgtoolboxconsoleexposurespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[ExposureType](#exposuretype)_ | The exposure mechanism. | clusterIP | Enum: [clusterIP route ingress gateway] <br />Optional: \{\} <br /> |
| `hostname` _string_ | The external hostname. Required for route, ingress and gateway. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$` <br />Optional: \{\} <br /> |
| `tls` _[ExposureTLSSpec](#exposuretlsspec)_ | TLS for route and ingress exposure. A Route always terminates TLS at<br />the edge; an Ingress uses certificateSecretRef, or the controller's<br />default certificate when it is omitted. For gateway exposure TLS<br />terminates at the Gateway listener. |  | Optional: \{\} <br /> |
| `ingressClassName` _string_ | IngressClass name, for ingress exposure only. |  | MaxLength: 253 <br />Optional: \{\} <br /> |
| `gateway` _[GatewayExposureSpec](#gatewayexposurespec)_ | Gateway API attachment, for gateway exposure only. |  | Optional: \{\} <br /> |
| `annotations` _object (keys:string, values:string)_ | Annotations added to the exposure object; keys under<br />pgtoolbox.fyannk.dev/ are rejected. |  | MaxProperties: 32 <br />Optional: \{\} <br /> |
| `labels` _object (keys:string, values:string)_ | Labels added to the exposure object under the operator's own<br />identity labels, which always win. Ingress controllers and OpenShift<br />router shards select by label; keys under pgtoolbox.fyannk.dev/ are<br />rejected. |  | MaxProperties: 32 <br />Optional: \{\} <br /> |


#### ExposureTLSSpec



ExposureTLSSpec configures TLS for route and ingress exposure.



_Appears in:_
- [ExposureSpec](#exposurespec)
- [PgToolBoxConsoleExposureSpec](#pgtoolboxconsoleexposurespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `certificateSecretRef` _[LocalObjectReference](#localobjectreference)_ | TLS certificate Secret (kubernetes.io/tls) in the CR's namespace,<br />for ingress exposure. |  | Optional: \{\} <br /> |


#### ExposureType

_Underlying type:_ _string_

ExposureType selects how a workload is published.

_Validation:_
- Enum: [clusterIP route ingress gateway]

_Appears in:_
- [ExposureSpec](#exposurespec)
- [PgToolBoxConsoleExposureSpec](#pgtoolboxconsoleexposurespec)

| Field | Description |
| --- | --- |
| `clusterIP` | ExposureTypeClusterIP exposes the workload only inside the cluster.<br /> |
| `route` | ExposureTypeRoute publishes an OpenShift Route (edge TLS).<br /> |
| `ingress` | ExposureTypeIngress publishes a Kubernetes Ingress.<br /> |
| `gateway` | ExposureTypeGateway publishes a Gateway API HTTPRoute.<br /> |


#### GatewayExposureSpec



GatewayExposureSpec configures the HTTPRoute parent reference.



_Appears in:_
- [ExposureSpec](#exposurespec)
- [PgToolBoxConsoleExposureSpec](#pgtoolboxconsoleexposurespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `parentRef` _[GatewayParentRef](#gatewayparentref)_ | The Gateway listener to attach to. |  |  |


#### GatewayParentRef



GatewayParentRef identifies a Gateway listener. A Gateway in another
namespace admits the route only through its own listener's allowedRoutes
(and a ReferenceGrant where its implementation needs one): the grant is
the Gateway owner's, never this object's.



_Appears in:_
- [GatewayExposureSpec](#gatewayexposurespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Gateway name. |  | MaxLength: 253 <br />MinLength: 1 <br /> |
| `namespace` _string_ | Gateway namespace; defaults to the CR's namespace. |  | MaxLength: 63 <br />Optional: \{\} <br /> |
| `sectionName` _string_ | Listener (section) name on the Gateway. |  | MaxLength: 253 <br />Optional: \{\} <br /> |


#### ImageSpec



ImageSpec selects a container image. Omitted, the operator's default
image applies — a digest-pinned reference built into the operator and
echoed into the kind's status. A tag alone is accepted but the resolved
reference in status says it is not pinned; production use names a digest.



_Appears in:_
- [EvidenceAgentSpec](#evidenceagentspec)
- [PgAdminOpenShiftOAuthProxySpec](#pgadminopenshiftoauthproxyspec)
- [PgAdminSpec](#pgadminspec)
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `repository` _string_ | Image repository, without tag or digest (registry/path/name). |  | MaxLength: 255 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]\{1,5\})?(/[a-z0-9]+([._-][a-z0-9]+)*)*$` <br /> |
| `tag` _string_ | Image tag; informative when a digest is set. |  | MaxLength: 128 <br />Pattern: `^[A-Za-z0-9_][A-Za-z0-9_.-]\{0,127\}$` <br />Optional: \{\} <br /> |
| `digest` _string_ | Image digest; when set it is what the Pod runs. |  | Pattern: `^sha256:[a-f0-9]\{64\}$` <br />Optional: \{\} <br /> |
| `pullPolicy` _[PullPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#pullpolicy-v1-core)_ | Image pull policy. Defaults to IfNotPresent for a digest and Always<br />for a tag alone. |  | Enum: [Always IfNotPresent Never] <br />Optional: \{\} <br /> |
| `pullSecrets` _[LocalObjectReference](#localobjectreference) array_ | Pull secrets for the image registry, in the CR's namespace. |  | MaxItems: 8 <br />Optional: \{\} <br /> |


#### LabelValue

_Underlying type:_ _string_

LabelValue is a Kubernetes label value (the keys are allow-listed by the
map's own rule).

_Validation:_
- MaxLength: 63
- Pattern: `^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`

_Appears in:_
- [WorkloadIdentitySpec](#workloadidentityspec)



#### LocalObjectReference



LocalObjectReference names an object in the referencing object's own
namespace. Cross-namespace references do not exist in v1alpha1; a future
one goes through an explicit grant object in the target
namespace, never through a selector.



_Appears in:_
- [EvidenceAgentSpec](#evidenceagentspec)
- [ExposureTLSSpec](#exposuretlsspec)
- [ImageSpec](#imagespec)
- [PgAdminAccessSpec](#pgadminaccessspec)
- [PgAdminConfigDatabaseSpec](#pgadminconfigdatabasespec)
- [PgAdminSpec](#pgadminspec)
- [PgToolBoxConsoleOIDCSpec](#pgtoolboxconsoleoidcspec)
- [PgToolBoxConsoleOpenShiftSpec](#pgtoolboxconsoleopenshiftspec)
- [PgToolBoxConsolePrometheusSpec](#pgtoolboxconsoleprometheusspec)
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | The name of the referenced object. |  | MaxLength: 253 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` <br /> |


#### MetadataPropagation



MetadataPropagation copies labels and annotations onto generated
objects (cost-centre labels, backup-tool annotations, mesh opt-outs). The
operator's own keys always win, keys under pgtoolbox.fyannk.dev/ are
rejected, and a change rolls only the objects it targets.



_Appears in:_
- [PgAdminSpec](#pgadminspec)
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `target` _[PropagationTarget](#propagationtarget)_ | Which generated objects receive the metadata. |  | Enum: [All Workload Pod Service Exposure Storage] <br /> |
| `labels` _object (keys:string, values:string)_ | Labels to add. |  | MaxProperties: 32 <br />Optional: \{\} <br /> |
| `annotations` _object (keys:string, values:string)_ | Annotations to add. |  | MaxProperties: 32 <br />Optional: \{\} <br /> |


#### PgAccessRequest



PgAccessRequest asks for one of the console's two ClusterRoles
(pgtoolbox-console-viewer, pgtoolbox-console-operator) in this namespace. Whoever may
create RoleBindings here and update the request's status approves it
through the console, which creates the RoleBinding with the approver's own
token; the operator deletes it when the request expires, is revoked or is
deleted.



_Appears in:_
- [PgAccessRequestList](#pgaccessrequestlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `PgAccessRequest` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[PgAccessRequestSpec](#pgaccessrequestspec)_ |  |  |  |
| `status` _[PgAccessRequestStatus](#pgaccessrequeststatus)_ |  |  | Optional: \{\} <br /> |


#### PgAccessRequestList



PgAccessRequestList contains a list of PgAccessRequest.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `PgAccessRequestList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[PgAccessRequest](#pgaccessrequest) array_ |  |  |  |


#### PgAccessRequestPhase

_Underlying type:_ _string_

PgAccessRequestPhase is where a request stands.

_Validation:_
- Enum: [Pending Approved Denied Expired Revoked]

_Appears in:_
- [PgAccessRequestStatus](#pgaccessrequeststatus)

| Field | Description |
| --- | --- |
| `Pending` | AccessPhasePending is waiting for a decision.<br /> |
| `Approved` | AccessPhaseApproved means a RoleBinding grants the capability.<br /> |
| `Denied` | AccessPhaseDenied is a refusal; terminal.<br /> |
| `Expired` | AccessPhaseExpired means expiresAt passed; terminal.<br /> |
| `Revoked` | AccessPhaseRevoked means the grant was withdrawn; terminal.<br /> |


#### PgAccessRequestSpec



PgAccessRequestSpec is a request for access to the CloudNativePG
Clusters of this namespace through the console. A request is immutable:
to change it, make a new one.



_Appears in:_
- [PgAccessRequest](#pgaccessrequest)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `subject` _[AccessSubject](#accesssubject)_ | Who the access is for. |  |  |
| `cluster` _string_ | The Cluster, in this namespace, the access is wanted for. Empty: every<br />Cluster of the namespace. Informative only: a RoleBinding cannot be<br />narrowed to one Cluster, so an approved request grants the<br />capability on every Cluster of the namespace, and its status says so. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` <br />Optional: \{\} <br /> |
| `capability` _[AccessCapability](#accesscapability)_ | The capability requested. |  | Enum: [viewer operator] <br /> |
| `justification` _string_ | Why the access is needed; shown to whoever decides. |  | MaxLength: 1024 <br />MinLength: 1 <br /> |
| `expiresAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | When the access ends. The operator deletes the RoleBinding at this<br />time and marks the request Expired; a Pending request past it can no<br />longer be approved. Omitted: until revoked. |  | Optional: \{\} <br /> |


#### PgAccessRequestStatus



PgAccessRequestStatus is the decision and its effect. The decision
fields are written by the deciding person (through the console, with
their own token: the API server records who wrote them); the conditions
by the operator.



_Appears in:_
- [PgAccessRequest](#pgaccessrequest)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | The generation the conditions were computed from. |  | Optional: \{\} <br /> |
| `phase` _[PgAccessRequestPhase](#pgaccessrequestphase)_ | Where the request stands. Empty until the operator or a decision<br />first writes it. |  | Enum: [Pending Approved Denied Expired Revoked] <br />Optional: \{\} <br /> |
| `decidedBy` _string_ | The username of the person who approved or denied the request, as<br />the API server named them when the console asked. |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `decidedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | When the request was approved or denied. |  | Optional: \{\} <br /> |
| `revokedBy` _string_ | The username of the person who revoked the grant, or<br />"pgtoolbox-operator" when the operator revoked a self-approval. |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `revokedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | When the grant was revoked. |  | Optional: \{\} <br /> |
| `note` _string_ | The decider's note: why it was denied (required), approved or<br />revoked; or the operator's explanation of a revocation. |  | MaxLength: 1024 <br />Optional: \{\} <br /> |
| `roleBinding` _string_ | The RoleBinding of an approved request, in this namespace. |  | MaxLength: 253 <br />Optional: \{\} <br /> |
| `clusterRole` _string_ | The ClusterRole the RoleBinding references. |  | MaxLength: 253 <br />Optional: \{\} <br /> |
| `grantedScope` _[AccessGrantScope](#accessgrantscope)_ | How far the grant reaches: always Namespace, since a RoleBinding<br />cannot be narrowed to one Cluster, even when spec.cluster names one. |  | Enum: [Namespace] <br />Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#condition-v1-meta) array_ | Conditions: Ready (summary), ConfigurationValid, RoleBindingSynced,<br />Progressing and Stalled. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### PgAdmin



PgAdmin is one pgAdmin for a team namespace, registered against the
CloudNativePG Clusters of that namespace. Only dba may open it: whoever
reaches it holds the credentials of every role it offers.



_Appears in:_
- [PgAdminList](#pgadminlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `PgAdmin` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[PgAdminSpec](#pgadminspec)_ |  |  |  |
| `status` _[PgAdminStatus](#pgadminstatus)_ |  |  | Optional: \{\} <br /> |


#### PgAdminAccess



PgAdminAccess is a grant, in the namespace of the Clusters it names: the
PgAdmin it names, in another namespace, may register those Clusters and
read their offered credentials. Creating one is the decision of whoever
may read this namespace's Secrets; who may create PgAdminAccess objects
here is therefore an RBAC decision of the namespace owner. Like the
Gateway API's ReferenceGrant it has no status: the PgAdmin reports
whether it is granted (ConfigurationValid, GrantMissing). Deleting it
revokes the access (the Role and RoleBinding it owns are garbage
collected).



_Appears in:_
- [PgAdminAccessList](#pgadminaccesslist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `PgAdminAccess` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[PgAdminAccessSpec](#pgadminaccessspec)_ |  |  |  |


#### PgAdminAccessList



PgAdminAccessList contains a list of PgAdminAccess.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `PgAdminAccessList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[PgAdminAccess](#pgadminaccess) array_ |  |  |  |


#### PgAdminAccessSpec



PgAdminAccessSpec grants one PgAdmin of another namespace the
registration of named Clusters of this namespace.



_Appears in:_
- [PgAdminAccess](#pgadminaccess)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `pgAdmin` _[PgAdminReference](#pgadminreference)_ | The PgAdmin granted access. |  |  |
| `clusters` _[LocalObjectReference](#localobjectreference) array_ | The Clusters of this namespace it may register. Their credentials<br />(the Secrets its spec.credentials offers and each Cluster's CA) become<br />readable by that PgAdmin's ServiceAccount through a name-scoped Role<br />the operator maintains here while this grant exists. |  | MaxItems: 32 <br />MinItems: 1 <br /> |
| `allowPrivileged` _boolean_ | Whether the privileged credentials of these Clusters (the superuser,<br />and DatabaseRoles that are superuser, createrole, bypassrls or<br />replication or members of a privileged role) may be offered, when the<br />PgAdmin asks for them (spec.credentials.superuser). Off by default:<br />the PgAdmin's own spec cannot widen what this grant allows. | false | Optional: \{\} <br /> |


#### PgAdminAuthSpec



PgAdminAuthSpec selects the single sign-on mode: pgAdmin's native OAuth2
against the platform's identity provider (Kubernetes), or the OpenShift
oauth-proxy in front of pgAdmin, which admits only users that may connect
to this PgAdmin (OpenShift).



_Appears in:_
- [PgAdminSpec](#pgadminspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `oauth2` _[PgAdminOAuth2Spec](#pgadminoauth2spec)_ | pgAdmin's native OAuth2 / OpenID Connect sign-in. |  | Optional: \{\} <br /> |
| `openshiftOAuthProxy` _[PgAdminOpenShiftOAuthProxySpec](#pgadminopenshiftoauthproxyspec)_ | The OpenShift oauth-proxy, authorizing with a SubjectAccessReview<br />"connect pgadmins/&lt;name>" (granted by the pgtoolbox-console-operator<br />role; not get, which the view role carries on OLM). |  | Optional: \{\} <br /> |


#### PgAdminClusterReference



PgAdminClusterReference names a Cluster of another namespace.



_Appears in:_
- [PgAdminSpec](#pgadminspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespace` _string_ | The namespace of the Cluster. |  | MaxLength: 63 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `name` _string_ | The name of the Cluster. |  | MaxLength: 253 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |


#### PgAdminConfigDatabaseSpec



PgAdminConfigDatabaseSpec puts pgAdmin's settings database on a
CloudNativePG Cluster.



_Appears in:_
- [PgAdminSpec](#pgadminspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterRef` _[LocalObjectReference](#localobjectreference)_ | The Cluster, in this namespace. |  |  |
| `database` _string_ | The database, and the role that owns it. Defaults to<br />pgadmin_&lt;PgAdmin name, dashes as underscores>; a name given here<br />starts with pgadmin_ too. CloudNativePG adopts a role of that name<br />that already exists in PostgreSQL and resets its password and<br />memberships: the prefix keeps the PgAdmin off the roles a DBA or an<br />application created. |  | MaxLength: 63 <br />Pattern: `^pgadmin_[a-z0-9_]+$` <br />Optional: \{\} <br /> |


#### PgAdminCredentialsSpec



PgAdminCredentialsSpec selects the credentials every registered Cluster
offers. Each offered credential becomes one shared server per endpoint.



_Appears in:_
- [PgAdminSpec](#pgadminspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `application` _boolean_ | The application user (the Cluster's app Secret, or the Secret its<br />bootstrap names). | true | Optional: \{\} <br /> |
| `superuser` _boolean_ | The privileged credentials: the superuser (when the Cluster enables<br />superuser access) and DatabaseRoles that are superuser, createrole,<br />bypassrls or replication, or members, directly or through roles the<br />Cluster declares, of a pg_* role, the superuser, the application<br />owner or another privileged role. Off by default. | false | Optional: \{\} <br /> |
| `databaseRoles` _boolean_ | DatabaseRoles of the Cluster that CloudNativePG reports applied, with<br />the password Secret at the version it applied. Privileged ones need<br />superuser as well. CloudNativePG 1.30 and later serve the<br />DatabaseRole kind; where an install does not (its CRDs lack it), none<br />is offered and the PgAdmin is still Ready: the ConfigurationValid<br />message says so. Where the operator may not list<br />DatabaseRoles, ConfigurationValid is False (DatabaseRoleAPIUnavailable). | true | Optional: \{\} <br /> |


#### PgAdminList



PgAdminList contains a list of PgAdmin.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `PgAdminList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[PgAdmin](#pgadmin) array_ |  |  |  |


#### PgAdminNetworkPolicySpec



PgAdminNetworkPolicySpec configures the optional NetworkPolicy.



_Appears in:_
- [PgAdminSpec](#pgadminspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Whether the operator renders a NetworkPolicy around the pgAdmin Pod<br />(ingress and egress). Off by default. | false | Optional: \{\} <br /> |
| `ingressFrom` _[NetworkPolicyPeer](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#networkpolicypeer-v1-networking) array_ | Who may reach pgAdmin's port. Empty: any source (the router or<br />ingress controller of the exposure lives in a namespace only the<br />admin knows). |  | MaxItems: 16 <br />Optional: \{\} <br /> |
| `identityProviderCIDRs` _string array_ | The identity provider's addresses (CIDRs), for pgAdmin's server-side<br />OAuth2 calls. Without them the generated rule allows port 443 to any<br />address, and PgAdminEgressConfined says so. |  | MaxItems: 16 <br />items:MaxLength: 49 <br />items:Pattern: `^[0-9a-fA-F:.]+/[0-9]\{1,3\}$` <br />Optional: \{\} <br /> |
| `extraEgress` _[NetworkPolicyEgressRule](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#networkpolicyegressrule-v1-networking) array_ | Additional egress rules, appended to the generated ones. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### PgAdminOAuth2Spec



PgAdminOAuth2Spec configures pgAdmin's OAuth2 provider from an OpenID
Connect issuer.



_Appears in:_
- [PgAdminAuthSpec](#pgadminauthspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `issuerURL` _string_ | The issuer; pgAdmin reads &lt;issuerURL>/.well-known/openid-configuration. |  | MaxLength: 512 <br />Pattern: `^https://[^\s'"\\]+$` <br /> |
| `clientID` _string_ | The OAuth2 client ID. |  | MaxLength: 256 <br />MinLength: 1 <br />Pattern: `^[^\s'"\\]+$` <br /> |
| `clientSecretRef` _[SecretKeySelector](#secretkeyselector)_ | The Secret (this namespace) holding the client secret. pgAdmin reads<br />it from a mounted file; it never enters the Pod spec or the operator. |  |  |
| `requiredClaims` _object (keys:string, values:[ClaimValues](#claimvalues))_ | The claims a user must carry to sign in, e.g. \{"groups": ["dba"]\}.<br />Required: on Kubernetes the identity provider, not RBAC, decides who<br />reaches pgAdmin, so this must name the same people the namespace's<br />pgtoolbox-console-operator RoleBinding names. |  | MaxProperties: 8 <br />MinProperties: 1 <br /> |
| `usernameClaim` _string_ | The claim that becomes the pgAdmin username. | email | MaxLength: 64 <br />Pattern: `^[A-Za-z0-9_.:-]+$` <br />Optional: \{\} <br /> |
| `scopes` _string array_ | Scopes requested. | [openid email profile] | MaxItems: 16 <br />items:Pattern: `^[A-Za-z0-9_.:/-]\{1,128\}$` <br />Optional: \{\} <br /> |
| `displayName` _string_ | The button label on pgAdmin's login page. |  | MaxLength: 64 <br />Pattern: `^[^'"\\\r\n]*$` <br />Optional: \{\} <br /> |


#### PgAdminOpenShiftOAuthProxySpec



PgAdminOpenShiftOAuthProxySpec configures the OpenShift oauth-proxy.



_Appears in:_
- [PgAdminAuthSpec](#pgadminauthspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _[ImageSpec](#imagespec)_ | The oauth-proxy image. Omitted, the operator's default. |  | Optional: \{\} <br /> |
| `sessionMaxAge` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | How long an oauth-proxy session lasts (--cookie-expire). The proxy<br />reviews connect at sign-in only, so this bounds how long a revoked<br />person keeps pgAdmin open; when it ends the proxy signs the person in<br />again, silently while their OpenShift session lasts, but a request<br />in flight (a form being posted) can be lost. Default 1h. |  | Optional: \{\} <br /> |


#### PgAdminReference



PgAdminReference names a PgAdmin, in any namespace.



_Appears in:_
- [PgAdminAccessSpec](#pgadminaccessspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespace` _string_ | The namespace of the PgAdmin. |  | MaxLength: 63 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `name` _string_ | The name of the PgAdmin. |  | MaxLength: 40 <br />MinLength: 1 <br />Pattern: `^[a-z]([-a-z0-9]*[a-z0-9])?$` <br /> |


#### PgAdminRetainPolicy

_Underlying type:_ _string_

PgAdminRetainPolicy says what happens to the settings volume when the
PgAdmin is deleted.

_Validation:_
- Enum: [Retain Delete]

_Appears in:_
- [PgAdminStorageSpec](#pgadminstoragespec)

| Field | Description |
| --- | --- |
| `Retain` | PgAdminRetainPolicyRetain keeps the settings volume (no owner<br />reference); a PgAdmin of the same name re-adopts it.<br /> |
| `Delete` | PgAdminRetainPolicyDelete lets garbage collection delete it.<br /> |


#### PgAdminServerStatus



PgAdminServerStatus is one server pgAdmin offers, or a credential it
does not. It names the role, never a secret.



_Appears in:_
- [PgAdminStatus](#pgadminstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cluster` _string_ | The Cluster. |  | MaxLength: 253 <br /> |
| `endpoint` _string_ | The endpoint (rw, ro, r, pooler:&lt;name>). |  | MaxLength: 70 <br /> |
| `user` _string_ | The PostgreSQL role. |  | MaxLength: 128 <br />Optional: \{\} <br /> |
| `pgAdminUser` _string_ | The pgAdmin user a personal server belongs to; empty for a shared<br />server. |  | MaxLength: 254 <br />Optional: \{\} <br /> |
| `state` _[ServerState](#serverstate)_ | The registration state. |  | Enum: [Registered Pending NotOffered] <br /> |
| `message` _string_ | Why a credential is not offered. |  | MaxLength: 256 <br />Optional: \{\} <br /> |


#### PgAdminSpec



PgAdminSpec declares one pgAdmin for a team namespace: the
Clusters of that namespace it registers, the credentials it offers, how
people sign in, and how it is published.

Who may open it: dba only. Whoever reaches pgAdmin holds the credentials
of every role it offers (pgAdmin hides the passwords but a connected role
can change its own, and a server a user registers can name a managed
libpq service), so opening it is equivalent to reading those Secrets.



_Appears in:_
- [PgAdmin](#pgadmin)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterRefs` _[LocalObjectReference](#localobjectreference) array_ | The CloudNativePG Clusters to register, by name, in this namespace.<br />Exactly one of clusterRefs and clusterSelector. |  | MaxItems: 32 <br />MinItems: 1 <br />Optional: \{\} <br /> |
| `clusterSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#labelselector-v1-meta)_ | A label selector over the Clusters of this namespace only; Clusters<br />of other namespaces are never selected, only named in<br />crossNamespaceClusterRefs. At most one of clusterRefs and<br />clusterSelector. |  | Optional: \{\} <br /> |
| `crossNamespaceClusterRefs` _[PgAdminClusterReference](#pgadminclusterreference) array_ | Clusters of other namespaces, by namespace and name. Each is<br />registered only while a PgAdminAccess in its namespace grants this<br />PgAdmin that Cluster (ConfigurationValid=False, GrantMissing,<br />otherwise); the grant is the target namespace owner's decision. |  | MaxItems: 32 <br />MinItems: 1 <br />Optional: \{\} <br /> |
| `endpoints` _[Endpoint](#endpoint) array_ | Which Services of every registered Cluster get a server. | [rw] | MaxItems: 8 <br />MaxLength: 70 <br />MinItems: 1 <br />Pattern: `^(rw\|ro\|r\|pooler:[a-z0-9]([-a-z0-9]*[a-z0-9])?)$` <br />Optional: \{\} <br /> |
| `credentials` _[PgAdminCredentialsSpec](#pgadmincredentialsspec)_ | Which credentials each registered Cluster offers. | \{  \} | Optional: \{\} <br /> |
| `image` _[ImageSpec](#imagespec)_ | The pgAdmin image. Omitted, the operator's digest-pinned default.<br />Changing the image backs up pgAdmin's settings database before the<br />new image rolls out (status.conditions: UpgradeBackup*). |  | Optional: \{\} <br /> |
| `storage` _[PgAdminStorageSpec](#pgadminstoragespec)_ | The settings volume (pgAdmin's SQLite database and STORAGE_DIR).<br />Unused with configDatabase: STORAGE_DIR is then per Pod and<br />ephemeral. | \{  \} | Optional: \{\} <br /> |
| `configDatabase` _[PgAdminConfigDatabaseSpec](#pgadminconfigdatabasespec)_ | pgAdmin's settings database on a CloudNativePG Cluster of this<br />namespace instead of SQLite on the settings volume (pgAdmin's<br />CONFIG_DATABASE_URI). The operator owns a DatabaseRole whose password<br />CloudNativePG generates (1.31 and later) and a Database it owns; the<br />password reaches the pgAdmin Pod only. Set at creation only. |  | Optional: \{\} <br /> |
| `replicas` _integer_ | pgAdmin Pods. Above 1 only with configDatabase; the Service then keeps<br />a client on one Pod (sessionAffinity ClientIP), and the exposure must<br />be sticky too. | 1 | Maximum: 10 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `auth` _[PgAdminAuthSpec](#pgadminauthspec)_ | How people sign in. pgAdmin has no local password login here. |  |  |
| `admins` _string array_ | Usernames (as the identity provider presents them) that get pgAdmin's<br />Administrator role; everyone else who signs in is a User. |  | MaxItems: 32 <br />items:MaxLength: 254 <br />items:MinLength: 1 <br />items:Pattern: `^[^-\s'"\\][^\s'"\\]*$` <br />Optional: \{\} <br /> |
| `exposure` _[ExposureSpec](#exposurespec)_ | How pgAdmin is published. | \{  \} | Optional: \{\} <br /> |
| `networkPolicy` _[PgAdminNetworkPolicySpec](#pgadminnetworkpolicyspec)_ | An optional NetworkPolicy around the pgAdmin Pod. Off by default: a<br />NetworkPolicy is the installing admin's choice, never a product<br />precondition. Whether it confines egress is reported by the advisory<br />PgAdminEgressConfined condition. | \{  \} | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#resourcerequirements-v1-core)_ | The pgAdmin container's resources. Defaults to a request of 100m CPU<br />and 256Mi memory and a 1Gi memory limit. |  | Optional: \{\} <br /> |
| `metadataPropagation` _[MetadataPropagation](#metadatapropagation) array_ | Labels and annotations copied onto generated objects. |  | MaxItems: 8 <br />Optional: \{\} <br /> |


#### PgAdminStatus



PgAdminStatus is the observed state of a PgAdmin.



_Appears in:_
- [PgAdmin](#pgadmin)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | The generation the status was computed from. |  | Optional: \{\} <br /> |
| `url` _string_ | Where pgAdmin is reached: the exposure URL, or the in-cluster<br />Service URL for clusterIP exposure. |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `image` _string_ | The image the Ready workload runs (the resolved reference, with its<br />digest when pinned). |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `upgradeBackup` _[PgAdminUpgradeBackupStatus](#pgadminupgradebackupstatus)_ | The last image change the pre-upgrade backup released. It outlives<br />the backup Job (deleted an hour after it finishes), so that a new<br />image slow to become Ready is neither rolled back nor backed up again. |  | Optional: \{\} <br /> |
| `pgAdminVersion` _string_ | The pgAdmin version the last registration Job reported. |  | MaxLength: 32 <br />Optional: \{\} <br /> |
| `registeredHash` _string_ | The hash of the offered set the last successful registration<br />applied. |  | MaxLength: 64 <br />Optional: \{\} <br /> |
| `personalServerOwners` _string array_ | The pgAdmin users the last successful registration gave personal<br />servers (per-person DatabaseRoles); the next registration removes<br />the managed personal servers of those no longer among them. |  | MaxItems: 64 <br />items:MaxLength: 254 <br />Optional: \{\} <br /> |
| `servers` _[PgAdminServerStatus](#pgadminserverstatus) array_ | The servers pgAdmin offers and the credentials it does not. |  | MaxItems: 256 <br />Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#condition-v1-meta) array_ | Conditions: Ready (summary of ConfigurationValid, WorkloadReady,<br />ExposureReady and ServersSynced), Progressing, Stalled, and the<br />advisory PgAdminEgressConfined, which never enters Ready. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### PgAdminStorageSpec



PgAdminStorageSpec configures the settings volume.



_Appears in:_
- [PgAdminSpec](#pgadminspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#quantity-resource-api)_ | Requested size; may grow, never shrink. Defaults to 1Gi. |  | Optional: \{\} <br /> |
| `storageClassName` _string_ | The StorageClass; the cluster default when omitted. Immutable. |  | MaxLength: 253 <br />Optional: \{\} <br /> |
| `retainPolicy` _[PgAdminRetainPolicy](#pgadminretainpolicy)_ | What happens to the volume when the PgAdmin is deleted. | Retain | Enum: [Retain Delete] <br />Optional: \{\} <br /> |


#### PgAdminUpgradeBackupStatus



PgAdminUpgradeBackupStatus is one image change the pre-upgrade backup
released.



_Appears in:_
- [PgAdminStatus](#pgadminstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `from` _string_ | The image whose settings database was backed up: the image the Ready<br />workload ran before the change. |  | MaxLength: 512 <br /> |
| `to` _string_ | The image the backup released. |  | MaxLength: 512 <br /> |
| `hash` _string_ | The hash of the change: it names the backup Job and labels the<br />backup file. |  | MaxLength: 64 <br /> |
| `skipped` _boolean_ | The backup was not taken: the skip-upgrade-backup annotation named<br />this target. |  | Optional: \{\} <br /> |


#### PgToolBoxConsole



PgToolBoxConsole runs the pgToolBox console for the CloudNativePG Clusters of
its namespace: the same console the chart installs, kept converged by the
operator. The console's own authority is read-only in this namespace;
whoever may create a PgToolBoxConsole may create a Deployment here anyway.



_Appears in:_
- [PgToolBoxConsoleList](#pgtoolboxconsolelist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `PgToolBoxConsole` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[PgToolBoxConsoleSpec](#pgtoolboxconsolespec)_ |  |  |  |
| `status` _[PgToolBoxConsoleStatus](#pgtoolboxconsolestatus)_ |  |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleAuthProvider

_Underlying type:_ _string_

PgToolBoxConsoleAuthProvider selects how people sign in to the console.

_Validation:_
- Enum: [token oidc openshift]

_Appears in:_
- [PgToolBoxConsoleAuthSpec](#pgtoolboxconsoleauthspec)

| Field | Description |
| --- | --- |
| `token` | PgToolBoxConsoleAuthToken signs in with an API-server token the user pastes<br />(break-glass; works everywhere).<br /> |
| `oidc` | PgToolBoxConsoleAuthOIDC signs in through the code flow of an IdP the API<br />server also trusts, so the token is an API-server token.<br /> |
| `openshift` | PgToolBoxConsoleAuthOpenShift signs in through the OpenShift OAuth code flow,<br />with the console's ServiceAccount as the OAuth client.<br /> |


#### PgToolBoxConsoleAuthSpec



PgToolBoxConsoleAuthSpec configures sign-in.



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `provider` _[PgToolBoxConsoleAuthProvider](#pgtoolboxconsoleauthprovider)_ | The sign-in provider. | token | Enum: [token oidc openshift] <br />Optional: \{\} <br /> |
| `oidc` _[PgToolBoxConsoleOIDCSpec](#pgtoolboxconsoleoidcspec)_ | OpenID Connect, for provider oidc. |  | Optional: \{\} <br /> |
| `openshift` _[PgToolBoxConsoleOpenShiftSpec](#pgtoolboxconsoleopenshiftspec)_ | OpenShift OAuth, for provider openshift. |  | Optional: \{\} <br /> |
| `tokenLogin` _boolean_ | Shows the break-glass form (paste an API-server token) next to SSO.<br />Default true. |  | Optional: \{\} <br /> |
| `sessionIdleTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | Idle timeout of a session. Default 1h. |  | Optional: \{\} <br /> |
| `sessionMaxAge` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | Maximum age of a session whatever its activity. Default 12h. |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleDigestDeliverySpec



PgToolBoxConsoleDigestDeliverySpec names the channels. Each credential is a
Secret key the operator mounts as a file; the console reads no Secret
through the API.



_Appears in:_
- [PgToolBoxConsoleDigestSpec](#pgtoolboxconsoledigestspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `chatWebhookSecretRef` _[SecretKeyReference](#secretkeyreference)_ | A Slack or Mattermost incoming-webhook URL (https), which receives<br />POST \{"text": ...\}. |  | Optional: \{\} <br /> |
| `webhookSecretRef` _[SecretKeyReference](#secretkeyreference)_ | A generic webhook URL (https), which receives the digest's JSON<br />summary. |  | Optional: \{\} <br /> |
| `smtp` _[PgToolBoxConsoleDigestSMTPSpec](#pgtoolboxconsoledigestsmtpspec)_ | Mail over TLS. |  | Optional: \{\} <br /> |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | Bound of one delivery attempt. Default 10s. |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleDigestSMTPSpec



PgToolBoxConsoleDigestSMTPSpec is the mail channel.



_Appears in:_
- [PgToolBoxConsoleDigestDeliverySpec](#pgtoolboxconsoledigestdeliveryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `address` _string_ | The mail server, host:port. |  | MaxLength: 261 <br />Pattern: `^[A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?:[0-9]\{1,5\}$` <br /> |
| `tls` _[PgToolBoxConsoleDigestSMTPTLS](#pgtoolboxconsoledigestsmtptls)_ | starttls (required, never optional) or tls (implicit, port 465).<br />Default starttls. | starttls | Enum: [starttls tls] <br />Optional: \{\} <br /> |
| `username` _string_ | The user for AUTH PLAIN (over TLS only). |  | MaxLength: 256 <br />Optional: \{\} <br /> |
| `passwordSecretRef` _[SecretKeyReference](#secretkeyreference)_ | The password. |  | Optional: \{\} <br /> |
| `from` _string_ | The sender, one bare address. |  | MaxLength: 254 <br />MinLength: 3 <br /> |
| `to` _string array_ | The recipients, bare addresses. |  | MaxItems: 20 <br />MinItems: 1 <br />items:MaxLength: 254 <br /> |


#### PgToolBoxConsoleDigestSMTPTLS

_Underlying type:_ _string_

PgToolBoxConsoleDigestSMTPTLS is how the mail connection is secured; there is
no plaintext mode.

_Validation:_
- Enum: [starttls tls]

_Appears in:_
- [PgToolBoxConsoleDigestSMTPSpec](#pgtoolboxconsoledigestsmtpspec)



#### PgToolBoxConsoleDigestSpec



PgToolBoxConsoleDigestSpec configures the daily digest (DIGEST_*).



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Make a digest every day. Default: on with persistent history; with<br />in-memory history each replica would make its own, so it is on only<br />when set. |  | Optional: \{\} <br /> |
| `hour` _integer_ | The UTC hour at which each digest closes its 24 hours. Default 7. |  | Maximum: 23 <br />Minimum: 0 <br />Optional: \{\} <br /> |
| `retention` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | How long digests are kept. Default 720h (30 days). |  | Optional: \{\} <br /> |
| `delivery` _[PgToolBoxConsoleDigestDeliverySpec](#pgtoolboxconsoledigestdeliveryspec)_ | Where each digest is sent: a summary and a link, never the details.<br />Omitted: nowhere (the page only). |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleDrillNetworkPolicy



PgToolBoxConsoleDrillNetworkPolicy is the drill Pods' NetworkPolicy: egress to
DNS, the API server and the object store; ingress from the CloudNativePG
operator and the console on the instance's status and metrics ports.



_Appears in:_
- [PgToolBoxConsoleRestoreDrillsSpec](#pgtoolboxconsolerestoredrillsspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Whether the operator renders it. Default true: unlike the pgAdmin<br />policy, it fences a copy of production data the console created. | true | Optional: \{\} <br /> |
| `operatorNamespace` _string_ | The CloudNativePG operator's namespace, which reaches every<br />instance's status port. Omitted: cnpg-system. |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Optional: \{\} <br /> |
| `apiServerCIDRs` _string array_ | The API server's addresses (CIDRs). Empty: ports 443 and 6443 to any<br />address. |  | MaxItems: 16 <br />items:MaxLength: 49 <br />items:Pattern: `^[0-9a-fA-F:.]+/[0-9]\{1,3\}$` <br />Optional: \{\} <br /> |
| `objectStore` _[NetworkPolicyPeer](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#networkpolicypeer-v1-networking) array_ | The object store the drills restore from: an ipBlock, or a<br />namespaceSelector and podSelector for an in-cluster store. Empty:<br />objectStorePorts to any address. |  | MaxItems: 16 <br />Optional: \{\} <br /> |
| `objectStorePorts` _integer array_ | The object store's ports. Omitted: 443. |  | MaxItems: 8 <br />items:Maximum: 65535 <br />items:Minimum: 1 <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleDrillQuantities



PgToolBoxConsoleDrillQuantities are a CPU and a memory quantity.



_Appears in:_
- [PgToolBoxConsoleDrillResources](#pgtoolboxconsoledrillresources)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cpu` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#quantity-resource-api)_ |  |  | Optional: \{\} <br /> |
| `memory` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#quantity-resource-api)_ |  |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleDrillResources



PgToolBoxConsoleDrillResources are a drill instance's requests and limits. The
console refuses to start on a quantity that is not positive or a limit
below its request (CEL quantity functions are not available on every
supported API server).



_Appears in:_
- [PgToolBoxConsoleRestoreDrillsSpec](#pgtoolboxconsolerestoredrillsspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `requests` _[PgToolBoxConsoleDrillQuantities](#pgtoolboxconsoledrillquantities)_ |  |  | Optional: \{\} <br /> |
| `limits` _[PgToolBoxConsoleDrillQuantities](#pgtoolboxconsoledrillquantities)_ |  |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleDrillStorage



PgToolBoxConsoleDrillStorage is a drill instance's volume.



_Appears in:_
- [PgToolBoxConsoleRestoreDrillsSpec](#pgtoolboxconsolerestoredrillsspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#quantity-resource-api)_ | The volume size. Omitted: the source Cluster's. |  | Optional: \{\} <br /> |
| `storageClass` _string_ | The StorageClass. Omitted: the source Cluster's. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleEvidenceSpec



PgToolBoxConsoleEvidenceSpec configures the EvidenceAgent channel.



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Read repository evidence from the namespace's EvidenceAgents. Default<br />true. Off, the backups screen says "disabled". |  | Optional: \{\} <br /> |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | How often each agent is asked again. Omitted: the console's default. |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleExposurePolicySpec



PgToolBoxConsoleExposurePolicySpec configures the console's ingress
NetworkPolicy.



_Appears in:_
- [PgToolBoxConsoleExposureSpec](#pgtoolboxconsoleexposurespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Whether the operator renders it. Omitted: true for route exposure,<br />and for ingress and gateway exposure when from is set. |  | Optional: \{\} <br /> |
| `from` _[PgToolBoxConsolePolicyPeer](#pgtoolboxconsolepolicypeer) array_ | Who may reach the console port: the router, ingress controller or<br />gateway Pods. For route exposure the namespaces labelled<br />policy-group.network.openshift.io/ingress (the OpenShift routers,<br />host-network ones included) are always admitted, and from adds to<br />them. Required for ingress and gateway. Every peer listed is trusted<br />to write X-Forwarded-For unless trustedProxy.cidrs narrows it; a peer<br />that admits (nearly) everyone (a namespaceSelector, or a podSelector<br />alone, that pins no label value; an ipBlock wider than /8 or /32 or<br />holding a whole private range) turns that trust off. The probe<br />port (health and /metrics) stays open to scrapers; it never reads the<br />header. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleExposureSpec



PgToolBoxConsoleExposureSpec is the console's exposure: the common
ExposureSpec, plus who the console believes about the client's address
and who may reach its port.



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[ExposureType](#exposuretype)_ | The exposure mechanism. | clusterIP | Enum: [clusterIP route ingress gateway] <br />Optional: \{\} <br /> |
| `hostname` _string_ | The external hostname. Required for route, ingress and gateway. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$` <br />Optional: \{\} <br /> |
| `tls` _[ExposureTLSSpec](#exposuretlsspec)_ | TLS for route and ingress exposure. A Route always terminates TLS at<br />the edge; an Ingress uses certificateSecretRef, or the controller's<br />default certificate when it is omitted. For gateway exposure TLS<br />terminates at the Gateway listener. |  | Optional: \{\} <br /> |
| `ingressClassName` _string_ | IngressClass name, for ingress exposure only. |  | MaxLength: 253 <br />Optional: \{\} <br /> |
| `gateway` _[GatewayExposureSpec](#gatewayexposurespec)_ | Gateway API attachment, for gateway exposure only. |  | Optional: \{\} <br /> |
| `annotations` _object (keys:string, values:string)_ | Annotations added to the exposure object; keys under<br />pgtoolbox.fyannk.dev/ are rejected. |  | MaxProperties: 32 <br />Optional: \{\} <br /> |
| `labels` _object (keys:string, values:string)_ | Labels added to the exposure object under the operator's own<br />identity labels, which always win. Ingress controllers and OpenShift<br />router shards select by label; keys under pgtoolbox.fyannk.dev/ are<br />rejected. |  | MaxProperties: 32 <br />Optional: \{\} <br /> |
| `trustedProxy` _[PgToolBoxConsoleTrustedProxySpec](#pgtoolboxconsoletrustedproxyspec)_ | The reverse proxies in front of the console: how many append to<br />X-Forwarded-For and which peers they are (TRUSTED_PROXY_HOPS,<br />TRUSTED_PROXY_CIDRS). The console reads the header only from a peer<br />it trusts, so the per-address sign-in budgets and the audit trail's<br />client_ip follow each client. Omitted: one hop when an exposure<br />object is published; trusted peers per networkPolicy. |  | Optional: \{\} <br /> |
| `networkPolicy` _[PgToolBoxConsoleExposurePolicySpec](#pgtoolboxconsoleexposurepolicyspec)_ | The ingress NetworkPolicy around the console Pods: only the router,<br />ingress controller or gateway reaches the console port. Omitted: on<br />for route exposure (the OpenShift router namespaces carry<br />policy-group.network.openshift.io/ingress), and for ingress and<br />gateway exposure only once from names the controller's namespace;<br />ExposureConfined says which. Never rendered for clusterIP. Every<br />in-cluster client of the Service's http port (a debug Pod, an<br />in-cluster MCP client) is then refused unless from lists it, and is<br />trusted with X-Forwarded-For once listed. |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleHistorySpec



PgToolBoxConsoleHistorySpec configures the history journal.



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `persistent` _boolean_ | Keep the history on a volume per replica, across restarts: the<br />console is a StatefulSet whose claim template makes<br />history-&lt;console>-&lt;ordinal> for each replica. false: history in<br />memory, a Deployment. Default true. | true | Optional: \{\} <br /> |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#quantity-resource-api)_ | Size of each replica's history volume. Default 1Gi, which holds the<br />default retention of a console of a few hundred instances. A change<br />replaces the StatefulSet (its Pods keep running) and applies to<br />volumes made later; grow an existing one by editing its claim. |  | Optional: \{\} <br /> |
| `storageClassName` _string_ | StorageClass of the history volumes. Omitted: the cluster default.<br />Like size, a change applies to volumes made later. |  | MaxLength: 253 <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleIPBlock



PgToolBoxConsoleIPBlock is a NetworkPolicy ipBlock.



_Appears in:_
- [PgToolBoxConsolePolicyPeer](#pgtoolboxconsolepolicypeer)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidr` _string_ | The block, as a CIDR (10.0.0.0/8, fd00::/64). |  | MaxLength: 49 <br /> |
| `except` _string array_ | CIDRs inside cidr that are not admitted. |  | MaxItems: 16 <br />items:MaxLength: 49 <br />items:XValidation: \{isCIDR(self) networkPolicy.from ipBlock.except entries must be CIDRs  FieldValueInvalid  &lt;nil>\} <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleInstanceCABundleStatus



PgToolBoxConsoleInstanceCABundleStatus reports the instance CA bundle.



_Appears in:_
- [PgToolBoxConsoleStatus](#pgtoolboxconsolestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `configMap` _string_ | The ConfigMap's name. |  | MaxLength: 253 <br /> |
| `keptBy` _[PgToolBoxConsoleInstanceCAKeeper](#pgtoolboxconsoleinstancecakeeper)_ | Who keeps it. |  | Enum: [Operator Referenced] <br /> |
| `clusters` _integer_ | How many selected Clusters have their server CA in the bundle, when<br />the operator keeps it (Clusters sharing one CA each count). |  | Optional: \{\} <br /> |
| `missing` _[PgToolBoxConsoleInstanceCAMissing](#pgtoolboxconsoleinstancecamissing) array_ | The selected Clusters whose server CA is not in the bundle, and why;<br />at most 32, by Cluster name (the condition message has the count). |  | MaxItems: 32 <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleInstanceCAKeeper

_Underlying type:_ _string_

PgToolBoxConsoleInstanceCAKeeper says who keeps the instance CA bundle.

_Validation:_
- Enum: [Operator Referenced]

_Appears in:_
- [PgToolBoxConsoleInstanceCABundleStatus](#pgtoolboxconsoleinstancecabundlestatus)

| Field | Description |
| --- | --- |
| `Operator` | InstanceCAKeptByOperator means the operator mirrors the selected<br />Clusters' server CAs into &lt;name>-instance-ca.<br /> |
| `Referenced` | InstanceCAReferenced means spec.instanceCABundleRef names a ConfigMap<br />the operator never reads or writes.<br /> |


#### PgToolBoxConsoleInstanceCAMissing



PgToolBoxConsoleInstanceCAMissing is a Cluster whose server CA the operator
could not put in the bundle.



_Appears in:_
- [PgToolBoxConsoleInstanceCABundleStatus](#pgtoolboxconsoleinstancecabundlestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cluster` _string_ | The Cluster's name. |  | MaxLength: 253 <br /> |
| `secret` _string_ | The server CA Secret read: status.certificates.serverCASecret, else<br />spec.certificates.serverCASecret, else &lt;cluster>-ca. |  | MaxLength: 253 <br />Optional: \{\} <br /> |
| `reason` _[Reason](#reason)_ | Why: SecretNotFound, SecretKeyMissing (no ca.crt), InstanceCAInvalid<br />(ca.crt holds no PEM certificate) or InstanceCABundleFull. |  | MaxLength: 64 <br /> |


#### PgToolBoxConsoleLabelRequirement



PgToolBoxConsoleLabelRequirement is one matchExpressions entry.



_Appears in:_
- [PgToolBoxConsoleLabelSelector](#pgtoolboxconsolelabelselector)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `key` _string_ | The label key ([prefix/]name). |  | MaxLength: 317 <br />MinLength: 1 <br />Pattern: `^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]\{0,61\}[A-Za-z0-9])?$` <br /> |
| `operator` _[LabelSelectorOperator](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#labelselectoroperator-v1-meta)_ | In, NotIn, Exists or DoesNotExist. |  | Enum: [In NotIn Exists DoesNotExist] <br /> |
| `values` _[PgToolBoxConsoleLabelValue](#pgtoolboxconsolelabelvalue) array_ | The values, for In and NotIn. |  | MaxItems: 16 <br />MaxLength: 63 <br />Pattern: `^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$` <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleLabelSelector



PgToolBoxConsoleLabelSelector is a label selector, bounded and checked
here so that the API server never refuses the NetworkPolicy rendered from
it: label keys and values in their syntax, operators from the four the
API knows, values only (and always) for In and NotIn.



_Appears in:_
- [PgToolBoxConsolePolicyPeer](#pgtoolboxconsolepolicypeer)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `matchLabels` _object (keys:string, values:[PgToolBoxConsoleLabelValue](#pgtoolboxconsolelabelvalue))_ | Labels the object must carry, with these values. |  | MaxProperties: 16 <br />Optional: \{\} <br /> |
| `matchExpressions` _[PgToolBoxConsoleLabelRequirement](#pgtoolboxconsolelabelrequirement) array_ | Requirements the object's labels must meet, all of them. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleLabelValue

_Underlying type:_ _string_

PgToolBoxConsoleLabelValue is a label value: at most 63 characters,
alphanumeric at both ends, '-', '_' and '.' inside.

_Validation:_
- MaxLength: 63
- Pattern: `^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`

_Appears in:_
- [PgToolBoxConsoleLabelRequirement](#pgtoolboxconsolelabelrequirement)
- [PgToolBoxConsoleLabelSelector](#pgtoolboxconsolelabelselector)



#### PgToolBoxConsoleLifecycleKeeper

_Underlying type:_ _string_

PgToolBoxConsoleLifecycleKeeper says who keeps the lifecycle ConfigMap.

_Validation:_
- Enum: [Operator User Referenced]

_Appears in:_
- [PgToolBoxConsoleLifecycleStatus](#pgtoolboxconsolelifecyclestatus)

| Field | Description |
| --- | --- |
| `Operator` | LifecycleKeptByOperator means the ConfigMap holds the operator's<br />built-in table, rewritten when the operator brings a newer one.<br /> |
| `User` | LifecycleKeptByUser means someone edited the ConfigMap the operator<br />created, or annotated it user-managed; the operator no longer writes it.<br /> |
| `Referenced` | LifecycleReferenced means spec.lifecycleConfigMapRef names a ConfigMap<br />the operator never writes.<br /> |


#### PgToolBoxConsoleLifecycleStatus



PgToolBoxConsoleLifecycleStatus reports the lifecycle ConfigMap.



_Appears in:_
- [PgToolBoxConsoleStatus](#pgtoolboxconsolestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `configMap` _string_ | The ConfigMap's name. |  | MaxLength: 253 <br /> |
| `keptBy` _[PgToolBoxConsoleLifecycleKeeper](#pgtoolboxconsolelifecyclekeeper)_ | Who keeps it. |  | Enum: [Operator User Referenced] <br /> |
| `reviewed` _string_ | The day the operator's table was last reviewed, when the operator<br />keeps it. |  | MaxLength: 10 <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleList



PgToolBoxConsoleList contains a list of PgToolBoxConsole.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `pgtoolbox.fyannk.dev/v1alpha1` | | |
| `kind` _string_ | `PgToolBoxConsoleList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[PgToolBoxConsole](#pgtoolboxconsole) array_ |  |  |  |


#### PgToolBoxConsoleMCPSpec



PgToolBoxConsoleMCPSpec configures the MCP endpoint. A client authenticates
with `Authorization: Bearer &lt;token>`, never the session cookie: a console
grant, which the console issues as the clients' OAuth authorization server
once the person signs in and approves the client, or (while
auth.tokenLogin is on) a raw API-server token. Every tool is authorized as
its owner exactly like the screen it mirrors.



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Serve the read-only MCP endpoint. Default false. |  | Optional: \{\} <br /> |
| `writeTools` _boolean_ | Add the MCP write tools (backup, reload, restart, promote) when the<br />console serves operations. Each runs the console's own operation as<br />the person, once, after a confirmation (MCP elicitation): backup and<br />reload in the client's form, which the client relays (it proves the<br />client asked); restart and promote on a console page in the person's<br />own browser session (it proves the person approved). A client that<br />cannot ask is refused. A console grant acts only if consented for<br />"read and operate". Requires enabled. Default false. |  | Optional: \{\} <br /> |
| `maxResultBytes` _integer_ | The cap on one tool result's JSON, in bytes (MCP_MAX_RESULT_BYTES);<br />a larger result is shortened and says so. Omitted: the console's<br />default, 262144 (256 KiB). Used only with enabled. |  | Maximum: 4.194304e+06 <br />Minimum: 16384 <br />Optional: \{\} <br /> |
| `callTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | The time limit of one tool call (MCP_CALL_TIMEOUT). Omitted: the<br />console's default, 20s. Used only with enabled. |  | Optional: \{\} <br /> |
| `oauthAccessTTL` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | The lifetime of an access token the console issues to an MCP client<br />(MCP_OAUTH_ACCESS_TTL); the client renews it with its refresh token.<br />Omitted: the console's default, 1h. Used only with enabled. |  | Optional: \{\} <br /> |
| `oauthClientMetadataDocuments` _boolean_ | Accept OAuth client ID metadata documents<br />(MCP_OAUTH_CLIENT_METADATA_DOCUMENTS): a client may name itself with<br />an HTTPS URL, and the console fetches the document at that URL, only<br />from a public address and, when oauthClientMetadataHosts is set, only<br />from those hosts. The console then needs egress to the Internet on<br />TCP 443; the operator renders no NetworkPolicy around the console, so<br />where egress is policed the site's policy must allow it. Dynamic<br />registration, always accepted, needs no egress. Used only with<br />enabled. Default false. |  | Optional: \{\} <br /> |
| `oauthClientMetadataHosts` _string array_ | The only hosts (lower-case DNS names) the console fetches client ID<br />metadata documents from (MCP_OAUTH_CLIENT_METADATA_HOSTS). It takes<br />effect with oauthClientMetadataDocuments and is ignored without it.<br />Omitted: any public address. Used only with enabled. |  | MaxItems: 32 <br />items:MaxLength: 253 <br />items:Pattern: `^[a-z0-9]([-a-z0-9]\{0,61\}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]\{0,61\}[a-z0-9])?)*$` <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleOIDCClient



PgToolBoxConsoleOIDCClient is one more OIDC client of the console's issuer.



_Appears in:_
- [PgToolBoxConsoleOIDCSpec](#pgtoolboxconsoleoidcspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clientID` _string_ | The client id. |  | MaxLength: 256 <br />MinLength: 1 <br /> |
| `clientSecretRef` _[SecretKeySelector](#secretkeyselector)_ | The Secret key holding the client secret, mounted as a file. |  |  |


#### PgToolBoxConsoleOIDCSpec



PgToolBoxConsoleOIDCSpec is the OpenID Connect code flow. Groups come from the
API server (SelfSubjectReview), never from a claim.



_Appears in:_
- [PgToolBoxConsoleAuthSpec](#pgtoolboxconsoleauthspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `issuerURL` _string_ | The issuer, https only. |  | MaxLength: 512 <br />Pattern: `^https://` <br /> |
| `clientID` _string_ | The client id. |  | MaxLength: 256 <br />MinLength: 1 <br /> |
| `clientSecretRef` _[SecretKeySelector](#secretkeyselector)_ | The Secret key holding the client secret, mounted as a file. |  |  |
| `redirectURL` _string_ | The callback URL. Defaults to &lt;publicURL>/auth/oidc/callback. |  | MaxLength: 512 <br />Pattern: `^https://` <br />Optional: \{\} <br /> |
| `scopes` _string array_ | Scopes requested. Default: openid, email, profile, offline_access. |  | MaxItems: 16 <br />items:MaxLength: 128 <br />Optional: \{\} <br /> |
| `caConfigMapRef` _[LocalObjectReference](#localobjectreference)_ | A ConfigMap with ca.crt trusting a private IdP. Omitted: the system<br />roots. |  | Optional: \{\} <br /> |
| `requireEmailVerified` _boolean_ | Refuse a login whose ID token lacks email_verified=true. Default true;<br />set false only for an IdP that omits the claim. |  | Optional: \{\} <br /> |
| `grantClient` _[PgToolBoxConsoleOIDCClient](#pgtoolboxconsoleoidcclient)_ | A second client of the same issuer for the identity provider sign-ins<br />of console grants (MCP clients), registered with the same redirect<br />URI. An identity provider that keeps one refresh token per person and<br />client (dex) otherwise ends the browser session's renewal each time an<br />assistant connects. The API server's JWT authenticator must accept<br />this client's audience too. Omitted: grants sign in as clientID. |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleOpenShiftSpec



PgToolBoxConsoleOpenShiftSpec is the OpenShift OAuth code flow. The console's
ServiceAccount is the OAuth client; its redirect annotation is rendered
from the public URL.



_Appears in:_
- [PgToolBoxConsoleAuthSpec](#pgtoolboxconsoleauthspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clientSecretRef` _[SecretKeyReference](#secretkeyreference)_ | The Secret key holding a long-lived ServiceAccount token to present as<br />the client secret. Omitted: the console presents its projected token. |  | Optional: \{\} <br /> |
| `caConfigMapRef` _[LocalObjectReference](#localobjectreference)_ | A ConfigMap with ca.crt trusting the OAuth server. Omitted: the<br />service CA OpenShift publishes in every namespace. |  | Optional: \{\} <br /> |


#### PgToolBoxConsolePolicyPeer



PgToolBoxConsolePolicyPeer is a NetworkPolicyPeer, bounded so that the
API server checks it here rather than refuse the policy the operator
renders from it.



_Appears in:_
- [PgToolBoxConsoleExposurePolicySpec](#pgtoolboxconsoleexposurepolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podSelector` _[PgToolBoxConsoleLabelSelector](#pgtoolboxconsolelabelselector)_ | Pods (of this namespace, or of the namespaces namespaceSelector<br />selects). Pinning no label value (no matchLabels, no In) with no<br />namespaceSelector: (nearly) every Pod of this namespace, which<br />ExposureConfined reports as broad. |  | Optional: \{\} <br /> |
| `namespaceSelector` _[PgToolBoxConsoleLabelSelector](#pgtoolboxconsolelabelselector)_ | Namespaces. Pinning no label value (no matchLabels, no In):<br />(nearly) every namespace, which ExposureConfined reports as broad. |  | Optional: \{\} <br /> |
| `ipBlock` _[PgToolBoxConsoleIPBlock](#pgtoolboxconsoleipblock)_ | Addresses: a CIDR, less the except CIDRs inside it. Alone in its<br />peer. Wider than /8 (IPv4) or /32 (IPv6), or holding a whole private<br />range, is broad. |  | Optional: \{\} <br /> |


#### PgToolBoxConsolePrometheusSpec



PgToolBoxConsolePrometheusSpec is the Prometheus HTTP API the charts query.



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `url` _string_ | The Prometheus (or Thanos querier) URL. |  | MaxLength: 512 <br />Pattern: `^https?://` <br /> |
| `tokenSecretRef` _[SecretKeyReference](#secretkeyreference)_ | A Secret key holding a bearer token, mounted as a file. |  | Optional: \{\} <br /> |
| `projectedToken` _boolean_ | Present the console's own ServiceAccount token, projected and rotated<br />by the kubelet (what the OpenShift thanos-querier accepts once the<br />ServiceAccount may view metrics). |  | Optional: \{\} <br /> |
| `tokenAudience` _string_ | The audience of the projected token. Omitted: the API server's own<br />audience, which a TokenReview-based proxy (OpenShift's thanos-querier<br />behind kube-rbac-proxy) expects; such a token is also accepted by the<br />API server itself, so only a Prometheus you trust may receive it. Set<br />it when the server checks a dedicated audience: the token is then<br />useless against the API server. |  | MaxLength: 256 <br />MinLength: 1 <br />Pattern: `^[^\s]+$` <br />Optional: \{\} <br /> |
| `caConfigMapRef` _[LocalObjectReference](#localobjectreference)_ | A ConfigMap with ca.crt that signed the server certificate. Omitted:<br />the system roots. |  | Optional: \{\} <br /> |
| `namespaced` _boolean_ | Add namespace=&lt;ns> to each query, as OpenShift's tenancy port requires. |  | Optional: \{\} <br /> |
| `range` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | The charts' window. Omitted: 1h. |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleRestoreDrillsSpec



PgToolBoxConsoleRestoreDrillsSpec configures restore drills. Drill Clusters run
in the console's namespace, the only one it watches.



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Offer the drill. Default false. |  | Optional: \{\} <br /> |
| `prefix` _string_ | The drill Clusters' name prefix. Omitted: drill-. |  | MaxLength: 20 <br />Pattern: `^[a-z]([-a-z0-9]*)?$` <br />Optional: \{\} <br /> |
| `timeLimit` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#duration-v1-meta)_ | When an unfinished drill is recorded as timed out and deleted.<br />Omitted: 2h. |  | Optional: \{\} <br /> |
| `resources` _[PgToolBoxConsoleDrillResources](#pgtoolboxconsoledrillresources)_ | The drill instance's CPU and memory. Omitted: the source Cluster's. |  | Optional: \{\} <br /> |
| `storage` _[PgToolBoxConsoleDrillStorage](#pgtoolboxconsoledrillstorage)_ | The drill instance's storage. Omitted: the source Cluster's. |  | Optional: \{\} <br /> |
| `maxConcurrent` _integer_ | The drills of this console running at once (one per source in any<br />case). Omitted: 3. |  | Maximum: 20 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `sealKeySecretRef` _[SecretKeyReference](#secretkeyreference)_ | The drill seal's own keys (RESTORE_DRILL_SEAL_KEY_FILE): a key of a<br />Secret in the console's namespace holding one key per line, at least<br />32 bytes each (the first seals, all verify), mounted read-only. Keep<br />the Secret readable by the console alone. Omitted: the keys derive<br />from the session secret, so whoever can read the session Secret can<br />forge a seal. |  | Optional: \{\} <br /> |
| `networkPolicy` _[PgToolBoxConsoleDrillNetworkPolicy](#pgtoolboxconsoledrillnetworkpolicy)_ | The NetworkPolicy around the drill Pods. A promoted drill is a live<br />copy of a production database: the console turns off the background<br />workers it knows (logical replication, pg_cron, TimescaleDB), and this<br />policy holds the rest to DNS, the API server and the object store. |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleSpec



PgToolBoxConsoleSpec declares one console in this namespace: the console the
chart installs, run and kept converged by the operator. It watches the
CloudNativePG Clusters of its own namespace only; every reader still sees
only what their own RBAC allows (SubjectAccessReview).



_Appears in:_
- [PgToolBoxConsole](#pgtoolboxconsole)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _[ImageSpec](#imagespec)_ | The console image. Omitted, the operator's default applies (the<br />console released with the operator). The repository must be in the<br />operator's allow-list (--console-image-repositories; by default the<br />default image's repository only). |  | Optional: \{\} <br /> |
| `replicas` _integer_ | Replicas of the console, with or without persistent history. Each<br />replica keeps its own history; the exposure keeps a person on one<br />(cookie affinity). | 2 | Maximum: 5 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `clusterSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#labelselector-v1-meta)_ | Narrows the Clusters shown to those this selector matches, on top of<br />what each user's own RBAC allows. Omitted: every Cluster of the<br />namespace. |  | Optional: \{\} <br /> |
| `auth` _[PgToolBoxConsoleAuthSpec](#pgtoolboxconsoleauthspec)_ | How people sign in. | \{ provider:token \} | Optional: \{\} <br /> |
| `publicURL` _string_ | The URL people type, https://host. Used for cookies, redirects and the<br />OpenShift callback. Defaults to https://&lt;exposure.hostname>. |  | MaxLength: 512 <br />Pattern: `^https://[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]\{1,5\})?/?$` <br />Optional: \{\} <br /> |
| `exposure` _[PgToolBoxConsoleExposureSpec](#pgtoolboxconsoleexposurespec)_ | Exposure of the console. The console streams events over SSE; the<br />operator sets the long edge timeouts it knows for each type. | \{ type:clusterIP \} | Optional: \{\} <br /> |
| `instanceCABundleRef` _[LocalObjectReference](#localobjectreference)_ | The CA bundle the console verifies each instance's /pg/status with:<br />a ConfigMap with the key ca.crt holding the PEM server CAs of the<br />namespace's Clusters. Omitted (recommended): the operator keeps<br />&lt;name>-instance-ca itself, copying only the public ca.crt of each<br />selected Cluster's server CA Secret, and follows CloudNativePG's CA<br />renewals and Clusters coming and going; the console still reads no<br />Secret. Set: the console mounts the named ConfigMap, which you keep<br />current yourself (CloudNativePG renews its self-signed CA every 90<br />days, so a one-off copy expires). A Cluster missing from the bundle<br />is shown as "not verifiable", never read unverified. |  | Optional: \{\} <br /> |
| `lifecycleConfigMapRef` _[LocalObjectReference](#localobjectreference)_ | The end-of-life table the diagnostics read. Omitted, the operator<br />creates &lt;console>-lifecycle with its built-in table and keeps it at<br />the operator's table until someone edits it (or annotates it<br />pgtoolbox.fyannk.dev/user-managed=true); from then on it is left<br />alone. Set, the console reads that ConfigMap (key lifecycle.json) and<br />the operator creates none. |  | Optional: \{\} <br /> |
| `evidence` _[PgToolBoxConsoleEvidenceSpec](#pgtoolboxconsoleevidencespec)_ | Repository evidence from the namespace's EvidenceAgents. |  | Optional: \{\} <br /> |
| `operations` _boolean_ | The operations endpoints (backup, reload, restart, promote). Every<br />write is made with the caller's own token and authorized by the API<br />server. Default true. |  | Optional: \{\} <br /> |
| `podLogs` _boolean_ | Pod logs: the bounded log tail and the log-based diagnostics, which<br />need `get pods/log`. Default true. |  | Optional: \{\} <br /> |
| `restoreDrills` _[PgToolBoxConsoleRestoreDrillsSpec](#pgtoolboxconsolerestoredrillsspec)_ | Restore drills: the "Run a restore drill" operation of the Backups<br />screen. A person starts a drill with their own token; the console<br />restores the latest backup into a throwaway one-instance Cluster in<br />this namespace (no WAL archiver, no backup configuration), checks it<br />without SQL and deletes it after its check or the time limit. Enabled,<br />the operator also binds the console's ServiceAccount to<br />pgtoolbox-console-restore-drills (get, list, watch and delete<br />clusters, in this namespace only). Needs operations. |  | Optional: \{\} <br /> |
| `mcp` _[PgToolBoxConsoleMCPSpec](#pgtoolboxconsolemcpspec)_ | The read-only MCP endpoint at /mcp, for AI agents and IDE<br />assistants. Off by default. |  | Optional: \{\} <br /> |
| `tracing` _[PgToolBoxConsoleTracingSpec](#pgtoolboxconsoletracingspec)_ | OpenTelemetry tracing: spans for HTTP requests, the Kubernetes API<br />calls they make and MCP tool calls, exported over OTLP/HTTP. Off<br />when omitted. |  | Optional: \{\} <br /> |
| `monitoringURL` _string_ | The link-out to the site's monitoring system: https, or<br />root-relative. |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `prometheus` _[PgToolBoxConsolePrometheusSpec](#pgtoolboxconsoleprometheusspec)_ | The PromQL source of the metrics charts. Omitted, the charts come<br />from the console's own ring. |  | Optional: \{\} <br /> |
| `history` _[PgToolBoxConsoleHistorySpec](#pgtoolboxconsolehistoryspec)_ | The history journal, and with it the state snapshots, the timeline<br />and the digests. Persistent by default: the console is then a<br />StatefulSet with one volume per replica. | \{ persistent:true \} | Optional: \{\} <br /> |
| `digest` _[PgToolBoxConsoleDigestSpec](#pgtoolboxconsoledigestspec)_ | The daily digest: a summary page per day, kept with the history, and<br />optional delivery to chat, a webhook or mail. Delivery needs<br />history.persistent (the message links to the digest's page); only the<br />replica holding the Lease &lt;console>-digest delivers. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#resourcerequirements-v1-core)_ | Compute resources of the console container. Omitted: requests 50m and<br />128Mi, limits 1 CPU and 512Mi (the chart's). |  | Optional: \{\} <br /> |
| `metadataPropagation` _[MetadataPropagation](#metadatapropagation) array_ | Labels and annotations copied onto the generated objects. |  | MaxItems: 8 <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleStatus



PgToolBoxConsoleStatus is the observed state of a PgToolBoxConsole.



_Appears in:_
- [PgToolBoxConsole](#pgtoolboxconsole)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | The generation the status was computed from. |  | Optional: \{\} <br /> |
| `url` _string_ | Where the console is reached. |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `image` _string_ | The image reference the console runs. |  | MaxLength: 512 <br />Optional: \{\} <br /> |
| `lifecycle` _[PgToolBoxConsoleLifecycleStatus](#pgtoolboxconsolelifecyclestatus)_ | The ConfigMap the console reads its end-of-life table from, and who<br />keeps it: Operator (the operator's table, updated with the operator),<br />User (edited or annotated user-managed, left alone) or Referenced<br />(spec.lifecycleConfigMapRef). |  | Optional: \{\} <br /> |
| `instanceCABundle` _[PgToolBoxConsoleInstanceCABundleStatus](#pgtoolboxconsoleinstancecabundlestatus)_ | The instance CA bundle the console mounts, who keeps it and, when the<br />operator does, how many Clusters it covers and which are missing. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#condition-v1-meta) array_ | Conditions: Ready (summary), ConfigurationValid, WorkloadReady,<br />ExposureReady, Progressing, Stalled and the advisory<br />InstanceCABundleComplete and ExposureConfined. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### PgToolBoxConsoleTracingSpec



PgToolBoxConsoleTracingSpec configures OpenTelemetry tracing (the console's
OTEL_* variables). Spans carry no payload unless includePayloads is set;
the egress to the collector is the site's NetworkPolicy decision.



_Appears in:_
- [PgToolBoxConsoleSpec](#pgtoolboxconsolespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | The OTLP/HTTP base URL (OTEL_EXPORTER_OTLP_ENDPOINT); /v1/traces is<br />appended. For example http://jaeger-collector.monitoring:4318 or<br />https://api.smith.langchain.com/otel. |  | MaxLength: 512 <br />MinLength: 1 <br />Pattern: `^https?://[^\s/?#@]+(/[^\s?#]*)?$` <br /> |
| `headersSecretRef` _[SecretKeyReference](#secretkeyreference)_ | A Secret key holding the exporter's headers, `key=value,...` with<br />percent-encoded values (an API key, a project name), mounted as a<br />file and read at start: the console reads no Secret through the API. |  | Optional: \{\} <br /> |
| `serviceName` _string_ | The service name (OTEL_SERVICE_NAME). Default pgtoolbox-console. |  | MaxLength: 128 <br />Pattern: `^[A-Za-z0-9][-._A-Za-z0-9]*$` <br />Optional: \{\} <br /> |
| `sampler` _string_ | The sampler (OTEL_TRACES_SAMPLER). Default parentbased_always_on: a<br />request the caller traces is traced. |  | Enum: [always_on always_off traceidratio parentbased_always_on parentbased_always_off parentbased_traceidratio] <br />Optional: \{\} <br /> |
| `samplerArg` _string_ | The ratio of the traceidratio samplers (OTEL_TRACES_SAMPLER_ARG),<br />from "0" to "1". |  | Pattern: `^(0(\.[0-9]\{1,6\})?\|1(\.0\{1,6\})?)$` <br />Optional: \{\} <br /> |
| `includePayloads` _boolean_ | Add MCP tool arguments and results to the tool spans, redacted and<br />capped (TRACING_INCLUDE_PAYLOADS). Default false; tokens never. |  | Optional: \{\} <br /> |


#### PgToolBoxConsoleTrustedProxySpec



PgToolBoxConsoleTrustedProxySpec says which X-Forwarded-For entries the
console believes.



_Appears in:_
- [PgToolBoxConsoleExposureSpec](#pgtoolboxconsoleexposurespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hops` _integer_ | How many proxies append to X-Forwarded-For: 2 for a load balancer<br />that appends in front of the router. 0 ignores the header. Omitted:<br />1 when an exposure object is published, else 0. |  | Maximum: 8 <br />Minimum: 0 <br />Optional: \{\} <br /> |
| `cidrs` _string array_ | The proxies' own addresses, as the console sees them (the router or<br />ingress controller Pods; the nodes for a host-network router). The<br />header is read only from a peer inside one. Omitted: with the<br />NetworkPolicy rendered, any peer it admits (every other Pod is<br />refused by the network); without it, none, and the header is<br />ignored: every client behind the proxy then shares its address, and<br />no per-address sign-in budget applies (only the per-flow, per-token<br />and per-replica bounds). |  | MaxItems: 32 <br />items:MaxLength: 49 <br />items:XValidation: \{isCIDR(self) trustedProxy.cidrs entries must be CIDRs  FieldValueInvalid  &lt;nil>\} <br />Optional: \{\} <br /> |


#### PropagationTarget

_Underlying type:_ _string_

PropagationTarget selects which generated objects receive propagated
metadata.

_Validation:_
- Enum: [All Workload Pod Service Exposure Storage]

_Appears in:_
- [MetadataPropagation](#metadatapropagation)

| Field | Description |
| --- | --- |
| `All` | PropagationTargetAll is every generated object.<br /> |
| `Workload` | PropagationTargetWorkload is Deployments, StatefulSets and Jobs.<br /> |
| `Pod` | PropagationTargetPod is Pod templates.<br /> |
| `Service` | PropagationTargetService is Services.<br /> |
| `Exposure` | PropagationTargetExposure is Routes, Ingresses and HTTPRoutes.<br /> |
| `Storage` | PropagationTargetStorage is PersistentVolumeClaims.<br /> |


#### Reason

_Underlying type:_ _string_

Reason is a condition or Event reason from the frozen catalogue. Reasons
are API surface — alerting and automation key on them — so they are
typed, registered once, checked against a golden file by a test, and only
ever extended. A reason that copies a child's fact (a Pod's
Unschedulable, a Job's failure) names the fact, never "waiting".



_Appears in:_
- [PgToolBoxConsoleInstanceCAMissing](#pgtoolboxconsoleinstancecamissing)

| Field | Description |
| --- | --- |
| `AsExpected` | ReasonAsExpected is the healthy reason of every aspect condition and<br />of Ready.<br /> |
| `Initializing` | ReasonInitializing is the reason of a condition the controller has<br />not evaluated yet: Unknown on first touch.<br /> |
| `Reconciling` | ReasonReconciling is Progressing=True while children converge.<br /> |
| `ReconcileError` | ReasonReconcileError is Ready=False when the last reconciliation<br />ended with an error; the message carries the bounded error text.<br /> |
| `ReconciliationPaused` | ReasonReconciliationPaused is reported while the object carries the<br />reconcile=skip annotation.<br /> |
| `ConfigurationInvalid` | ReasonConfigurationInvalid is ConfigurationValid=False (and<br />Stalled=True): a cross-object check the CRD cannot express failed.<br /> |
| `ResourceNotOwned` | ReasonResourceNotOwned is ConfigurationValid=False when an object the<br />operator must manage exists under the generated name and is<br />controlled by someone else.<br /> |
| `SecretNotFound` | ReasonSecretNotFound names a referenced Secret that does not exist.<br /> |
| `SecretKeyMissing` | ReasonSecretKeyMissing names a referenced Secret key that is absent.<br /> |
| `ConfigMapNotFound` | ReasonConfigMapNotFound names a referenced ConfigMap that does not<br />exist.<br /> |
| `ClusterNotFound` | ReasonClusterNotFound names a referenced CloudNativePG Cluster that<br />does not exist in the namespace (an EvidenceAgent's<br />spec.clusterRef, a PgAdmin's spec.clusterRefs).<br /> |
| `ImageNotAllowed` | ReasonImageNotAllowed: the image a CR names (spec.image, an<br />oauth-proxy image) is outside the operator's allow-list of<br />repositories (--evidence-agent-image-repositories,<br />--pgadmin-image-repositories).<br /> |
| `HostnameNotAllowed` | ReasonHostnameNotAllowed: spec.exposure.hostname is outside the<br />operator's allow-list of host suffixes<br />(EXPOSURE_ALLOWED_HOST_SUFFIXES), so the operator publishes no Route,<br />Ingress or HTTPRoute for it.<br /> |
| `RolloutInProgress` | Workload facts, copied from the child.<br /> |
| `RolloutStuck` |  |
| `PodUnschedulable` |  |
| `ImagePullFailed` |  |
| `CrashLoopBackOff` |  |
| `ReplicaFailure` |  |
| `PodNotReady` |  |
| `ScaledToZero` |  |
| `NotRequested` | Exposure facts.<br /> |
| `NotAdmitted` |  |
| `NotAccepted` |  |
| `GatewayNotFound` |  |
| `HostnameConflict` |  |
| `RouteAPIUnavailable` | ReasonRouteAPIUnavailable, ReasonGatewayAPIUnavailable: the optional<br />API the exposure needs is not served or not readable (ProbeListable<br />returned NotFound/NoMatch/Forbidden at start).<br /> |
| `GatewayAPIUnavailable` |  |
| `ReconciliationFailed` | Event reasons of the classified Warning recorded when an error ends<br />a reconciliation.<br /> |
| `QuotaExceeded` |  |
| `AdmissionDenied` |  |
| `BarmanPluginNotConfigured` | ReasonBarmanPluginNotConfigured: the Cluster declares neither an<br />enabled barman-cloud plugin with a barmanObjectName nor an in-tree<br />spec.backup.barmanObjectStore, and spec.objectStoreRef is not set.<br /> |
| `ObjectStoreNotFound` | ReasonObjectStoreNotFound: the resolved ObjectStore, or the pgBackRest<br />plugin's Stanza or Archive, does not exist.<br /> |
| `ObjectStoreAPIUnavailable` | ReasonObjectStoreAPIUnavailable: barmancloud.cnpg.io ObjectStore (or,<br />for a Cluster backed up with pgBackRest, pgbackrest.dalibo.com Stanza<br />or pgbackrest.cnpg.opera.com Archive) is not served or not readable by<br />the operator.<br /> |
| `UnsupportedObjectStore` | ReasonUnsupportedObjectStore: the backup configuration (an<br />ObjectStore, the in-tree spec.backup.barmanObjectStore, or a pgBackRest<br />plugin's Stanza or Archive) is outside the agent's profile, or the<br />Cluster backs up through a plugin the agent does not read; the message<br />names the shape and the field.<br /> |
| `DestinationInvalid` | ReasonDestinationInvalid: destinationPath, endpointURL, the server<br />name (or a pgBackRest bucket, path, endpoint or stanza) is rejected by<br />the shared evidence contract parser.<br /> |
| `RegionRequired` | ReasonRegionRequired: AWS S3 (no endpointURL) without s3Credentials.region.<br /> |
| `ImageRequired` | ReasonImageRequired: spec.image is unset and the operator has no<br />default evidence agent image.<br /> |
| `ServiceCAPending` | ReasonServiceCAPending: the OpenShift service CA has not yet issued<br />the serving certificate or injected the CA bundle.<br /> |
| `CertificateIssued` | ReasonCertificateIssued is the Event of a newly issued or renewed<br />operator-signed serving certificate.<br /> |
| `AwaitingDecision` | ReasonAwaitingDecision: the request is Pending; nothing is bound.<br /> |
| `AccessGranted` | ReasonAccessGranted: the request is Approved and its RoleBinding is<br />in place.<br /> |
| `AccessDenied` | ReasonAccessDenied: the request was denied; nothing is bound.<br /> |
| `AccessExpired` | ReasonAccessExpired: spec.expiresAt passed; the RoleBinding, if any,<br />was deleted.<br /> |
| `AccessRevoked` | ReasonAccessRevoked: the grant was revoked; the RoleBinding was<br />deleted.<br /> |
| `RoleBindingMissing` | ReasonRoleBindingMissing: the request is Approved and its RoleBinding<br />does not exist (deleted by hand, or the approval's create never<br />landed). The operator never recreates it: revoke and request again.<br /> |
| `RoleBindingDrifted` | ReasonRoleBindingDrifted: the request's RoleBinding binds another<br />subject or ClusterRole than the request names; the operator deleted<br />it (fail closed).<br /> |
| `RoleBindingOrphaned` | ReasonRoleBindingOrphaned: a RoleBinding of a request that is not<br />Approved (an approval whose status write failed) outlived the grace<br />period and was deleted.<br /> |
| `SelfApprovalRefused` | ReasonSelfApprovalRefused: the recorded approver is the subject<br />itself; the operator revoked the grant (four eyes).<br /> |
| `RoleBindingDeleted` | ReasonRoleBindingDeleted is the Normal Event of a RoleBinding the<br />operator deleted (expiry, revocation, drift, orphan).<br /> |
| `NoClusterSelected` | ClusterNotFound (a clusterRefs entry names no Cluster) is the shared<br />ReasonClusterNotFound, registered with the EvidenceAgent reasons.<br />ReasonNoClusterSelected: clusterSelector matches no Cluster.<br /> |
| `PoolerNotFound` | ReasonPoolerNotFound: a pooler:&lt;name> endpoint names no Pooler of a<br />registered Cluster.<br /> |
| `TooManyServers` | ReasonTooManyServers: the offered set exceeds the bound the<br />credential files and the registration accept.<br /> |
| `PgAdminNotRunning` | ReasonPgAdminNotRunning: pgAdmin has no Ready Pod yet; registration<br />and the egress statement wait for it.<br /> |
| `RegistrationRunning` | ReasonRegistrationRunning: the registration Job for the current<br />offered set is running.<br /> |
| `RegistrationFailed` | ReasonRegistrationFailed: the registration Job failed; the message<br />carries its fact.<br /> |
| `ServersRegistered` | ReasonServersRegistered is the Event of a successful registration.<br /> |
| `RegistrationStarted` | ReasonRegistrationStarted is the Event of a new registration Job.<br /> |
| `UpgradeBackupRunning` | ReasonUpgradeBackupRunning: the pre-upgrade backup of pgAdmin's<br />settings database runs before the new image rolls out.<br /> |
| `UpgradeBackupFailed` | ReasonUpgradeBackupFailed: the pre-upgrade backup failed; the old<br />image keeps running.<br /> |
| `UpgradeBackupCompleted` | ReasonUpgradeBackupCompleted is the Event of a completed backup.<br /> |
| `UpgradeBackupSkipped` | ReasonUpgradeBackupSkipped is the Warning Event of a pre-upgrade<br />backup skipped by AnnotationSkipUpgradeBackup.<br /> |
| `RolloutStarted` | ReasonRolloutStarted is the Event of a changed workload hash.<br /> |
| `PgAdminEgressOpen` | ReasonPgAdminEgressOpen: PgAdminEgressConfined=False.<br /> |
| `DatabaseRoleAPIUnavailable` | ReasonDatabaseRoleAPIUnavailable: the CNPG DatabaseRole API is not<br />readable (a refused List), or not served while spec.configDatabase<br />needs it; only application and superuser credentials are offered.<br />CloudNativePG 1.30 and later serve the kind; an install whose CRDs<br />lack it is not a problem for the offered credentials: none is<br />offered, and the PgAdmin is Ready.<br /> |
| `PoolerAPIUnavailable` | ReasonPoolerAPIUnavailable: the CNPG Pooler API is not readable.<br /> |
| `GrantMissing` | ReasonGrantMissing: a crossNamespaceClusterRefs entry names a Cluster<br />no PgAdminAccess of its namespace grants this PgAdmin.<br /> |
| `ConfigDatabaseNotReady` | ReasonConfigDatabaseNotReady: the configDatabase's DatabaseRole or<br />Database is not applied by CloudNativePG yet, or its generated<br />password Secret is not recorded; pgAdmin waits for it.<br /> |
| `PasswordGenerationUnavailable` | ReasonPasswordGenerationUnavailable: the CloudNativePG DatabaseRole API<br />of this cluster cannot generate a password (CloudNativePG 1.31 and<br />later can), which configDatabase needs: the operator never holds one.<br /> |
| `DatabaseAPIUnavailable` | ReasonDatabaseAPIUnavailable: the CloudNativePG Database API, which<br />configDatabase needs, is not served or not readable.<br /> |
| `ConsoleImageRequired` | ReasonConsoleImageRequired: spec.image is unset and the operator has<br />no default console image.<br /> |
| `ConsoleRoleMissing` | ReasonConsoleRoleMissing: the ClusterRole the console's RoleBinding<br />references (pgtoolbox-console, shipped with the operator) does not<br />exist, so the console could read nothing.<br /> |
| `InstanceCAMissing` | ReasonInstanceCAMissing: InstanceCABundleComplete=False, at least<br />one selected Cluster's server CA is not in the mirrored bundle;<br />status.instanceCABundle.missing says which and why.<br /> |
| `InstanceCAInvalid` | ReasonInstanceCAInvalid: a Cluster's server CA Secret has a ca.crt<br />that holds no PEM certificate.<br /> |
| `InstanceCABundleFull` | ReasonInstanceCABundleFull: the mirrored bundle reached the size a<br />ConfigMap can hold; the Clusters after it (by name) are left out.<br /> |
| `ExposureOpen` | ReasonExposureOpen: ExposureConfined=False, the console is exposed<br />without its ingress NetworkPolicy, so any Pod can reach its port.<br /> |
| `ExposurePeersBroad` | ReasonExposurePeersBroad: ExposureConfined=False, a networkPolicy.from<br />peer admits (nearly) every namespace (a namespaceSelector that pins<br />no label value), every Pod of the console's namespace (such a<br />podSelector alone), or the Pod network or every address (an ipBlock<br />wider than /8 or /32, holding a whole private range, or a console<br />Pod's address): its peers are not the proxies, so the console trusts<br />none of them with X-Forwarded-For.<br /> |
| `ExposurePolicyBypassed` | ReasonExposurePolicyBypassed: ExposureConfined=False, another<br />NetworkPolicy of the namespace admits peers the exposure does not<br />list to the console port (NetworkPolicies add up), or there were too<br />many to check: the console trusts none of the peers with<br />X-Forwarded-For.<br /> |
| `ExposurePolicyInvalid` | ReasonExposurePolicyInvalid: ConfigurationValid=False, the API<br />server refused the ingress NetworkPolicy rendered from<br />spec.exposure.networkPolicy (a selector or block the CRD could not<br />check). Looked at again every five minutes and on every spec change.<br /> |


#### SecretKeyReference



SecretKeyReference names a key of a Secret in the referencing object's
namespace; unlike SecretKeySelector, the key has no default.



_Appears in:_
- [EvidenceAgentPgBackRestSpec](#evidenceagentpgbackrestspec)
- [PgToolBoxConsoleDigestDeliverySpec](#pgtoolboxconsoledigestdeliveryspec)
- [PgToolBoxConsoleDigestSMTPSpec](#pgtoolboxconsoledigestsmtpspec)
- [PgToolBoxConsoleOpenShiftSpec](#pgtoolboxconsoleopenshiftspec)
- [PgToolBoxConsolePrometheusSpec](#pgtoolboxconsoleprometheusspec)
- [PgToolBoxConsoleRestoreDrillsSpec](#pgtoolboxconsolerestoredrillsspec)
- [PgToolBoxConsoleTracingSpec](#pgtoolboxconsoletracingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | The Secret name. |  | MaxLength: 253 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` <br /> |
| `key` _string_ | The key. |  | MaxLength: 253 <br />MinLength: 1 <br />Pattern: `^[-._a-zA-Z0-9]+$` <br /> |


#### SecretKeySelector



SecretKeySelector names a key of a Secret in the referencing object's
namespace.



_Appears in:_
- [PgAdminOAuth2Spec](#pgadminoauth2spec)
- [PgToolBoxConsoleOIDCClient](#pgtoolboxconsoleoidcclient)
- [PgToolBoxConsoleOIDCSpec](#pgtoolboxconsoleoidcspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | The Secret name. |  | MaxLength: 253 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` <br /> |
| `key` _string_ | The key. | client-secret | MaxLength: 253 <br />Pattern: `^[-._a-zA-Z0-9]+$` <br />Optional: \{\} <br /> |


#### ServerState

_Underlying type:_ _string_

ServerState is the registration state of one offered server.

_Validation:_
- Enum: [Registered Pending NotOffered]

_Appears in:_
- [PgAdminServerStatus](#pgadminserverstatus)

| Field | Description |
| --- | --- |
| `Registered` | ServerStateRegistered means the last successful registration carries it.<br /> |
| `Pending` | ServerStatePending means offered, not registered yet.<br /> |
| `NotOffered` | ServerStateNotOffered marks a credential the Cluster has that this<br />PgAdmin does not offer; the message says why.<br /> |


#### WorkloadIdentitySpec



WorkloadIdentitySpec binds the agent to a cloud identity. Setting it (even
empty, as GKE workload identity federation needs nothing more) is the
opt-in: without it a configuration using the Pod's cloud identity is
refused, so it never falls back to the node's.



_Appears in:_
- [EvidenceAgentSpec](#evidenceagentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `serviceAccountAnnotations` _object (keys:string, values:string)_ | Annotations added to the agent's ServiceAccount, among the identity<br />webhooks' own: eks.amazonaws.com/role-arn,<br />eks.amazonaws.com/sts-regional-endpoints,<br />eks.amazonaws.com/token-expiration (IRSA),<br />azure.workload.identity/client-id, azure.workload.identity/tenant-id<br />(Azure workload identity) and iam.gke.io/gcp-service-account (GKE).<br />Any other ServiceAccount annotation can change what the account is<br />(an OpenShift OAuth client, for one) and is refused. |  | MaxProperties: 6 <br />Optional: \{\} <br /> |
| `podLabels` _object (keys:string, values:[LabelValue](#labelvalue))_ | Labels added to the agent's Pod: azure.workload.identity/use only<br />(Azure workload identity). Any other label could select the agent<br />into a Service or a policy (a CloudNativePG -rw Service, for one). |  | MaxProperties: 1 <br />Optional: \{\} <br /> |




## Cross-field rules

Rules written in CEL (`x-kubernetes-validations`) that the API server enforces and
that the Validation column cannot express, with the type or field each applies to.
They are collected from the same Go markers (`+kubebuilder:validation:XValidation`)
by `docs/site/crd-ref-docs/cel-rules.awk`.

- `ImageSpec`: an image needs a tag, a digest or both
- `ExposureSpec`: hostname is required for route, ingress and gateway exposure
- `ExposureSpec`: gateway is required exactly when type is gateway
- `ExposureSpec`: ingressClassName applies to ingress exposure only
- `ExposureSpec`: tls.certificateSecretRef applies to ingress exposure only
- `ExposureSpec.annotations`: annotation keys under pgtoolbox.fyannk.dev/ are reserved
- `ExposureSpec.labels`: label keys under pgtoolbox.fyannk.dev/ are reserved
- `MetadataPropagation.labels`: label keys under pgtoolbox.fyannk.dev/ and app.kubernetes.io/ are reserved
- `MetadataPropagation.annotations`: annotation keys under pgtoolbox.fyannk.dev/ are reserved
- `EvidenceAgentSpec`: the evidence agent is reachable in-cluster only: exposure.type must be clusterIP
- `EvidenceAgentSpec`: pgbackrest reads a Cluster backed up with a pgBackRest plugin: objectStoreRef and serverName are barman-cloud settings
- `EvidenceAgentSpec.serverName`: serverName must not contain '/' or '&#92;' and must not be '.' or '..'
- `EvidenceAgentSpec.refreshInterval`: refreshInterval must be between 30s and 24h
- `WorkloadIdentitySpec.serviceAccountAnnotations`: serviceAccountAnnotations accepts only the workload-identity annotations eks.amazonaws.com/role-arn, eks.amazonaws.com/sts-regional-endpoints, eks.amazonaws.com/token-expiration, azure.workload.identity/client-id, azure.workload.identity/tenant-id and iam.gke.io/gcp-service-account
- `WorkloadIdentitySpec.podLabels`: podLabels accepts only azure.workload.identity/use
- `EvidenceAgent`: metadata.name must be a DNS-1035 label of at most 40 characters (it prefixes generated object names)
- `AccessSubject`: a ServiceAccount subject is the name of a ServiceAccount in the request's namespace (a DNS subdomain)
- `AccessSubject`: system: users and groups cannot be granted access through a request (system:authenticated would grant everyone); name a person, a group or a ServiceAccount
- `PgAccessRequestSpec`: a PgAccessRequest cannot be changed once made; create a new one
- `PgAccessRequestStatus`: phase moves only Pending to Approved, Denied or Expired, and Approved to Revoked or Expired
- `PgAccessRequestStatus`: decidedBy cannot change once recorded
- `PgAccessRequestStatus`: roleBinding cannot change once recorded
- `PgAccessRequestStatus`: an Approved or Denied request records decidedBy and decidedAt
- `PgAccessRequestStatus`: an Approved request records its roleBinding and clusterRole
- `PgAccessRequestStatus`: a Denied request records why in note
- `PgAccessRequestStatus`: a Revoked request records revokedBy and revokedAt
- `PgAccessRequest`: metadata.name must be a DNS-1123 label of at most 63 characters (it names the RoleBinding and labels it)
- `PgAdminSpec`: clusterRefs and clusterSelector are mutually exclusive
- `PgAdminSpec`: one of clusterRefs, clusterSelector and crossNamespaceClusterRefs is required
- `PgAdminSpec`: replicas above 1 need configDatabase: SQLite on a ReadWriteOnce volume serves one pgAdmin
- `PgAdminSpec`: configDatabase cannot be added to or removed from an existing PgAdmin: pgAdmin's settings do not move between SQLite and PostgreSQL automatically (see the manual move in the documentation)
- `PgAdminSpec.configDatabase`: configDatabase is immutable
- `PgAdminStorageSpec.storageClassName`: storageClassName is immutable
- `PgAdminAuthSpec`: exactly one of oauth2 and openshiftOAuthProxy is required
- `PgAdminOAuth2Spec.requiredClaims`: claim names are 1-64 characters of letters, digits and _.:-
- `PgAdminOpenShiftOAuthProxySpec.sessionMaxAge`: sessionMaxAge must be between 5m and 12h
- `PgAdmin`: metadata.name must be a DNS-1035 label of at most 40 characters (it prefixes generated object names)
- `PgToolBoxConsoleExposureSpec`: networkPolicy.enabled needs networkPolicy.from for ingress and gateway exposure: the controller's namespace is not known
- `PgToolBoxConsolePolicyPeer`: each networkPolicy.from peer needs podSelector, namespaceSelector or ipBlock
- `PgToolBoxConsolePolicyPeer`: a networkPolicy.from peer with ipBlock may not also have podSelector or namespaceSelector
- `PgToolBoxConsoleLabelSelector`: matchLabels keys must be label keys ([prefix/]name, name at most 63 characters)
- `PgToolBoxConsoleLabelRequirement`: values are required for In and NotIn and forbidden for Exists and DoesNotExist
- `PgToolBoxConsoleIPBlock`: networkPolicy.from ipBlock.except entries must be CIDRs strictly inside ipBlock.cidr
- `PgToolBoxConsoleIPBlock.cidr`: networkPolicy.from ipBlock.cidr must be a CIDR
- `PgToolBoxConsoleSpec`: auth.oidc is required with auth.provider=oidc
- `PgToolBoxConsoleSpec`: auth.provider=openshift needs publicURL or exposure.hostname: OpenShift redirects only to the callback the ServiceAccount lists
- `PgToolBoxConsoleSpec`: mcp needs publicURL or exposure.hostname: the MCP endpoint advertises only its configured origin
- `PgToolBoxConsoleSpec`: restoreDrills needs operations: a drill is started through the operations endpoints
- `PgToolBoxConsoleSpec`: prometheus: set tokenSecretRef or projectedToken, not both
- `PgToolBoxConsoleSpec`: digest.delivery needs history.persistent: a delivered digest links to its page, which in-memory history loses at the next restart
- `PgToolBoxConsoleSpec`: digest.delivery needs digest.enabled
- `PgToolBoxConsoleSpec.monitoringURL`: monitoringURL must be https or root-relative
- `PgToolBoxConsoleAuthSpec`: sessionIdleTimeout must not exceed sessionMaxAge
- `PgToolBoxConsoleAuthSpec.sessionIdleTimeout`: sessionIdleTimeout must be between 1m and 12h
- `PgToolBoxConsoleAuthSpec.sessionMaxAge`: sessionMaxAge must be between 5m and 168h
- `PgToolBoxConsoleOIDCSpec`: oidc.grantClient.clientID must differ from oidc.clientID
- `PgToolBoxConsoleEvidenceSpec.interval`: evidence.interval must be between 30s and 1h
- `PgToolBoxConsoleMCPSpec`: mcp.writeTools requires mcp.enabled
- `PgToolBoxConsoleMCPSpec.callTimeout`: mcp.callTimeout must be between 1s and 50s
- `PgToolBoxConsoleMCPSpec.oauthAccessTTL`: mcp.oauthAccessTTL must be between 10s and 1h
- `PgToolBoxConsoleTracingSpec`: tracing.samplerArg needs a traceidratio sampler
- `PgToolBoxConsoleRestoreDrillsSpec.timeLimit`: restoreDrills.timeLimit must be between 10m and 24h
- `PgToolBoxConsolePrometheusSpec`: prometheus.tokenAudience applies to projectedToken only
- `PgToolBoxConsolePrometheusSpec`: prometheus.url must be https when a token is configured: the token is never sent in clear
- `PgToolBoxConsolePrometheusSpec.range`: prometheus.range must be between 5m and 24h
- `PgToolBoxConsoleDigestSpec.retention`: digest.retention must be between 24h and 8784h
- `PgToolBoxConsoleDigestDeliverySpec`: digest.delivery names no channel
- `PgToolBoxConsoleDigestDeliverySpec.timeout`: digest.delivery.timeout must be between 1s and 1m
- `PgToolBoxConsoleDigestSMTPSpec`: smtp.username and smtp.passwordSecretRef are set together
- `PgToolBoxConsole`: metadata.name must be a DNS-1035 label of at most 40 characters (it prefixes generated object names)
