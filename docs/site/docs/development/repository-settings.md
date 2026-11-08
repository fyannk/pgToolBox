---
sidebar_position: 3
title: Repository settings
---

# Repository settings

The workflows enforce what a workflow can enforce. These settings are the other half,
and live only in the GitHub UI (or API) of `github.com/fyannk/pgToolBox`: until they
are set, the checks run but nothing makes a pull request wait for them. Set them
once, after the first push, and again if the repository is ever recreated.

## Ruleset on `main`

Settings, Rules, Rulesets, New branch ruleset. Target the default branch.

| Rule | Setting |
|---|---|
| Restrict deletions, block force pushes | on |
| Require a pull request before merging | on; 0 required approvals while one person maintains it; require conversation resolution |
| Require status checks to pass | on; "Require branches to be up to date" on; the checks below |
| Require code scanning results | on; tool CodeQL, security alerts "High or higher", alerts "Errors" |
| Require linear history | on (squash merges, which is what `automerge.yml` uses) |
| Require signed commits | optional |

**Required status checks** (the names are the jobs' names):

| Check | Workflow | Why it is the only CI check you need |
|---|---|---|
| `ci-ok` | `ci.yml` | aggregates every job; skipped jobs count as passed, failed or cancelled ones do not, and it names no individual job, so adding or renaming a job never means editing the ruleset |
| `security-gate` | `security.yml` | merges every scanner's result and fails on a fixable critical, a secret, a reachable Go vulnerability, a new fixable high on a pull request, or a scanner that did not run |

Do not add the individual jobs: a job skipped by the path filter would not report and
would block the merge. Allow the Actions bot (`github-actions`) and Dependabot to
merge, so that auto-merge can complete.

## Pages

Settings, Pages, Build and deployment, Source: **GitHub Actions**. The `github-pages`
environment that `pages.yml` deploys to only accepts deployments from `main` by
default; leave that branch policy as it is. The site is then
`https://fyannk.github.io/pgToolBox/` and the Security page
`https://fyannk.github.io/pgToolBox/security/`.

## Security

Settings, Code security:

- **Private vulnerability reporting**: enable. `SECURITY.md` points reporters to
  `https://github.com/fyannk/pgToolBox/security/advisories/new`.
- **Code scanning**: nothing to configure for CodeQL (`security.yml` uploads
  it) beyond leaving it enabled; the scanners' SARIF appear under their categories
  (`grype-*`, `govulncheck-*`, `gitleaks`, `gosec-*`, `zizmor`, `kube-linter-*`,
  `osv-scanner`, `scorecard`).
- **Dependabot alerts** and **Dependabot security updates**: enable; the version
  updates come from `.github/dependabot.yml`.
- **Secret scanning** and **push protection**: enable.

## Actions

Settings, Actions, General:

- Workflow permissions: **Read repository contents and packages permissions**
  (workflows request more per job and say why).
- Allow GitHub Actions to create and approve pull requests: off.
- Fork pull request workflows: require approval for first-time contributors.
- Allowed actions: allow actions created by GitHub, and the owners listed in
  `.github/workflows` (verified creators); every `uses:` is already pinned to a
  commit.

## Packages

The first release creates the packages under `github.com/fyannk?tab=packages`:
`pgtoolbox-operator`, `pgtoolbox-console`, `pgtoolbox-evidence-agent`,
`pgtoolbox-bundle`, `pgtoolbox-catalog`, and the charts `charts/pgtoolbox` and
`charts/pgtools`. For each package: Package settings, Danger zone, **Change
visibility to public**, and confirm the package is linked to the repository (the
`org.opencontainers.image.source` label does that). Until a package is public, an
anonymous `docker pull`, `helm pull` and the Security page's published-image scan
all fail with "not found".

## Releasing

Nothing in the workflows pushes a tag. A release is cut by a person:

1. `make release-check TAG=vX.Y.Z` and `make release-dry-run` on a clean `main`.
2. `git tag -a vX.Y.Z -m "pgToolBox X.Y.Z"` and `git push origin vX.Y.Z`.
3. `release.yml` waits for `ci-ok` and `security-gate` on that commit, then publishes.
   If it waits for a commit that has no run, dispatch `ci.yml` and `security.yml` on
   `main`.
