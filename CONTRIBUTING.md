# Contributing

This page explains how to build pgToolBox, what the checks expect, and the
invariants every change must keep. [ARCHITECTURE.md](ARCHITECTURE.md) describes
how the pieces fit together; [AGENTS.md](AGENTS.md) is the same material for
coding agents. By contributing you agree to follow the
[code of conduct](CODE_OF_CONDUCT.md).

## Development environment

`mise.toml` is the single source of tool versions (Go, Node, pnpm, helm, kind,
kubectl, golangci-lint, govulncheck, controller-gen, setup-envtest, hadolint,
actionlint, zizmor). CI installs exactly that file.

```bash
mise install              # every tool at the pinned version
make doctor               # compares this machine with mise.toml, prints what to fix
make help                 # every target
make check                # what CI runs without Docker or kind, in CI's order
```

Without mise the Makefile falls back to `go run` of the pinned version for the
Go-built tools, so a fresh checkout can `make lint` before `mise install`.
`make test-e2e-console`, `test-e2e-chart`, `test-e2e-pgadmin`,
`test-e2e-evidenceagent`, `test-e2e-team`, `test-e2e-pgbackrest`,
`test-container`, `test-multiarch` and the image targets need Docker (and kind
for the end-to-end tests). An end-to-end run starts one kind cluster with
CloudNativePG, a PostgreSQL pair and the pods of its scenario; budget about
4.5 GiB of free memory for it.

## Generated files

Generated files are changed by their generator, never by hand, and committed in
the same change as their source; CI fails when one is stale.

| After a change to | Run |
|---|---|
| `api/` | `make generate manifests crd-ref-docs` |
| the console API registrations or view models | `make openapi`, then `cd ui && pnpm generate` |
| the diagnostics catalog | `make catalog-docs` |
| the `pgtools` chart's insight pack | `make insight-pack` |
| the `pkg/evidence` types | `make generate-evidence-artifacts` |
| `ui/tokens/tokens.css` | `cd ui/tokens && pnpm generate` |

