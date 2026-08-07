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
  Consumed as a container image; NOT imported. Pending changes there (separate
  repo work): `X-PgToolBox-Level` header authz replacing SAR gating, dba
  review panel for access requests.
- `../object-store-viewer` (aka `../objectstoreviewer`, symlinked) — backup
  repository evidence tool. Its `api/` module IS imported via
  `replace github.com/fyannk/objectstoreviewer/api => ../objectstoreviewer/api`
  in go.mod (evidence fingerprint must be its canonicalization).
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
- The agent does NOT run git mutations; the user commits themselves.

## Build order & current state

1. ✅ **Scaffolding + 4 CRDs** (`api/v1alpha1/`): `PgConsole` (pgc),
   `PgToolBoxRole` (pgrole), `PgToolBoxUser` (pguser),
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
   watches for `PgToolBoxUser`/`PgToolBoxRole` and RBAC rules for
   `pgtoolboxusers`, `pgtoolboxroles`, and CNPG `databaseroles`.
   `databaseRoleNameForRole` has a deterministic placeholder for profile-based
   roles until Step 5 formalizes the role controller's naming convention.
5. ✅ **Role/User provisioning end-to-end**: new `PgToolBoxRole` controller
   (`internal/controller/pgtoolboxrole/`) materializes a CNPG `DatabaseRole` +
   password Secret for profile-based roles (`monitor` → `pg_monitor`;
   `database-readonly` → `pg_read_all_data`; `database-owner` → `createdb` +
   `createrole`) and validates `databaseRoleRef` bring-your-own roles. It sets
   `status.databaseRoleName`, waits for CloudNativePG to apply the credential,
   and deletes managed objects on role deletion. The `PgConsole` controller now
   resolves the console's `PgToolBoxUser` set once per reconcile, renders known
   users into the proxy configuration (with bcrypt hashes in local mode),
   degrades individual users with missing roles/passwords instead of failing
   the whole reconcile, and patches each user's `RoleReady`, `ProxySynced` and
   `PgAdminSynced` conditions. pgAdmin sync only proceeds after the backing
   `DatabaseRole` is applied and the password Secret resourceVersion matches.
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
   `pgconsole` repo (see `../pgconsole/docs/change-brief.md`).

## Conventions

- kubebuilder v4 layout, group `pgtoolbox.fyannk.dev/v1alpha1`, module
  `github.com/fyannk/pgtoolbox`, Go 1.26.4.
- controller-gen via `go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0`
  (see Makefile `generate`/`manifests`).
- Condition types/reasons and label keys in `api/v1alpha1/conditions.go` and
  `common_types.go` are frozen API surface — extend, never rename.
- Dep versions pinned to the predecessor's: controller-runtime v0.24.1,
  k8s.io/* v0.36.2, gateway-api v1.6.1, cloudnative-pg/api v1.30.0.
- Packaging: one multi-target `Dockerfile` builds the manager (`--target
  manager`) and proxy (`--target proxy`) images; the build context must be
  the parent directory (the go.mod replace reaches
  `object-store-viewer/api`). The operator image reference is baked into the
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
  `docs.yml` (GitHub Pages), `release.yml` (Helm chart release on tags).
  `make lint` (golangci-lint incl. gosec, config `.golangci.yml`) must stay
  at 0 issues. Workflows clone `fyannk/object-store-viewer` as a sibling for
  the go.mod replace, and image builds pass `REPO_DIR` (repo directory name)
  plus the baked `IMG` reference.
