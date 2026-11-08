#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The EvidenceAgent kind scenario: a kind cluster with CloudNativePG,
# cert-manager, the Barman Cloud Plugin and MinIO; the operator and the
# evidence agent images built from this tree and side-loaded; the chart; then
# TestEvidenceAgent (test/e2e/evidenceagent_test.go) publishes an agent and
# reads it as a console would, with a bound token, and checks that the wrong
# audience and an unauthorized subject are refused.
#
# Host rules (AGENTS.md): KUBECONFIG and CLUSTER_NAME come from the caller
# (the CI job sets them per run id); nothing is written outside the work tree
# and $TMPDIR; the cluster is deleted on exit unless KEEP_CLUSTER=1.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${repo_root}"

CLUSTER_NAME="${CLUSTER_NAME:-ptb-e2e-evidenceagent}"
# The pins (kind node, CloudNativePG, cert-manager, the plugin) and kind_load,
# log, install_cnpg_stack: shared with test/e2e/team/run.sh.
# shellcheck source=hack/e2e-cnpg-stack.sh
source "${repo_root}/hack/e2e-cnpg-stack.sh"
MANAGER_IMG="${MANAGER_IMG:-pgtoolbox-operator:e2e-evidenceagent}"
EVIDENCE_IMG="${EVIDENCE_IMG:-pgtoolbox-evidence-agent:e2e-evidenceagent}"

for tool in kind kubectl helm docker go; do
  command -v "${tool}" >/dev/null || { echo "missing required tool: ${tool}" >&2; exit 1; }
done

teardown() {
  if [[ "${KEEP_CLUSTER:-}" == "1" ]]; then
    log "leaving cluster ${CLUSTER_NAME} up (KEEP_CLUSTER=1)"
    return
  fi
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
}
trap teardown EXIT

log "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE})"
kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
hack/kind-create.sh --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config hack/kind/ci.yaml --wait 300s

install_cnpg_stack

log "building the operator and evidence agent images from this tree"
make docker-build docker-build-evidence OPERATOR_IMG="${MANAGER_IMG}" EVIDENCE_IMG="${EVIDENCE_IMG}" VERSION=e2e
kind_load "${MANAGER_IMG}" "${EVIDENCE_IMG}"

log "installing the operator via the Helm chart"
helm install pgtoolbox charts/pgtoolbox \
  --namespace pgtoolbox --create-namespace \
  --set image.repository="${MANAGER_IMG%%:*}" \
  --set image.tag="${MANAGER_IMG##*:}" \
  --set image.pullPolicy=Never \
  --set operator.enabled=true \
  --set replicaCount=1 \
  --set operator.evidenceAgent.image="${EVIDENCE_IMG}" \
  --wait --timeout 300s

log "running the EvidenceAgent scenario"
set +e
go test -tags e2e ./test/e2e/ -run '^TestEvidenceAgent' -count=1 -timeout 20m -v \
  -args -evidence-agent-image="${EVIDENCE_IMG}"
status=$?
set -e
if [[ "${status}" != 0 ]]; then
  log "diagnostics"
  kubectl get evidenceagents.pgtoolbox.fyannk.dev -A -o yaml || true
  kubectl -n pgtoolbox-e2e get pods,deployments,services -o wide || true
  kubectl -n pgtoolbox-e2e logs -l app.kubernetes.io/name=evidence-agent --tail=100 || true
  kubectl -n pgtoolbox logs deployment/pgtoolbox --tail=200 || true
fi
exit "${status}"
