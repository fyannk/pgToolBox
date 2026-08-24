# AGENTS.md — pgToolBox

You are working on **pgToolBox**, a Kubernetes operator that manages, per
CloudNativePG (CNPG) cluster, a complete access stack: auth proxy, observation
console, and embedded pgAdmin — with declarative user/role provisioning.

The code is the source of truth. The Docusaurus site in `web/docs/`
explains implemented behavior in a pretty way — it must describe what the
code does, never a design spec. When code and docs disagree, fix the docs
(or the code, deliberately); never let the site document aspirations.

## Sibling repos (read-only references, never modify)

- `../pgtoolbox` (lowercase) — predecessor operator. Mature, tested. This repo
  is a fresh rewrite of it; port proven machinery from it, file-by-file, WITH
  its tests. Do not resurrect its standalone `PgAdmin`/`PgAdminRegistration`
  kinds, CNPG-I enrollment, or multi-operator provider seam.
- `../pgconsole` — the console app (Go, K8s-API-only, no auth by design).
  Consumed as a container image; NOT imported. Both changes once pending
  there have shipped: `X-PgToolBox-Level` header authz replaced SAR gating
  (`internal/authz`), and the dba review panel exists (`internal/review`,
  behind `ALLOW_ACCESS_REVIEW`). The contract with it is four things, all
  covered by tests here: the header names, the listen port (3000), the read
  Role matching `../pgconsole/deploy/kubernetes-example.yaml` rule for rule,
  and `patch` (not just `update`) on `pgtoolboxaccessrequests/status`. When
  that manifest changes there, `readRole` changes here.
  **pgConsole 0.2.0 is the floor.** The operator renders `PGADMIN_URL` as
  the root-relative `/pgadmin`, which only 0.2.0+ accepts; 0.1.x validates
  link-outs as absolute URLs only and refuses to start on it.
- `../object-store-viewer` — backup repository evidence tool, published as
  `github.com/fyannk/pgObjectStoreViewer`. Its `api/` module IS imported, at
  its published version (`.../api v0.1.1`) — NOT through a `replace` to the
  sibling checkout, which used to make the build depend on a local symlink
  and broke CI. The evidence fingerprint must be that module's
  canonicalization (`FingerprintS3`), never a local reimplementation. The
  runtime contract with the sidecar image is separate and unversioned: the
  `pgconsole-sidecar` RUNTIME_MODE, the fixed socket
  `/var/run/objectstoreviewer/evidence.sock`, the `/objectstoreviewer probe`
  liveness command, and the sidecar-mode rules that PROVIDER be `s3`,
  REPOSITORY_FORMAT `barman-cloud`, exactly one BARMAN_SERVER_NAMES entry,
  and LISTEN_ADDR/TRUSTED_USER_HEADER unset.
- `../pgAdmin` — the family's own repackaging of pgadmin4 (image
  `ghcr.io/fyannk/pgadmin`), not the upstream `dpage/pgadmin4`. Runs as uid
  5050 in group root, listens per `PGADMIN_LISTEN_ADDRESS`/`_PORT`, and
  refuses to start without `PGADMIN_DEFAULT_EMAIL` plus a password (the
  operator generates a bootstrap credential per console). Its `setup.py` is
  the only sanctioned way to change users, so the admin-sync sidecar runs
  that image and mounts the settings volume.
- `../oauth2-proxy` — pristine upstream clone; the fork plan was ABANDONED in
  favor of our own `pgtoolbox-proxy` (see architecture doc for rationale).

## Hard rules (inherited from the predecessor, enforced by tests)

- CNPG 1.30+ only (`DatabaseRole` CRD always exists; no fallback paths).
- Never write to the CNPG `Cluster` object (tests assert resourceVersion).
- No secret material in status, logs, or events. CEL rules ASCII-only.
- Deterministic byte-stable rendering: no-op reconcile = zero object updates.
- Apps stay auth-free; the proxy is the only auth boundary. NetworkPolicy
  ensures nothing bypasses it.
- License boilerplate (`hack/boilerplate.go.txt`) on all new Go files.
- Verify with: `go build ./...`, `go vet ./...`, `go test ./... -race -count=1`.
  Before changing the generated Roles or anything the component containers
  read, also run `make test-e2e` (kind + CNPG + the published component
  images). The unit tests use a fake client, which cannot fail the two ways a
  real cluster can: RBAC escalation prevention refusing a Role rule the
  operator does not itself hold, and a component container rejecting the
  environment rendered for it.
- The agent does NOT run git mutations; the user commits themselves.

## Build order & current state

