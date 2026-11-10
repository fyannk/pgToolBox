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

## Tags and the `release` environment

Settings, Rules, Rulesets, New tag ruleset. Target tags matching `v*` and
`pkg/evidence/v*` (fnmatch patterns: `v0.1.0` and `v0.1.0-rc.1` both match), with
**Restrict updates** and **Restrict deletions** on, so a published tag never moves.

Settings, Environments, `release`: deployment branches and tags, **Selected branches
and tags**, add the tag patterns `v*` and `pkg/evidence/v*` (and no branch); required
reviewer: the maintainer. `release.yml`'s first job runs in this environment, so a
dispatch from a branch never starts and nothing is built before the approval.

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

### The Scorecard alerts on the Security tab

`scorecard.yml` uploads OpenSSF Scorecard's SARIF, so four checks appear as code
scanning alerts. They describe the project's situation rather than a flaw to patch;
each is listed here with what it means and whether it should stay open. Dismissing
is a write to the repository, so the maintainer does it (the workflow never does).

| Rule | What it says | Why it fires | Suggested handling |
|---|---|---|---|
| `CodeReviewID` | Changes are not approved by a second person | One maintainer; pull requests are checked by CI, not by another human | Dismiss, `won't fix`: stays true until there is a second maintainer |
| `MaintainedID` | The project is young or inactive | Scorecard scores a repository under 90 days old as unmaintained, whatever the activity | Dismiss, `false positive`; it clears itself, re-check after 90 days |
| `VulnerabilitiesID` | Known vulnerabilities in dependencies (OSV) | The same advisories the `osv-scanner` job gates; accepted ones are listed with a reason in `hack/security-accepted.txt` | Do not dismiss blindly: open the alert, and dismiss only advisories that file accepts |
| `BranchProtectionID` | Branch protection is weaker than Scorecard's ideal | The `main` ruleset is read with a token that cannot see rulesets; required approvals are 0 for the reason in `CodeReviewID` | Dismiss, `won't fix`, after checking the ruleset matches the table above |

List them with their numbers, then dismiss one (reasons accepted by the API are
`false positive`, `won't fix`, `used in tests`):

```bash
gh api 'repos/fyannk/pgToolBox/code-scanning/alerts?state=open&tool_name=Scorecard' \
  --jq '.[] | [.number, .rule.id, .most_recent_instance.location.path] | @tsv'

gh api --method PATCH repos/fyannk/pgToolBox/code-scanning/alerts/NUMBER \
  -f state=dismissed -f dismissed_reason="won't fix" \
  -f dismissed_comment='Single maintainer; see docs/site/docs/development/repository-settings.md'
```

Check the reasons above against the alerts' own text before dismissing (they are
Scorecard's assessment on the day, and the table is the maintainer's reading of it),
and review the dismissed ones once the repository is 90 days old.

## Actions

Settings, Actions, General:

- Workflow permissions: **Read repository contents and packages permissions**
  (workflows request more per job and say why).
- Allow GitHub Actions to create and approve pull requests: off.
- Fork pull request workflows: require approval for first-time contributors.
- Allowed actions (owner step; the API default is `all`): set
  `allowed_actions` to **selected** (Settings, Actions, General, "Allow
  enterprise/owner, and select non-owner, actions"). Tick "Allow actions created by
  GitHub" (covers `actions/*` and `github/codeql-action/*`) and paste the list
  below into "Allow specified actions and reusable workflows". With "Require actions
  to be pinned to a full-length commit SHA" on, every `uses:` is already compliant.
  The list is generated from the workflows; regenerate it after a Dependabot bump:

  ```bash
  grep -rhoE 'uses: [^./][^ ]+@[0-9a-f]{40}' .github/workflows | sed 's/uses: //' | sort -u | grep -vE '^(actions|github)/'
  ```

  Allow-list at this commit (`owner/repo@sha`, the SHA is what makes it exact):

  - `anchore/sbom-action/download-syft@66cbf4bc1f1c0d2edc94016e65bc221b6bb0ad6c`
  - `dataaxiom/ghcr-cleanup-action@d52806a0dc70b430571a37da1fde39733ffd640f`
  - `dependabot/fetch-metadata@25dd0e34f4fe68f24cc83900b1fe3fe149efef98`
  - `docker/build-push-action@c3c9e263c25d99ce0380d002d59b67737d91b0dc`
  - `docker/login-action@81c6f93d4bc991e510b342a00b419b3bb4efc5c6`
  - `docker/metadata-action@dc802804100637a589fabce1cb79ff13a1411302`
  - `docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069`
  - `ossf/scorecard-action@fa7117db83c52d787ae5a89f350999701505d78e`
  - `sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6`

## Packages

The first release creates the packages under `github.com/fyannk?tab=packages`:
`pgtoolbox-operator`, `pgtoolbox-console`, `pgtoolbox-evidence-agent`,
`pgtoolbox-bundle`, `pgtoolbox-catalog`, and the charts `charts/pgtoolbox` and
`charts/pgtools`. For each package: Package settings, Danger zone, **Change
visibility to public**, and confirm the package is linked to the repository (the
`org.opencontainers.image.source` label does that). Until a package is public, an
anonymous `docker pull`, `helm pull` and the Security page's published-image scan
all fail with "not found". `release.yml` checks this itself: before it publishes the
release it asks for every pushed digest with no credentials, and if any is private it
prints each package's settings URL and keeps polling (every minute, for about 20
minutes), so making them public while the run waits turns it green. After that it
fails naming the packages and leaves the release a draft: make them public, then use
**Re-run failed jobs** on the run.

Pruning (`release.yml`, job `prune`) is opt-in: set the repository variable
`PRUNE_GHCR` to `true` (Settings, Secrets and variables, Actions, Variables) once a
release has verified without it. It deletes untagged versions with the Actions
token. On a package owned by a user account that token may lack the right to delete
versions: give the repository **Admin** on each package (Package settings, Manage
Actions access), or prune by hand. A failed prune does not stop `verify`.

## Repository topics and release immutability

Not set by the repository; do these as the owner.

- **Topics** (About, gear icon): `cloudnativepg`, `postgresql`, `kubernetes`,
  `openshift`, `operator`, `observability`, `backup`, `barman`, `pgbackrest`,
  `helm`, `golang`, `mcp`. They are what the repository is found by.
- **Immutable releases** (Settings, General, Releases, *Enable release
  immutability*): turn on **after the first release is published** (tag and assets
  then cannot be changed or deleted, and the release gets an attestation). Not before:
  `release.yml` creates the release as a draft, uploads the assets and publishes it
  last, which immutability allows, but a failed release candidate's release would be
  locked as soon as it is published. Check afterwards with
  `gh release verify vX.Y.Z`.

## Releasing

Nothing in the workflows pushes a tag. A release is cut by a person:

1. `make release-check TAG=vX.Y.Z` and `make release-dry-run` on a clean `main`, then
   `make release-preflight TAG=vX.Y.Z` (dated CHANGELOG, `ci-ok` and `security-gate`
   green on the commit to tag: check runs must exist on the tagged commit).
2. `git tag -a vX.Y.Z -m "pgToolBox X.Y.Z"` and `git push origin vX.Y.Z`.
3. `release.yml` waits for `ci-ok` and `security-gate` on that commit, then publishes.
   If it waits for a commit that has no run, dispatch `ci.yml` and `security.yml` on
   `main`.
