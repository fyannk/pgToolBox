# Contributing

Thanks for considering a contribution! This page explains how to build the
project, what the tests expect, and the invariants every change must keep.

## Development environment

- Go 1.26+ (the module pins the toolchain)
- `make`, `docker` (or `podman`), `helm`, `kubectl`

```bash
make generate manifests   # regenerate deepcopy code, CRDs and RBAC after ANY api/ change
make build                # build bin/manager
make test                 # unit tests
make lint                 # golangci-lint
make helm-lint            # chart lint + template rendering
make docs                 # build the documentation site
make docker-build         # operator image (manager)
make docker-build-proxy   # pgtoolbox-proxy image
make deploy               # kustomize dev overlay into the pgtoolbox namespace
make helm-package         # package deploy/helm/pgtoolbox into dist/
make bundle-build         # OLM bundle image
make catalog-build        # OLM catalog index image (also validates the catalog)
```

`go build ./... && go vet ./... && go test ./... -race -count=1` must pass
before you consider a change done. If `make generate manifests` changes
generated files, commit them in the same change.

## Repository invariants

These are hard rules — a change that violates one is a bug:

1. **CNPG 1.30+ only.** The `DatabaseRole` CRD always exists; no fallback
   paths.
2. **Never write to the CNPG `Cluster` object.** Tests assert the
   resourceVersion is unchanged.
3. **No secret material in status, logs, or events.** CEL rules are
   ASCII-only.
4. **Deterministic byte-stable rendering.** A no-op reconcile produces zero
   object updates.
5. **Apps stay auth-free.** The proxy is the only authentication and coarse
   authorization boundary; the generated `NetworkPolicy` ensures nothing
   bypasses it.
6. **License boilerplate** (`hack/boilerplate.go.txt`) on all new Go files.
7. **Condition types/reasons and label keys** in `api/v1alpha1/conditions.go`
   and `common_types.go` are frozen API surface — extend, never rename.
8. **One version, said everywhere.** `make validate-packaging` is the
   arbiter; see below.

## Cutting a release

A release is a tag: `git tag -a vX.Y.Z -m "pgToolBox X.Y.Z" && git push
origin vX.Y.Z`. That fires the Images workflow (operator, proxy, OLM bundle
and catalog — the leading `v` is stripped, so image tags are plain semver)
and the Release workflow (packages the chart, creates the GitHub release).

Before tagging, the version has to agree in five places:

| Where | What |
|---|---|
| `CHANGELOG.md` | a dated `## [X.Y.Z]` section |
| `deploy/helm/pgtoolbox/Chart.yaml` | `version`, `appVersion`, and the tag inside `icon:` |
| `deploy/olm/bundle/manifests/*.clusterserviceversion.yaml` | `spec.version`, `metadata.name`, the image tags in the manager args and `relatedImages` |
| `deploy/olm/catalog/pgtoolbox/catalog.yaml` | the channel entry and the bundle image |
| `Makefile` | `OLM_VERSION` |

`make validate-packaging` checks all five and runs in CI, so a mismatch is
a failed build rather than a bad release. The icon URL is the subtle one:
it names a tag on purpose — a chart pinned to a version must not have its
artwork change underneath it — which means it goes stale unless bumped.

One ordering constraint follows from this: the chart renders image
references at its own `appVersion`, and those images do not exist until the
tag is pushed. A checkout of `main` cannot `helm install` a version that has
not been released. Tag first, then every reference resolves.

## Git workflow

- One focused change per commit; keep generated files with their source
  changes.
- The agent does not run git mutations for you: you commit yourself.
- Open a pull request against `main` on GitHub.

## Merging

The ruleset on `main` requires every job
[`ci.yml`](.github/workflows/ci.yml) runs on a pull request — the image
build and the docs deploy are push-only and are not among them — plus
both [`codeql.yml`](.github/workflows/codeql.yml) analyses and the
code-scanning result, and requires that review threads be resolved. It
requires no approvals: the gate is the pipeline and the reading, not a
rubber stamp. Copilot is requested on every pull request automatically;
its review is always a comment, never an approval, so it cannot approve a
change — but an unresolved thread it opens does hold the merge until
someone answers it.

Branches need not be up to date with `main` to merge. Requiring that
would catch the case where two pull requests pass alone and break
together, but nothing rebases a stale branch on its own: auto-merge only
waits and merges, and Dependabot refreshes a branch when its manifest
conflicts, not when `main` moves. The requirement would therefore trade a
rare class of conflict for a queue that stalls on every merge. `ci.yml`
runs on pushes to `main` as well, so a conflict that slips through
surfaces there within minutes.

Dependabot's patch and minor bumps queue themselves through
[`automerge.yml`](.github/workflows/automerge.yml) and land the moment
the required checks go green. Majors are left for a person: the workflow
arms auto-merge from an allowlist, so an update type it does not
recognize is left alone rather than merged.

## Layout

- `api/v1alpha1/` — CRD types and CEL validation.
- `cmd/manager/` — operator binary (also the admin-sync init/sidecar image).
- `cmd/proxy/` — pgtoolbox-proxy binary.
- `internal/controller/pgconsole/` — PgConsole reconciler.
- `internal/controller/pgtoolboxaccessrequest/` — review-decision reconciler.
- `internal/adminsync/` — in-pod pgAdmin sync client and sidecar server.
- `internal/proxy/` — proxy config, session, providers, pages.
- `config/` — kustomize bases (CRDs, RBAC, manager, dev overlay).
- `deploy/helm/pgtoolbox/` — Helm chart.
- `deploy/olm/` — OLM bundle and file-based catalog.
- `web/` — Docusaurus documentation site.

## Documentation

User-facing behavior changes must come with doc updates in `web/docs/`.
Build the site locally:

```bash
cd web
npm install
npm start
```
