# Changelog

All notable changes to pgToolBox are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

pgToolBox is **pre-1.0**: 0.x releases may change CRD fields, generated
object shapes, and the contract with the components it deploys without a
deprecation period. Pin an exact image tag and read the notes before
upgrading.

## [Unreleased]

Everything below is the first release's content, kept under Unreleased
until a version is cut.

### Added

- **`PgConsole`** — one access stack per CloudNativePG cluster: the
  `pgtoolbox-proxy` authentication proxy, the pgConsole observation UI, an
  embedded pgAdmin dedicated to that cluster, and an optional
  pgObjectStoreViewer evidence sidecar, composed into a single Pod with the
  Service, ServiceAccount, Roles, PVC, NetworkPolicy and exposure it needs.
- **`pgtoolbox-proxy`** — the single authentication and coarse authorization
  boundary. OIDC (authorization code with PKCE S256, state and nonce),
  OpenShift integrated OAuth using the workload ServiceAccount as the OAuth
  client, and local bcrypt accounts rendered from `PgToolBoxUser`.
  AES-256-GCM session cookies with key rotation, per-IP lockout, hot config
  reload, and header hygiene that strips forged identity headers before
  setting its own.
- **`PgToolBoxRole` / `PgToolBoxUser`** — declarative provisioning. A role
  maps a console level (`view` / `poweruser` / `dba`) to a postgres role,
  either an operator-managed CNPG `DatabaseRole` from a profile (`monitor`,
  `database-readonly`, `database-owner`) or a bring-your-own reference. A
  user binds an identity to a role.
- **`PgToolBoxAccessRequest`** — the self-service flow. The proxy's 403 page
  files a request, a `dba` decides it in the console's review panel, and the
  controller materializes the `PgToolBoxUser` on approval under a
  deterministic name, so repeated approvals converge.
- **Embedded pgAdmin sync** — per-user pgAdmin accounts and a server
  definition carrying the saved password of the user's postgres role,
  delivered over an in-pod mTLS admin-sync API. The operator never execs into
  the pod, and posts desired state only after the rollout reaches the target
  config revision.
- **Every exposure type** — OpenShift `Route`, Kubernetes `Ingress`, Gateway
  API `HTTPRoute`, or plain `ClusterIP`, behind a generated default-deny
  `NetworkPolicy`.
- **Repository evidence** — optional pgObjectStoreViewer sidecar consumed
  over a pod-private Unix socket with an immutable per-pod token Secret,
  rotated by annotation and garbage-collected.
- **Packaging** — Helm chart (`deploy/helm/pgtoolbox`), OLM bundle and
  file-based catalog (`deploy/olm`), and a kustomize development overlay
  (`config/default`).

### Changed

- The console's generated read Role now carries the whole read surface
  pgConsole's own deploy manifest grants — poolers, declared database
  objects, services, persistent volume claims, image catalogs, failover
  quorums, the pooler-ownership walk, and the children-inventory kinds —
  instead of the six rules it originally had. It still grants nothing on
  `secrets`.
- `spec.console` replaces the hardcoded `ALLOW_OPERATIONS` and `ALLOW_LOGS`
  environment variables and makes the console's behaviour declarative. Each
  capability moves its authority with it: switching one off removes the
  matching rules from the generated Roles, and the operate Role is deleted
  rather than left empty.
- The pgObjectStoreViewer API module is consumed from its published version
  rather than a `replace` directive pointing at a sibling checkout, so the
  repository builds without one.

### Fixed

- The dba access-request review panel is now reachable. `ALLOW_ACCESS_REVIEW`
  was never rendered and the console defaults it off, so the review loop the
  operator already implemented end to end had no UI.
- `TRUSTED_LEVEL_HEADER` and `PGADMIN_URL` are now rendered: the level header
  is stated explicitly rather than inherited from the application's default,
  and the console offers a pgAdmin link-out derived from the exposure
  hostname.
- `make manifests` generates the manager ClusterRole alongside the CRDs and
  syncs both into the Helm chart and the OLM bundle, which were previously
  hand-maintained copies that a new controller rule could silently miss.
- Lint exclusions were declared under the golangci-lint v1 `issues` key,
  which v2 parses and ignores; `make lint` was passing only on a warm cache.
