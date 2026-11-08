<!--
Thanks for contributing. Keep one focused change per PR, with tests and docs
alongside their source change. The code and tests are the source of truth;
the docs site explains their behavior. Use a Conventional Commit title
(feat:, fix:, refactor:, test:, docs:, build:, ci:, chore:).
-->

## What and why

<!-- What does this change, and what problem does it solve? -->

## How it was verified

<!-- Commands you ran, and behavior you confirmed. -->

- [ ] `make lint test` passes (`make check` for everything that runs without Docker or kind)
- [ ] `make verify-generated` leaves the tree clean (generated files are changed by their generator, never by hand)
- [ ] `make docs` passes if the site changed
- [ ] `make test-e2e-<scenario>` run if the change touches what a real API server
      validates: CRD rules, RBAC, or anything the operator composes (CI runs them too)

## Invariants

<!-- pgToolBox's boundaries are structural. Confirm this change keeps them. -->

- [ ] The console is read-only unless explicitly enabled, and the mutation
      surface stays enumerated (`internal/console/ops`, `hack/check-readonly.sh`)
- [ ] Authorization is the API server's: the console decides none of its own,
      and every read is checked with the signed-in person's token
- [ ] The operator never writes to a CloudNativePG `Cluster`, and a capability
      that is off removes its rules from the generated Roles
- [ ] The evidence agent claims no more than it read; object-store credentials
      never enter the console
- [ ] No credential is logged, put in a status field or event, or written
      outside the Pod-private paths that carry it
- [ ] Every install path stays consistent: CRDs, Helm charts, kustomize, OLM
      bundle and catalog agree (`make validate-packaging`)
- [ ] User-facing behaviour changes update `docs/site/docs/` and `CHANGELOG.md`
