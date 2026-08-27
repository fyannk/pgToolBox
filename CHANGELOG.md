# Changelog

All notable changes to pgToolBox are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

pgToolBox is **pre-1.0**: 0.x releases may change CRD fields, generated
object shapes, and the contract with the components it deploys without a
deprecation period. Pin an exact image tag and read the notes before
upgrading.

## [Unreleased]

## [0.1.3] - 2026-08-27

### Security

- **The components it deploys are current.** pgConsole moves 0.3.0 → 0.6.1
  and pgObjectStoreViewer 0.1.1 → 0.1.2. Both of those are the same fix
  0.1.2 made to this operator's own binaries: 0.6.1 is pgConsole's Go 1.27
  rebuild, and pgObjectStoreViewer 0.1.2 is a security release its notes
  recommend for every 0.1.1 and 0.1.0 deployment. An operator that repaired
  its own standard library while deploying components carrying the
  equivalent problem had not repaired much. pgAdmin stays at
  `9.17-hardened`, the only hardened tag published. The
  `pgObjectStoreViewer/api` module the evidence fingerprint comes from
  moves to v0.1.2 with it, so build and runtime agree on which release
  defines the canonicalization.

### Added

- **The console's Role grants `resourcequotas` `list`/`watch`.** pgConsole
  0.6.0 reads namespace quota for its quota-exhausted diagnostic; without
  the grant the check reports "could not run" and the quota diagnostics
  stay dark. The manager ClusterRole gains the same grant, because RBAC
  escalation prevention refuses to create a Role carrying a permission the
  creator does not hold.

### Upgrading

Nothing to do by hand. The chart and the CSV carry the new component
images and the new grant; a `helm upgrade` or an OLM upgrade applies both.
An existing `PgConsole` picks up the quota diagnostics when its Role is
reconciled, which happens on the operator's next pass over the object.

## [0.1.2] - 2026-08-27

### Security

- **Go toolchain moved to 1.27.0.** govulncheck found six standard-library
  advisories reachable from this code on 1.26.5, all fixed in 1.26.6 and
  carried into 1.27: `net/url` (GO-2026-6218), `html/template`
  (GO-2026-6091), `crypto/tls` (GO-2026-6090), `net/http` (GO-2026-6089
  and GO-2026-5026), and `encoding/asn1` (GO-2026-5972). The traces run
  through the proxy's reverse-proxy path and the admin-sync client's TLS
  and HTTP calls, so they are reachable in a deployment rather than only
  under test. 1.27.0 rather than 1.26.7 because the builder image tracks
  1.27, and a `toolchain` directive behind the image makes every container
  build download a second toolchain before it compiles. The language
  version in `go.mod` was raised to 1.26.6 for the same reason: it is the
  floor below which a `GOTOOLCHAIN=local` build could still compile against
  a vulnerable standard library.

### Changed

- `golang.org/x/crypto` 0.54.0 → 0.55.0 and `k8s.io/api`, `k8s.io/apimachinery`
  and `k8s.io/client-go` 0.36.3 → 0.36.4, picked up by the routine dependency
  updates that now merge themselves once the pipeline is green.

## [0.1.1] - 2026-08-10

### Security

- **Open redirect in the post-login target (`?rd=`).** `SafeRedirect`
  rejected `\r` and `\n` but not TAB, and browsers strip TAB from a URL
  before parsing it — so `/<TAB>/evil.example` passed the "does it start
  with `//`" check and the browser resolved it as `//evil.example`. An
  attacker could send a signed-in user to another origin through a link to
  the console's own login page. Every control character and backslash is
  rejected now, and the result is re-checked with the URL parser. Found by
  CodeQL (`go/bad-redirect-check`) and confirmed against a payload set.
- The OIDC and OpenShift transient cookies were cleared without the
  `HttpOnly`, `Secure` and `SameSite` attributes they were set with, which
  advertises a laxer policy than the value ever had and can leave a
  browser declining to replace the original. They now match, as the
  session cookie already did.

## [0.1.0] - 2026-08-09 — withdrawn

**This release was withdrawn.** It shipped the open redirect fixed in
0.1.1, and its artifacts — the GitHub release, the git tag, and the
`0.1.0` container images — have been deleted. It had no downloads and was
public for less than a day, so removing it was possible in a way it will
not be for later versions; a future release with a defect gets an
advisory and a fix, not a deletion.

The section is kept because the record should be accurate about what was
published and why it was taken back. Everything below describes what
0.1.0 contained, and 0.1.1 contains all of it.

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

[Unreleased]: https://github.com/fyannk/pgToolBox/compare/v0.1.3...HEAD
[0.1.3]: https://github.com/fyannk/pgToolBox/releases/tag/v0.1.3
[0.1.2]: https://github.com/fyannk/pgToolBox/releases/tag/v0.1.2
[0.1.1]: https://github.com/fyannk/pgToolBox/releases/tag/v0.1.1
[0.1.0]: https://github.com/fyannk/pgToolBox/blob/main/CHANGELOG.md#010---2026-08-09--withdrawn
