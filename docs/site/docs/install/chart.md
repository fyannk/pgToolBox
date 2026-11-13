---
sidebar_position: 2
title: Helm chart
---

# Helm chart

Two charts, published as OCI artifacts next to the images:

| Chart | Installs | Installed by |
|---|---|---|
| `oci://ghcr.io/fyannk/charts/pgtools` | a team's tools: the console (and optionally evidence agents and a pgAdmin), in the team's namespace | an admin of that namespace: no CRD, nothing cluster-scoped by default |
| `oci://ghcr.io/fyannk/charts/pgtoolbox` | the operator (`PgToolBoxConsole`, `PgAdmin`, `EvidenceAgent`), once per cluster | a cluster admin |

A team needs only `pgtools`. The operator adds what a chart cannot do: it follows
the cluster (a `PgToolBoxConsole` or `PgAdmin` kept converged, new Clusters picked up).

The commands below name the chart version being shipped (`make validate-packaging` fails when
they differ from `Chart.yaml`; replace it with the version you install). The chart version equals the product version (a `vX.Y.Z` tag publishes chart
`X.Y.Z`). A release candidate is a prerelease chart version: name it in full
with `--version` (for example `0.1.0-rc.2`), since Helm skips prereleases otherwise. [Verify the chart's signature](../release.md#verify) before the first
install. Both charts carry the same version. `values.yaml` is the annotated
reference and `values.schema.json` is strict: an unknown key under `console`
(pgtools) or anywhere in pgtoolbox is an error.

## A team console

Install each console as its own release **in the team's namespace**:

```bash
helm install console oci://ghcr.io/fyannk/charts/pgtools \
  --version 0.1.0 --namespace team-a \
  --set console.auth.provider=token
```

By default it watches the release namespace. To watch several namespaces the team
owns, list them; the chart creates a Role and RoleBinding in each:

```bash
  --set 'console.watchNamespaces={team-a,team-a-staging}'
```

The chart ships no CRD and, by default, nothing cluster-scoped. Then create the
[CA bundle](../console/configuration.md#console-installed-by-the-pgtools-chart)
(and keep it current: CloudNativePG renews its CAs every 90 days) and
[bind users](../console/authorization.md#the-two-clusterroles). Until the CA
bundle ConfigMap exists, every instance reads "not verifiable"; the console picks
it up when it appears, without a restart, so it may be created before or after
the install.

Key values (the full list is in the chart's `values.yaml`):

| Key | Default | Meaning |
|---|---|---|
| `console.watchNamespaces` | `[]` (the release namespace) | At most sixteen; never "all". |
| `console.clusterSelector` | `{}` | Label filter on top of each user's RBAC. |
| `console.replicas` | `2` | With or without persistent history ([replicas and history](../console/configuration.md#replicas-and-history)). |
| `console.history.persistent` | `false` | `true`: a StatefulSet with one claim of `console.history.size` (1Gi) per replica. Recommended wherever the namespace has a StorageClass. |
| `console.exposure.sticky.enabled` | `true` | Cookie affinity at the edge, so a person keeps seeing one replica's charts. |
| `console.auth.provider` | `token` | `token`, `oidc` or `openshift` ([sign-in](../console/authentication.md)). |
| `console.instanceStatus.caBundleConfigMap` | `pgtoolbox-console-instance-ca` | The ConfigMap you fill and keep current. |
| `console.exposure.type` | `none` | `ingress`, `route`, `httpRoute` ([exposure](../console/configuration.md#exposure)). |
| `console.networkPolicy.enabled` | `false` | [The admin's choice](../console/networkpolicy.md). |
| `console.clusterImageCatalogs.enabled` | `false` | Adds a read-only ClusterRole for `clusterimagecatalogs`. |
| `console.rbac.podLogs` | `true` | `false` removes `get pods/log`: no log views, no log-based checks. |
| `console.operations.enabled` | `true` | `false` makes the [write endpoints](../operations.md) refuse. |
| `console.image.digest`, `global.imageRegistry` | empty in git; the published chart carries the released digests | A digest wins over the tag. [The published chart is digest-pinned](../release.md#the-charts-digest-pinned-images-and-their-own-signatures); air-gapped installs prefix a mirror. |
| `console.logs.streamEnabled`, `console.logs.matchMaxAge` | `true`, `6h` | The diagnostics log matcher (`LOG_STREAM_ENABLED`, `LOG_MATCH_MAX_AGE`); forced off without `console.rbac.podLogs`. |
| `console.mcp.enabled` | `false` | The [MCP endpoint](../console/mcp.md). It adds a one-replica Deployment `<fullname>-mcp`, the only Pod that serves MCP, with its Service, Lease and Role; the exposure sends the MCP paths there. A crash of that Pod costs tens of seconds of MCP, the loss of its node about 5 minutes ([one Pod serves MCP](../console/mcp.md#one-pod-serves-mcp)). |

## The operator

Install it once per Kubernetes cluster:

```bash
helm install pgtoolbox oci://ghcr.io/fyannk/charts/pgtoolbox \
  --version 0.1.0 --namespace pgtoolbox --create-namespace
```

It renders the operator Deployment (two replicas, leader election), its generated
ClusterRole, the CRDs, and the two [user ClusterRoles](../console/authorization.md#the-two-clusterroles).
The CRDs carry `helm.sh/resource-policy: keep`: uninstalling the release does not
delete every `PgToolBoxConsole`, `PgAdmin` and `EvidenceAgent` in the cluster. Operator
values are explained on [the operator page](../operator/index.md); `image.digest`
and `operator.console.image.digest` pin the operator and the console it runs for a
`PgToolBoxConsole`. With a `PgToolBoxConsole`'s MCP on, every console replica is a
candidate and the operator labels the one serving MCP: a hand-over takes about
2 seconds when that replica stops, about 17 when it crashes, plus the
operator's reaction ([one Pod serves MCP](../console/mcp.md#one-pod-serves-mcp)).

## Upgrade and uninstall

- `helm upgrade` updates the CRDs rendered by the chart, because they are chart
  templates, not Helm `crds/` files.
- The console's session key is generated once and kept across upgrades (so logins
  survive a roll). Under a GitOps tool that renders client-side, `lookup` has no
  cluster to ask: bring your own with `console.auth.sessionKey.existingSecret`.
- Uninstalling the console removes everything it created; the Role, RoleBinding,
  ClusterRole and ClusterRoleBinding names include the namespace and release, so
  two consoles never share them.
- Uninstalling the operator keeps the CRDs, hence the `PgAdmin` and
  `EvidenceAgent` objects. Delete them yourself when you mean it. A `PgAdmin`'s
  settings volume is retained by default
  ([`retainPolicy`](../operator/pgadmin.md#storage)).
- Uninstalling the operator also leaves the objects it created itself, which Helm
  never knew: the cluster-scoped `pgtoolbox-evidence-agent` ClusterRole and
  ClusterRoleBinding (created for the first `EvidenceAgent`, owned by no object), and
  the children of every remaining `PgToolBoxConsole`, `PgAdmin` and `EvidenceAgent`
  (their owners are kept with the CRDs). To remove everything:

  ```bash
  kubectl delete pgtoolboxconsoles,pgadmins,pgadminaccesses,evidenceagents,pgaccessrequests -A --all
  helm uninstall pgtoolbox -n pgtoolbox
  kubectl delete clusterrole,clusterrolebinding pgtoolbox-evidence-agent
  kubectl get crd -o name | grep pgtoolbox.fyannk.dev | xargs kubectl delete
  ```

  The operator sets no finalizers, so the order matters only for tidiness: the
  children of a deleted CR go through Kubernetes' garbage collector either way.

## Air-gapped installs

The runtime fetches nothing. Mirror the images listed in the
[release page](../release.md#what-a-release-contains), set `global.imageRegistry`
(pgtools) and the operator's image values, and prefer digests
(`console.image.digest`; `operator.console.image.digest`,
`operator.evidenceAgent.image`, `operator.pgAdmin.image`).
On OpenShift set `operator.oauthProxyImage` to the release payload's own
oauth-proxy so a mirrored registry serves it.
