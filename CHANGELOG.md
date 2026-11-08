# Changelog

All notable changes to pgToolBox are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org/). Before 1.0.0, and while the custom
resources are `v1alpha1`, a field can change between minor releases; every such
change is announced here. Pin an exact image tag, or better a digest, and read
the notes before upgrading.

## [0.1.0] - Unreleased

The first public release: a console, an optional operator and an evidence agent
for [CloudNativePG](https://cloudnative-pg.io) 1.30 and later, on Kubernetes and
OpenShift.

### Added

- **The console**, installed once per team namespace by the `pgtools` chart or
  declared as a `PgToolBoxConsole`. It shows the CloudNativePG Clusters of the
  namespaces it watches: an overview with the sources and blind spots each verdict
  rests on, instances as the operator, the Pod and the instance's own `/pg/status`
  report them, replication and a failover rehearsal, storage with a forecast of
  when each volume fills, backups next to the repository evidence, diagnostics
  and triage, events, a revision history with structural diffs, an incident
  timeline, logs, metrics (built-in or from Prometheus), the declarative objects,
  the topology, and two Clusters side by side.
- **Authorization by the API server.** People sign in with OIDC, OpenShift OAuth
  or a pasted API-server token; every read is checked with a
  SelfSubjectAccessReview sent with the person's own token. The console stores
  no level, role or group of its own.
- **Four operations** (backup, reload, restart, promote), off unless enabled,
  each sent to the API server with the signed-in person's token, plus opt-in
  restore drills that restore the latest backup into a throwaway Cluster, check
  it and delete it.
- **Access requests** (`PgAccessRequest`): a person asks for a role in the
  console, someone who may grant it approves, and the grant is a RoleBinding the
  operator removes when the request expires or is revoked.
- **An MCP endpoint** (`/mcp`, off by default, read-only unless its write tools
  are enabled) that serves the same facts as the screens to AI agents, as the
  token's owner.
- **A daily digest**, alerting rules and Grafana dashboards, the insight pack
  (PostgreSQL metrics from the instances' exporter), and OpenTelemetry tracing.
- **The operator** (optional), reconciling `PgAdmin` (one pgAdmin per team,
  registered against many Clusters, with libpq credential files written by a
  sidecar), `EvidenceAgent`, `PgToolBoxConsole` and `PgAccessRequest`, and honouring
  `PgAdminAccess` grants across namespaces. It never writes to a CloudNativePG
  `Cluster` and puts no secret material in status, events or logs.
- **The evidence agent**, a read-only reader of backup repositories: the Barman
  Cloud layout on S3, Azure Blob Storage and Google Cloud Storage, and the
  pgBackRest layout (repository format 5) for Clusters backed up through the
  Dalibo or Opera CNPG-I plugin. It reports what the repository actually holds,
  with conservative roll-ups, so the console can compare it with what
  CloudNativePG claims. It is the only component that holds object-store
  credentials.
- **Install paths**: the `pgtoolbox` chart (the operator and its CRDs), the
  `pgtools` chart (a team's console, evidence agents and pgAdmin, without the
  operator), kustomize, and an OLM bundle and catalog. Images for `linux/amd64`
  and `linux/arm64`: `ghcr.io/fyannk/pgtoolbox-operator`,
  `ghcr.io/fyannk/pgtoolbox-console` and `ghcr.io/fyannk/pgtoolbox-evidence-agent`.
- The custom resources are `pgtoolbox.fyannk.dev/v1alpha1`.

[0.1.0]: https://github.com/fyannk/pgToolBox/releases/tag/v0.1.0
