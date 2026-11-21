---
name: council
description: Review a pgToolBox change, a part or the whole project from several sides at once — Go, cluster-side and application security, CloudNativePG facts, the operator, the UI and its screens, CI and packaging, tests, a simplifier and a contrarian — then fold the findings into one checked, ranked list. Use on /council, before asking to tag a release, or when a review round is asked for.
---

# The council

Several reviewers (`.claude/agents/`) look at one target in parallel;
`council-moderator` checks their findings against the code and ranks them.
The rules they hold to, and the P1–P3 scale, are in `.claude/council/README.md`.

Nothing is written as a document: the report is given in the conversation;
what holds is fixed in the code, and what is not fixed now becomes a GitHub
issue on fyannk/pgToolBox.

## 1. Read the target

`/council [target]`:

| Target | Level | Files |
| --- | --- | --- |
| nothing | change | `git diff origin/main...HEAD` plus uncommitted and untracked files |
| `A..B`, `since <ref>` | change | `git diff --stat A..B` (a batch of work) |
| files | change | those files |
| a package or folder (`internal/console/auth`, `charts/pgtools`, `ui/src/screens`) | part | the folder |
| a voice's name | area | that voice's paths across the project |
| `all` | project | every area |

## 2. Pick the voices

| Voice | Paths |
| --- | --- |
| `go-reviewer` | any `*.go` |
| `go-operator-engineer` | `internal/operator/**`, `cmd/operator/**`, `api/**`, `internal/backupresolve/**` |
| `cnpg-status-expert` | `pkg/cnpgstatus/**`, `pkg/diagnose/**`, `internal/console/{status,readmodel,sweep,viewmodel}/**`, `internal/backupresolve/**`, `internal/evidence/selfresolve/**` |
| `k8s-security-reviewer` | `internal/console/{auth,authz,kube,ops}/**`, `internal/operator/**` (RBAC markers, rendered NetworkPolicy and pods), `internal/operator/admissionpolicy/**`, `config/rbac/**`, `charts/*/templates/**`, `deploy/olm/**`, `api/**` (CEL) |
| `council-security` | every target; most weight on `internal/console/{auth,api,mcp,mcpauth,mcpoauth,assistant,sse,digest}/**`, `pkg/redact/**`, `internal/evidence/{formats,provider,redact,evidenceapi}/**`, `pkg/evidence/**`, `pkg/cnpgstatus/{scrape,metrics,logstream,pgconf}/**`, `internal/operator/adminsync/**`, `ui/src/**` (HTML sinks) |
| `frontend-engineer` | `ui/**` |
| `ux-reviewer` | `ui/src/{screens,routes,ds}/**` |
| `release-engineer` | `.github/**`, `hack/**`, `Makefile`, `mise.toml`, `Dockerfile`, `charts/**`, `deploy/**`, `config/**`, `go.mod` |
| `test-engineer` | no paths: by level (below) |
| `council-architect` | no paths: by level (below); what is over-built, duplicated, heavier than the job |
| `council-contrarian` | no paths: by level (below); challenges the premises and the recorded decisions |

By level:

- **Change**: `council-security`, `go-reviewer` when Go changed, and the
  matched voices. For one commit, the one or two with the most changed lines
  (`git diff --numstat`); for a batch, every voice its files match, plus
  `council-architect`, `council-contrarian` and `test-engineer` (not for one
  commit: a single change is too small to weigh). A change touching only docs
  or words needs no council.
- **Part**: as for a change, plus every matched voice, `test-engineer`,
  `council-architect` and `council-contrarian`.
- **Area**: that voice alone over its paths, with `council-security`,
  `council-architect` and `council-contrarian` on the same paths.
- **Screens**: a change a person sees (a route, a pattern, the tokens) also
  gets `ux-reviewer` over its stories and the Playwright screenshots
  (`ui/e2e/__screenshots__`).
- **Project** (the review before a release): every voice over its own paths.
  Split what one reviewer cannot read in one go: `go-reviewer` once each for
  the console, the operator, the evidence agent and `pkg/`;
  `council-security` once for sign-in, routes and MCP, once for what comes
  from outside (`internal/evidence`, `pkg/cnpgstatus`, `pkg/evidence`).
  `council-architect` and `council-contrarian` read the whole project once
  each, from `AGENTS.md`, `ARCHITECTURE.md`, `CONTRIBUTING.md` and the tree
  (`wc -l` per package), then go deep where the weight is. Add a triage of
  the open code-scanning, Dependabot and secret-scanning alerts
  (`gh api repos/fyannk/pgToolBox/code-scanning/alerts?state=open`, and the
  same for `dependabot` and `secret-scanning`): list them, never dismiss one
  unless the owner asks.

## 3. Brief them

Start every chosen voice in one message, so they run in parallel. Each brief
holds only:

- the level and the target: the diff command (`git diff A..B -- <paths>`) or
  the list of files or folders, narrowed to that voice's paths;
- the checkout to read (a worktree path when the work is in one);
- one line on what the change was for, when there is a change.

Not your own guesses about what is wrong: let each voice find it.

## 4. Moderate

Hand every report, verbatim, to `council-moderator`, with the target and the
level. It checks the findings against the code and answers one ranked list.

## 5. Act on it

- Give the owner the moderator's report, shortened to what holds: the ranked
  fixes, the owner's questions, a line on what was dropped.
- Fix what holds and is clear, smallest fix first, each with its test,
  `make lint test`, a conventional commit saying what was wrong. Do not
  over-engineer: a fix that adds more machinery than the risk it removes is a
  P3.
- P3s and anything not fixed now become GitHub issues on fyannk/pgToolBox,
  one each with its `path:line`.
- A question only the owner can answer is asked, not guessed.
- Push, open a PR, merge or tag only as the owner has said.
