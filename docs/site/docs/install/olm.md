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
| Install modes | `OwnNamespace` and `AllNamespaces`; `SingleNamespace` and `MultiNamespace` are not supported |
| Minimum Kubernetes | 1.28 |
| Owned CRDs | `pgadmins`, `pgadminaccesses`, `evidenceagents`, `pgtoolboxconsoles` and `pgaccessrequests` (all `.pgtoolbox.fyannk.dev`, `v1alpha1`) |
| Suggested namespace | `pgtoolbox`, labelled Pod Security `restricted` (enforce, audit, warn) |
| Also ships | the `pgtoolbox-console-viewer`, `pgtoolbox-console-operator` and metrics-reader ClusterRoles, a PodDisruptionBudget |

The bundle image is `ghcr.io/fyannk/pgtoolbox-bundle:<version>` and
the file-based catalog image `ghcr.io/fyannk/pgtoolbox-catalog:<version>`.
Both are signed like the other images
([verify](../release.md#verify)). Add the catalog as a `CatalogSource`, then
subscribe to package `pgtoolbox`.

The CSV lists every image the operator can run in `relatedImages`, so
`oc mirror` and `oc adm catalog mirror` mirror them for a disconnected cluster.
Check the list against the [images the operator uses](../operator/images.md).

`make bundle-validate` runs `operator-sdk bundle validate` with the operator
framework suite on every pull request; the OLM scorecard is not run yet.
