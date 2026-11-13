# AGENTS.md

Instructions for AI coding agents working in this repository. They apply to the
whole tree unless a more specific `AGENTS.md` exists below the path you change.
[CONTRIBUTING.md](CONTRIBUTING.md) has the same rules for human contributors, with
the invariants numbered; cite those numbers in a finding.

## Read first

1. [ARCHITECTURE.md](ARCHITECTURE.md): the binaries, the packages (one line
   each), the layering rules, the console's data flow, the generators and how
   the tests are organised.
2. The code and its tests. **The code is the truth**: when a documentation page
   and the code disagree, the code is right and the page is the bug; fix the page
   in the same change.
3. When a task conflicts with an invariant below, say so and stop; do not quietly
   reinterpret it.

## What this repository is

One product, **pgToolBox**, for CloudNativePG clusters on Kubernetes and
OpenShift:

- **the console** (`cmd/console`, Go API + single-page application in `ui/`):
  one per team namespace, authorizing with a SelfSubjectAccessReview sent with
  the reader's token; installed by the `pgtools` chart or by the operator from a
  `PgToolBoxConsole`.
- **the operator** (`cmd/operator`): optional; reconciles `PgAdmin`,
  `EvidenceAgent`, `PgToolBoxConsole` and `PgAccessRequest` (`api/v1alpha1`), one
  controller each. `PgAdminAccess` is a cross-namespace grant read by the
  `PgAdmin` controller, with no controller or status of its own. Installed by
  the `pgtoolbox` chart, kustomize or OLM.
- **the evidence agent** (`cmd/evidence-agent`): a read-only reader of backup
  repositories that produces evidence: the Barman Cloud layout (S3, Azure Blob,
  GCS) and, for Clusters backed up through the Dalibo or Opera CNPG-I plugin,
  the pgBackRest layout (repository format 5, S3 and Azure Blob). It never runs
  `pgbackrest` and refuses by name what it cannot account for. Deployed by the
  operator from an `EvidenceAgent`, or by the `pgtools` chart without the
  operator.

