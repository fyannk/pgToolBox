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
| Does not ship | the admission policies (OLM cannot install them): [apply them yourself](#admission-policies) |

The bundle image is `ghcr.io/fyannk/pgtoolbox-bundle:<version>` and
the file-based catalog image `ghcr.io/fyannk/pgtoolbox-catalog:<version>`.
Both are signed like the other images
([verify](../release.md#verify)). Add the catalog as a `CatalogSource`, then
subscribe to package `pgtoolbox`.

The CSV lists every image the operator can run in `relatedImages`, so
`oc mirror` and `oc adm catalog mirror` mirror them for a disconnected cluster.
Check the list against the [images the operator uses](../operator/images.md).

## Admission policies

:::warning The bundle carries no admission policy
OLM cannot install a `ValidatingAdmissionPolicy`, so an OLM install has **none** of
the checks the operator chart applies by default:

- only the listed users and groups may set `spec.image` on a `PgAdmin` or an
  `EvidenceAgent`, or `spec.workloadIdentity` on an `EvidenceAgent`;
- whoever creates a `PgToolBoxConsole`, a `PgAdmin` or an `EvidenceAgent`, or
  changes its spec, must be able to `get` every Secret it references;
- whoever creates or changes a `PgAdmin` that offers the credentials of Clusters of
  its namespace must be able to `get` the namespace's Secrets;
- a projected Prometheus token, and exposure or Service annotations, take the
  right to mint that token or to create the exposure object;
- the [restore-drill delete guard](../console/restore-drills.md#the-delete-guard).

Without them, grant `create`/`update` on `pgtoolboxconsoles`, `pgadmins` and
`evidenceagents` only to people you would trust with every Secret of their
namespace. (OLM's own `-edit` role, aggregated to `edit`, grants exactly that, so on
OLM every `edit` user of a namespace is such a person.)
:::

Apply them yourself, once per cluster, after the operator. Either from the chart
(set `operator.admissionPolicy.imageOverride` and `workloadIdentity` to your
allow-lists):

```bash
helm template pgtoolbox charts/pgtoolbox --set operator.enabled=true \
  --show-only templates/operator/admissionpolicy.yaml \
  --show-only templates/operator/restoredrill-guard.yaml \
  --set operator.restoreDrillGuard.enabled=true | kubectl apply -f -
```

or from the kustomize copies, rendered from the chart's defaults (allow-lists:
the group `system:masters`): `kubectl apply -f config/components/admission/admission_policy.yaml`
and `kubectl apply -k config/admission`. See [security](../security.md#secret-references-need-get).

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
