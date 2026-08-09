#!/usr/bin/env bash
# Copyright © contributors to the pgtoolbox project.
# SPDX-License-Identifier: Apache-2.0
#
# Provision a kind cluster with CloudNativePG and this operator, then run the
# e2e smoke test against it.
#
# The unit tests run against a fake client, which cannot fail the two ways a
# real cluster can: RBAC escalation prevention refusing to let the operator
# grant a Role rule it does not itself hold, and a component container
# rejecting the environment the operator rendered for it. Both are silent
# until something real is on the other end, so this test uses the published
# component images rather than stubs.
#
#   ./hack/e2e.sh              provision, test, tear down
#   KEEP_CLUSTER=1 ./hack/e2e.sh   leave the cluster up to inspect it
#   REUSE_CLUSTER=1 ./hack/e2e.sh  reuse an existing cluster, skip provisioning
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

# Pinned so a failure is the operator's, never a moving platform underneath.
CLUSTER_NAME="${CLUSTER_NAME:-pgtoolbox-e2e}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
CNPG_VERSION="${CNPG_VERSION:-1.30.0}"
CNPG_MANIFEST="${CNPG_MANIFEST:-https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.30/releases/cnpg-${CNPG_VERSION}.yaml}"

# The published component images. The point of the test is that the real
# binaries accept what the operator renders, so these are not substitutable —
# and every one of them is a pgtoolbox family image, including pgAdmin, which
# is our own repackaging rather than the upstream one.
PGCONSOLE_IMAGE="${PGCONSOLE_IMAGE:-ghcr.io/fyannk/pgconsole:0.2.0}"
PGADMIN_IMAGE="${PGADMIN_IMAGE:-ghcr.io/fyannk/pgadmin:latest}"
VIEWER_IMAGE="${VIEWER_IMAGE:-ghcr.io/fyannk/pgobjectstoreviewer:0.1.1}"

# The Barman Cloud Plugin serves the ObjectStore API the evidence composition
# resolves against. The operator discovers it once at startup, so it has to be
# installed before the operator, not after.
BARMAN_PLUGIN_MANIFEST="${BARMAN_PLUGIN_MANIFEST:-https://github.com/cloudnative-pg/plugin-barman-cloud/releases/latest/download/manifest.yaml}"

# The Barman plugin issues its mTLS certificates through cert-manager.
CERT_MANAGER_MANIFEST="${CERT_MANAGER_MANIFEST:-https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml}"

MANAGER_IMG="${MANAGER_IMG:-pgtoolbox:e2e}"
PROXY_IMG="${PROXY_IMG:-pgtoolbox-proxy:e2e}"

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

for tool in kind kubectl helm docker go; do
  command -v "${tool}" >/dev/null || { echo "missing required tool: ${tool}" >&2; exit 1; }
done

teardown() {
  if [[ "${KEEP_CLUSTER:-}" == "1" ]]; then
    log "leaving cluster ${CLUSTER_NAME} up (KEEP_CLUSTER=1)"
    return
  fi
  log "deleting cluster ${CLUSTER_NAME}"
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
}

if [[ "${REUSE_CLUSTER:-}" != "1" ]]; then
  trap teardown EXIT

  log "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE})"
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  # Generous: on a loaded machine the control plane can take well over
  # two minutes, and kind reports that as a hard creation failure.
  kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --wait 300s

  log "installing CloudNativePG ${CNPG_VERSION}"
  kubectl apply --server-side -f "${CNPG_MANIFEST}"
  kubectl -n cnpg-system wait --for=condition=Available deployment/cnpg-controller-manager --timeout=300s

  log "installing cert-manager (the Barman plugin issues its certificates through it)"
  kubectl apply --server-side -f "${CERT_MANAGER_MANIFEST}"
  for deployment in cert-manager cert-manager-webhook cert-manager-cainjector; do
    kubectl -n cert-manager rollout status "deployment/${deployment}" --timeout=300s
  done

  # Before the operator: BarmanObjectStoreAvailable is discovered once at
  # startup, and the evidence composition degrades to "API not served"
  # without it.
  log "installing the Barman Cloud Plugin (serves the ObjectStore API)"
  kubectl apply --server-side -f "${BARMAN_PLUGIN_MANIFEST}"
  kubectl -n cnpg-system rollout status deployment/barman-cloud --timeout=300s

  # A store the cluster can actually archive into. A cluster pointed at an
  # unreachable endpoint may never reach a running primary, and the
  # admin-sync assertions need one.
  log "deploying the object store the cluster archives into"
  kubectl apply --server-side -f test/e2e/testdata/minio.yaml
  kubectl -n minio rollout status deployment/minio --timeout=300s

  log "building operator images"
  make docker-build docker-build-proxy IMG="${MANAGER_IMG}" PROXY_IMG="${PROXY_IMG}" VERSION=e2e

  log "loading images into the cluster"
  kind load docker-image "${MANAGER_IMG}" "${PROXY_IMG}" --name "${CLUSTER_NAME}"

  # Pull the component images on the host and side-load them: the node has no
  # registry credentials, and side-loading also keeps the test off the
  # network once it starts.
  for image in "${PGCONSOLE_IMAGE}" "${PGADMIN_IMAGE}" "${VIEWER_IMAGE}"; do
    log "side-loading ${image}"
    docker pull --quiet "${image}"
    kind load docker-image "${image}" --name "${CLUSTER_NAME}"
  done

  # Installing through the Helm chart rather than the kustomize overlay puts
  # a real install path under test too: the chart's CRDs, its ClusterRole,
  # and the flags it passes to the manager.
  log "installing the operator via the Helm chart"
  helm install pgtoolbox deploy/helm/pgtoolbox \
    --namespace pgtoolbox --create-namespace \
    --set image.repository="${MANAGER_IMG%%:*}" \
    --set image.tag="${MANAGER_IMG##*:}" \
    --set image.pullPolicy=Never \
    --set proxyImage="${PROXY_IMG}" \
    --set defaultImages.pgConsole="${PGCONSOLE_IMAGE}" \
    --set defaultImages.pgAdmin="${PGADMIN_IMAGE}" \
    --set defaultImages.objectStoreViewer="${VIEWER_IMAGE}" \
    --set replicaCount=1 \
    --wait --timeout 300s
fi

log "running the e2e smoke test"
set +e
go test -tags e2e ./test/e2e/... -count=1 -timeout 30m -v \
  -args \
  -pgconsole-image="${PGCONSOLE_IMAGE}" \
  -pgadmin-image="${PGADMIN_IMAGE}" \
  -viewer-image="${VIEWER_IMAGE}"
status=$?
set -e

if [[ ${status} -ne 0 ]]; then
  log "test failed — dumping state"
  kubectl get pgconsole,pgtoolboxuser,deploy,pod -A -l app.kubernetes.io/managed-by=pgtoolbox || true
  kubectl get pgconsole -A -o yaml | grep -A40 'conditions:' | head -60 || true
  kubectl get cluster -A || true
  kubectl -n pgtoolbox logs deployment/pgtoolbox --tail=100 || true
  kubectl get events -A --sort-by=.lastTimestamp | tail -40 || true
fi

exit ${status}
