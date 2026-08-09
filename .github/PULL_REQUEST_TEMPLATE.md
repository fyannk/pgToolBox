<!--
Thanks for contributing. Keep one focused change per PR, with tests and docs
alongside their source change. The code and tests are the source of truth;
the docs site explains their behavior.
-->

## What and why

<!-- What does this change, and what problem does it solve? -->

## How it was verified

<!-- Commands you ran, and behavior you confirmed. -->

- [ ] `make test lint vuln audit` passes
- [ ] `make manifests` leaves the tree clean (`make verify-manifests`)
- [ ] `make docs` passes if the site changed
- [ ] `make test-e2e` run if the change touches what a real API server
      validates: CRD rules, RBAC, or anything the operator composes

## Invariants

<!-- pgToolBox's boundaries are structural. Confirm this change keeps them. -->

- [ ] The operator provisions access, not databases: no postgres role is
      created, and nothing writes to a managed cluster's data
- [ ] Authorization stays in the proxy — the console and pgAdmin trust the
      forwarded identity only because the NetworkPolicy confines ingress to
      the proxy, which strips any client-supplied copy first
- [ ] A capability that is off removes its rules from the generated Roles,
      so RBAC denies the operation whatever the application is told
- [ ] Every install path stays consistent: CRDs, Helm chart, OLM bundle and
      catalog agree (`make validate-packaging`)
- [ ] No credential is logged, put in a CR status, or written outside the
      Pod-private paths that carry it
- [ ] User-facing behaviour changes update `web/docs/` and `CHANGELOG.md`
