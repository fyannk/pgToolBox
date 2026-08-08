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
# binaries accept what the operator renders, so these are not substitutable.
PGCONSOLE_IMAGE="${PGCONSOLE_IMAGE:-ghcr.io/fyannk/pgconsole:0.1.0}"

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
  kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --wait 120s

  log "installing CloudNativePG ${CNPG_VERSION}"
  kubectl apply --server-side -f "${CNPG_MANIFEST}"
  kubectl -n cnpg-system wait --for=condition=Available deployment/cnpg-controller-manager --timeout=300s

  log "building operator images"
  make docker-build docker-build-proxy IMG="${MANAGER_IMG}" PROXY_IMG="${PROXY_IMG}" VERSION=e2e

  log "loading images into the cluster"
  kind load docker-image "${MANAGER_IMG}" "${PROXY_IMG}" --name "${CLUSTER_NAME}"

  # Pull the console image on the host and side-load it: the node has no
  # registry credentials, and side-loading also keeps the test off the
  # network once it starts.
  docker pull --quiet "${PGCONSOLE_IMAGE}"
  kind load docker-image "${PGCONSOLE_IMAGE}" --name "${CLUSTER_NAME}"

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
    --set replicaCount=1 \
    --wait --timeout 300s
fi

log "running the e2e smoke test"
set +e
go test -tags e2e ./test/e2e/... -count=1 -timeout 20m -v \
  -args -pgconsole-image="${PGCONSOLE_IMAGE}"
status=$?
set -e

if [[ ${status} -ne 0 ]]; then
  log "test failed — dumping state"
  kubectl get pgconsole,deploy,pod,role,rolebinding -A -l app.kubernetes.io/managed-by=pgtoolbox || true
  kubectl -n pgtoolbox logs deployment/pgtoolbox --tail=100 || true
  kubectl get events -A --sort-by=.lastTimestamp | tail -40 || true
fi

exit ${status}
