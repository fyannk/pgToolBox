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

- **`make dev-up`** — a browsable console on kind: the operator, one
  `PgConsole` with pgAdmin and the evidence sidecar, a user seeded at each
  authorization level, and the real proxy forwarded to localhost.
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

- pgAdmin offers the connections the CloudNativePG cluster publishes — the
  application user, the superuser where one is enabled, and every
  `DatabaseRole` carrying a password — instead of anything derived from who
  signed in. Every account gets the same list.
- A console with pgAdmin is revisited every two minutes. pgAdmin accounts
  appear on sign-in, which is not an API event, so no watch fires and no
  reconcile is queued; without the periodic pass a new reader would sit in
  front of an empty server list until something unrelated changed.
- Whether the desired state is already present is now decided by the
  sidecar rather than by an annotation on the Deployment. Only that side can
  see it: the state is files in each account's storage and rows in pgAdmin's
  database, and accounts arrive without the operator being told.

- **`PgToolBoxRole` and `PgToolBoxUser` configure the proxy and nothing
  else.** A role is a console authorization level; it is not a postgres
  role and has no relationship with the CloudNativePG cluster. The
  postgres backing removed with this: `spec.postgresRole` and its
  profiles, `status.databaseRoleName`, the `DatabaseRoleReady` and
  `CredentialReady` conditions, the per-user `PgAdminSynced` condition and
  status field, and the `DatabaseRole` + password Secret machinery in the
  `PgToolBoxRole` controller, which now creates nothing. This is a
  breaking CRD change: `spec.postgresRole` was required.
- pgAdmin server provisioning is inert while it is rebuilt on the
  cluster's own credentials. `PgAdminSynced` reports that plainly rather
  than claiming a sync; a dba can add a connection by hand meanwhile.

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
- A `PgConsole` that named no images was rejected by the API server. The
  image fields were value structs, and Go does not omit an empty struct, so
  they serialized as `image: {}` and failed `ImageSpec`'s required
  `repository` and `tag` — which made the operator's `--default-*-image`
  flags unreachable and the documented quick start invalid. They are
  pointers now, as `evidence.image` already was.
- **The embedded pgAdmin could never start, and its user sync could never
  run.** Five independent defects, each of which alone was enough to break
  the feature, all found by the new e2e smoke test:
  - the operator never rendered `PGADMIN_DEFAULT_EMAIL` /
    `PGADMIN_DEFAULT_PASSWORD_FILE`, so pgAdmin refused to start and its
    settings database was never initialized. A per-console bootstrap
    credential is now generated once and mounted as a file; nobody signs in
    with it, it exists only to unlock the settings database.
  - `Reconciler.AdminSync` was never wired in `main.go`, so the controller
    reported pgAdmin sync as "not configured" and provisioned nothing.
  - the sync gate compared the configuration checksum against the
    Deployment's own annotations, where nothing writes it, instead of the
    Pod template's — so it was permanently "not at the current revision".
  - the admin-sync sidecar never mounted the pgAdmin settings volume, so
    every `setup.py` call failed on a SQLite path it could not see.
  - the sidecar called `update-external-user` without ever calling
    `add-external-user`, so the account did not exist and loading its server
    definition failed. The client timeout was also raised from 30s: one sync
    is a batch of `setup.py` invocations, each booting Flask and running
    migrations.
- The pgAdmin link-out is now the root-relative `/pgadmin`, which resolves
  correctly however the console was reached. It used to be built from
  `consoleBaseURL`, whose fallback without an exposure hostname is the
  proxy's own in-Pod loopback address — so a `clusterIP` console rendered
  a link to `http://localhost:8080/pgadmin` that resolved to nothing.
  This raises the pgConsole floor to **0.2.0**, which accepts
  root-relative link-outs for same-origin siblings; 0.1.x validates them
  as absolute URLs only and refuses to start on this value.
- **pgAdmin had the password and never used it.** The `.pgpass` named the
  cluster's Service, but libpq matches a line against the host string it was
  given rather than the host it resolves, so a connection made by address
  matched nothing and failed with `fe_sendauth: no password supplied` — the
  credential present in the file and never consulted. The host field is now
  the wildcard, which matches however pgAdmin connects. It costs nothing:
  the file is pod-private, mounted only by pgAdmin and the sidecar that
  writes it, and holds none but this console's roles for this one cluster.
- **The generated NetworkPolicy forbade the one connection pgAdmin exists
  to make.** Egress allowed DNS and the Kubernetes API and nothing else, so
  "connect to server" hung on a dropped connection until the proxy's
  upstream timeout and surfaced as a bare `502 Bad Gateway` with nothing in
  any log to explain it. Egress to PostgreSQL is now granted when pgAdmin
  is composed, scoped by CloudNativePG's own `cnpg.io/cluster` label to
  this console's cluster rather than to PostgreSQL at large.
- **A restarted console lost its pgAdmin credentials and reported success.**
  The sync revision was recorded on the Deployment, which survives a
  rollout, while the `.pgpass` it tracks lives in an `emptyDir`, which does
  not. After any restart the annotation still matched, the sync was skipped
  as "up to date", and pgAdmin was left with no credentials while
  `PgAdminSynced` stayed True. The recorded revision now names the Pod it
  was applied to, so replacing the Pod invalidates it.
- **pgAdmin asked for credentials a second time.** The admin-sync sidecar
  has always created webserver-auth accounts — accounts with no password of
  their own — but pgAdmin was left on its default internal authentication,
  so it offered a login form that none of those accounts could satisfy. It
  now trusts the `X-Forwarded-User` the proxy already established, which is
  trustworthy for the same reason the console's copy is: the proxy strips
  any client-supplied one before setting its own, and the generated
  NetworkPolicy confines ingress to the proxy. Reaching pgAdmin still
  requires a session at or above `pgAdmin.accessMinLevel`; a request with a
  forged header and no session is redirected to sign in as before.
- **The embedded pgAdmin was unreachable through the proxy.** The proxy
  routes `/pgadmin` to it but forwards the path as-is, so pgAdmin — which
  serves at the root — answered 404 to every request under that prefix.
  It is now told its mount point with `SCRIPT_NAME`, which is also what
  makes it generate URLs under the prefix; stripping the prefix instead
  would send its `/static` and `/login` links to the console.
- The proxy replaced the client's `Host` with the loopback upstream's, so
  an upstream building an absolute URL handed the browser an address
  inside the Pod — pgAdmin's trailing-slash redirect pointed at
  `127.0.0.1:8081`. The client `Host` is preserved and the forwarding is
  stated in `X-Forwarded-*`, overwriting anything a client sent.
- pgAdmin's own container shared the console-wide 256Mi limit, like the
  sidecar did; both run the same Python stack and now share a budget
  sized for it.
- The admin-sync sidecar shared the console-wide 256Mi memory limit and was
  OOMKilled mid-sync, which surfaced only as a sync failure with no hint of
  the cause. It now has its own budget, because every sync shells out to
  pgAdmin's `setup.py`, which boots a Flask application.
- The pgAdmin container mounted the admin-sync passfile volume even when no
  sidecar was composed, naming a volume the Pod never declared.
- `spec.podSecurityContext.fsGroup` makes the evidence sidecar deployable on
  clusters that do not default an fsGroup. pgObjectStoreViewer requires the
  shared socket directory to be setgid with group rwx, which on an `emptyDir`
  comes from the kubelet applying fsGroup; the Pod set none, so evidence
  could only come up where the platform supplied one (OpenShift's
  `restricted-v2` SCC does).