The pgAdmin image is built in its own repository,
[fyannk/pgAdmin](https://github.com/fyannk/pgAdmin).

## Layout

```
api/v1alpha1/            CRD types: PgAdmin, EvidenceAgent, PgToolBoxConsole, PgAccessRequest, PgAdminAccess
cmd/                     console, operator, evidence-agent
pkg/cnpgstatus/          read model: observe, instancestatus, scrape, metrics, history, logstream,
                         volumetrend, insight, pgconf, preview, evidence (projection of the agent's conclusions)
pkg/diagnose/            engine, catalog (version pins), triage
pkg/redact/ pkg/discovery/ pkg/lifecycletable/ pkg/memlimit/
pkg/evidence/            NESTED MODULE, zero dependencies: the evidence wire contract
internal/console/        api, auth, authz, cache, readmodel, status, viewmodel, sweep, diag, ops, operations, ...
internal/operator/       pgadmin, evidenceagent, console, accessrequest, and their shared machinery
internal/evidence/       the agent: providers, store, inventory, formats, evidenceapi, selfresolve, web
internal/backupresolve/  Cluster -> backup repository, shared by the operator and the agent
ui/                      the SPA (embedded in the console image through internal/console/uiassets); ui/tokens
headlamp/                Headlamp plugin that links a Cluster to its console
charts/pgtoolbox/        the operator and its CRDs        charts/pgtools/  a team's tools, no operator
deploy/olm/ config/      OLM bundle and catalog; kustomize
test/e2e/ test/crds/     kind scenarios; vendored CloudNativePG CRDs per minor
docs/site/               the documentation site (Docusaurus)
hack/                    scripts and make fragments
```

`go.work` joins the root module and `pkg/evidence`; the root `go.mod` also has a
`replace` for it so that `GOWORK=off` builds (the Dockerfile) work.

Layering is enforced by `hack/check-layering.sh` on the real import graph:
`pkg/**` imports no `internal/**`; `pkg/evidence` has no requirements at all;
`internal/evidence` imports neither console nor operator; console and operator
do not import each other or the agent; `internal/backupresolve` imports no other
`internal/` package. The console receives evidence conclusions over HTTPS;
object-store credentials never enter it.

## Commands

Use the Makefile (`make help`); the targets carry the right flags.

| Target | What |
|---|---|
| `make doctor` | local tools against `mise.toml` (the single source of tool versions) |
| `make check` | what CI runs without Docker or kind, in CI's order |
| `make lint` | golangci-lint on both modules, the format check and `hack/check-*.sh` |
| `make test test-race test-fuzz test-envtest` | unit, race, fuzz of the trust-boundary parsers, CRD CEL rules on envtest |
| `make manifests verify-manifests validate-packaging helm-lint` | generated CRD and RBAC copies; Helm vs kustomize vs OLM field by field |
| `make test-e2e-console test-e2e-chart test-e2e-pgadmin test-e2e-evidenceagent test-e2e-team test-e2e-pgbackrest test-container test-multiarch` | kind and Docker tests |
| `make docs` | the documentation site build and its gates |

Verify a Go change with `make lint test`. A change to generated Roles, anything a
component container reads, or the packaging also needs the end-to-end tests (a
fake client cannot fail the two ways a real cluster can: RBAC escalation
prevention and a container rejecting the environment it was rendered with).

## Hard rules

The numbered list is in [CONTRIBUTING.md](CONTRIBUTING.md#repository-invariants).
In short:

- **Operator**: CloudNativePG 1.30 and later (the CNPG API is vendored at 1.30;
  the optional kinds `DatabaseRole`, `Pooler` and `Database` are probed at
  startup and their features turned off when not served); never write to a CNPG
  `Cluster`; no secret material in status, logs or events; byte-stable rendering
  (a no-op reconcile does zero updates); condition types/reasons and label keys
  are frozen API surface (extend, never rename); status is published on every
  exit path.
- **Console**: read-only unless explicitly enabled, with an enumerated mutation
  surface (`internal/console/ops` originates writes; `hack/check-readonly.sh`);
  its own identity writes only its state ConfigMap, its digest Lease when digests
  are delivered, its MCP Lease and EndpointSlices when `MCP_LEASE` is set and,
  opt-in, the restore-drill cleanup; no SQL, no object
  storage, no Secrets; the console decides no authorization of its own; preserve
  uncertainty (a broken watch is stale/unknown, never healthy); render from
  snapshots; redact at the boundary; `html/template` only; attribute every claim
  to its source.
- **Evidence agent**: read-only by construction; never overstate evidence (four
  states, conservative roll-ups; "structurally usable" is the strongest claim);
  no secret disclosure; all work bounded and cancelable; keys are opaque;
  deterministic output; a failed refresh never replaces good evidence.
- **Everywhere**: no secret in logs; every external read bounded; times are UTC
  at boundaries and clocks are injected where age matters; wrap errors with
  `%w`; consumer-owned small interfaces (do not mirror client-go or SDK
  clients); `context.Context` first and never replaced by `Background()` on an
  active path; close bodies on every path.

## Go and repository conventions

- Go and every tool are pinned in `mise.toml`; `go.mod`'s `toolchain` line and
  the Dockerfile's builder follow it (`hack/check-toolchain.sh`).
- One `.golangci.yml`. A linter is enforced everywhere, or excluded for a named
  area with a comment saying why. Formatters are gofumpt and gci (`make fmt`).
- Licence header on every Go file, from `hack/boilerplate/`:
  `Copyright 2026 The pgToolBox Authors.` Generated files are skipped.
- Generated files (`zz_generated.*`, `config/crd/bases`, the Helm and OLM copies
  of the CRDs and the manager Role, the kustomize admission component
  `config/components/admission/admission_policy.yaml`, `internal/console/api/openapi.yaml` and its
  fixtures, `docs/site/docs/reference/checks.md`,
  `docs/site/docs/reference/api.md`, `deploy/insight-pack/insight-pack.yaml`, the
  evidence schemas and goldens under `pkg/evidence/v1alpha*`,
  `ui/tokens/tokens.ts`, `ui/src/api/generated/`, `ui/src/routeTree.gen.ts`) are
  changed by their generator (`make generate manifests openapi catalog-docs
  crd-ref-docs insight-pack generate-evidence-artifacts`; in `ui/`, `pnpm
  generate` and the tokens' own `pnpm generate`), never by hand; CI fails when
  they are stale. The table is in `ARCHITECTURE.md`.
- Atomic conventional commits (`feat:`, `fix:`, `refactor:`, `test:`, `docs:`,
  `build:`, `ci:`, `chore:`); stage explicit paths; no drive-by cleanup mixed
  into a feature. Work on a branch; push, tag and publish only when asked.
- Tests are table-driven and hermetic; golden files for rendered objects and
  view models; native fuzz targets for every untrusted parser, asserting the
  invariant rather than "does not panic"; negative tests are mandatory for
  redaction, isolation, incomplete scans and disabled features. Do not weaken a
  test to make a change pass.
- The console UI is a single-page application over a typed JSON + SSE API; its
  client is generated from `openapi.yaml`.
- Documentation: user-facing behaviour changes update `docs/site/docs/` in the
  same change (build: `make docs`). Two generated copies are not committed: the
  console's embedded docs bundle (`internal/console/docsbundle/data`; the console
  image builds it, `make docs-bundle` makes it for a local run, tests skip what
  needs it) and the tour's screenshots (`docs/site/static/img/screens`, copied
  from `ui/e2e/__screenshots__` by `npm run build`). Never commit either.

## CI

CI is GitHub Actions on `ubuntu-latest`; `.github/workflows/ci.yml` is the
pipeline and `release.yml` the release workflow. Rules for workflow changes:

- Every third-party action is pinned to a full commit SHA with a version comment;
  `permissions` are set per job (`{}` at the top); `persist-credentials: false` on
  checkout; no `${{ inputs.* }}` or other untrusted expression inside `run:`
  (use `env:`); `actionlint` and `zizmor` clean (`make lint-workflows`).
- Tools come from `mise.toml`.
- A `changes` job (`hack/changed-paths.sh`) decides which job families run; a
  skipped job is skipped on purpose, never silently.
- Images: `linux/amd64,linux/arm64`, `-trimpath -ldflags "-s -w -X ..."`,
  digest-pinned bases, OCI labels, provenance and SBOM, signed by digest;
  `latest` only from a stable tag, never from `main`.
- Never push, tag or publish without being told to.

## Naming

Product and repository **pgToolBox** (module `github.com/fyannk/pgToolBox`).
Components are named neutrally: **the console** (image
`ghcr.io/fyannk/pgtoolbox-console`; "pgToolBox console" where a product name is
needed), **the operator** (`ghcr.io/fyannk/pgtoolbox-operator`), **the evidence
agent** (`ghcr.io/fyannk/pgtoolbox-evidence-agent`, binary `/evidence-agent`).
The console's kind is `PgToolBoxConsole`, never `Console`: that would collide with
OpenShift's `consoles` resources. The API group `pgtoolbox.fyannk.dev` and its
version `v1alpha1` are a product contract.

## Handoff

At the end of a task report: the user-visible outcome; files and contracts
changed; tests and checks that passed; what was not run and why; remaining
unknowns and follow-up decisions. A change that compiles is not done: the
behaviour is present, negative cases fail safely, `make check` (plus the
Docker/kind targets the change touches) passes, and the documentation matches.
