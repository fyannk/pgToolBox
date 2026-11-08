# pgToolBox

![licence](https://img.shields.io/badge/licence-Apache--2.0-0064b5?style=for-the-badge)
![cloudnativepg](https://img.shields.io/badge/CloudNativePG-1.30%2B-0064b5?style=for-the-badge)
![platforms](https://img.shields.io/badge/Kubernetes%20%7C%20OpenShift-amd64%20%7C%20arm64-555555?style=for-the-badge)
[![docs](https://img.shields.io/badge/docs-fyannk.github.io-555555?style=for-the-badge)](https://fyannk.github.io/pgToolBox/)

> A per-team console, a small operator and a read-only evidence agent for CloudNativePG clusters.

pgToolBox shows the real state of [CloudNativePG](https://cloudnative-pg.io)
clusters to the people who run them, and lets those who hold the right RBAC act
on them. It holds no authority of its own: the Kubernetes API server decides what
each person sees and may do, and every write is sent with that person's token, so
the API server's audit log names who did what.

## What it is

| | |
|---|---|
| **For** | Teams that run PostgreSQL with CloudNativePG 1.30 or later, on Kubernetes 1.28 or later or on OpenShift. |
| **Shows** | Each Cluster as the operator, its Pods and each instance's own `/pg/status` report describe it: health with the source of every fact, replication, storage forecasts, backups checked against the repository, diagnostics, history, timeline, logs and metrics. |
| **Does** | Four operations (backup, reload, restart, promote), off unless enabled, plus opt-in restore drills and time-bound access requests. |
| **Does not** | Run SQL, read Secrets, hold object-store credentials in the console, or keep a role model of its own. There is no cluster-wide console: a console reaches the namespaces it watches and nothing else. |

## Components and images

| Component | What it is | Image |
|---|---|---|
| **console** | A Deployment in a team namespace, serving a JSON API and a single-page application. People sign in with OIDC, OpenShift OAuth or a pasted API-server token; every read is checked with a SelfSubjectAccessReview sent with their own token. | `ghcr.io/fyannk/pgtoolbox-console` |
| **operator** | Optional. Reconciles `PgAdmin` (one pgAdmin per team, registered against many Clusters), `EvidenceAgent`, `PgToolBoxConsole` (a console declared as an object) and `PgAccessRequest` (removes a time-bound grant when it expires or is revoked). `PgAdminAccess` is a grant, read by the `PgAdmin` controller, that lets a pgAdmin of another namespace use a namespace's Clusters. | `ghcr.io/fyannk/pgtoolbox-operator` |
| **evidence agent** | Reads a backup repository, read-only (Barman Cloud on S3, Azure Blob or Google Cloud Storage; pgBackRest repository format 5 through the Dalibo or Opera CNPG-I plugin), and tells the console what it actually holds, to check what CloudNativePG claims. The only component that holds object-store credentials. | `ghcr.io/fyannk/pgtoolbox-evidence-agent` |
| **pgAdmin** | The pgAdmin image the operator and the `pgtools` chart run, built in [fyannk/pgAdmin](https://github.com/fyannk/pgAdmin). | `ghcr.io/fyannk/pgadmin` |

Two Helm charts install them:

| Chart | Installs | Installed by |
|---|---|---|
| `oci://ghcr.io/fyannk/charts/pgtools` | A team's console, and optionally evidence agents and a pgAdmin, with no operator and nothing cluster-scoped by default. | an admin of the namespace |
| `oci://ghcr.io/fyannk/charts/pgtoolbox` | The operator and its CRDs (`pgtoolbox.fyannk.dev/v1alpha1`). Also available through kustomize and an OLM bundle. | a cluster admin, once |

All images are built for `linux/amd64` and `linux/arm64`.

## Quick start

A team console, signing in with a pasted token:

```bash
helm install console oci://ghcr.io/fyannk/charts/pgtools \
  --version 0.1.0 --namespace team-a \
  --set console.auth.provider=token
```

Then create the CA bundle the console verifies instances with, and bind people
to the `pgtoolbox-console-viewer` or `pgtoolbox-console-operator` ClusterRole. The
[installation guide](https://fyannk.github.io/pgToolBox/docs/install/) walks
through it, and through single sign-on, the operator, kustomize and OLM.

The operator, once per cluster:

```bash
helm install pgtoolbox oci://ghcr.io/fyannk/charts/pgtoolbox \
  --version 0.1.0 --namespace pgtoolbox --create-namespace
```

## Security model

- **The API server authorizes.** Every read is a SelfSubjectAccessReview sent with
  the signed-in person's token; every write is sent with that token. The console's own
  identity writes only its own state (one ConfigMap, and a Lease when it delivers
  the daily digest) and, opt-in, deletes the restore-drill Clusters it created; it
  never impersonates.
- **One console per team.** A console has Roles in the few namespaces it watches
  and no Secret verb, so a compromised console reaches those namespaces only.
- **Credentials stay where they are needed.** Object-store credentials live in
  the evidence agent alone; the console receives its conclusions over HTTPS. The
  operator writes libpq credential files for pgAdmin and never puts secret
  material in status, events or logs. The operator never writes to a
  CloudNativePG `Cluster`.
- **Uncertainty is shown, not hidden.** A fact the console cannot verify is shown
  as not verifiable, and a check that could not run never counts as passed.
- **Signed releases.** Images and charts are signed by digest and carry an SBOM
  and provenance attestations.

[SECURITY.md](SECURITY.md) says how to report a vulnerability; the
[security model](https://fyannk.github.io/pgToolBox/docs/security/) page has the
full picture, and the [security scan results](https://fyannk.github.io/pgToolBox/security/)
page shows the latest run of every scanner.

## Documentation

[fyannk.github.io/pgToolBox](https://fyannk.github.io/pgToolBox/): installation,
the console and its configuration, the operator and its kinds, the evidence
agent, the security model, what the console may do, and the generated reference
(CRD fields, diagnostics checks). The site is built from `docs/site`
(`make docs`). [ARCHITECTURE.md](ARCHITECTURE.md) explains how the code fits
together, package by package.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). `make help` lists the targets, `make
doctor` checks the toolchain against `mise.toml`, and `make check` runs what CI
runs without Docker or kind. The pipeline is described in
[docs/site/docs/development/ci.md](docs/site/docs/development/ci.md) and the
repository settings it relies on in
[docs/site/docs/development/repository-settings.md](docs/site/docs/development/repository-settings.md).

## Licence

[Apache 2.0](LICENSE).
