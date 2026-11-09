---
sidebar_position: 5
title: Clusters backed up with pgBackRest
---

# Clusters backed up with pgBackRest

A Cluster that archives and backs up through a pgBackRest CNPG-I plugin has no
`Cluster.status.firstRecoverabilityPoint` and no `lastSuccessfulBackup`: neither
plugin writes them. For such a Cluster the [evidence agent](../evidence-agent.md)
is the **only** source of recoverability. It reads the pgBackRest repository
itself and tells the console which backups exist, whether their chains and WAL
are complete, and how far back and forward a restore can reach. This page says
what is supported, how to wire it, and where it stops.

## Supported

| | Dalibo | Opera |
|---|---|---|
| Plugin name | `pgbackrest.dalibo.com` | `pgbackrest.cnpg.opera.com` |
| Version tested | v0.0.3 (alpha) | v0.9.0 (alpha) |
| Object the Cluster names | a `Stanza` (`pgbackrest.dalibo.com/v1`), through `parameters.stanzaRef` | an `Archive` (`pgbackrest.cnpg.opera.com/v1`), through `parameters.pgbackrestObjectName` |
| Repositories read | S3, Azure Blob | S3 |
| Stanza | `spec.stanzaConfiguration.name` | `parameters.stanza`, else the Cluster's name; `configuration.stanza` for the WAL when it differs |
| Repository CA | none (no field; the plugin's `verifyTLS` is a bool, off by default) | `endpointCA` |

- Both plugins are pre-1.0. The agent reads only what they write in their own
  objects and in the repository, and refuses by name what it cannot account for
  ([Limits](#limits)). A newer plugin version is supported once its example
  manifests have been run through the decode test of `internal/backupresolve`.
- **Repository format 5** (pgBackRest's default) is read in full, encrypted
  repositories (`aes-256-cbc`) included. The repository's cipher pass is mounted
  as a file like the credentials.
- **One repository per agent**, `repo1` unless `pgbackrest.repository` says
  another. Dalibo numbers a Stanza's S3 repositories first, then its Azure ones;
  Opera numbers the Archive's repositories in order.
- **CloudNativePG 1.30 and later**, as everywhere else in pgToolBox.
- The agent never runs the `pgbackrest` binary and never reads backup data files:
  it checks the info files and manifests (checksums, `.copy` agreement, chains),
  WAL continuity per timeline, and the retention against the plugin's own
  options. "Structurally usable" is the strongest claim it makes.

Verified end to end on kind (`make test-e2e-pgbackrest`): CloudNativePG 1.30,
each plugin at the tag above with its images pinned by digest, MinIO over TLS,
a full and a differential backup, switched WAL, the operator's agent and the
`pgtools` chart's agent, the console's Backups screen, a restore drill, and the
broken cases below. The sidecar images of those two tags ship pgBackRest 2.58.0;
the reader's fixtures are 2.60.0's, and the agent reads both.

## Wiring

### With the operator

An `EvidenceAgent` resolves the repository from the Cluster and the plugin
object, in the namespace of the Cluster. Every Secret the plugin object names,
and the CA, must be listed in `allowedSecrets`:

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: EvidenceAgent
metadata:
  name: ledger
spec:
  clusterRef: {name: ledger}
  allowedSecrets: [ledger-s3, ledger-ca]       # the credentials and the CA
  pgbackrest:
    repository: 1                              # repo<n>; default 1
    endpointCA: {name: ledger-ca, key: ca.crt} # over the plugin object's CA
```

`status.pgbackrest` records the plugin, the object (`Stanza` or `Archive`), the
stanza and the repository number it resolved. See
[EvidenceAgent](../operator/evidenceagent.md#pgbackrest) for the refusals.

### With the `pgtools` chart (no operator)

The agent resolves the same object itself and reads only the Secrets the entry
lists:

```yaml
evidence:
  agents:
    - cluster: ledger
      stanza: store            # Dalibo: the Stanza the Cluster names
      # archive: store         # Opera: the Archive the Cluster names
      secrets: [ledger-s3, ledger-ca]
      pgbackrest:
        repository: 1
        endpointCA: {name: ledger-ca, key: ca.crt}
```

`stanza` and `archive` are exclusive with each other and with `objectStore`. The
agent's Role names the object in `resourceNames`, so it can read that one
`Stanza` or `Archive` and no other.

**A namespace admin installs this chart**, and Kubernetes lets an admin grant
only what the admin holds. `admin` does not aggregate the CloudNativePG groups:
bind a Role that grants `postgresql.cnpg.io`, `barmancloud.cnpg.io`,
`pgbackrest.dalibo.com` and `pgbackrest.cnpg.opera.com` to the installer first
(the console's Role reads the Stanzas and Archives of both plugins, whichever
one the Cluster uses), or the install is refused with "attempting to grant RBAC
permissions not currently held".

### The contract

What the two install paths hand the agent, for people who render it themselves:

| Setting | Operator (`EvidenceAgent`) | `pgtools` | Agent environment |
|---|---|---|---|
| The backup stanza | resolved | resolved | `PGBACKREST_STANZA` |
| The WAL stanza, when it differs | resolved | resolved | `PGBACKREST_ARCHIVE_STANZA` |
| The repository number | `spec.pgbackrest.repository` | `pgbackrest.repository` | `PGBACKREST_REPOSITORY` (reported; the destination already points at that repository's path), `AGENT_PGBACKREST_REPOSITORY` (self-resolving) |
| The repository's CA | `spec.pgbackrest.endpointCA` | `pgbackrest.endpointCA` | `ENDPOINT_CA_FILE` (a mounted file), `AGENT_PGBACKREST_ENDPOINT_CA_SECRET` and `AGENT_PGBACKREST_ENDPOINT_CA_KEY` (self-resolving; the Secret must be in `AGENT_ALLOWED_SECRETS`) |
| The cipher pass | resolved | resolved | `PGBACKREST_CIPHER_PASS_FILE` |
| The plugin's retention | resolved | resolved | `PGBACKREST_RETENTION_FULL`, `_FULL_TYPE`, `_DIFF`, `_ARCHIVE`, `_ARCHIVE_TYPE` |
| Credentials | Secret keys as files | read through the API from the listed Secrets | `S3_*_FILE`, as for barman-cloud |

`PGBACKREST_STANZAS` (plural) is the viewer's, not the agent's.

## TLS

TLS to the repository is **always verified**, whatever the plugin object says:
Dalibo's `verifyTLS: false` and Opera's `disableVerifyTLS: true` are not
followed. The agent holds read credentials to every backup of the Cluster, and a
party on the path must not be able to feed it a fake catalog that reports the
Cluster recoverable.

- The trusted roots are the system roots plus one CA: `endpointCA` of the
  agent (above), which wins over Opera's `endpointCA`. Dalibo's plugin has no
  such field, so an S3 endpoint with a private CA needs the agent's.
- A plain `http` endpoint is refused unless `allowInsecureEndpoint` is set.
- `keyType: web-id` or `auto` (Opera) needs `workloadIdentity`.
- A verification failure is **reported as one**. The console shows the evidence
  as stale (the last good answer is kept, marked), shows no recovery window from a
  scan that failed, and the agent's log names the cause
  (`x509: certificate signed by unknown authority`). The Backups screen never
  turns that into a green claim.

## What the console shows

For such a Cluster the Backups screen shows the plugin as the backup method, the
stanza and the repository, the **recovery window** from the oldest usable backup's
stop to the receipt of the last contiguous WAL segment, each backup as full,
differential or incremental with its chain, the Backup objects joined to the
repository's labels (Dalibo's `status.backupName`, Opera's `status.backupId`),
the stanza against the plugin's retention, and the findings. A confirmed hole in
the WAL is critical, ends the window before it, and names the missing range. A
gap only one scan has seen is a candidate: a warning, drawn as unconfirmed rather
than missing, until the next complete scan confirms it or it fills.

Right after a failover or a switchover, PostgreSQL archives the new timeline's
`.history` file at once, but the new timeline's first segment only once it fills
or `archive_timeout` passes (five minutes on CloudNativePG). Until it arrives,
the backups of the previous timeline stay usable, the window ends just before
the switch, and the stanza is a warning ("a timeline switch is not archived
yet"), not a loss. A backup that stopped inside the switch segment waits for it.
Once the new timeline has WAL, a missing segment before the switch, or the
switch segment missing while later ones exist, is reported as lost again.

## Restore drills

[Restore drills](../console/restore-drills.md#pgbackrest-clusters) recover the
newest backup of the agent's window into a throwaway Cluster through the same
plugin, with only the parameters that say where to read, and no archiver. Two
things to know before turning them on:

- a drill runs in the **source's namespace** only, because both plugins read their
  `Stanza` or `Archive` in the recovering Cluster's namespace;
- a drill **reuses the source's credentials**: the plugin object names them, and
  the drill Cluster has no other. They usually can write to the repository. What
  keeps a drill from writing is that it has no archiver and no backup
  configuration; for the same reason a read-only key in the plugin object is a
  good idea.

## Limits

- **Repository format 6** (SHA-256, key rotation; opt-in in pgBackRest 2.60.0) is
  detected and refused: `RepositoryFormatUnsupported`. The agent does not guess.
- **Drills run in the source namespace only** (`PgBackRestOtherNamespace`), and
  only for a Cluster whose agent reports a recovery window (`NoRecoveryWindow`).
  The Stanza or Archive the agent resolved must be the one the Cluster names
  (`StanzaObjectMismatch`), and Opera's stanza the one the agent reads
  (`StanzaMismatch`).
- **Drills reuse the source's credentials** (above).
- **A drill is signed by the source's server CA** with a `pgtools` console (and
  a `PgToolBoxConsole` with `spec.instanceCABundleRef`): the console verifies the
  drill's `/pg/status` against a bundle filled before the drill exists, which
  already holds the source's CA. A `PgToolBoxConsole` whose operator mirrors the CAs
  keeps the drill's own. The console reads no Secret either way; see
  [which CA signs a drill](../console/restore-drills.md#which-ca-signs-a-drill).
- **`uriStyle: host`** (virtual-hosted style, the plugins' default for AWS) is
  accepted but **not verified against AWS S3**: the end-to-end and provider
  journeys run MinIO and Azurite with `path`.
- **S3 and Azure only.** GCS and posix repositories are written by neither
  plugin.
- **One repository per agent.** The agent reads `repo1`, or the one
  `pgbackrest.repository` names; the others are not read.
- **Refused by name** (`UnsupportedObjectStore`): no S3 or Azure repository at the
  chosen number; `keyType: web-id` without `workloadIdentity`; a `customEnvVar`,
  sidecar `env` key or additional argument that could move the repository, the
  stanza or the configuration (`PGBACKREST_REPO*`, `PGBACKREST_STANZA`,
  `PGBACKREST_CONFIG*`, the deprecated `PGBACKREST_BACKUP_HOST` family, `--repo*`,
  `--stanza*`, `--config*`, `--backup-host` and its family, their `--no-` and
  `--reset-` forms, and any abbreviation pgBackRest would expand to one of them).
- **A split stanza** (Opera: `configuration.stanza` differing from the backup
  stanza) is read, with the finding `StanzaSplit`: pgBackRest `restore` looks for
  the WAL under the backup stanza unless told otherwise.
- **Every pgBackRest producer is an agent**, and agents speak `evidence/v1alpha1`.
- **Both plugins in one cluster.** Their release manifests collide: both ship a
  Service `pgbackrest`, a Role `leader-election-role` and a
  ClusterRoleBinding `metrics-auth-rolebinding` in `cnpg-system`, bound to
  different ServiceAccounts, and applied together the second overwrites the
  first. Rename Opera's (kustomize patches in `hack/e2e-pgbackrest-stack.sh`; the
  Service name is also the server certificate's) or run one plugin per cluster.
- **Dalibo's manifest at v0.0.3** names its controller image 0.0.2; the release
  notes describe 0.0.3's behavior (stanza maintenance, which writes
  `Stanza.status.recoveryWindow`). The scenario sets the 0.0.3 controller by
  digest.
- The check in the console never says a restore will work. A drill does, for one
  backup, once.
