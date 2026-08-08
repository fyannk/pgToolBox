# Development

Build and test:

```bash
make generate manifests   # after any api/ change
make build
make test
make lint
```

## A browsable dev console

```bash
make dev-up
```

Stands up kind + CloudNativePG + the operator, declares one `PgConsole`
with pgAdmin and the evidence sidecar, seeds a user at each authorization
level, and forwards the console's own proxy to
[localhost:3000](http://localhost:3000). First run ~6 minutes; re-running
against a live cluster just relaunches the forward. `RECREATE=true` starts
clean, `SKIP_BACKUP=true` drops the object store, `NO_FORWARD=true` sets
up and exits.

| Sign in as | Password | Reaches |
|---|---|---|
| `viewer@pgtoolbox.dev` | `viewer` | the overviews and the metrics screens |
| `operator@pgtoolbox.dev` | `operator` | + every other read screen and the log tails |
| `dba@pgtoolbox.dev` | `dba` | + day-2 operations, the review panel, pgAdmin |

Signing in as an unknown identity shows the proxy's 403 page, whose form
files a real `PgToolBoxAccessRequest` for the `dba` to approve.

Unlike pgConsole's `dev-up.sh`, this does not fake the trusted proxy on
several ports. pgConsole has no authentication of its own, so its script
injects the forwarded headers itself; pgToolBox **is** that proxy, so
there is one port, a real login, and real sessions — the level ladder
comes from the seeded `PgToolBoxRole` and `PgToolBoxUser` objects.

:::note
The subjects are email addresses on purpose. pgAdmin keys its accounts on
one, so a `PgToolBoxUser` whose subject is not an email address cannot be
provisioned in pgAdmin — its sync fails for that user.
:::

## The e2e smoke test

```bash
make test-e2e
```

It provisions a kind cluster, installs CloudNativePG, installs this
operator through the Helm chart, and brings one `PgConsole` up against the
**published pgConsole image** — then tears the cluster down. Set
`KEEP_CLUSTER=1` to leave it running, and `REUSE_CLUSTER=1` to re-run the
test against a cluster that is already up.

It exists for the two failure modes `make test` structurally cannot reach,
because the unit tests run against a fake client:

- **RBAC escalation prevention.** A real API server refuses to let the
  operator create a Role carrying a rule its own ClusterRole does not hold.
  A missing `+kubebuilder:rbac` marker is therefore invisible until a
  generated Role hits a live cluster.
- **A component rejecting its configuration.** pgConsole validates its
  whole environment at startup and refuses to serve on a value it rejects.
  A container reaching Ready is the proof that what the operator rendered
  is what the real binary accepts.

Both matter when you touch `readRole`, `operateRules`, or `consoleEnv`.
Run it before changing any of them.

Two tests run:

- **`TestConsoleSmoke`** — the fast path, pgAdmin off. Console reaches
  `Ready`, the generated Roles survived escalation prevention, and
  switching the write capabilities off withdraws the operate Role.
- **`TestFullComposition`** — every optional component on, against a real
  CloudNativePG cluster archiving into an in-cluster object store: the
  pgAdmin container and its PVC, the admin-sync init container and
  sidecar, the evidence sidecar, and the whole provisioning chain through
  to `PgAdminSynced`.

It runs the **family images** — `ghcr.io/fyannk/pgconsole`,
`ghcr.io/fyannk/pgadmin` and `ghcr.io/fyannk/pgobjectstoreviewer` — not
substitutes. CloudNativePG, cert-manager, the Barman Cloud Plugin and
MinIO are test infrastructure around them.

:::note
The test earns its runtime. Its first runs found six defects nothing in
the unit suite could see: a `PgConsole` naming no images was rejected by
the API server; and the embedded pgAdmin could never start or provision a
user, for five independent reasons — missing bootstrap credentials, an
unwired `AdminSync`, a revision gate reading an annotation nothing writes,
a settings volume the sidecar never mounted, and a user that was updated
without ever being created.
:::

The full contribution guide — repository invariants, layout, git workflow —
lives in [`CONTRIBUTING.md`](https://github.com/fyannk/pgtoolbox/blob/main/CONTRIBUTING.md)
in the repository.

Build this site locally:

```bash
cd web
npm install
npm start
```
