# Changelog

All notable changes to pgToolBox are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

pgToolBox is **pre-1.0**: 0.x releases may change CRD fields, generated
object shapes, and the contract with the components it deploys without a
deprecation period. Pin an exact image tag and read the notes before
upgrading.

## [Unreleased]

## [0.1.0] - 2026-08-09

First release. Everything below is what it contains; the development
history that led here is in the git log rather than in this file, because
none of it changed anything anyone had installed.

### Added

- **`PgConsole`** — one access stack per CloudNativePG cluster: the
  `pgtoolbox-proxy` authentication proxy, the pgConsole observation UI, an
  embedded pgAdmin dedicated to that cluster, and an optional
  pgObjectStoreViewer evidence sidecar, composed into a single Pod with the
  Service, ServiceAccount, Roles, PVC, NetworkPolicy and exposure it needs.
  `cnpgClusterRef` is immutable: a console serves one cluster for its whole
  life.
- **`pgtoolbox-proxy`** — the single authentication and coarse authorization
  boundary. AES-256-GCM session cookies with key rotation, per-IP lockout,
  hot configuration reload, and header hygiene that strips any forged
  identity header before setting its own.
- **Three authentication providers, any mix at once** — OIDC
  (authorization code with PKCE S256, state and nonce), OpenShift integrated
  OAuth using the workload ServiceAccount as the OAuth client, and local
  bcrypt accounts. With more than one enabled the login page shows the local
  form with a button per identity provider beside it, and the session
  records which provider authenticated the user. A provider that cannot
  start — an unreachable issuer, a rejected client secret — is logged and
  skipped rather than taking the others down with it, so local sign-in
  survives an outage at the identity provider.
- **`bootstrapAdmin`** — every console declares its first administrator, and
  the operator materializes it as a `dba` `PgToolBoxUser` it owns and
  re-creates if deleted. Without it a console could be deployed with nobody
  able to approve the first access request.
- **`PgToolBoxUser`** — one identity on one console at one level (`view` <
  `poweruser` < `dba`). The level set is closed and hardcoded on both sides
  of the contract. An optional `localPasswordSecretRef` (bcrypt) is what
  makes a user usable at the local form; a federated user carries none.
- **`PgToolBoxAccessRequest`** — the self-service flow. The proxy's 403 page
  files a request, a `dba` decides it in the console's review panel, and the
  controller materializes the `PgToolBoxUser` on approval under a
  deterministic name, so repeated approvals converge.
- **Embedded pgAdmin, ready to connect** — reachable at `/pgadmin` behind
  the same proxy for sessions at or above `pgAdmin.accessMinLevel` (`dba` by
  default), authenticated by the identity the proxy already established. It
  offers the connections the CloudNativePG cluster publishes — the
  application user, the superuser where one is enabled, and every
  `DatabaseRole` carrying a password — with credentials delivered through a
  pod-private `PGPASSFILE`, never through pgAdmin's saved-password store.
  Accounts and server definitions are synced over an in-pod mTLS admin-sync
  API; the operator never execs into the pod.
- **Declarative console capabilities** — `spec.console` decides which
  screens exist, and each capability moves its authority with it: switching
  one off removes the matching rules from the generated Roles, so RBAC
  denies the operation whatever the application is told.
- **Every exposure type** — OpenShift `Route`, Kubernetes `Ingress`, Gateway
  API `HTTPRoute`, or plain `ClusterIP`, behind a generated default-deny
  `NetworkPolicy`.
- **Repository evidence** — optional pgObjectStoreViewer sidecar consumed
  over a pod-private Unix socket with an immutable per-pod token Secret,
  rotated by annotation and garbage-collected.
- **Packaging** — Helm chart (`deploy/helm/pgtoolbox`), OLM bundle and
  file-based catalog (`deploy/olm`), and a kustomize development overlay
  (`config/default`). The CRDs and the manager ClusterRole are generated
  once and copied into all three by `make manifests`.
- **`make dev-up`** — a browsable console on kind: the operator, one
  `PgConsole` with pgAdmin and the evidence sidecar, users seeded at each
  authorization level, and the real proxy forwarded to localhost. It stands
  up against a real identity provider too (`AUTH_MODE=local,oidc`).
- **`make test-e2e`** — the stack on a real cluster: CRD validation rules
  that only an API server evaluates, pgAdmin actually reaching PostgreSQL,
  the evidence sidecar, and the bootstrap admin returning after deletion.

### Known limitations

- **No group or claim mapping.** Access is granted per identity, either by
  declaring a `PgToolBoxUser` or by a `dba` approving a request. An identity
  provider's groups do not select a level.
- **One subject per user.** A person whose identity-provider subject differs
  from their local username needs one object per subject.

[Unreleased]: https://github.com/fyannk/pgToolBox/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/fyannk/pgToolBox/releases/tag/v0.1.0
