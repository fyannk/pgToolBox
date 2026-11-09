#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The PgAdmin scenario on kind: CloudNativePG, the operator from the
# working tree, one Cluster and one
# PgAdmin, then pgadmin_test.go. It needs a kind cluster to itself: CI runs
# it on a Docker host, one e2e job at a time.
#
#   CLUSTER_NAME=pgadmin-e2e-local ./test/e2e/pgadmin/run.sh
#   KEEP_CLUSTER=1 leaves the cluster up; REUSE_CLUSTER=1 reuses one, skipping provisioning
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "${repo_root}"

CLUSTER_NAME="${CLUSTER_NAME:-pgadmin-e2e-local}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
CNPG_VERSION="${CNPG_VERSION:-1.30.0}"
CNPG_MANIFEST="${CNPG_MANIFEST:-https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.30/releases/cnpg-${CNPG_VERSION}.yaml}"
PGADMIN_IMAGE="${PGADMIN_IMAGE:-ghcr.io/fyannk/pgadmin:9.18-hardened}"
MANAGER_IMG="${MANAGER_IMG:-pgtoolbox-operator:e2e-pgadmin}"
KIND_IMAGE_PLATFORM="${KIND_IMAGE_PLATFORM:-linux/amd64}"

# shellcheck source=hack/lib/net.sh
source hack/lib/net.sh

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

for tool in kind kubectl docker go; do
  command -v "${tool}" >/dev/null || { echo "missing required tool: ${tool}" >&2; exit 1; }
done

# One platform only: see kind_load in hack/e2e-chart.sh.
kind_load() {
  local archive status=0
  archive="$(mktemp "${TMPDIR:-/tmp}/kind-load.XXXXXX")"
  if ! { docker save --platform "${KIND_IMAGE_PLATFORM}" "$@" > "${archive}" &&
    kind load image-archive "${archive}" --name "${CLUSTER_NAME}"; }; then
    status=1
  fi
  rm -f "${archive}"
  return "${status}"
}

teardown() {
  if [[ "${KEEP_CLUSTER:-}" == "1" ]]; then
    log "leaving cluster ${CLUSTER_NAME} up (KEEP_CLUSTER=1)"
    return
  fi
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
}

if [[ "${REUSE_CLUSTER:-}" != "1" ]]; then
  trap teardown EXIT
  log "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE})"
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  hack/kind-create.sh --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config hack/kind/ci.yaml --wait 300s

  log "installing CloudNativePG ${CNPG_VERSION}"
  apply_manifest "${CNPG_MANIFEST}" --server-side
  kubectl -n cnpg-system wait --for=condition=Available deployment/cnpg-controller-manager --timeout=300s

  log "building the operator image"
  make docker-build OPERATOR_IMG="${MANAGER_IMG}" VERSION=e2e
  kind_load "${MANAGER_IMG}"
  log "side-loading ${PGADMIN_IMAGE}"
  pull_image "${KIND_IMAGE_PLATFORM}" "${PGADMIN_IMAGE}"
  kind_load "${PGADMIN_IMAGE}"

  log "installing the operator (CRDs, generated RBAC)"
  kubectl apply --server-side -f config/crd/bases/
  kubectl create namespace pgtoolbox-pgadmin --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply --server-side -f config/rbac/role.yaml
  sed -e "s|@MANAGER_IMG@|${MANAGER_IMG}|g" -e "s|@PGADMIN_IMAGE@|${PGADMIN_IMAGE}|g" \
    test/e2e/pgadmin/testdata/operator.yaml | kubectl apply --server-side -f -
  kubectl -n pgtoolbox-pgadmin rollout status deployment/pgtoolbox-pgadmin --timeout=300s
fi

log "running the PgAdmin scenario"
set +e
go test -tags e2e ./test/e2e/pgadmin/... -count=1 -timeout 25m -v
status=$?
set -e
if [[ ${status} -ne 0 ]]; then
  log "test failed — dumping state"
  kubectl get pgadmin,cluster,deploy,job,pod -A || true
  kubectl get pgadmin -A -o yaml | sed -n '/status:/,$p' | head -120 || true
  kubectl -n pgtoolbox-pgadmin logs deployment/pgtoolbox-pgadmin --tail=200 || true
  kubectl -n pgadmin-e2e logs deployment/team-pgadmin -c passfile-sync --tail=100 || true
  kubectl -n pgadmin-e2e logs -l app.kubernetes.io/component=registration --tail=100 || true
  kubectl get events -A --sort-by=.lastTimestamp | tail -40 || true
fi
exit ${status}
