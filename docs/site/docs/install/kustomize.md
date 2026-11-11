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

The manager's args carry `--operator-image`, `--default-console-image` and
`--default-evidence-agent-image`, as the chart and the OLM bundle do, so a
`PgToolBoxConsole` or `EvidenceAgent` without `spec.image` runs the images of
this release instead of failing with `ImageRequired`. In the tree they are tags;
`install-operator.yaml` on the release page has them by digest. If you apply the
base after changing the operator image, change the three flags with it
(`make validate-packaging` checks them against the chart).

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

### The admission policy

`config/default` does **not** apply the operator chart's admission policy (the
`ValidatingAdmissionPolicy` behind `operator.admissionPolicy.enabled`, on by default in
the chart). That policy is the only check that whoever edits a `PgToolBoxConsole`, a
`PgAdmin` or an `EvidenceAgent` may `get` the Secrets it references, that whoever makes
a `PgAdmin` offer Cluster credentials may `get` the namespace's Secrets, and that only
the allowed users set `spec.image` or `spec.workloadIdentity` (see
[security](../security.md#secret-references-need-get)). Opt in with the component
`config/components/admission`, from an overlay:

```yaml
# kustomization.yaml of your overlay
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../../config/default
components:
- ../../config/components/admission
```

The component is the chart's policy rendered with the chart's defaults (`make
manifests` regenerates it, `make validate-packaging` compares it with the chart):
the allow-lists of `spec.image` and `spec.workloadIdentity` hold the group
`system:masters` only. To list your own users and groups, patch the variables
`allowedUsers`, `allowedGroups`, `identityUsers` and `identityGroups` of the
`ValidatingAdmissionPolicy` `pgtoolbox-image-override` in your overlay (each is a
CEL list literal, for example `'["platform-team"]'`), or render the chart with your
values instead:

```bash
helm template pgtoolbox charts/pgtoolbox --set operator.enabled=true \
  --show-only templates/operator/admissionpolicy.yaml | kubectl apply -f -
```

Without it, grant `create`/`update` on those kinds only to people trusted with every
Secret of their namespace.
