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
#   2. it reconciles a PgAdmin, a PgToolBoxConsole and an EvidenceAgent in a
#      watched namespace, and no PgAdmin in another: each one's children
#      exist (so the console's RoleBindings to the three console roles and the
#      agents' ClusterRoleBinding pass RBAC escalation prevention in this
#      mode) and no condition carries an API server Forbidden;
#   3. a watched namespace deleted after the install does not stop it at its
#      next start, and it keeps reconciling the namespaces left;
#   4. switching to the default mode with helm upgrade succeeds (each mode
#      has its own ClusterRoleBinding: a roleRef is immutable), and
#   5. so does switching back to watchNamespaces, which confines it again.
#
# observedGeneration alone proves nothing: status is published on error
# paths too. Only CloudNativePG's CRDs and operator are installed, and no
# database runs: the PgAdmin names a Cluster that does not exist (it still
# renders), and the EvidenceAgent reads a Cluster whose reconciliation is
# disabled (cnpg.io/reconciliationLoop: disabled: CloudNativePG creates no
# Pod) and whose in-tree object store is a fake bucket. The agent's and the
# console's Pods may never become ready (their images are pulled, the bucket
# does not answer): the scenario asserts what the operator renders, not what
# they serve.
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
  kubectl get pgadmin,pgtoolboxconsole,evidenceagent -A -o yaml || true
  kubectl -n scope-a get deployments,services,serviceaccounts,rolebindings || true
  kubectl get clusterrolebinding pgtoolbox-evidence-agent -o yaml || true
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

# retry <seconds> <command...>: until the command succeeds, or fail.
retry() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || return 1
    sleep 2
  done
}

# exist <namespace> <kind/name>...: every object exists.
exist() {
  local ns=$1 object
  shift
  for object in "$@"; do
    kubectl -n "${ns}" get "${object}" >/dev/null 2>&1 || return 1
  done
}

# no_forbidden <namespace> <kind/name>: the object has conditions and none of
# them carries the API server's Forbidden ("... is forbidden: User ... cannot
# ..."); a registry's "403 Forbidden" in an image pull message is not one.
no_forbidden() {
  local conditions
  conditions="$(kubectl -n "$1" get "$2" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}')"
  if [[ -z "${conditions}" ]]; then
    echo "$1/$2 has no condition" >&2
    return 1
  fi
  if grep -qi 'is forbidden' <<<"${conditions}"; then
    echo "$1/$2 carries a Forbidden condition:" >&2
    echo "${conditions}" >&2
    return 1
  fi
}

# reconciled <namespace> <generation>: the PgAdmin's status names the
# generation, its children exist and no condition is a Forbidden.
reconciled() {
  kubectl -n "$1" wait pgadmin/scope --for=jsonpath='{.status.observedGeneration}'="$2" --timeout=180s || return 1
  retry 120 exist "$1" deployment/scope-pgadmin service/scope-pgadmin serviceaccount/scope-pgadmin || {
    echo "the PgAdmin's Deployment, Service or ServiceAccount is missing in $1" >&2
    return 1
  }
  no_forbidden "$1" pgadmin/scope
}

# console_and_agent <namespace>: a PgToolBoxConsole with the three console
# roles to bind (restore drills on, pod logs by default) and an EvidenceAgent
# reading a Cluster that never runs.
console_and_agent() {
  local ns=$1
  kubectl -n "${ns}" create secret generic fake-s3 --from-literal=ACCESS_KEY_ID=e2e --from-literal=ACCESS_SECRET_KEY=e2e
  kubectl apply -f - <<YAML
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: inert
  namespace: ${ns}
  annotations:
    cnpg.io/reconciliationLoop: disabled
spec:
  instances: 1
  storage: {size: 1Gi}
  backup:
    barmanObjectStore:
      destinationPath: s3://scope-e2e/
      endpointURL: https://fake-s3.invalid
      s3Credentials:
        accessKeyId: {name: fake-s3, key: ACCESS_KEY_ID}
        secretAccessKey: {name: fake-s3, key: ACCESS_SECRET_KEY}
---
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: EvidenceAgent
metadata:
  name: scope
  namespace: ${ns}
spec:
  clusterRef: {name: inert}
  allowedSecrets: [fake-s3]
  refreshInterval: 30m
---
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxConsole
metadata:
  name: console
  namespace: ${ns}
spec:
  replicas: 1
  history: {persistent: false}
  auth: {tokenLogin: true}
  restoreDrills: {enabled: true}
YAML
}

# console_and_agent_reconciled <namespace>: their children exist, the agent's
# ServiceAccount is a subject of the agents' ClusterRoleBinding, and neither
# carries a Forbidden.
console_and_agent_reconciled() {
  local ns=$1
  retry 180 exist "${ns}" serviceaccount/console service/console deployment/console \
    rolebinding/console rolebinding/console-pod-logs rolebinding/console-restore-drills || {
    echo "the console's ServiceAccount, Service, Deployment or RoleBindings are missing in ${ns}" >&2
    return 1
  }
  retry 180 exist "${ns}" serviceaccount/scope-evidence-agent service/scope-evidence-agent deployment/scope-evidence-agent || {
    echo "the EvidenceAgent's ServiceAccount, Service or Deployment is missing in ${ns}" >&2
    return 1
  }
  kubectl get clusterrolebinding pgtoolbox-evidence-agent -o jsonpath='{range .subjects[*]}{.namespace}/{.name}{"\n"}{end}' |
    grep -qx "${ns}/scope-evidence-agent" || {
    echo "the agents' ClusterRoleBinding does not bind ${ns}/scope-evidence-agent" >&2
    return 1
  }
  no_forbidden "${ns}" pgtoolboxconsole/console && no_forbidden "${ns}" evidenceagent/scope
}

