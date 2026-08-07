# Installation

## Prerequisites

- Kubernetes ≥ 1.28 or OpenShift ≥ 4.14.
- [CloudNativePG](https://cloudnative-pg.io) ≥ 1.30 — the `DatabaseRole`
  CRD must exist.
- Operator and proxy images built from this repository:

```bash
make docker-build IMG=<registry>/pgtoolbox:<tag>
make docker-build-proxy PROXY_IMG=<registry>/pgtoolbox-proxy:<tag>
```

Optional, for the evidence sidecar: the Barman Cloud Plugin's
`ObjectStore` API (`barmancloud.cnpg.io/v1`) served on the cluster and an
`objectstoreviewer` image.

## Install with Helm (recommended)

```bash
helm install pgtoolbox deploy/helm/pgtoolbox \
  --namespace pgtoolbox --create-namespace \
  --set image.repository=<registry>/pgtoolbox \
  --set image.tag=<tag> \
  --set proxyImage=<registry>/pgtoolbox-proxy:<tag>
```

Useful values:

| Value | Default | Meaning |
|---|---|---|
| `image.repository` / `image.tag` | `pgtoolbox:latest` | operator image |
| `proxyImage` | `""` | default pgtoolbox-proxy image for consoles |
| `defaultImages.pgConsole` | `""` | default pgconsole image |
| `defaultImages.pgAdmin` | `""` | default pgAdmin image |
| `defaultImages.objectStoreViewer` | `""` | default evidence sidecar image |
| `replicaCount` | `2` | manager replicas |
| `leaderElection` | `true` | controller-runtime leader election |

The chart installs the CRDs, ServiceAccount, ClusterRole and binding,
leader-election Role/RoleBinding, and the manager Deployment. The
`--operator-image` argument is rendered from `image`, so the pgAdmin
admin-sync init container copies the operator binary out of the deployed
image.

## Install from the OLM catalog

```bash
make bundle-build  BUNDLE_IMG=<registry>/pgtoolbox-bundle:v0.1.0
make catalog-build CATALOG_IMG=<registry>/pgtoolbox-catalog:v0.1.0
```

Push both images, create a `CatalogSource` for the catalog image, and a
`Subscription` for package `pgtoolbox`, channel `alpha`. OLM installs the
CSV (CRDs + Deployment) and binds the generated ClusterRole.

## Development install (kustomize)

```bash
make deploy    # kubectl apply -k config/default
```

This installs into the `pgtoolbox` namespace with image `pgtoolbox:latest`
and is intended for development.

## Verify

```bash
kubectl get pods -n pgtoolbox
kubectl get crd | grep pgtoolbox.fyannk.dev
```

Next: [Configuration](configuration.md).
