# Checklist: the architect (simpler, smaller)

One person maintains pgToolBox, with agents writing most of the code;
every line is a line they will one day read, fix or move. A finding here
is concrete when it names **what to remove or merge, what that saves**
(lines, files, a dependency, a CRD field, a chart value, a concept a
newcomer must learn) **and what the DBA team loses** (nothing, or this
one case). "It feels big" is not a finding; "these 400 lines serve one
caller that could call X" is. Count before you claim: `grep -c`,
`wc -l`, `git log --format= -- path | wc -l`, callers through `go doc`
or grep.

- **Built for a case that does not come.** A setting, a chart value, a
  CRD field, an interface, a generic, an option struct or a mode with one
  implementation and one caller; a fallback for a state the code cannot
  reach; a feature no screen, API route, MCP tool, test or doc page uses
  (grep `ui/src`, `internal/console/api`, `internal/console/mcp`, the
  tests and `docs/site/docs` before saying so).
- **The same thing twice.** Two packages, two clients or two code paths
  doing one job with small differences (the operator and the `pgtools`
  chart rendering the same object; the console and the agent each
  parsing the same thing); a cache or a snapshot that holds what another
  already says; a helper rewritten next to one in `pkg/`. Some copies are
  on purpose (Helm, kustomize and OLM, held together by
  `validate-packaging`; generated files): those hold, but a copy without
  a test holding it to its source does not.
- **Heavier than the job.** A goroutine, a worker, a Lease, a queue, a
  controller or a dependency where a function would do; a module pulled
  for one call; state kept in three places that must agree; a watch
  where a single read at start would do, or the reverse.
- **Layers that only pass things on.** A wrapper that renames, a view
  model copied field by field into another, an interface whose every
  method forwards.
- **The cost of keeping it.** A package that changes in every PR for
  reasons that are not its own; a file past a thousand lines that holds
  several jobs; tests that need a long setup because the code is
  tangled; a review fix that grew a package far past the bug it fixed.
- **What the owner pays later.** Weigh the DBA team's gain against what it
  costs to keep: a feature used once a year that touches ten files, three
  install paths and a doc page is worth naming, even when it works.

Do not flag a choice `AGENTS.md`, `ARCHITECTURE.md` or `CONTRIBUTING.md`
records as deliberate as a finding; if you think the choice itself is too
heavy, put it under the owner's questions with what it costs and the
simpler shape. Severity here: **P2** a part whose weight slows every
change near it, or a removable piece with a clear saving; **P3** a tidy
when nearby.
