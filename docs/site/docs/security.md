---
sidebar_position: 7
title: Security model
---

# Security model

Report vulnerabilities through GitHub private vulnerability reporting
(<https://github.com/fyannk/pgToolBox/security/advisories/new>), as
[SECURITY.md](https://github.com/fyannk/pgToolBox/blob/main/SECURITY.md) explains;
the results of the latest security run (every scanner, the open findings with
whether a fix exists, the scanned image digests and the OpenSSF Scorecard score) are
on the [security scan results](/security/) page. This page states
what the design guarantees and, as plainly, what it does not.

## The design in five rules

1. **Kubernetes owns identity and authorization.** The console decides nothing about
   who a person is or what they may do; it asks the API server
   ([authentication](console/authentication.md), [authorization](console/authorization.md)).
2. **No level is ever stored.** The session cookie holds the person's sealed
   token and nothing else. Revocation is the next SelfSubjectAccessReview (views, within
   10 seconds) or the API server refusing the token (writes, immediately), plus a
   [revocation list](console/authentication.md#revoking-sessions) for console
   sessions.
3. **Writes are the person's.** Every write goes to the API server with the
   person's own token, so the API server authorizes, admits and audits it under their
   name. A stale cache can never grant a write.
4. **No cluster-scoped console, no Secret verb.** A console's reach is bounded by the
   namespaces its Roles name. Its ServiceAccount cannot read a Secret, cannot write,
   and cannot impersonate. The one opt-in exception is the
   [restore drill](console/restore-drills.md#which-clusters-the-console-deletes)
   teardown: `delete clusters` in the drill namespace, used only on Clusters this
   console sealed. On Kubernetes 1.30+ an admission policy enforces that on the
   API server ([the delete guard](console/restore-drills.md#the-delete-guard)).
5. **Credentials stay where they belong.** Object-store credentials stay in the
   Cluster's namespace inside the evidence agent; database passwords stay in
   Secrets and reach only the pgAdmin Pod's memory-backed volume.

## Who can do what

| Component | Reads | Writes | Cannot |
|---|---|---|---|
| **Console ServiceAccount** | The CloudNativePG kinds, pods, services, PVCs, events, PDBs, leases, jobs, `pgadmins`, `evidenceagents`, `objectstores` in the watched namespaces; `pods/log` unless withheld; one ConfigMap by name | Nothing. It asks the API server with SelfSubjectAccessReviews sent with the reader's own token, which need no grant | Read a Secret; use `impersonate`; read outside its namespaces; write |
| **A person through the console** | What their own RBAC lets them `get` | Back up, reload, restart, promote, **with their token** | Anything the API server denies them |
| **Operator** | Its watched namespaces' CRs and what they reference (Secret *metadata* only for user Secrets) | The children of `PgAdmin` and `EvidenceAgent`; their status | Hold a database password |
| **Evidence agent ServiceAccount** | The object store: with the operator, through credential files mounted in its own Pod; in team mode, through the Secrets its values list, read with a name-scoped Role | Nothing in the store | With the operator, anything on the API server except creating TokenReviews and SubjectAccessReviews; in team mode, anything but its Cluster, ObjectStore and listed Secrets |
| **pgAdmin Pod** | Offered Secrets and `<cluster>-ca`, through a name-scoped Role | Its own volumes | Read any other Secret |

## Boundaries and their checks

- **Browser to console**: TLS at the edge or in the pod; a sealed `__Host-`
  prefixed cookie (`HttpOnly`, `SameSite=Strict`); a custom request header plus a
  same-origin check on every unsafe method; sign-in rate limit; the server-rendered sign-in
  pages work without JavaScript and carry a Content-Security-Policy that allows no
  script and no framing.
- **Console to API server**: informers as the ServiceAccount; reviews
  (SelfSubjectReview, SelfSubjectAccessReview) and writes as the person, with
  their own token.
- **Console to instance pods**: `/pg/status` over TLS 1.3 verified against a CA
  bundle the admin supplies. A Cluster not in the bundle is "not verifiable" and
  never read; there is no insecure mode and no plaintext read on any CloudNativePG
  version.
- **Console to evidence agent**: verified TLS to `status.endpoint` with
  `status.caBundle` (trusted only when the operator wrote them), and a kubelet-rotated
  token bound to the agent namespace's own audience; the agent checks
  it with TokenReview and SubjectAccessReview ([details](evidence-agent.md#the-channel)).
- **Operator to workloads**: server-side objects the operator owns; a metadata-only
  view of Secrets, cached without annotations (so not kubectl's
  `last-applied-configuration`, which holds the data) and with only the labels the
  operator reads; `ResourceNotOwned` instead of adopting.
- **pgAdmin to PostgreSQL**: `verify-full`, SCRAM with channel binding.
- **Metrics**: authenticated and authorized by token on both the operator and the
  console.
- **Client addresses**: `X-Forwarded-For` is believed only from the proxies
  named in `TRUSTED_PROXY_CIDRS`, else ignored, so a Pod that reaches the console
  directly cannot escape the per-address sign-in budgets or forge `client_ip`.
  Without trusted CIDRs behind a declared proxy, or with no proxy declared
  behind a public URL, every client may have the proxy's address, so no
  per-address budget is charged and only the per-replica bounds limit sign-ins;
  the operator renders an ingress NetworkPolicy for a published Route (and for an
  Ingress or Gateway that names its controller) and only then trusts every
  admitted peer ([details](operator/console.md#trusted-proxies-and-the-ingress-networkpolicy)).

- **What an unauthenticated flood still costs.** The per-replica bounds
  protect the API server and the identity provider, and are kept apart from
  what a genuine sign-in needs once it has proved something, so junk cannot
  starve people below these rates (per replica):
  - junk OAuth callbacks: a refused code exchange each, at most 100 a second
    to the IdP, and no SelfSubjectReview (that is charged only after a
    successful exchange). Above 100 a second new OAuth sign-ins are refused
    while the flood lasts;
  - junk pasted tokens and unknown MCP bearers: a SelfSubjectReview each that
    the API server refuses, at most 100 a second. Above that, new token
    sign-ins and new raw bearers are refused; an MCP client whose token the
    replica accepted before re-reviews from a bucket of its own;
  - junk `/mcp/oauth/token` requests, and codes or refresh tokens the console
    sealed but that no longer buy anything (expired, another client's, a
    failed PKCE check, a revoked person or grant, a spent code replayed): a
    form parse and a decryption each; they never reach the bucket connected
    clients refresh from, so holding a token once is not a way to starve
    them; past 20 a second they get `429` and no audit line. A live grant
    spends a bucket of its own (6 at once, 12 a minute) before the shared
    one, so one grant refreshing in a loop cannot starve the others either;
  - `/mcp/oauth/register`: up to 10 a second sealed and audited; above that,
    new registrations wait. Registered clients are unaffected.

  Open sessions, connected MCP clients and issued grants keep working in every
  case. Each refusal is counted in
  `pgtoolbox_console_throttled_requests_total{bucket}`; alert on a sustained
  rate (see [metrics](console/metrics.md#throttled-requests)) and name the
  proxies so per-address budgets stop a single sender first.

## Writes: token passthrough, never impersonation

The IdP token is an API-server token (OpenShift OAuth, API-server OIDC flags or
`AuthenticationConfiguration`, or a pasted token), so the console can send the
person's own token to the API server. This is the same prerequisite Headlamp, Argo CD
and the Kubernetes Dashboard have. Platforms where the API server cannot accept the
IdP's tokens use the `token` provider.

An *impersonation mode* (the console verifies the OIDC token itself and calls the API
server with its own token plus `Impersonate-User`) does not exist. If it is ever
added it will be an opt-in with a chart flag, off by default, with loud
documentation: the grant is equivalent to cluster-admin and the console would
become the authority on who the user is, which is exactly what the design avoids.
No Role or ClusterRole in the product contains the `impersonate` verb.

## pgAdmin is a credential-holder

Whoever can open a pgAdmin holds the credentials of every role it offers. The
operator therefore:

- offers the **application user and applied `DatabaseRole`s only**, with privileged
  credentials behind an explicit `credentials.superuser: true`, and (admission
  policy) lets only someone who may `get` the namespace's Secrets ask for them,
  widen who may sign in (`spec.auth` but its display name, `spec.admins`), raise
  the OpenShift proxy's `sessionMaxAge`, or put the settings database on a Cluster
  of the namespace (`configDatabase`);
- gates opening it by identity (`requiredClaims` on Kubernetes,
  `connect pgadmins/<name>` on OpenShift, a verb that neither `view` nor `edit`
  carries, even where OLM aggregates `get` on its CRDs into `view`), and makes
  `pgtoolbox-console-operator` the only user role that grants that verb (besides the
  namespace `admin`), and bounds the OpenShift oauth-proxy's session (it reviews
  `connect` at sign-in only) to the session lifetime, 1 h by default, up to
  `auth.openshiftOAuthProxy.sessionMaxAge`; raising it is checked like widening
  who may sign in (below);
- with `configDatabase`, creates a login role on that Cluster, whose password the
  pgAdmin Pod holds: setting it is checked like offering credentials, and its
  name must start with `pgadmin_`, because CloudNativePG adopts a role of that
  name that already exists in PostgreSQL and resets its password and
  memberships;
- keeps passwords out of the operator, out of the Pod spec, out of
  pgAdmin's file manager and off disk (a memory-backed volume);
- reports, but does not enforce, whether the Pod's egress is confined.

Details: [PgAdmin](operator/pgadmin.md).

## Exposure annotations that configure the ingress controller

`spec.exposure.annotations`, and the annotations of `metadataPropagation` rules
whose target is `All` or `Exposure`, land on the Ingress, Route or HTTPRoute the
operator writes; rules whose target is `All` or `Service` land on the Service it
routes to, where Kong and HAProxy read some of the same keys. Some annotations
are configuration an ingress controller runs: written by the operator, they
would let an editor who may not create an Ingress inject proxy configuration,
claim a host outside `EXPOSURE_ALLOWED_HOST_SUFFIXES` (the operator checks only
`spec.exposure.hostname`), open the console to other networks, or send pgAdmin's
and the console's traffic, cookies included, to a server they run. The admission
policy (on by default) therefore requires **`create` on the exposure kind**
(`ingresses`, `routes` or `httproutes` in the namespace) to add or change one of
these keys, or to switch an object carrying one from `clusterIP` to an exposure.
In the table, `name*` is every key that starts with `name`:

| Prefix | Keys | Controller | What they change |
|---|---|---|---|
| (any) | `*snippet*` | every controller with snippets | raw proxy configuration |
| `nginx.ingress.kubernetes.io/` | `auth-*`, `mirror-*`, `canary*`, `cors-*`, `proxy-redirect-*`, `enable-global-auth`, `satisfy`, `proxy-ssl-secret`, `server-alias`, `permanent-redirect`, `temporal-redirect`, `app-root`, `default-backend`, `custom-http-errors`, `whitelist-source-range`, `allowlist-source-range`, `denylist-source-range`, `enable-cors` | ingress-nginx | authentication, extra hosts, source ranges, redirects, another backend, traffic split, cross-origin access |
| `alb.ingress.kubernetes.io/` | `actions.*`, `conditions.*`, `auth-*`, `scheme`, `group.name`, `group.order`, `inbound-cidrs`, `security-groups`, `listen-ports` | AWS Load Balancer Controller | internet-facing scheme, a shared load balancer, source ranges, routing actions, authentication |
| `haproxy-ingress.github.io/` | `config-*`, `auth-*`, `cors-*`, `allowlist-source-range`, `whitelist-source-range`, `denylist-source-range`, `redirect-from`, `redirect-to`, `app-root` | haproxy-ingress | raw configuration, authentication, source ranges, redirects |
| `haproxy.org/` | `auth-*`, `cors-*`, `allow-list`, `whitelist`, `deny-list`, `blacklist`, `request-redirect`, `route-acl` | HAProxy Kubernetes Ingress | authentication, source ranges, redirects, routing |
| `haproxy.router.openshift.io/` | `ip_whitelist`, `ip_allowlist` | OpenShift router | source ranges |
| `traefik.ingress.kubernetes.io/` | `router.middlewares`, `router.entrypoints` | Traefik | middlewares, which entry point (network) serves it |
| `konghq.com/` | `plugins`, `host-aliases` | Kong | plugins, extra hosts |

A kept annotation with the same value, on a rule reaching the same objects and
an object exposed the same way, is not checked again. Keys that tune how a
request reaches the workload's own Service are not checked: timeouts, body size,
affinity, `rewrite-target`, `use-regex`, `upstream-vhost`, `backend-protocol`
(they can break the route, not move it). The Service the operator renders is
always `ClusterIP` (written with server-side apply), so the cloud load-balancer
keys (`service.beta.kubernetes.io/*`) do nothing on it. Choosing the exposure is
not gated: `spec.exposure.type`, the hostname (under
`EXPOSURE_ALLOWED_HOST_SUFFIXES`), `ingressClassName` and the Gateway `parentRef`
(which the Gateway's `allowedRoutes` accepts or not) are what an editor is
allowed to set; restrict the classes and Gateways a namespace may use where the
platform provides them.

**Limits:** this is a deny-list of known keys, not every controller's. A
controller run with a custom annotation prefix (ingress-nginx
`--annotations-prefix`, for example) reads keys this list does not name. A
`clusterIP` object renders no exposure, so its Service annotations are not
checked; an Ingress someone else creates for that Service makes its controller
read them. Where the policy is off the operator writes what the spec says. Grant
`pgadmins` and `pgtoolboxconsoles` writes only to people you would let create
the Ingress.

## Secret references need `get`

A `PgToolBoxConsole`, a `PgAdmin` or an `EvidenceAgent` names Secrets of its own
namespace: the SMTP password, the chat and generic webhook URLs of the digest,
the OIDC or OpenShift client secrets, the Prometheus bearer token, the tracing
exporter's headers, the restore-drill seal keys, an Ingress certificate, image
pull secrets, the Secrets an evidence agent may read (`spec.allowedSecrets`). The
operator mounts them into the workload, and the workload sends them where the
spec says (an SMTP host, a webhook URL, an identity provider). RBAC on the custom
resource alone would therefore let anyone who may edit a `PgToolBoxConsole` read
any Secret of the namespace by pointing the console at a server they run.

**Editing a `PgToolBoxConsole`, a `PgAdmin` or an `EvidenceAgent` requires `get` on
every Secret it references.** The rule is part of the operator chart's
`ValidatingAdmissionPolicy` (on by default), which asks the API server's authorizer, as the
requesting user, whether that user may `get` each named Secret, and refuses the
request otherwise (reason `Forbidden`, message "editing a PgToolBoxConsole, a PgAdmin
or an EvidenceAgent requires get on every Secret it references").

- **It is on by default** in the operator chart (`operator.admissionPolicy.enabled`;
  `validationActions` selects `Deny`, `Warn` and/or `Audit`, `Deny` alone enforces).
  It is the only check of this rule: where the policy is off, grant
  `create`/`update` on `pgtoolboxconsoles`, `pgadmins` and `evidenceagents` only to
  people you would trust with every Secret of the namespace.
- **kustomize applies it only when you opt in, and the OLM bundle cannot ship it.**
  kustomize: add the component `config/components/admission` (the chart's policy
  with its default allow-lists, see [kustomize](install/kustomize.md#the-admission-policy)).
  OLM: render it from the chart and apply it yourself (see [OLM](install/olm.md#admission-policies)):
  `helm template pgtoolbox charts/pgtoolbox --set operator.enabled=true --show-only templates/operator/admissionpolicy.yaml | kubectl apply -f -`,
  or apply the same trust rule as above.
- **On update, only new or changed references are checked.** The operator's own
  writes (finalizers) and edits that leave the references alone keep working on
  objects that exist already, and a Secret already referenced at a field, and
  sent to the same place, is no new disclosure. Moving a Secret to another field,
  or renaming it, is a change. Objects created before the policy was turned on
  keep what they reference until they are edited to reference something else:
  review them when you enable it.
- **The policy pairs a reference with where it is sent.** Keeping a reference and
  changing its destination is a new reference, checked again. The destination is
  every field that decides where the Secret goes and whom it is trusted with:
  `auth.oidc.issuerURL` and `auth.oidc.caConfigMapRef` for both OIDC client
  secrets, `auth.openshift.caConfigMapRef` for the OpenShift client secret,
  `prometheus.url` and `prometheus.caConfigMapRef` for `prometheus.tokenSecretRef`,
  `digest.delivery.smtp.address` and `tls` for the SMTP password,
  `tracing.endpoint` for `tracing.headersSecretRef`, `auth.oauth2.issuerURL` for
  the PgAdmin client secret, and the image's `repository` for its `pullSecrets`.
  For an `EvidenceAgent`'s `allowedSecrets` it is `clusterRef`, `objectStoreRef`,
  `allowInsecureEndpoint` and `pgbackrest.repository`, `endpoint` and
  `endpointCA`. The other references name no destination in the spec: the digest
  webhooks keep their URL inside the Secret, the seal keys and the Ingress
  certificate are sent nowhere. A test classifies every field beside a reference
  as a destination or not, with a reason, and fails on a new one.
  **Limit:** what the policy compares is the fields, not what they lead to.
  Editing the CA ConfigMap itself, the `ObjectStore`, `Stanza` or `Archive` an
  agent reads, a DNS record or a proxy is not seen; so do not let people who may
  not read a Secret edit those objects.
- **More than two new references at once** need `get` on every Secret of the
  namespace. An authorizer check costs most of a CEL expression's runtime
  budget, so the policy checks one or two added references name by name, and a
  larger change with one namespace-wide check. Somebody who may read only some
  Secrets adds them two at a time.
- **What is covered**: every Secret reference in the three kinds. On a
  `PgToolBoxConsole`: `auth.oidc.clientSecretRef`,
  `auth.oidc.grantClient.clientSecretRef`, `auth.openshift.clientSecretRef`,
  `prometheus.tokenSecretRef`, `digest.delivery.*`, `tracing.headersSecretRef`,
  `restoreDrills.sealKeySecretRef`, `exposure.tls.certificateSecretRef` and
  `image.pullSecrets`. On a `PgAdmin`: `auth.oauth2.clientSecretRef`,
  `exposure.tls.certificateSecretRef` and the `pullSecrets` of both images. On an
  `EvidenceAgent`: each name of `spec.allowedSecrets`. The list is what the agent
  editor consents to the agent reading; it means something only from somebody who
  may read those Secrets. A test walks the API types (typed references, core/v1
  Secret types, map values, and string fields named after Secrets) and fails when
  a new Secret reference is missing from the policy or has no stated destination.
- **What is not covered on an `EvidenceAgent`.** `pgbackrest.endpointCA` is a CA
  bundle the agent only trusts, and the agent reads it only if `spec.allowedSecrets`
  lists it too. `image.pullSecrets` goes to the kubelet for `spec.image`, which only
  the `imageOverride` allow-list may set. `exposure.tls.certificateSecretRef` cannot
  be set, since an agent is `clusterIP` only. The Secrets the agent reads for the
  repository are named by the Cluster and the `ObjectStore`, `Stanza` or `Archive`,
  and limited to `spec.allowedSecrets` ([evidence agent](evidence-agent.md)). An
  empty `allowedSecrets` (the deprecated behaviour, barman-cloud `ObjectStore`
  only) lets the agent read what the `ObjectStore` names, which the backup plugin
  already sends to the endpoint the `ObjectStore`'s editors chose.

**A `PgAdmin` that offers Cluster credentials requires `get` on the Secrets of its
namespace.** Whoever opens pgAdmin holds the credentials it offers
(`spec.credentials`: the application user and `DatabaseRole`s by default, the
superuser with `superuser: true`). Their Secrets are named by the CloudNativePG
Cluster, not by the `PgAdmin`, so the policy cannot list them by name. It asks
instead whether the requesting user may `get` Secrets in the namespace (any
name): a Role with `get` on a few named Secrets does not qualify. The rule
applies when a `PgAdmin` is created with a credential kind on and a Cluster of
its namespace (`clusterRefs` or `clusterSelector`), and on an update that turns a
kind on, adds a Cluster or changes the selector. Narrowing, and edits that leave
the offer alone, are not checked. Without it, a custom role with `create pgadmins`
and `connect pgadmins` but no `get secrets` could ask for `superuser: true` and
open pgAdmin with the superuser's password. `crossNamespaceClusterRefs` are
outside the rule: the `PgAdminAccess` grant of the target namespace, created by
its owner, decides them (privileged credentials only with `allowPrivileged`).
Changing who may sign in to such a `PgAdmin` is checked the same way, since it
hands the same credentials to other people: any change to `spec.auth` (the
issuer, client, required claims, username claim and scopes of `oauth2`, a switch
between `oauth2` and `openshiftOAuthProxy`, the proxy's image) or to
`spec.admins`. Only `auth.oauth2.displayName`, a button label, is left out; a
test fails when a new `PgAdmin` field is not classified. A `PgAdmin` that offers
no credentials is not concerned.
Message: "a PgAdmin that offers the credentials of Clusters of its namespace
(spec.credentials) requires get on the Secrets of the namespace".

**A `PgToolBoxConsole` that projects its ServiceAccount token to Prometheus
requires `create` on that ServiceAccount's token.** `spec.prometheus.projectedToken`
sends the console ServiceAccount's own token to `spec.prometheus.url`; with the
default audience (the API server's, which OpenShift's thanos-querier expects),
whoever runs that URL can use it against the API server for up to an hour. So
setting it, or changing the `url`, `caConfigMapRef` or `tokenAudience` it goes
with, requires `create` on `serviceaccounts/token` for the console's
ServiceAccount (the console's name, or `<name>-console` when the name does not
contain `console`): the power to mint the same token. Reading Secrets does not
qualify. Set `spec.prometheus.tokenAudience` when your Prometheus front accepts
a dedicated audience: the token is then useless against the API server.
Message: "a PgToolBoxConsole that sends its projected ServiceAccount token to
Prometheus (spec.prometheus.projectedToken) requires create on
serviceaccounts/token for the console's ServiceAccount".

## Known limitations

These are accepted for the current release and listed so you can decide with them
in view.

| Limitation | Effect | Mitigation |
|---|---|---|
| **Who may write `evidenceagents/status` in a namespace can receive that namespace's evidence token.** The console calls an agent only when the operator's field manager wrote `status.endpoint` and `status.caBundle`, but a writer can name itself after the operator. | Read access to the backup metadata of that same namespace's agents only: each namespace has its own token audience. | Grant `update`/`patch` on `evidenceagents/status` to the operator only. Details on [the evidence agent](evidence-agent.md#trust). |
| **Sign-out does not revoke the token** at the identity provider or OpenShift's OAuth server. | A copied token works until it expires. | Short token lifetimes at the IdP; the console's [revocation list](console/authentication.md#revoking-sessions) ends *console* sessions. |
| **The console caches a positive identity for 30 s and a positive SAR for 10 s.** | A revoked person may keep seeing views for up to that long; writes stop at once. | None needed; stated for incident response. |
| **An expired or revoked access request does not end access already open.** Deleting the RoleBinding stops new authorizations only. | Up to the console's caches above (10 s per SAR, 30 s per identity) in the console; in pgAdmin on OpenShift, whose oauth-proxy checks access at sign-in only, for the session lifetime: **1 h by default, up to `auth.openshiftOAuthProxy.sessionMaxAge`**; and as long as they last, pgAdmin's open database connections and websockets. | For an incident, delete the PgAdmin's `<name>-pgadmin-proxy-cookie` Secret and then its Pod (this ends every pgAdmin session of it), and terminate the user's backends in PostgreSQL ([access requests](console/access-requests.md#lifecycle)). |
| **The four-eyes check covers the console's approval path only.** Approvers hold `create rolebindings` and `bind` on both roles. | An approver can bind `pgtoolbox-console-operator` to themselves directly with `kubectl`, with no request and no second person; the operator does not see such a binding. | Grant `pgtoolbox-console-approver` only to people trusted with the namespace's access; watch RoleBindings to the two roles in the API server's audit log. |
| **`/metrics` on the console's probe port is plain HTTP when the console has no certificate.** | A scraper's token crosses the pod network in clear text. | Set `console.tls.secretName`: the probe port, `/metrics` included, is then HTTPS with the console's certificate ([metrics](console/metrics.md)). |
| **The OpenShift sign-in using the ServiceAccount's projected token as client secret is not yet verified on a live cluster.** | If OpenShift rejects it, sign-in fails with `invalid_client`. | Use a long-lived token Secret ([checklist](console/openshift.md#manual-checklist), step 5). |
| **The pgAdmin container's root filesystem is writable.** | An attacker with code execution in the container can write to its filesystem. | Restricted Pod Security, no capabilities, optional NetworkPolicy; read-only is an image change on the roadmap. |
| **Shared pgAdmin servers carry one identity per role.** | PostgreSQL's logs cannot name the person behind a shared server. | The Kubernetes API audit log names who opened pgAdmin on OpenShift; per-person roles are a roadmap item. |
| **The operator's ServiceAccount is effectively cluster-admin.** Its ClusterRole holds Secret, ServiceAccount, Deployment, Role and RoleBinding verbs and `bind` on named ClusterRoles in every namespace, because it creates the Secrets, accounts and Roles of the workloads it runs. | Whoever obtains its token (a compromised operator, or anyone who can exec into its Pod, edit its Deployment or read a token of its ServiceAccount) can read every Secret, and run a Deployment as any ServiceAccount, in every namespace: treat it as cluster-admin. | Run the operator in a namespace of its own where only cluster administrators hold any right; restrict `pods/exec`, Deployment edits and `serviceaccounts/token` there. `operator.config.watchNamespaces` narrows what it reconciles, not what its ClusterRole allows; one operator per cluster is the supported layout. |
| **`pgtoolbox-console-operator` is effectively database-superuser.** `patch clusters` covers the whole `Cluster` spec (`enableSuperuserAccess`, `managed.roles`, `postgresql.parameters`). | Its holder, and the grantee of an `operator` access request, can become the superuser of the namespace's Clusters. | Grant and approve it as you would the `postgres` password ([authorization](console/authorization.md#the-two-clusterroles)). |

## Network

NetworkPolicies are optional and off by default, except the console's ingress
policy, which the operator renders for a published Route and for an Ingress or
Gateway that names its controller in `spec.exposure.networkPolicy.from`. RBAC and SubjectAccessReview
are the isolation the product guarantees; a policy is an additional boundary where
your CNI enforces one. See [console](console/networkpolicy.md),
[operator](operator/index.md#networkpolicy-and-admission-policy) and
[PgAdmin](operator/pgadmin.md#networkpolicy).
