---
sidebar_position: 3
title: kustomize
---

# kustomize

The `config/` tree renders the same objects as the chart without Helm. Two bases:

| Base | Installs | Notes |
|---|---|---|
| `config/console` | one team console | A hand-written second rendering of the chart's console half, compared with it field by field by `make validate-packaging`. |
| `config/default` | the operator, its CRDs and RBAC, in the `pgtoolbox` namespace | A development layout. The comment in the file says production installs are expected to come from Helm or OLM. |

## The console

```bash
cd config/console
kustomize edit set namespace team-a
kubectl apply -k .
```

It watches the namespace it is applied to. Several namespaces, or several
consoles in one cluster, need Helm: the ClusterRole names in this base are fixed.

This base does not create what only you can supply:

- the session key, a Secret named `pgtoolbox-console-session` with key `session-key`
  (at least 32 bytes of random data, for example
  `kubectl -n team-a create secret generic pgtoolbox-console-session --from-literal=session-key="$(openssl rand -base64 32)"`);
  the Deployment mounts it as a file and does not start without it;
- the [CA bundle ConfigMap](../console/configuration.md#console-installed-by-the-pgtools-chart)
  `pgtoolbox-console-instance-ca` (optional in the manifest: without it every Cluster
  reads "not verifiable"), kept current across CloudNativePG's 90-day CA renewals;
- the sign-in configuration: `config/console/deployment.yaml` sets
  `AUTH_PROVIDER=token`. For SSO edit the environment variables
  ([the list](../console/authentication.md#environment-variables)) or use Helm;
- the RoleBindings that let people see Clusters.

The base does create the empty revocation ConfigMap `pgtoolbox-console-revocations`, and
the empty state ConfigMap `pgtoolbox-console-state` with the Role that lets the console
write it (`config/console/state.yaml`).

## The operator

```bash
kubectl apply -k config/default
```

This creates the `pgtoolbox` namespace, the CRDs under `config/crd`, the RBAC
under `config/rbac` (including the `pgtoolbox-console-viewer`, `pgtoolbox-console-operator` and `pgtoolbox-console-approver`
ClusterRoles), the manager Deployment and the
[restore-drill delete guard](../console/restore-drills.md#the-delete-guard)
(`config/admission`). Generated files (`config/crd/bases`,
`config/rbac/role.yaml`) come from `make manifests`; do not edit them.

:::danger `kubectl delete -k config/default` deletes every pgToolBox object
Unlike the chart, `config/default` does not keep the CRDs: deleting the kustomization
deletes the CRDs, hence every `PgToolBoxConsole`, `PgAdmin`, `PgAdminAccess`,
`EvidenceAgent` and `PgAccessRequest` in the cluster, and the namespace `pgtoolbox`.
To remove only the operator, delete its Deployment (`kubectl -n pgtoolbox delete
deployment pgtoolbox`), or build without the CRDs. Either
way the operator leaves the `pgtoolbox-evidence-agent` ClusterRole and
ClusterRoleBinding it created for the evidence agents; delete them by name when you
remove pgToolBox for good.
:::

`config/default` does **not** apply the operator chart's admission policy (the
`ValidatingAdmissionPolicy` behind `operator.admissionPolicy.enabled`, on by default in
the chart). That policy is the only check that whoever edits a `PgToolBoxConsole` or a
`PgAdmin` may `get` the Secrets it references, and that only the allowed users set
`spec.image` or `spec.workloadIdentity`. Render it from the chart and apply it, or grant
`create`/`update` on those kinds only to people trusted with every Secret of their
namespace (see [security](../security.md)):

```bash
helm template pgtoolbox charts/pgtoolbox --set operator.enabled=true \
  --show-only templates/operator/admissionpolicy.yaml | kubectl apply -f -
```
