#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The operator confined to some namespaces, on kind: charts/pgtoolbox installed
# in pgtoolbox with operator.config.watchNamespaces={scope-a,scope-b}, the
# release namespace not among them. It proves on a real API server what a fake
# client cannot:
#
#   1. the operator starts and becomes Ready (it reads its own ConfigMap and
#      Secret, by name, in a namespace it does not watch);
#   2. it reconciles a PgAdmin in a watched namespace, and none in another;
#   3. a watched namespace deleted after the install does not stop it at its
#      next start, and it keeps reconciling the namespaces left;
#   4. switching back to the default mode with helm upgrade succeeds (each
#      mode has its own ClusterRoleBinding: a roleRef is immutable).
#
# Only CloudNativePG's CRDs and operator are installed: the PgAdmin names a
# Cluster that does not exist, so its status (observedGeneration) is the
# proof of a reconcile, without a database.
#
# Host rules (AGENTS.md): KUBECONFIG and CLUSTER_NAME come from the caller;
# nothing is written outside the work tree and $TMPDIR; the cluster is deleted
# on exit unless KEEP_CLUSTER=1.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "${repo_root}"

CLUSTER_NAME="${CLUSTER_NAME:-ptb-e2e-scope}"
# shellcheck source=hack/e2e-cnpg-stack.sh
source "${repo_root}/hack/e2e-cnpg-stack.sh"
MANAGER_IMG="${MANAGER_IMG:-pgtoolbox-operator:e2e-scope}"

for tool in kind kubectl helm docker; do
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

diagnostics() {
  log "diagnostics"
  kubectl get pgadmin -A -o yaml || true
  kubectl -n pgtoolbox get pods,deployments,roles,rolebindings -o wide || true
  kubectl get clusterrolebindings -l app.kubernetes.io/name=pgtoolbox || true
  kubectl -n pgtoolbox logs deployment/pgtoolbox --tail=200 || true
}
fail() {
  echo "FAIL: $*" >&2
  diagnostics
  exit 1
}

log "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE})"
kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
hack/kind-create.sh --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config hack/kind/ci.yaml --wait 300s

log "installing CloudNativePG ${CNPG_VERSION}"
apply_manifest "${CNPG_MANIFEST}" --server-side
kubectl -n cnpg-system wait --for=condition=Available deployment/cnpg-controller-manager --timeout=300s

log "building the operator image from this tree"
make docker-build OPERATOR_IMG="${MANAGER_IMG}" VERSION=e2e
kind_load "${MANAGER_IMG}"

for ns in pgtoolbox scope-a scope-b scope-x; do
  kubectl create namespace "${ns}"
done
# The optional configuration objects, in the release namespace only.
kubectl -n pgtoolbox create configmap pgtoolbox-operator-config --from-literal=UPGRADE_BACKUPS_KEPT=4
kubectl -n pgtoolbox create secret generic pgtoolbox-operator-config --from-literal=MAX_CONCURRENT_RECONCILES=2

helm_install() {
  helm upgrade --install pgtoolbox charts/pgtoolbox \
    --namespace pgtoolbox \
    --set image.repository="${MANAGER_IMG%%:*}" \
    --set image.tag="${MANAGER_IMG##*:}" \
    --set image.pullPolicy=Never \
    --set operator.enabled=true \
    --set replicaCount=1 \
    "$@" --wait --timeout 300s
}

log "1. installing the operator confined to scope-a and scope-b"
helm_install --set 'operator.config.watchNamespaces={scope-a,scope-b}' || fail "helm install"
kubectl -n pgtoolbox rollout status deployment/pgtoolbox --timeout=300s || fail "the operator never became Ready"

pgadmin() {
  local ns=$1
  kubectl -n "${ns}" create secret generic pgadmin-oidc --from-literal=client-secret=e2e-client-secret
  kubectl apply -f - <<YAML
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata:
  name: scope
  namespace: ${ns}
spec:
  clusterRefs: [{name: absent}]
  auth:
    oauth2:
      issuerURL: https://idp.invalid
      clientID: pgadmin
      clientSecretRef: {name: pgadmin-oidc}
      requiredClaims: {groups: [dba]}
  admins: [alice@example.com]
YAML
}

reconciled() { # reconciled <namespace> <generation>
  kubectl -n "$1" wait pgadmin/scope --for=jsonpath='{.status.observedGeneration}'="$2" --timeout=180s
}

log "2. a PgAdmin in a watched namespace is reconciled, one elsewhere is not"
pgadmin scope-a
pgadmin scope-x
reconciled scope-a 1 || fail "the PgAdmin in scope-a was not reconciled"
sleep 10
if [[ -n "$(kubectl -n scope-x get pgadmin/scope -o jsonpath='{.status.observedGeneration}')" ]]; then
  fail "the operator reconciled a PgAdmin in scope-x, which it does not watch"
fi

log "3. a watched namespace deleted after the install"
kubectl delete namespace scope-b --wait=true --timeout=180s
kubectl -n pgtoolbox rollout restart deployment/pgtoolbox
kubectl -n pgtoolbox rollout status deployment/pgtoolbox --timeout=300s || fail "the operator did not restart Ready without scope-b"
kubectl -n pgtoolbox logs deployment/pgtoolbox | grep -q 'watched namespace left out' ||
  fail "the operator did not log scope-b as left out"
kubectl -n scope-a patch pgadmin/scope --type merge -p '{"spec":{"admins":["alice@example.com","bob@example.com"]}}'
reconciled scope-a 2 || fail "the PgAdmin in scope-a was not reconciled after the restart"

log "4. back to the default mode with helm upgrade"
helm_install || fail "helm upgrade to the default mode (immutable roleRef?)"
kubectl get clusterrolebinding pgtoolbox-manager >/dev/null || fail "no default-mode ClusterRoleBinding"
if kubectl get clusterrolebinding pgtoolbox-manager-cluster >/dev/null 2>&1; then
  fail "the watchNamespaces ClusterRoleBinding is still there"
fi
kubectl -n pgtoolbox rollout status deployment/pgtoolbox --timeout=300s || fail "the operator is not Ready in the default mode"
reconciled scope-x 1 || fail "in the default mode the PgAdmin in scope-x was not reconciled"

log "the scope scenario passed"
