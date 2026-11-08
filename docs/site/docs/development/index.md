---
sidebar_position: 1
title: Developing pgToolBox
---

# Developing pgToolBox

This section is for people changing pgToolBox itself. The full material lives
in the repository, next to the code:

- [ARCHITECTURE.md](https://github.com/fyannk/pgToolBox/blob/main/ARCHITECTURE.md):
  the programs, the packages (one line each), the layering rules and the checks
  that enforce them, the console's data flow and background loops, the generated
  files and how to regenerate them, and how the tests are organised.
- [CONTRIBUTING.md](https://github.com/fyannk/pgToolBox/blob/main/CONTRIBUTING.md):
  the development environment, the invariants every change keeps, how to
  propose a change.
- [AGENTS.md](https://github.com/fyannk/pgToolBox/blob/main/AGENTS.md):
  the same rules written for coding agents.

How CI chooses its jobs and how a tag becomes a release is on
[CI and releases](ci.md); the settings the workflows cannot set for themselves
(the ruleset, Pages, private vulnerability reporting, package visibility) are on
[Repository settings](repository-settings.md).

## Tools

Every tool version is pinned in `mise.toml`. `make doctor` compares your machine
with it and prints what is missing. `make help` lists every target.

## The targets you will use most

| Target | What it does |
|---|---|
| `make lint` | golangci-lint, format check and the repository checks (licence headers, layering, the console's read-only boundary) |
| `make test` | unit tests of both Go modules |
| `make check` | everything CI runs without Docker or kind |
| `make verify-generated` | regenerates every generated file and fails on any difference |
| `make generate manifests` | deepcopy code, then the CRDs and RBAC and their Helm and OLM copies |
| `make openapi` | the console's `openapi.yaml` and API fixtures (`make verify-openapi` checks them) |
| `make catalog-docs crd-ref-docs` | the generated [reference](../reference/api.md) pages |
| `make ui-dev` | the console's web interface on Vite with mock data, no cluster needed |
| `make ui-check` | the web interface's full gate (lint, types, Vitest, Storybook stories, build, size) |
| `make ui-e2e-update` | re-render the Playwright screenshot baselines; review the image diff before committing |
| `make docs`, `make docs-serve` | build this site (fails on a broken link), or serve it with live reload |
| `make test-envtest` | tests against a real API server without nodes (CRD rules, RBAC, reconcilers) |
| `make test-e2e-console`, `make test-e2e-team`, `make test-e2e-pgbackrest`, `make test-e2e-matrix` | end-to-end scenarios on kind (need Docker) |

After changing an API type, regenerate in this order: `make generate manifests
openapi crd-ref-docs`, then `cd ui && pnpm generate`.

## Rules that surprise newcomers

- **Generated files are never edited by hand.** CI fails when one is stale.
- **The console writes nothing on its own behalf.** Every write is sent with the
  signed-in person's token and starts in one package; a check fails the build if
  a write appears anywhere else.
- **Layering is checked, not just agreed on.** `pkg/` imports nothing from
  `internal/`, and the console, the operator and the evidence agent do not import
  each other.
- **The code is the reference.** When this site and the code disagree, the page is
  the bug: fix it in the same change.