# refresh_console_and_agent <namespace> <n>: force a reconcile under the
# operator's CURRENT mode and wait for its status. The first reconcile of a
# mode switch can predate it; its conditions are the old mode's. The agent's
# refreshInterval is a harmless spec field: changing it bumps the generation,
# and observedGeneration must catch up. The console's drills RoleBinding is
# owned, so deleting it is a watch event the operator must repair, which it can
# only do with the new mode's permissions.
refresh_console_and_agent() {
  local ns=$1 n=$2 agent_gen console_gen
  kubectl -n "${ns}" patch evidenceagent/scope --type merge -p "{\"spec\":{\"refreshInterval\":\"$((30 + n))m\"}}" || return 1
  kubectl -n "${ns}" delete rolebinding/console-restore-drills --wait=true || return 1
  retry 180 exist "${ns}" rolebinding/console-restore-drills || {
    echo "the operator did not recreate rolebinding/console-restore-drills in ${ns}" >&2
    return 1
  }
  agent_gen="$(kubectl -n "${ns}" get evidenceagent/scope -o jsonpath='{.metadata.generation}')"
  kubectl -n "${ns}" wait evidenceagent/scope --for=jsonpath='{.status.observedGeneration}'="${agent_gen}" --timeout=180s || return 1
  console_gen="$(kubectl -n "${ns}" get pgtoolboxconsole/console -o jsonpath='{.metadata.generation}')"
  kubectl -n "${ns}" wait pgtoolboxconsole/console --for=jsonpath='{.status.observedGeneration}'="${console_gen}" --timeout=180s || return 1
}

# operator_log_clean: the newest operator Pod's log carries no API server
# Forbidden (RBAC escalation prevention or a missing verb in this mode).
operator_log_clean() {
  local newest
  newest="$(kubectl -n pgtoolbox get pods -l app.kubernetes.io/instance=pgtoolbox,app.kubernetes.io/name=pgtoolbox \
    --sort-by=.metadata.creationTimestamp -o name | tail -n 1)"
  [[ -n "${newest}" ]] || { echo "no operator Pod" >&2; return 1; }
  if kubectl -n pgtoolbox logs "${newest}" | grep -i 'is forbidden'; then
    echo "${newest} logged a Forbidden" >&2
    return 1
  fi
}

log "2. a PgAdmin, a console and an agent in a watched namespace are reconciled, a PgAdmin elsewhere is not"
pgadmin scope-a
pgadmin scope-x
console_and_agent scope-a
reconciled scope-a 1 || fail "the PgAdmin in scope-a was not reconciled"
console_and_agent_reconciled scope-a || fail "the console or the agent in scope-a was not reconciled"
operator_log_clean || fail "the operator logged a Forbidden confined to scope-a and scope-b"
sleep 10
if [[ -n "$(kubectl -n scope-x get pgadmin/scope -o jsonpath='{.status.observedGeneration}')" ]]; then
  fail "the operator reconciled a PgAdmin in scope-x, which it does not watch"
fi

log "3. a watched namespace deleted after the install"
kubectl delete namespace scope-b --wait=true --timeout=180s
kubectl -n pgtoolbox rollout restart deployment/pgtoolbox
kubectl -n pgtoolbox rollout status deployment/pgtoolbox --timeout=300s || fail "the operator did not restart Ready without scope-b"
# rollout status returns while the old Pod is still terminating, and
# `kubectl logs deployment/...` may pick it: read the newest Pod.
newest="$(kubectl -n pgtoolbox get pods -l app.kubernetes.io/instance=pgtoolbox,app.kubernetes.io/name=pgtoolbox \
  --sort-by=.metadata.creationTimestamp -o name | tail -n 1)"
kubectl -n pgtoolbox logs "${newest}" | grep 'watched namespace left out' | grep -q 'scope-b' ||
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
refresh_console_and_agent scope-a 1 || fail "in the default mode the console or the agent in scope-a did not reconcile again"
console_and_agent_reconciled scope-a || fail "in the default mode the console or the agent in scope-a is not reconciled"
operator_log_clean || fail "in the default mode the operator logged a Forbidden"

log "5. back to watchNamespaces with helm upgrade"
helm_install --set 'operator.config.watchNamespaces={scope-a}' || fail "helm upgrade to watchNamespaces (immutable roleRef?)"
kubectl get clusterrolebinding pgtoolbox-manager-cluster >/dev/null || fail "no watchNamespaces ClusterRoleBinding"
if kubectl get clusterrolebinding pgtoolbox-manager >/dev/null 2>&1; then
  fail "the default-mode ClusterRoleBinding is still there"
fi
kubectl -n pgtoolbox rollout status deployment/pgtoolbox --timeout=300s || fail "the operator is not Ready confined to scope-a"
kubectl -n scope-a patch pgadmin/scope --type merge -p '{"spec":{"admins":["alice@example.com"]}}'
reconciled scope-a 3 || fail "confined again, the PgAdmin in scope-a was not reconciled"
refresh_console_and_agent scope-a 2 || fail "confined again, the console or the agent in scope-a did not reconcile again"
console_and_agent_reconciled scope-a || fail "confined again, the console or the agent in scope-a is not reconciled"
operator_log_clean || fail "confined again, the operator logged a Forbidden"
kubectl -n scope-x patch pgadmin/scope --type merge -p '{"spec":{"admins":["alice@example.com","bob@example.com"]}}'
sleep 10
[[ "$(kubectl -n scope-x get pgadmin/scope -o jsonpath='{.status.observedGeneration}')" == "1" ]] ||
  fail "confined again, the operator still reconciled the PgAdmin in scope-x"

log "the scope scenario passed"
