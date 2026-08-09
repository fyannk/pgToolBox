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

## Git workflow

- One focused change per commit; keep generated files with their source
  changes.
- The agent does not run git mutations for you: you commit yourself.
- Open a merge request against `main` on the project GitLab.

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
