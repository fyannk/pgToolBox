# The council: what every reviewer holds to

Several reviewers look at one target, each from its own side, and
`council-moderator` folds what they found into one ranked list. They run
through the `council` skill (`/council`). Each reviewer is an agent in
`.claude/agents/`; the four council voices (`council-architect`,
`council-contrarian`, `council-security`, `council-moderator`) read their
checklist in this folder, the specialists carry theirs in their own file.

The code is the specification. Reviewers do not ask for a plan, a spec or
an ADR, and the council writes none: what it finds is fixed in the code,
or becomes a GitHub issue on fyannk/pgToolBox.

## Rules

- **"No concerns" is a good answer.** Never write a finding to fill the
  report. One real finding beats five maybes.
- **Concrete or nothing.** A finding says what goes wrong: these inputs,
  this state, this wrong result, leak or crash. "Could be cleaner" is not
  a finding.
- **Check when you can.** A finding you reproduced (a test you ran, a
  manifest you rendered, a request you replayed against `httptest`) is
  worth more than one you inferred from reading. Say which it is, and
  what the check printed; make the check run the code's own case, not a
  toy that only resembles it.
- **Stay in your area.** Go style is `go-reviewer`'s, cluster-side
  security (RBAC, NetworkPolicy, Pod Security, SAR) is
  `k8s-security-reviewer`'s, everything else that lets someone in or
  leaks something is `council-security`'s, CNPG facts are
  `cnpg-status-expert`'s, screens are `ux-reviewer`'s.
- **The written decisions hold.** `AGENTS.md`, `ARCHITECTURE.md` and the
  numbered invariants of `CONTRIBUTING.md` record choices made on
  purpose (the console runs no SQL and reads no Secret; the operator never
  writes a CNPG `Cluster`; the evidence agent never claims more than
  "structurally usable"). Do not flag a deliberate choice as a bug; flag
  code that breaks it, and cite the invariant's number. The one exception
  is `council-contrarian`, whose job is to challenge them: as challenges
  with a case and a way to settle them, never as bugs.
  `council-architect` weighs size, not correctness: its finding is
  concrete when it names what to remove or merge, what that saves and
  what the DBA team loses.
- **Read only what you were given**, plus what you need to follow a call.
  You start cold: do not survey the repository.

## What you may run

- Read, grep, `git log` / `git diff` / `git show`, `helm template`.
- Go tests of the packages you review, one command at a time and short
  (`go test -count=1 ./internal/console/auth/ -run TestX`); never
  `-race` (one race run at a time on this machine), never `make
  test-e2e*`, never anything that creates a kind cluster or pushes an
  image.
- Keep `TMPDIR` and `GOTMPDIR` as the session sets them; scratch files in
  your scratchpad, never in the checkout. You never edit
  the repository, commit, push, or reach a real cluster.
- Never print a secret: tokens, keys, kubeconfigs, `.env` files,
  connection strings.

## How to answer

```
## <your name> — <target>

Verdict: no concerns | N findings

- [P1|P2|P3] [checked|read] path/to/file.go:123
  What goes wrong (input or state → wrong result), in one or two lines.
  Invariant: CONTRIBUTING.md #n (when one is broken)
  Fix: the smallest change.

Questions for the owner (only what the code cannot decide):
- …
```

Severity:

- **P1** lets someone in or past an authorization, leaks a secret, writes
  where the product promises not to, overstates evidence or shows a
  broken cluster as healthy, or crashes a component in a normal run.
- **P2** a wrong result a DBA will meet in an edge case, an unbounded
  read or a real operational risk (an upgrade, a restart, a slow API
  server).
- **P3** worth fixing when nearby; it becomes an issue, not a fix now.
