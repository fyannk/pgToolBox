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


## Fuzzing

`make test-fuzz` fuzzes the parsers on the trust boundary, each against
the invariant it owns rather than against "does not panic" — in this
codebase the dangerous failure is accepting something, not crashing on
it.

`session.Codec.Open` takes the most untrusted input the proxy sees: a
sealed cookie from a browser. Invariant 5 makes the proxy the only
authorization boundary, so a forged cookie that opens is a complete
authentication bypass; the target asserts that nothing the codec did not
seal ever opens, and that a rejected cookie leaves the destination
untouched so a caller ignoring the error still gets nothing usable.
`ValidCSRF` asserts the same for a token the codec did not mint,
including that a token minted for one expiry does not verify for
another. `evidence.ParseDestination` asserts that an accepted `s3://`
path round-trips exactly, because the bucket and prefix it returns feed
a fingerprint that identifies a backup repository.

It runs in the `Build, vet, test` job at `FUZZ_TIME` (5s per target) — a
smoke run, not a campaign. Raise it when chasing something:
`FUZZ_TIME=10m make test-fuzz`. A failing input is written to
`testdata/fuzz/` beside the target and becomes a regression case; commit
it with the fix.

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
and the Release workflow (packages the chart, pushes it to
`oci://ghcr.io/fyannk/charts`, and creates the GitHub release). The chart
is published as an OCI artifact rather than through a repository index, so
`helm install oci://ghcr.io/fyannk/charts/pgtoolbox --version X.Y.Z` needs
no `helm repo add` and nothing hosts an `index.yaml`. To publish the chart
for a tag that already shipped, run the Release workflow by hand with the
tag as its input: the release step is push-only, so a dispatch republishes
the chart and touches nothing else.

Before tagging, the version has to agree in six places:

| Where | What |
|---|---|
| `CHANGELOG.md` | a dated `## [X.Y.Z]` section |
| `deploy/helm/pgtoolbox/Chart.yaml` | `version`, `appVersion`, and the tag inside `icon:` |
| `deploy/olm/bundle/manifests/*.clusterserviceversion.yaml` | `spec.version`, `metadata.name`, the image tags in the manager args and `relatedImages` |
| `deploy/olm/catalog/pgtoolbox/catalog.yaml` | the channel entry and the bundle image |
| `Makefile` | `OLM_VERSION` |
| `web/docs/operator/installation.md` | the `--version` in the published `helm install` |

`make validate-packaging` checks all six and runs in CI, so a mismatch is
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
runs on pushes to `main` as well, and an auto-merge now lands as an
ordinary push because the merge is made with `AUTOMERGE_TOKEN` rather
than `GITHUB_TOKEN` — a push made with the latter starts no workflow run,
which used to mean the merges no human watched were exactly the ones the
push trigger missed. The daily schedule stays as a backstop rather than
as the only cover.

Dependabot's patch and minor bumps queue themselves through
[`automerge.yml`](.github/workflows/automerge.yml) and land the moment
the required checks go green. Majors are left for a person: the workflow
arms auto-merge from an allowlist, so an update type it does not
recognize is left alone rather than merged.

That workflow merges with `AUTOMERGE_TOKEN`, which **must be registered
as a Dependabot secret, not an Actions one** — it runs on
`pull_request`, and a Dependabot-triggered run sees only Dependabot
secrets. `tool-pins.yml` needs the same token as an ordinary Actions
secret, because it runs on a schedule. If either is missing the workflow
fails loudly and the bump waits for a person, rather than merging
unobserved.

Every `actions/checkout` sets `persist-credentials: false`. The default
leaves the job's token in `.git/config` for every later step to reach,
and nothing here pushes with it. Write scopes are granted on the job
that uses them rather than at the top of a workflow, so a job added
later inherits nothing it was not given.

[`scorecard.yml`](.github/workflows/scorecard.yml) runs OpenSSF
Scorecard weekly and on pushes to `main`, auditing what this repository
does to itself — pinned actions, token permissions, release signing,
dangerous workflow patterns — and files findings in code scanning beside
CodeQL's.

## Version pins

The Go version lives in `go.mod`. CI reads it with setup-go's
`go-version-file`, but the builder image in the `Dockerfile` has to
carry it a second time — an image tag cannot be read from a module file
— and Dependabot moves that tag on its own schedule.
[`hack/check-go-version.sh`](hack/check-go-version.sh) fails `make lint`
when the two disagree, because a release whose binaries and container
were built by different toolchains is not one the packaging can honestly
call a single build.

The language floor is held at 1.26.6, above the 1.26.4 the module graph
derives, because 1.26.4 and 1.26.5 carry standard-library
vulnerabilities that a `GOTOOLCHAIN=local` build would compile against.
CI never sees that, because CI builds at the pinned toolchain and never
at the floor; the reason sits beside the directive in `go.mod`.

`GOLANGCI_LINT_VERSION` and `GOVULNCHECK_VERSION` live in the `Makefile`
and are invisible to Dependabot, which reads manifests and not
`Makefile` variables.
[`hack/check-tool-pins.sh`](hack/check-tool-pins.sh) compares them
against the module proxy and
[`tool-pins.yml`](.github/workflows/tool-pins.yml) runs it weekly and
opens a pull request when one is behind. It proposes only; the required
checks decide. The script is deliberately not part of `make lint`: it
needs the network, and it would turn CI red the day upstream tags a
release.

It opens that pull request with `AUTOMERGE_TOKEN` as an ordinary Actions
secret. No document restates a tool version — the script rewrites the
`Makefile` and nothing else, so prose naming a version would be
falsified by the bump's own commit.

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
