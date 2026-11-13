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
| Admission | the operator chart's rules, as a validating webhook the operator serves (OLM issues its certificate): [admission rules](#admission-rules) |

The bundle image is `ghcr.io/fyannk/pgtoolbox-bundle:<version>` and
the file-based catalog image `ghcr.io/fyannk/pgtoolbox-catalog:<version>`.
Both are signed like the other images
([verify](../release.md#verify)). Add the catalog as a `CatalogSource`, then
subscribe to package `pgtoolbox`.

The CSV lists every image the operator can run in `relatedImages`, so
`oc mirror` and `oc adm catalog mirror` mirror them for a disconnected cluster.
Check the list against the [images the operator uses](../operator/images.md).

## Admission rules

OLM installs enforce the same rules as the operator chart's
`ValidatingAdmissionPolicies`, through a validating webhook the operator serves
(`--admission-webhook`, set by the CSV). A bundle cannot ship a
`ValidatingAdmissionPolicy`, so the CSV declares two webhooks instead, and OLM
issues, mounts and rotates their serving certificate:

- `vresources.pgtoolbox.fyannk.dev`, on create and update of `PgToolBoxConsoles`,
  `PgAdmins` and `EvidenceAgents`: only the listed users and groups may set
  `spec.image` (on a `PgAdmin` or an `EvidenceAgent`) or `spec.workloadIdentity`;
  creating one or changing its spec takes `get` on every Secret it references
  (on every Secret of the namespace past two), a `PgAdmin` offering Cluster
  credentials takes `get` on the namespace's Secrets, a projected Prometheus token
  or exposure annotations take the right to mint the token or to create the
  exposure object (see [security](../security.md#secret-references-need-get));
- `vclusterdelete.pgtoolbox.fyannk.dev`, on delete of CNPG `Clusters`: the
  [restore-drill delete guard](../console/restore-drills.md#the-delete-guard).

Both ask the API server, with a `SubjectAccessReview`, what the requesting user
may do, as the policies' authorizer checks do, with a 5-second timeout. The
resources webhook fails closed (`failurePolicy: Fail`); the delete guard fails
open (`failurePolicy: Ignore`, see below). The allow-lists come from the
operator's configuration (`ADMISSION_IMAGE_OVERRIDE_USERS`, `_GROUPS`,
`ADMISSION_WORKLOAD_IDENTITY_USERS`, `_GROUPS`; default: the group
`system:masters`), set through the `Subscription`'s `spec.config.env` or the
[operator ConfigMap](../operator/index.md#configuration).

:::warning While the operator is down
With no operator replica ready, creating or updating the three pgToolBox kinds is
refused until it is back. The delete guard fails open instead: OLM's webhook
definitions take no `matchConditions`, so it is called for **every** `Cluster`
delete in the namespaces of the install, and failing closed would block those of
people too. While no replica answers, a ServiceAccount holding the drills role can
delete a `Cluster` that is not a drill's target. The bundle runs two replicas and a
PodDisruptionBudget, and every replica serves the webhook.
:::

Do not also apply the chart's policies (`config/components/admission`,
`config/admission`) on an OLM install: one install runs the policies or the
webhook, never both.

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