1. ✅ **Scaffolding + 4 CRDs** (`api/v1alpha1/`): `PgConsole` (pgc),
   `PgToolBoxUser` (pguser),
   `PgToolBoxAccessRequest` (pgreq). CEL: immutable `cnpgClusterRef`, oidc
   block required iff `mode=oidc`, postgresRole profile XOR databaseRoleRef,
   immutable access-request subject. Generated CRDs in `config/crd/bases/`.
2. ✅ **pgtoolbox-proxy** (`cmd/proxy`, `internal/proxy/`): OIDC (PKCE S256,
   state+nonce) + local (bcrypt, per-IP lockout) modes; AES-256-GCM session
   cookies with key rotation; hot config reload (users/routes/keys only,
   provider settings restart-fixed); level authz (none < view < poweruser <
   dba); header hygiene (strips forged identity headers); access-request flow
   with CSRF. Unknown-but-authenticated users get a `none`-level session that
   binds the CSRF token. Redirect validation rejects backslashes too.
3. ✅ **PgConsole controller** (`internal/controller/pgconsole/`): composes
   Deployment (proxy + pgconsole + pgAdmin + evidence sidecar) + Secret + SA/
   RBAC + Service + PVC + NetworkPolicy + Ingress/Route/HTTPRoute. Proxy config
   rendered then round-tripped through the proxy's own strict Parse. Session
   key generated once per console, stable. Evidence composition ported as-is
   (immutable token Secrets, `rotate-evidence-token` annotation, GC).
   Deviations: Deployment strategy always Recreate (pgAdmin PVC); NetworkPolicy
   uses create/update not SSA (fake-client limitation); evidence defaults to
   disabled.
4. ✅ **Embedded pgAdmin user/server sync** (`internal/adminsync/`,
   `internal/controller/pgconsole/pgadmin_sync.go`): the operator stays the
   single control-plane owner. The sidecar (`admin-sync-sidecar`) receives
   per-user postgres passwords over an in-pod mTLS API and writes the combined
   `.pgpass` file itself; the operator only posts the desired state. The
   operator injects an `admin-sync-init` init container (copies the operator
   binary from `--operator-image`) plus the `admin-sync` sidecar when pgAdmin
   is enabled and `OperatorImage` is set, generates the TLS/token Secret, and
   drives sync only after the rollout reaches the target config revision.
   New API surface: `PgConsoleConditionPgAdminSynced`,
   `AdminSyncSecretVersionAnnotation`, `PgAdminSyncRevisionAnnotation`. Added
   watches for `PgToolBoxUser` and RBAC rules for
   `pgtoolboxusers` and CNPG `databaseroles`.
   `databaseRoleNameForRole` has a deterministic placeholder for profile-based
   roles until Step 5 formalizes the role controller's naming convention.
5. ✅ **Users configure the proxy, and nothing else**: `PgToolBoxUser`
   binds an identity to one of three hardcoded levels (`view`,
   `poweruser`, `dba`) on one console. There is no `PgToolBoxRole` — the
   level set is closed and shared with pgConsole, so a level is a field
   rather than an object. There is no postgres backing either: pgAdmin
   connects with the cluster's own credentials, never with anything
   derived from who signed in. Matching is by subject in every
   authentication mode, so an OIDC deployment declares one user per
   identity with no local password; group/claim mapping does not exist
   yet. The `PgConsole` controller resolves the user set once per
   reconcile, renders it into the proxy configuration (bcrypt hashes in
   local mode), and patches each user's `ProxySynced` condition.

6. ✅ **OpenShift provider** for the proxy (`internal/proxy/openshift/`):
   OAuth2 authorization-code flow with PKCE S256 against OpenShift's
   integrated OAuth server, using the workload service account as the OAuth
   client (`system:serviceaccount:<ns>:<sa>`). The service-account token
   doubles as the client secret and is read at redemption time so projected
   rotation works. Endpoints are discovered from
   `https://openshift.default.svc/.well-known/oauth-authorization-server`;
   the current-user lookup calls
   `/apis/user.openshift.io/v1/users/~` with the user's access token. The
   `PgConsole` controller renders the OpenShift provider block, adds the
   `serviceaccounts.openshift.io/oauth-redirecturi.pgconsole` annotation to
   the console ServiceAccount when the console is exposed, and no longer
   rejects `mode=openshift`.
7. ✅ **Access-request review flow (operator side)**: the proxy 403 form
   already creates `PgToolBoxAccessRequest`; the new
   `internal/controller/pgtoolboxaccessrequest/` controller now materializes
   a `PgToolBoxUser` when a reviewer approves a request with a
   `requestedRoleRef`. User names are deterministic (`<console>-pguser-<sha256>`)
   so repeated approvals converge, existing users are aligned to the granted
   role, and missing console/role marks the request `UserReady=False` instead
   of failing the reconcile. The console's generated operate Role now also
   grants `get;list;watch` on `pgtoolboxaccessrequests` and
   `update;patch` on `pgtoolboxaccessrequests/status` so the pgconsole dba
   review panel can decide requests; the panel itself lives in the
   `pgconsole` repo.
