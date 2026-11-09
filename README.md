<p align="center">
  <img src="docs/site/static/img/logo.svg" width="140" alt="pgToolBox logo: an elephant peeking over a toolbox bearing an ermine">
</p>

<h1 align="center">pgToolBox</h1>

<p align="center">
  <strong>See the real state of your CloudNativePG clusters, as the operator, the Pods and each instance report it.</strong><br>
  The Kubernetes API server decides what each person sees and may do; the console holds no authority of its own.
</p>

<p align="center">
  <a href="https://fyannk.github.io/pgToolBox/"><img src="https://img.shields.io/badge/docs-fyannk.github.io-090c0a?style=flat-square&labelColor=1b231d" alt="Documentation"></a>
  <a href="https://github.com/fyannk/pgToolBox/actions/workflows/ci.yml"><img src="https://github.com/fyannk/pgToolBox/actions/workflows/ci.yml/badge.svg" alt="CI status"></a>
  <a href="https://fyannk.github.io/pgToolBox/security/"><img src="https://img.shields.io/badge/security_report-every_push-090c0a?style=flat-square&labelColor=1b231d" alt="Security scan results"></a>
  <a href="https://www.bestpractices.dev/projects/16490"><img src="https://img.shields.io/cii/level/16490?style=flat-square&label=openssf%20best%20practices&labelColor=1b231d&color=090c0a" alt="OpenSSF Best Practices"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/fyannk/pgToolBox"><img src="https://img.shields.io/ossf-scorecard/github.com/fyannk/pgToolBox?style=flat-square&label=openssf%20scorecard&labelColor=1b231d&color=090c0a" alt="OpenSSF Scorecard"></a>
  <a href="https://fyannk.github.io/pgToolBox/docs/release/"><img src="https://img.shields.io/badge/signed-cosign-090c0a?style=flat-square&labelColor=1b231d" alt="Signed with cosign"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-090c0a?style=flat-square&labelColor=1b231d" alt="Apache-2.0 license"></a>
  <img src="https://img.shields.io/badge/CloudNativePG-1.30%2B-090c0a?style=flat-square&logo=postgresql&logoColor=b3bf3a&labelColor=1b231d" alt="CloudNativePG 1.30 or later">
  <img src="https://img.shields.io/badge/Go-1.27-090c0a?style=flat-square&logo=go&logoColor=b3bf3a&labelColor=1b231d" alt="Go 1.27">
  <img src="https://img.shields.io/badge/status-0.x_pre--1.0-090c0a?style=flat-square&labelColor=1b231d" alt="Pre-1.0">
</p>

pgToolBox shows the real state of [CloudNativePG](https://cloudnative-pg.io)
clusters to the people who run them, one team console per namespace, and lets
those who hold the right RBAC act on them. A small, optional operator adds a
managed pgAdmin per team, and a read-only evidence agent checks what the backup
repository actually holds. It runs on Kubernetes 1.34 or later and on
OpenShift, for CloudNativePG 1.30 or later.

> [!IMPORTANT]
> pgToolBox reports what it can read and says where each fact comes from; it
> does not prove that a backup restores. The evidence agent's strongest claim
> is that a backup is structurally usable, and a check that could not run is
> shown as not run, never as passed. It runs no SQL, reads no Secret from the
> console, and holds no role model of its own.

> [!NOTE]
> An independent, personal project. It is not affiliated with, endorsed by or
> sponsored by the CloudNativePG project, the PostgreSQL project, the pgAdmin
> project, or any company.

## Why use it?

- **Every fact has a source**: health, replication, storage forecasts and
  backups, each attributed to the operator, the Pod or the instance's own
  `/pg/status`.
- **No authority of its own**: every read is a SelfSubjectAccessReview and every
  write is sent with the signed-in person's token, so the audit log names who
  did what.
- **Backups checked against the repository**: the evidence agent reads Barman
  Cloud and pgBackRest repositories, read-only, and tells the console what is
  really there.
- **Small blast radius**: one console per team, Roles only in the namespaces it
  watches, no Secret verb.

## What is included

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

## Project status

Pre-1.0. The custom resources are `pgtoolbox.fyannk.dev/v1alpha1`, and a field
can change between minor releases; every such change is announced in
[CHANGELOG.md](CHANGELOG.md). Pin an exact version, or better a digest.

## Documentation

[fyannk.github.io/pgToolBox](https://fyannk.github.io/pgToolBox/): installation,
the console and its configuration, the operator and its kinds, the evidence
agent, the security model, what the console may do, and the generated reference
(CRD fields, diagnostics checks). The site is built from `docs/site`
(`make docs`). [ARCHITECTURE.md](ARCHITECTURE.md) explains how the code fits
together, package by package.

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) first; for
larger changes, open an issue before writing code. `make help` lists the
targets, `make doctor` checks the toolchain against `mise.toml`, and
`make check` runs what CI runs without Docker or kind.

## Security

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

The
[security model](https://fyannk.github.io/pgToolBox/docs/security/) page has the
full picture, and the [security scan results](https://fyannk.github.io/pgToolBox/security/)
page shows the latest run of every scanner.

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
