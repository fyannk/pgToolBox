---
sidebar_position: 1
title: Choose an install
---

# Install

pgToolBox has two independent halves. Install the one you need; they do not
depend on each other at install time.

| Half | Install it when | Scope |
|---|---|---|
| **Console** | A team wants to see and operate its CloudNativePG clusters. The `pgtools` chart can also run the team's evidence agents and a pgAdmin. | One per team namespace. Roles in the watched namespaces, nothing cluster-scoped. |
| **Operator** | You want pgAdmin, evidence agents, consoles or access requests declared as custom resources (`PgAdmin`, `EvidenceAgent`, `PgToolBoxConsole`, `PgAccessRequest`). | One per Kubernetes cluster. Owns the CRDs and a cluster-wide ClusterRole. |

The console reads `PgAdmin` and `EvidenceAgent` objects when they exist, to link
to pgAdmin and to show repository evidence. Without the operator, the `pgtools`
chart gives it the same two things from its values: see
[team mode](../evidence-agent.md#team-mode-without-the-operator) and
[pgAdmin without the operator](pgadmin-without-operator.md).

Three ways to install, all rendering the same objects (`make validate-packaging`
compares the chart, the kustomize bases and the OLM bundle field by field):

| Way | Console | Operator | Page |
|---|---|---|---|
| Helm charts: `pgtools` (console) and `pgtoolbox` (operator) | yes | yes | [Helm chart](chart.md) |
| kustomize (`config/`) | yes, one namespace | yes, development layout | [kustomize](kustomize.md) |
| OLM bundle and catalog | no | yes | [OLM](olm.md) |

## Before you install

- **CloudNativePG** 1.30 or later on the cluster the console will watch. Without
  CloudNativePG CRDs the console has nothing to show.
- **The console needs a CA bundle** of the Clusters' server CAs, or every
  Cluster reads "not verifiable". The operator keeps it for a `PgToolBoxConsole`; with
  the chart alone you create it and keep it current across CloudNativePG's 90-day
  CA renewals ([how](../console/configuration.md#the-instance-ca-bundle)).
- **Sign-in needs a decision**: `token` works everywhere; SSO needs the API server
  to trust your identity provider ([details](../console/authentication.md)).
- **Behind an Ingress or a Gateway, name the controller.** A `PgToolBoxConsole`
  with `exposure.type: ingress` or `gateway` needs
  `exposure.networkPolicy.from` naming the controller's Pods (for the community
  ingress-nginx: `namespaceSelector` `kubernetes.io/metadata.name: ingress-nginx`
  and `podSelector` `app.kubernetes.io/name: ingress-nginx`, as in
  `config/samples/pgtoolboxconsole.yaml`). Without it the console ignores
  `X-Forwarded-For` and every user behind the controller shares one sign-in
  budget; with it, only those peers reach the console port and are believed
  ([trusted proxies](../operator/console.md#trusted-proxies-and-the-ingress-networkpolicy)).
  With the chart alone, set `console.trustedProxyCIDRs` (or
  `console.networkPolicy.enabled`) instead.
- **People need RoleBindings.** Nobody sees a Cluster until RBAC says so
  ([the two ClusterRoles](../console/authorization.md#the-two-clusterroles)).
- The console needs no cluster-scoped object: an admin of the team's namespace
  can install it, given read on the CloudNativePG kinds there (CloudNativePG does
  not add them to `admin`; see the chart README). It checks each reader's rights
  with that reader's own token.

## Verify what you install

Releases are signed and carry attestations. See
[Release and verification](../release.md) before you pin images in production.
