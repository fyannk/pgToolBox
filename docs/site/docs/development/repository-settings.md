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

The ruleset "Protect main" as it is configured (`gh api repos/fyannk/pgToolBox/rulesets`),
with no bypass actor:

| Rule | Setting |
|---|---|
| Restrict deletions, block force pushes | on |
| Require a pull request before merging | on; 0 required approvals while one person maintains it; require conversation resolution; merge, squash and rebase allowed (`automerge.yml` squashes) |
| Require status checks to pass | on, the two checks below; "Require branches to be up to date" **off** (`do_not_enforce_on_create` on) |

Not enabled today, each an optional owner toggle (none is needed for the release
process, and each costs something):

| Optional rule | Trade-off |
|---|---|
| Require branches to be up to date | Every merge would need a re-run of `ci-ok` on the new base; with one maintainer and the weekly and per-merge full runs on `main`, the cost outweighs it |
| Require code scanning results (CodeQL, "High or higher") | CodeQL runs in `security.yml`, whose `security-gate` already fails on what matters; this rule would add a second, slower gate on the same alerts |
| Require linear history | Only a policy: `automerge.yml` squashes and the maintainer's merges are squash too, but the rule would refuse a merge commit |
| Require signed commits | Optional; the bot's squash commits are signed by GitHub |

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
  The list is generated from the workflows and the local actions under `.github/actions`, and `make check-repo` fails when it differs. It names each action with `@*`: an entry that names a SHA allows only that SHA, so an exact list would refuse every Dependabot bump, while the SHA pins themselves are enforced by the setting above. Regenerate it only when a workflow starts using a new action:

  ```bash
  grep -rhoE 'uses: [^./][^ ]+@[0-9a-f]{40}' .github/workflows .github/actions | sed -E 's/uses: //; s/@[0-9a-f]{40}$/@*/' | sort -u | grep -vE '^(actions|github)/'
  ```

  Allow-list:

  - `anchore/sbom-action/download-syft@*`
  - `dataaxiom/ghcr-cleanup-action@*`
  - `dependabot/fetch-metadata@*`
  - `docker/build-push-action@*`
  - `docker/login-action@*`
  - `docker/metadata-action@*`
  - `docker/setup-buildx-action@*`
  - `jdx/mise-action@*`
  - `ossf/scorecard-action@*`
  - `sigstore/cosign-installer@*`

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