[ARCHITECTURE.md](ARCHITECTURE.md#generated-files) has the full table.

## Tests

Tests are table-driven and hermetic, with golden files for rendered objects and
view models. Negative tests are mandatory for redaction, isolation, incomplete
scans and disabled features. Do not weaken a test to make a change pass.

`make test-fuzz` fuzzes the parsers on the trust boundary, each against the
invariant it owns rather than against "does not panic": in this codebase the
dangerous failure is accepting something, not crashing on it. The list, and the
invariant of each target, is in the `Makefile` beside the target. CI runs it at
`FUZZ_TIME` (5s per target), a smoke run rather than a campaign
(`FUZZ_TIME=10m make test-fuzz` to chase something). A failing input is written to
`testdata/fuzz/` beside the target and becomes a regression case; commit it with
the fix.

Verify a Go change with `make lint test`. A change to generated Roles, to anything
a component container reads, or to the packaging also needs the end-to-end tests:
a fake client cannot fail the two ways a real cluster can, RBAC escalation
prevention and a container rejecting the environment it was rendered with.

## Repository invariants

These are hard rules; a change that violates one is a bug.

**Everywhere**

1. **No authorization of our own.** The console authorizes every read and write
   with a SubjectAccessReview against the signed-in identity; pgAdmin is offered
   to whoever may `get` the `PgAdmin`. Nothing impersonates.
2. **No secret in status, events or logs.** CEL rules are ASCII-only.
3. **Every external read is bounded** in size and time, and every body is closed
   on every path. `context.Context` comes first and is never replaced by
   `context.Background()` on an active path.
4. **Times are UTC at boundaries**, and clocks are injected wherever age matters.
5. **Errors are wrapped with `%w`.** Interfaces are small and owned by their
   consumer; do not mirror client-go or SDK clients.
6. **Licence header** from `hack/boilerplate/` on every Go file
   (`Copyright 2026 The pgToolBox Authors.`); generated files are skipped
   (`make check-repo`).
7. **Layering** (`hack/check-layering.sh`): `pkg/**` imports no `internal/**`;
   `pkg/evidence` has no module requirements; the evidence agent, the console and
   the operator do not import each other; `internal/backupresolve` imports no
   other `internal/` package.
8. **One version, said everywhere.** `make validate-packaging` is the arbiter: it
   compares the chart versions and compares Helm, kustomize and OLM field by field
   (CRDs, the manager's RBAC rules, security context, resources, ports, probes,
   args), so the install paths cannot drift apart.

**The operator**

9. **CloudNativePG 1.30 and later.** The CloudNativePG API is vendored at 1.30. The
   optional kinds (`DatabaseRole`, `Pooler`, `Database`) are probed at startup and
   the features that need them are off when they are not served; the `Cluster`
   kind is required.
10. **Never write to a CloudNativePG `Cluster`.** Tests assert that its
    resourceVersion is unchanged.
11. **Deterministic, byte-stable rendering.** A no-op reconcile produces zero
    object updates.
12. **Condition types, reasons and label keys** in `api/v1alpha1/conditions.go`
    and `common_types.go` are frozen API surface: extend, never rename. Status is
    published on every exit path of a reconcile.

**The console**

13. **Read-only unless enabled, through an enumerated surface.** Writes originate
    only in `internal/console/ops`, sent with the signed-in person's token;
    `hack/check-readonly.sh` and `hack/readonlycheck` list every call that could
    change something, with its exact count. The console's own identity writes
    only its state ConfigMap, its digest Lease when digests are delivered, its
    MCP Lease when `MCP_LEASE` is set and, opt-in, the
    restore-drill cleanup.
14. **No SQL, no object storage, no Secrets** in the console
    (`hack/check-deps.sh`).
15. **Preserve uncertainty.** A broken watch is stale or unknown, never healthy;
    a check that could not run never counts as clear.
16. **Render from snapshots**, redact at the boundary (`pkg/redact`), use
    `html/template` only, and attribute every claim to its source.

**The evidence agent**

17. **Read-only by construction**: the providers expose no write call.
18. **Never overstate evidence.** Four states and conservative roll-ups;
    "structurally usable" is the strongest claim the agent makes.
19. **No secret disclosure**, keys treated as opaque, all work bounded and
    cancelable, deterministic output, and a failed refresh never replaces good
    evidence.

## Git workflow

- One focused change per pull request, in atomic
  [conventional commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`,
  `refactor:`, `test:`, `docs:`, `build:`, `ci:`, `chore:`). Keep generated files
  with their source, and keep unrelated cleanup out of a feature.
- Open the pull request against `main` on
  [github.com/fyannk/pgToolBox](https://github.com/fyannk/pgToolBox).

## Continuous integration

CI is GitHub Actions on `ubuntu-latest`. `.github/workflows/ci.yml` is the
pipeline: a `changes` job decides which families of jobs a change needs, and a
skipped job is skipped on purpose. Every third-party action is pinned to a full
commit SHA, permissions are set per job, and the workflows are kept clean by
`actionlint` and `zizmor` (`make lint-workflows`). Locally, `make check` runs
the same checks as the jobs that need neither Docker nor kind. The workflows, the
jobs and what each one gates are in
[docs/site/docs/development/ci.md](docs/site/docs/development/ci.md); the settings
the repository itself needs (rulesets, required checks, Pages, secrets) are in
[docs/site/docs/development/repository-settings.md](docs/site/docs/development/repository-settings.md).

## Releases

A release is an annotated tag `vX.Y.Z`; `make release-check TAG=vX.Y.Z` checks
it first. The product version is written in `charts/pgtoolbox/Chart.yaml` and
`charts/pgtools/Chart.yaml` (`version` equals `appVersion`, the same in both),
and has to agree with the `CHANGELOG.md` section, the OLM bundle and catalog,
and the `--version` in the documented `helm install` commands;
`make validate-packaging` checks all of it. The nested evidence module is tagged
`pkg/evidence/vX.Y.Z` (`make release-check-module`) and changes only when the
wire contract does. The documentation's
[release page](https://fyannk.github.io/pgToolBox/docs/release/) describes what
a release publishes and how to verify it.

## Documentation

A change to user-facing behaviour updates `docs/site/docs/` in the same change.
Build the site locally:

```bash
make docs                 # npm ci, typecheck, tests, Mermaid check, build
cd docs/site && npm start # live, at http://localhost:3000/pgToolBox/
```

The build fails on a broken internal link or anchor.
