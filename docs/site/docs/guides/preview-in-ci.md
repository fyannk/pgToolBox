---
sidebar_position: 6
title: Previewing a Cluster change in CI
---

# Previewing a Cluster change in CI

`pgtoolbox preview` reads two versions of your CloudNativePG Cluster manifests,
the one in force and the one proposed, and says what applying the second sets in
motion: a reload, a restart, a rolling update, a switchover, a shutdown, or a
parameter the operator refuses. It is the same computation as the console's
[change preview](../console/timeline-and-digest.md#change-preview)
(`pkg/cnpgstatus/preview`), worded the same way. It works offline and never
contacts a cluster, so a pull request can be gated on it before anything is
merged or synced.

## Build it

The release ships images and charts, not standalone binaries, so build the
command from a checkout of the repository (`go install …@version` does not work:
the root module has a `replace` for its nested evidence module):

```sh
make build-pgtoolbox        # bin/pgtoolbox
```

## Run it

```sh
pgtoolbox preview [--output text|json] [--cnpg-version 1.30] [--postgres-major N] OLD.yaml NEW.yaml
```

- `OLD.yaml` and `NEW.yaml` are files, or `-` for one of them read from stdin.
  Each may hold several YAML documents. Clusters (`apiVersion:
  postgresql.cnpg.io/v1`, `kind: Cluster`) are matched by namespace and name; other
  documents are ignored and counted. The namespace is compared as written: both
  files must spell it the same way. A Cluster without `metadata.namespace` and
  the same Cluster with `namespace: default` do not match, and are reported as
  one removed and one added Cluster.
- A Cluster only in `NEW` is **added** (no running instance is touched). A Cluster
  only in `OLD` is **removed**: deleting it deletes its instances. A renamed Cluster
  is one of each.
- `--cnpg-version` is the version of the CloudNativePG operator that will apply the
  change. Without it, or with a version the tables do not cover, the preview is
  marked **unverified**.
- `--postgres-major` gives the PostgreSQL major when it cannot be read from the
  `imageName` tag (an image catalog, or the operator's default image).
  Parameters whose context differs between majors are then **unknown**.

Flags come before the two files.

A manifest holds the spec only, so the order of the update is given by role
(replicas, then the primary), not by instance name.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Nothing disruptive: at most a configuration reload or a start. |
| `1` | Disruptive: at least one Cluster restarts, rolls out, shuts down, runs a major upgrade, has fewer instances (`spec.instances` decreases: replicas are removed and fewer remain to fail over to) or is removed; or carries a parameter the operator refuses; or has a change whose effect is **unknown**. An unknown effect counts as disruptive because a restart cannot be ruled out. |
| `2` | Invalid input: a bad flag, an unreadable file, invalid YAML, a document that is not a Kubernetes object, a Cluster without a name or listed twice, or no Cluster in either file. Nothing is printed on stdout. |

The text report goes to stdout. Errors go to stderr.

## The JSON report

`--output json` prints one document:

```json
{
  "cnpgVersion": "1.30",
  "clusters": [
    {
      "namespace": "team-a",
      "name": "orders",
      "change": "changed",
      "disruptive": true,
      "preview": {
        "effect": "restart",
        "parameters": [
          { "name": "max_connections", "before": "100", "after": "200", "effect": "restart", "context": "postmaster" }
        ],
        "fields": [{ "path": "spec.primaryUpdateMethod", "effect": "none", "reason": "operatorOnly" }],
        "strategy": "unsupervised",
        "method": "switchover",
        "primary": "switchover",
        "steps": [
          { "role": "replicas", "instances": [], "action": "recreate" },
          { "role": "primary", "instances": [], "action": "switchover" }
        ],
        "postgresMajor": 17,
        "verified": true
      }
    }
  ],
  "ignored": 2,
  "disruptive": true
}
```

`change` is `changed`, `unchanged`, `added` or `removed`. A changed Cluster whose
`spec.instances` decreases also has `"fewerInstances": {"before": 3, "after": 1}`
(the preview itself classifies a scale as touching no running instance). `preview` is present only
for a `changed` Cluster, and it is the console's change preview object: `effect`
is `none`, `reload`, `start`, `restart`, `rollout`, `shutdown`, `majorUpgrade` or
`unknown` (with `atLeast`, the worst effect that was classified). A refused
parameter has `"effect": "refused"`.

To fail only on refused parameters and let a restart through with a review:

```sh
pgtoolbox preview --output json --cnpg-version 1.30 old.yaml new.yaml > preview.json || true
jq -e '[.clusters[].preview.parameters[]? | select(.effect == "refused")] | length == 0' preview.json
```

## In a pipeline

Compare the manifests of the merge base with the pull request's:

```yaml
# .github/workflows/cluster-preview.yml
on:
  pull_request:
    paths: ["clusters/**"]
permissions:
  contents: read
jobs:
  preview:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4   # pin to a commit SHA
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/checkout@v4   # pin to a commit SHA
        with:
          repository: fyannk/pgToolBox
          ref: main                    # placeholder: pin a commit SHA (see below)
          path: .pgtoolbox
          persist-credentials: false
      - uses: actions/setup-go@v5   # pin to a commit SHA
        with:
          go-version-file: .pgtoolbox/go.mod
      - run: cd .pgtoolbox && CGO_ENABLED=0 go build -o "${RUNNER_TEMP}/pgtoolbox" ./cmd/pgtoolbox
      - name: Preview the Cluster changes
        env:
          BASE: ${{ github.event.pull_request.base.sha }}
        run: |
          git show "${BASE}:clusters/production.yaml" > old.yaml
          set +e
          "${RUNNER_TEMP}/pgtoolbox" preview --cnpg-version 1.30 old.yaml clusters/production.yaml
          code=$?
          set -e
          case "${code}" in
            0) echo "No disruptive change." ;;
            1) echo "::warning::This change restarts, switches over, is refused or cannot be classified: review it before merging."; exit 1 ;;
            *) exit "${code}" ;;
          esac
```

`ref: main` is a placeholder. No release carries the command yet, so pin the
commit SHA you reviewed, and pin a release tag once one ships `pgtoolbox
preview`. Tracking `main` means the rules can change under your gate.

For a GitOps repository rendered by Kustomize or Helm, preview the rendered
output: `kustomize build overlays/prod | pgtoolbox preview old.yaml -`.

## What it does not know

It predicts from CloudNativePG's rules, never from the instances. Once the change
is applied, the instance's own `pending_restart` (shown by the console) is the
fact. It reads only the Cluster objects: a change to a referenced ConfigMap,
Secret or ImageCatalog is not seen.