8. ✅ **pgconsole alignment** (`internal/controller/pgconsole/consoleconfig.go`,
   `rbac.go`): the generated read Role now carries the console's whole read
   manifest (poolers, declared database objects, services, PVCs, image
   catalogs, failover quorums, the children-inventory kinds) instead of the
   six rules it had, and still grants nothing on `secrets`. New
   `spec.console` block makes the application's behaviour declarative —
   `allowOperations`, `allowLogs`, `allowAccessReview`,
   `allowClusterCatalogs`, `allowInsecureLinks`, `monitoringURL`, and the
   log-tail / metrics / history tunables — replacing the hardcoded
   `ALLOW_OPERATIONS=true` / `ALLOW_LOGS=true` pair. Two rules govern the
   rendering: capabilities are always emitted and also decide the Role
   rules (an off capability loses its authority, and the operate Role is
   deleted when it would be empty), while tunables are emitted only when
   set, so the application keeps ownership of every numeric default.
   `ALLOW_ACCESS_REVIEW` and `TRUSTED_LEVEL_HEADER` are now rendered — the
   review panel was previously dead despite the RBAC and the controller in
   step 7 — and `PGADMIN_URL` is derived from the exposure hostname.
   `allowClusterCatalogs` generates a ClusterRole/ClusterRoleBinding pair
   that no owner reference can collect, so the finalizer deletes it.

## Conventions

- kubebuilder v4 layout, group `pgtoolbox.fyannk.dev/v1alpha1`, module
  `github.com/fyannk/pgtoolbox`, Go 1.26.4.
- controller-gen via `go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0`
  (see Makefile `generate`/`manifests`). `make manifests` generates the CRDs
  *and* `config/rbac/role.yaml`, then runs `hack/sync-manifests.sh` to copy
  both into the Helm chart and the OLM bundle — those carry their own copies,
  and hand-maintaining them meant a new controller rule reached the kustomize
  overlay while silently missing the two install paths most users take.
  `make verify-manifests` fails when the checked-in copies are stale.
- Condition types/reasons and label keys in `api/v1alpha1/conditions.go` and
  `common_types.go` are frozen API surface — extend, never rename.
- Dep versions pinned to the predecessor's: controller-runtime v0.24.1,
  k8s.io/* v0.36.2, gateway-api v1.6.1, cloudnative-pg/api v1.30.0.
- Packaging: one multi-target `Dockerfile` builds the manager (`--target
  manager`) and proxy (`--target proxy`) images; the build context is this
  repository (it had to be the parent directory while the go.mod replace
  reached a sibling checkout). The operator image reference is baked into the
  manager binary via `-X main.defaultOperatorImage=$(IMG)` so
  `--operator-image` only overrides it. `config/default` is a development
  overlay (CRDs + RBAC + manager in the `pgtoolbox` namespace) — production
  installs are expected from platform packaging. A Helm chart lives at
  `deploy/helm/pgtoolbox` (`make helm-package`), and an OLM bundle + file-based
  catalog at `deploy/olm` (`make bundle-build`, `make catalog-build`); the
  catalog Dockerfile runs `opm serve --cache-only` at build time, which also
  validates the catalog.
- Documentation: the Docusaurus site in `web/` explains the implemented
  behavior (build with `cd web && npm install && npm run build`; `npm run
  typecheck` must pass). User-facing behavior changes must update `web/docs/`
  in the same change, so the site never drifts from the code.
- CI targets GitHub (`.github/workflows/`): `ci.yml` (build/vet/test,
  golangci-lint, Helm, OLM bundle/catalog build, docs build), `images.yml`
  (manager/proxy to ghcr.io on main and tags; bundle/catalog on tags),
  `docs.yml` (GitHub Pages), `release.yml` (Helm chart release on tags),
  `codeql.yml` (Go and JavaScript analysis), `automerge.yml` (arms
  auto-merge on Dependabot's patch and minor bumps; majors are left for a
  person, and the ruleset's required checks still gate the merge).
  `make lint` (golangci-lint incl. gosec, config `.golangci.yml`) must stay
  at 0 issues — the lint job runs `make lint` rather than
  golangci-lint-action, because the action's prebuilt binary is built with an
  older Go than go.mod targets and refuses to load the config at all; `go
  run` compiles the pinned version with the repo toolchain. Image builds pass
  the baked `IMG` reference. `make verify-manifests` runs in CI and fails when
  the Helm/OLM copies of the CRDs or the manager ClusterRole are stale.
