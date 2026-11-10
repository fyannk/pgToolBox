---
sidebar_position: 4
title: OLM
---

# OLM (OpenShift and operator-lifecycle-manager clusters)

The OLM bundle installs **the operator only**. The console is installed from the
[chart](chart.md) or [kustomize](kustomize.md), per team namespace.

| | |
|---|---|
| Package | `pgtoolbox` |
| Channel | `alpha` (the default and only channel) |
| Install modes | `AllNamespaces` only. `OwnNamespace`, `SingleNamespace` and `MultiNamespace` are not supported: the bundle does not map OLM's `olm.targetNamespaces` to the operator's `WATCH_NAMESPACES`, so the operator would watch every namespace whatever the OperatorGroup says. To scope it to some namespaces, install the [chart](chart.md) with `operator.config.watchNamespaces` |
| Minimum Kubernetes | 1.34 |
| Owned CRDs | `pgadmins`, `pgadminaccesses`, `evidenceagents`, `pgtoolboxconsoles` and `pgaccessrequests` (all `.pgtoolbox.fyannk.dev`, `v1alpha1`) |
| Suggested namespace | `pgtoolbox`, labelled Pod Security `restricted` (enforce, audit, warn) |
| Also ships | the `pgtoolbox-console-viewer`, `pgtoolbox-console-operator`, `pgtoolbox-console-approver` and metrics-reader ClusterRoles, a PodDisruptionBudget |

The bundle image is `ghcr.io/fyannk/pgtoolbox-bundle:<version>` and
the file-based catalog image `ghcr.io/fyannk/pgtoolbox-catalog:<version>`.
Both are signed like the other images
([verify](../release.md#verify)). Add the catalog as a `CatalogSource`, then
subscribe to package `pgtoolbox`.

The CSV lists every image the operator can run in `relatedImages`, so
`oc mirror` and `oc adm catalog mirror` mirror them for a disconnected cluster.
Check the list against the [images the operator uses](../operator/images.md).

## Who can open pgAdmin

OLM renders, for each CRD it owns, roles aggregated into the built-in ones:
`get`/`list`/`watch` into `view`, the writes into `edit`, `*` into `admin`. So
every `view` user of a namespace may `get` its `PgAdmin` objects. Opening a
pgAdmin is a different verb, `connect pgadmins/<name>`, which the OpenShift
oauth-proxy in front of pgAdmin checks and which neither `view` nor `edit`
carries. It is granted by `pgtoolbox-console-operator` (bind it per namespace
to the DBAs) and by `admin` (through OLM's `*` role and
`pgtoolbox-aggregate-to-admin`). See
[who may open pgAdmin](../operator/pgadmin.md#who-may-open-it).

`make bundle-validate` runs `operator-sdk bundle validate` with the operator
framework suite on every pull request; the OLM scorecard is not run yet.
