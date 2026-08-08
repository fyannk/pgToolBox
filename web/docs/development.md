# Development

Build and test:

```bash
make generate manifests   # after any api/ change
make build
make test
make lint
```

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

:::note
The test found a real defect on its first run: a `PgConsole` naming no
images was rejected by the API server, because the image fields serialized
as `image: {}` against a schema requiring `repository` and `tag`. Nothing
in the unit suite could see it — a fake client does not run CRD validation.
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
