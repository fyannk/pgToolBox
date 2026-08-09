# Installation

## Prerequisites

- Kubernetes ≥ 1.28 or OpenShift ≥ 4.14.
- [CloudNativePG](https://cloudnative-pg.io) ≥ 1.30 — the `DatabaseRole`
  CRD must exist.
- A pgAdmin image. This is the one component with no released version, so
  nothing defaults it: set `defaultImages.pgAdmin` here or
  `spec.pgAdmin.image` on each console.

Optional, for the evidence sidecar: the Barman Cloud Plugin's
`ObjectStore` API (`barmancloud.cnpg.io/v1`) served on the cluster.

The operator, proxy, pgConsole and evidence images are published and are
what the chart and the OLM bundle default to. To run your own instead:

```bash
make docker-build IMG=<registry>/pgtoolbox:<tag>
make docker-build-proxy PROXY_IMG=<registry>/pgtoolbox-proxy:<tag>
```

## Install with Helm (recommended)

```bash
helm install pgtoolbox deploy/helm/pgtoolbox \
  --namespace pgtoolbox --create-namespace \
  --set defaultImages.pgAdmin=<registry>/pgadmin:<tag>
```

The operator and proxy default to this chart's own version, and pgConsole
and the evidence sidecar to the versions it was tested against. Override
any of them with `--set`; an empty `image.tag` means the chart's
`appVersion`, so an installed chart runs the operator it shipped with.

Useful values:

| Value | Default | Meaning |
|---|---|---|
| `image.repository` | `ghcr.io/fyannk/pgtoolbox` | operator image |
| `image.tag` | `""` → chart `appVersion` | operator version |
| `proxyImage` | `""` → `proxyImageRepository` at `image.tag` | proxy for consoles that name none |
| `defaultImages.pgConsole` | `ghcr.io/fyannk/pgconsole:0.3.0` | default pgconsole image |
| `defaultImages.pgAdmin` | `""` | default pgAdmin image — **has to be set** |
| `defaultImages.objectStoreViewer` | `ghcr.io/fyannk/pgobjectstoreviewer:0.1.1` | default evidence sidecar image |
| `replicaCount` | `2` | manager replicas |
| `leaderElection` | `true` | controller-runtime leader election |

The chart installs the CRDs, ServiceAccount, ClusterRole and binding,
leader-election Role/RoleBinding, and the manager Deployment. The
`--operator-image` argument is rendered from `image`, so the pgAdmin
admin-sync init container copies the operator binary out of the deployed
image.

## Install from the OLM catalog

A release publishes the bundle and catalog images, so a `CatalogSource`
pointing at the published catalog is enough:

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: pgtoolbox
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: ghcr.io/fyannk/pgtoolbox-catalog:0.1.0
  displayName: pgToolBox
```

Then a `Subscription` for package `pgtoolbox`, channel `alpha`. OLM
installs the CSV (CRDs + Deployment) and binds the generated ClusterRole.
The CSV carries `relatedImages`, so a disconnected mirror can resolve
everything the operator deploys — except pgAdmin, which has no release to
name; mirror your own and set `spec.pgAdmin.image`.

To build them yourself instead:

```bash
make bundle-build  BUNDLE_IMG=<registry>/pgtoolbox-bundle:0.1.0
make catalog-build CATALOG_IMG=<registry>/pgtoolbox-catalog:0.1.0
```

## Development install (kustomize)

```bash
make deploy    # kubectl apply -k config/default
```

This installs into the `pgtoolbox` namespace and is intended for
development; edit `config/default/kustomization.yaml` to point it at an
image you built.

## Verify

```bash
kubectl get pods -n pgtoolbox
kubectl get crd | grep pgtoolbox.fyannk.dev
```

Next: [Configuration](configuration.md).
