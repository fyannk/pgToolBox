#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Team mode on kind: charts/pgtools installed by a user who holds
# nothing but the admin role of one namespace, with no pgtoolbox operator in the
# cluster. It proves on a real API server and a real CloudNativePG what neither
# the chart e2e (CRDs only, no PostgreSQL) nor the EvidenceAgent e2e (the
# operator renders the agent) can:
#
#   * the user's authority is a RoleBinding to the built-in ClusterRole admin plus
#     a Role on the CloudNativePG groups (CloudNativePG does not aggregate into
#     admin); nothing cluster-wide, and the installation succeeds under it
#     (helm --kube-as-user: Kubernetes refuses a Role that grants what its author
#     does not hold, so the chart cannot ask for more than admin + that Role);
#   * the release holds only namespaced kinds, all its Pods become Ready;
#   * the evidence agent declared in values reads the barman-cloud repository of a
#     real archiving Cluster, and the console shows it live;
#   * pgAdmin's sidecar reads the Cluster and its application Secret, connects
#     with verify-full and logs "server credential verified";
#   * a ServiceAccount the team admin binds to a namespaced Role (the rules of
#     the pgtoolbox-console-viewer ClusterRole, which exists only with the operator)
#     signs in with its token, and another one with no binding is refused;
#   * (1.3.0) persistent history: the console is a StatefulSet, both replicas
#     are Ready and both history claims are Bound;
#   * (1.3.0) a digest channel: the chart renders the digest Lease Role and
#     RoleBinding, and the namespace admin's install of them passed the
#     escalation check (leases are in admin);
#   * (1.3.0) MCP at /mcp with a ServiceAccount token as the bearer: the viewer
#     initializes, lists the tools and sees the Cluster in list_clusters; the
#     stranger sees no Cluster and gets a tool error on cluster_status;
#   * (1.3.0) the <console>-state ConfigMap the replicas share is written.
#
# The provisioning is shared with test/e2e/evidenceagent.sh (hack/e2e-cnpg-stack.sh).
#
#   CLUSTER_NAME=ptb-team-local ./test/e2e/team/run.sh
#   KEEP_CLUSTER=1 leaves the cluster up. Failure dumps land in artifacts/e2e-team/.
#
# It needs a kind cluster to itself: CI runs it on a Docker host, one e2e job
# at a time (ci.yml e2e-team).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "${repo_root}"

CLUSTER_NAME="${CLUSTER_NAME:-ptb-team-local}"
# shellcheck source=hack/e2e-cnpg-stack.sh
source "${repo_root}/hack/e2e-cnpg-stack.sh"
MANAGER_IMG="${MANAGER_IMG:-pgtoolbox-operator:e2e-team}"
CONSOLE_IMG="${CONSOLE_IMG:-pgtoolbox-console:e2e-team}"
EVIDENCE_IMG="${EVIDENCE_IMG:-pgtoolbox-evidence-agent:e2e-team}"
PGADMIN_IMAGE="${PGADMIN_IMAGE:-ghcr.io/fyannk/pgadmin:9.18-hardened}"
OUT="${E2E_ARTIFACTS:-artifacts/e2e-team}"
NS=lab-team
RELEASE=team
ADMIN=team-admin
# The URL people type (console.publicURL). MCP needs one; it is only a name for
# the console's origin checks, nothing resolves it (the test uses the node port).
PUBLIC_URL=https://console.team-e2e.invalid
CONSOLE=${RELEASE}-console

if [[ -n "${E2E_REQUIRE_KUBECONFIG:-}" ]]; then
  case "${KUBECONFIG:-}" in
    "" | *:* | "${HOME}/.kube/config")
      echo "[team] E2E_REQUIRE_KUBECONFIG needs KUBECONFIG set to one private file, got '${KUBECONFIG:-}'" >&2
      exit 1 ;;
  esac
fi
for tool in kind kubectl helm docker curl openssl make; do
  command -v "${tool}" >/dev/null || { echo "missing required tool: ${tool}" >&2; exit 1; }
done

mkdir -p "${OUT}"
work="$(mktemp -d "${TMPDIR:-/tmp}/ptb-team-e2e.XXXXXX")"

dump() {
  log "failed: dumping state to ${OUT}"
  kubectl get pods,deploy,sts,svc,pvc,role,rolebinding,cluster,backup,scheduledbackup,objectstore -A -o wide >"${OUT}/objects.txt" 2>&1 || true
  kubectl -n "${NS}" get events --sort-by=.lastTimestamp >"${OUT}/events.txt" 2>&1 || true
  kubectl -n "${NS}" get configmap "${CONSOLE}-state" -o yaml >"${OUT}/state-configmap.yaml" 2>&1 || true
  kubectl -n "${NS}" describe pods >"${OUT}/describe-pods.txt" 2>&1 || true
  kubectl -n "${NS}" get cluster ledger -o yaml >"${OUT}/cluster.yaml" 2>&1 || true
  kubectl -n "${NS}" logs -l app.kubernetes.io/name=evidence-agent --all-containers --tail=-1 >"${OUT}/agent.log" 2>&1 || true
  kubectl -n "${NS}" logs -l app.kubernetes.io/component=console --all-containers --tail=-1 >"${OUT}/console.log" 2>&1 || true
  kubectl -n "${NS}" logs "deployment/${RELEASE}-pgadmin" --all-containers --tail=-1 >"${OUT}/pgadmin.log" 2>&1 || true
  kubectl -n cnpg-system logs deployment/cnpg-controller-manager --tail=300 >"${OUT}/cnpg.log" 2>&1 || true
  kind export logs --name "${CLUSTER_NAME}" "${OUT}/kind" >/dev/null 2>&1 || true
  tail -n 5 "${work}/wait.err" 2>/dev/null || true
  tail -n 40 "${OUT}/agent.log" "${OUT}/console.log" "${OUT}/pgadmin.log" 2>/dev/null || true
  # The runner keeps no artifacts: print where pgAdmin's first error starts,
  # which a 40-line tail of a Python traceback cuts off.
  echo "==> first errors in ${OUT}/pgadmin.log"
  grep -n -m 5 -B 5 -A 25 -iE 'error|exception|traceback' "${OUT}/pgadmin.log" 2>/dev/null | head -n 150 || true
  echo "==> pod states"
  grep -E 'pgadmin|console|evidence' "${OUT}/objects.txt" 2>/dev/null || true
  tail -n 30 "${OUT}/events.txt" 2>/dev/null || true
}

cleanup() {
  local status=$?
  ((status == 0)) || dump
  rm -rf "${work:?}"
  if [[ "${KEEP_CLUSTER:-}" == "1" ]]; then
    log "leaving cluster ${CLUSTER_NAME} up (KEEP_CLUSTER=1)"
  else
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  exit "${status}"
}
trap cleanup EXIT

fail() { echo "[team] FAIL: $*" >&2; exit 1; }

# wait_for <what> <seconds> <command...>: poll until the command succeeds.
wait_for() {
  local what=$1 limit=$2 i=0
  shift 2
  # stderr goes to a file, not away: a fail inside the polled command is shown by dump.
  until "$@" >/dev/null 2>>"${work}/wait.err"; do
    i=$((i + 3))
    ((i <= limit)) || fail "${what}: not true after ${limit}s"
    sleep 3
  done
  echo "[team] ok: ${what}"
}

# Everything the team does goes through these two: the impersonated admin.
as_admin() { kubectl --as="${ADMIN}" -n "${NS}" "$@"; }
helm_admin() { helm --kube-as-user "${ADMIN}" --namespace "${NS}" "$@"; }

log "pulling ${PGADMIN_IMAGE} and building the operator, console and evidence agent images"
pull_image "${KIND_IMAGE_PLATFORM}" "${PGADMIN_IMAGE}"
# The operator image is only the carrier of the binary pgAdmin's init container and
# sidecar run (pgadmin.tools.image); the operator is never deployed.
make docker-build docker-build-console docker-build-evidence \
  OPERATOR_IMG="${MANAGER_IMG}" CONSOLE_IMG="${CONSOLE_IMG}" EVIDENCE_IMG="${EVIDENCE_IMG}" VERSION=e2e

log "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE})"
kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
hack/kind-create.sh --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config hack/kind/ci.yaml --wait 300s

install_cnpg_stack
kind_load "${MANAGER_IMG}" "${CONSOLE_IMG}" "${EVIDENCE_IMG}" "${PGADMIN_IMAGE}"

# The platform's only act: a namespace, and the team admin's authority in it.
log "the team namespace and its admin (admin + a Role on the CloudNativePG groups; nothing cluster-wide)"
kubectl apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata: {name: ${NS}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: ${ADMIN}-admin, namespace: ${NS}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: admin}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: User, name: ${ADMIN}}]
---
# CloudNativePG and its plugins do not aggregate into admin. The chart's console
# Role reads the pgBackRest plugins' Stanzas and Archives too, so the admin must
# hold those groups to install it (guides/pgbackrest.md).
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: cnpg-all, namespace: ${NS}}
rules:
  - {apiGroups: [postgresql.cnpg.io, barmancloud.cnpg.io, pgbackrest.dalibo.com, pgbackrest.cnpg.opera.com], resources: ["*"], verbs: ["*"]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: ${ADMIN}-cnpg, namespace: ${NS}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: cnpg-all}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: User, name: ${ADMIN}}]
YAML

[[ "$(kubectl auth can-i create clusterroles --as="${ADMIN}")" == no ]] || fail "${ADMIN} may create ClusterRoles"
[[ "$(kubectl auth can-i create customresourcedefinitions --as="${ADMIN}")" == no ]] || fail "${ADMIN} may create CRDs"
[[ "$(kubectl auth can-i get nodes --as="${ADMIN}")" == no ]] || fail "${ADMIN} may read Nodes"

log "as ${ADMIN}: the S3 Secret, the ObjectStore, a barman-cloud Cluster"
access_key="$(kubectl -n minio get secret minio-credentials -o jsonpath='{.data.ACCESS_KEY_ID}' | base64 -d)"
secret_key="$(kubectl -n minio get secret minio-credentials -o jsonpath='{.data.ACCESS_SECRET_KEY}' | base64 -d)"
as_admin create secret generic team-s3 \
  --from-literal=ACCESS_KEY_ID="${access_key}" --from-literal=ACCESS_SECRET_KEY="${secret_key}"
as_admin create secret generic pgadmin-oidc --from-literal=client-secret="$(openssl rand -hex 16)"
# The digest channel: a generic webhook to a name that never resolves. Delivery
# is retried and shown on the admin page, it blocks nothing; the test only needs
# the channel configured so that the chart renders the digest Lease Role.
as_admin create secret generic team-digest --from-literal=url=https://digest-sink.team-e2e.invalid/hook
as_admin apply -f - <<YAML
apiVersion: barmancloud.cnpg.io/v1
kind: ObjectStore
metadata: {name: team-store}
spec:
  configuration:
    destinationPath: s3://e2e-backups/ledger
    endpointURL: http://minio.minio.svc:9000
    s3Credentials:
      accessKeyId: {name: team-s3, key: ACCESS_KEY_ID}
      secretAccessKey: {name: team-s3, key: ACCESS_SECRET_KEY}
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: ledger}
spec:
  instances: 1
  storage: {size: 1Gi}
  plugins:
    - name: barman-cloud.cloudnative-pg.io
      isWALArchiver: true
      parameters: {barmanObjectName: team-store}
YAML
as_admin wait --for=condition=Ready cluster/ledger --timeout=900s

log "as ${ADMIN}: a ScheduledBackup with immediate: true, and its Backup"
# CloudNativePG's mutating webhook can be briefly away (a manager restart on a
# loaded host): retry the create a few times rather than fail the run.
scheduled_backup() {
  as_admin apply -f - <<YAML
apiVersion: postgresql.cnpg.io/v1
kind: ScheduledBackup
metadata: {name: ledger-daily}
spec:
  schedule: "0 0 3 * * *"
  immediate: true
  method: plugin
  pluginConfiguration: {name: barman-cloud.cloudnative-pg.io}
  cluster: {name: ledger}
YAML
}
for attempt in 1 2 3 4 5 6; do
  scheduled_backup && break
  [[ "${attempt}" -lt 6 ]] || fail "the ScheduledBackup could not be created"
  sleep 10
done
backup_phases() { as_admin get backups.postgresql.cnpg.io -o jsonpath='{.items[*].status.phase}'; }
backup_completed() {
  local phases
  phases="$(backup_phases)"
  [[ "${phases}" != *failed* ]] || fail "a Backup failed: ${phases}"
  [[ "${phases}" == *completed* ]]
}
wait_for "the immediate Backup completes" 600 backup_completed

log "as ${ADMIN}: helm install charts/pgtools (console, an evidence agent, pgAdmin)"
split_ref() { printf '%s' "${1%:*}"; }
tag_of() { printf '%s' "${1##*:}"; }
cat >"${work}/values.yaml" <<YAML
console:
  replicas: 2
  cookieSecure: "false"
  publicURL: ${PUBLIC_URL}
  history: {persistent: true, size: 1Gi}
  digest:
    delivery:
      webhook: {existingSecret: team-digest, key: url}
  mcp: {enabled: true}
  service: {type: NodePort}
  image: {repository: $(split_ref "${CONSOLE_IMG}"), tag: "$(tag_of "${CONSOLE_IMG}")", pullPolicy: Never}
evidence:
  image: {repository: $(split_ref "${EVIDENCE_IMG}"), tag: "$(tag_of "${EVIDENCE_IMG}")", pullPolicy: Never}
  agents:
    # MinIO in the test speaks plain http.
    - {cluster: ledger, objectStore: team-store, secrets: [team-s3], allowInsecureEndpoint: true}
pgadmin:
  enabled: true
  # The sidecar and the init containers run the operator image's binary.
  tools:
    image: {repository: $(split_ref "${MANAGER_IMG}"), tag: "$(tag_of "${MANAGER_IMG}")", pullPolicy: Never}
  # The loaded image, by tag: the chart's default digest names another build.
  image: {repository: $(split_ref "${PGADMIN_IMAGE}"), tag: "$(tag_of "${PGADMIN_IMAGE}")", digest: "", pullPolicy: Never}
  admins: [ops@example.org]
  auth:
    provider: oidc
    # Not reachable: pgAdmin reads the issuer's metadata when someone signs in,
    # not at start; this test never signs in to pgAdmin.
    oidc:
      issuerURL: https://idp.team-e2e.invalid/realms/team
      clientID: pgadmin
      clientSecret: {existingSecret: pgadmin-oidc}
  servers:
    - cluster: ledger
      endpoint: rw
      credentials:
        application: {secret: ledger-app}
YAML
helm_admin install "${RELEASE}" charts/pgtools -f "${work}/values.yaml" --wait --timeout 600s

log "the release holds only namespaced kinds"
helm_admin get manifest "${RELEASE}" | sed -n 's/^kind: //p' | sort -u >"${work}/release-kinds"
kubectl api-resources --namespaced=false --no-headers | awk '{ print $NF }' | sort -u >"${work}/cluster-kinds"
echo "[team] kinds in the release: $(tr '\n' ' ' <"${work}/release-kinds")"
for kind in Deployment Role RoleBinding ServiceAccount Service; do
  grep -qx "${kind}" "${work}/release-kinds" || fail "the release lacks a ${kind}"
done
[[ -z "$(comm -12 "${work}/release-kinds" "${work}/cluster-kinds")" ]] ||
  fail "the release holds cluster-scoped kinds: $(comm -12 "${work}/release-kinds" "${work}/cluster-kinds" | tr '\n' ' ')"
[[ -z "$(kubectl get crd -o name | grep 'pgtoolbox.fyannk.dev' || true)" ]] ||
  fail "the pgtoolbox CRDs are installed: this is not team mode without the operator"

log "persistent history: a StatefulSet, both replicas Ready, both claims Bound"
[[ "$(as_admin get statefulset "${CONSOLE}" -o jsonpath='{.kind}' 2>/dev/null)" == StatefulSet ]] ||
  fail "console.history.persistent=true did not render a StatefulSet named ${CONSOLE}"
[[ -z "$(as_admin get deployment "${CONSOLE}" -o name --ignore-not-found)" ]] ||
  fail "a Deployment ${CONSOLE} still exists next to the StatefulSet"
sts_ready() { [[ "$(as_admin get statefulset "${CONSOLE}" -o jsonpath='{.status.readyReplicas}')" == 2 ]]; }
wait_for "the console StatefulSet has 2 ready replicas" 300 sts_ready
for ordinal in 0 1; do
  claim="history-${CONSOLE}-${ordinal}"
  [[ "$(as_admin get pvc "${claim}" -o jsonpath='{.status.phase}' 2>/dev/null)" == Bound ]] ||
    fail "PVC ${claim} is '$(as_admin get pvc "${claim}" -o jsonpath='{.status.phase}' 2>&1)', want Bound"
  echo "[team] ok: ${claim} is Bound"
done

log "the digest Lease Role the namespace admin installed (leases: get/update by name, create)"
lease="${CONSOLE}-digest"
[[ "$(as_admin get role "${lease}" -o jsonpath='{.rules[*].resources}' 2>/dev/null)" == *leases* ]] ||
  fail "the digest channel did not render the Role ${lease} on leases: the install is the escalation check, and it did not pass or the chart did not render it"
as_admin get rolebinding "${lease}" -o jsonpath='{.subjects[0].name}' | grep -qx "${CONSOLE}" ||
  fail "RoleBinding ${lease} does not bind the console's ServiceAccount ${CONSOLE}"
[[ "$(as_admin get role "${lease}" -o jsonpath='{.rules[?(@.resourceNames)].resourceNames[0]}')" == "${lease}" ]] ||
  fail "the Role ${lease} does not scope get/update to the Lease ${lease} by name"
echo "[team] ok: ${lease} Role and RoleBinding exist, installed by ${ADMIN} with no cluster-wide grant"

log "every Pod of the release is Ready"
kubectl -n "${NS}" wait --for=condition=Ready pod -l "app.kubernetes.io/instance=${RELEASE}" --timeout=300s
kubectl -n "${NS}" get pods -o wide

log "pgAdmin's sidecar verified the Cluster's application credential"
pgadmin_verified() {
  kubectl -n "${NS}" logs "deployment/${RELEASE}-pgadmin" -c passfile-sync --tail=-1 |
    grep 'server credential verified' | grep -qF 'ledger rw (application)'
}
wait_for "passfile-sync logs \"server credential verified\" for ledger rw (application)" 300 pgadmin_verified

# The console: a person signs in with a ServiceAccount token. The ClusterRole
# pgtoolbox-console-viewer exists only with the operator chart; the team binds a Role
# with the same read rules.
log "as ${ADMIN}: a viewer (Role with the rules of pgtoolbox-console-viewer) and a stranger (no binding)"
as_admin apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: viewer}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: stranger}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: console-viewer}
rules:
  - {apiGroups: [postgresql.cnpg.io], resources: [backups, clusters, databaseroles, databases, failoverquorums, imagecatalogs, poolers, publications, scheduledbackups, subscriptions], verbs: [get, list, watch]}
  - {apiGroups: [barmancloud.cnpg.io], resources: [objectstores], verbs: [get, list, watch]}
  - {apiGroups: [""], resources: [events, persistentvolumeclaims, pods, services], verbs: [get, list, watch]}
  - {apiGroups: [""], resources: [pods/log], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: console-viewer}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: console-viewer}
subjects: [{kind: ServiceAccount, name: viewer, namespace: ${NS}}]
YAML

node_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CLUSTER_NAME}-control-plane")"
node_port="$(kubectl -n "${NS}" get svc -l "app.kubernetes.io/instance=${RELEASE}" -o jsonpath='{.items[?(@.spec.type=="NodePort")].spec.ports[0].nodePort}')"
[[ -n "${node_ip}" && -n "${node_port}" ]] || fail "no console address (node '${node_ip}', port '${node_port}')"
base="http://${node_ip}:${node_port}"
echo "[team] console at ${base}"
# MCP is served by the chart's one MCP Pod behind the ClusterIP Service
# ${RELEASE}-console-mcp; the test reaches it through a NodePort of its own
# (the chart's Service is never changed: helm owns it).
as_admin expose deployment "${RELEASE}-console-mcp" --name team-e2e-mcp --type NodePort --port 80 --target-port http >/dev/null
mcp_port="$(kubectl -n "${NS}" get svc team-e2e-mcp -o jsonpath='{.spec.ports[0].nodePort}')"
[[ -n "${mcp_port}" ]] || fail "no NodePort for the MCP Pod"
mcp_base="http://${node_ip}:${mcp_port}"
echo "[team] MCP at ${mcp_base}"

# status <jar> <path>: the HTTP status of one API call, the body in ${work}/body.
status() { curl -s -o "${work}/body" -w '%{http_code}' --max-time 20 -b "${work}/$1.jar" "${base}$2" || echo curl-error; }

# With console.publicURL set the console's CSRF check expects that origin, not the
# node address the test reaches it on.
sign_in() { # <serviceaccount>
  local token code i
  token="$(as_admin create token "$1" --duration=1h)"
  : >"${work}/$1.jar"
  for i in $(seq 1 20); do
    code="$(curl -s -o "${work}/body" -w '%{http_code}' --max-time 20 -c "${work}/$1.jar" -H "Origin: ${PUBLIC_URL}" \
      --data-urlencode "token=${token}" --data-urlencode "rd=/" "${base}/auth/token" || echo curl-error)"
    [[ "${code}" == 302 ]] && { echo "[team] ok: ${1} signs in with its token"; return; }
    sleep 3
  done
  fail "sign-in as $1: HTTP ${code}: $(head -c 300 "${work}/body")"
}
sign_in viewer
sign_in stranger

path="/api/v1/namespaces/${NS}/clusters/ledger/backups"
evidence_live() {
  [[ "$(status viewer "${path}")" == 200 ]] || return 1
  cp "${work}/body" "${OUT}/last-backups.json"
  grep -Eq '"evidence":\{[[:space:]]*"state":[[:space:]]*"live"' "${work}/body"
}
wait_for "the viewer reads ${path} and evidence.state is live" 600 evidence_live

# No binding, no view: the same call is refused, and nothing leaks the Cluster.
[[ "$(status stranger "${path}")" == 403 ]] || fail "the stranger got HTTP $(status stranger "${path}") for ${path}, want 403"
echo "[team] ok: a ServiceAccount with no binding is refused (403)"

# MCP (1.3.0): the same ServiceAccount tokens, as bearers. Raw API-server tokens
# are accepted at /mcp while auth.tokenLogin is on (break-glass); every tool then
# runs as the token's owner, authorized by the API server like the screen it
# mirrors. The transport is stateless streamable HTTP; the console answers JSON,
# and an SSE reply would carry the message after "data: ".
log "MCP: initialize, tools/list and list_clusters as the viewer; the stranger sees nothing"
mcp_id=0
# mcp_call <serviceaccount> <method> <params-json>: the JSON-RPC response in ${work}/mcp.json.
mcp_call() {
  local token code
  mcp_id=$((mcp_id + 1))
  token="$(cat "${work}/$1.token")"
  code="$(curl -s -o "${work}/mcp.raw" -w '%{http_code}' --max-time 30 -X POST \
    -H "Authorization: Bearer ${token}" -H 'content-type: application/json' \
    -H 'accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-06-18' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":${mcp_id},\"method\":\"$2\",\"params\":$3}" "${mcp_base}/mcp" || echo curl-error)"
  [[ "${code}" == 200 ]] || fail "MCP $2 as $1: HTTP ${code}, want 200: $(head -c 300 "${work}/mcp.raw")"
  # Strip the SSE framing when there is one; plain JSON passes through.
  sed -n 's/^data: *//p' "${work}/mcp.raw" >"${work}/mcp.json"
  [[ -s "${work}/mcp.json" ]] || cp "${work}/mcp.raw" "${work}/mcp.json"
  if grep -q '"error":{' "${work}/mcp.json"; then
    fail "MCP $2 as $1: JSON-RPC error: $(head -c 300 "${work}/mcp.json")"
  fi
  return 0
}
for sa in viewer stranger; do as_admin create token "${sa}" --duration=1h >"${work}/${sa}.token"; done

# The MCP Pod answers 503 until it holds the Lease and has loaded the grants.
mcp_unauthorized() {
  [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -X POST -H 'content-type: application/json' \
    -H 'accept: application/json, text/event-stream' --data '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' "${mcp_base}/mcp")" == 401 ]]
}
wait_for "the MCP Pod leads" 180 mcp_unauthorized
# Without a bearer the endpoint challenges, it does not serve.
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -X POST -H 'content-type: application/json' \
  -H 'accept: application/json, text/event-stream' --data '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' "${mcp_base}/mcp" || echo curl-error)"
[[ "${code}" == 401 ]] || fail "POST /mcp with no bearer: HTTP ${code}, want 401"
echo "[team] ok: /mcp without a bearer is refused (401)"

mcp_call viewer initialize '{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"team-e2e","version":"1"}}'
grep -q '"name":"pgtoolbox"' "${work}/mcp.json" || fail "MCP initialize as viewer: serverInfo is not pgtoolbox: $(head -c 300 "${work}/mcp.json")"
echo "[team] ok: viewer initializes against /mcp"

mcp_call viewer tools/list '{}'
for tool in list_clusters cluster_status; do
  grep -q "\"name\":\"${tool}\"" "${work}/mcp.json" || fail "MCP tools/list as viewer lacks ${tool}: $(head -c 300 "${work}/mcp.json")"
done
# writeTools is off: no write tool may be offered.
for tool in backup reload restart promote; do
  if grep -q "\"name\":\"${tool}\"" "${work}/mcp.json"; then
    fail "MCP tools/list offers the write tool ${tool} although console.mcp.writeTools is off"
  fi
done
echo "[team] ok: viewer lists the read tools and no write tool"

mcp_list_clusters_has_ledger() {
  mcp_call viewer tools/call '{"name":"list_clusters","arguments":{}}'
  if grep -q '"isError":true' "${work}/mcp.json"; then
    fail "MCP list_clusters as viewer is a tool error: $(head -c 300 "${work}/mcp.json")"
  fi
  grep -q '"name":"ledger"' "${work}/mcp.json"
}
wait_for "MCP list_clusters as viewer returns the Cluster ledger" 300 mcp_list_clusters_has_ledger
cp "${work}/mcp.json" "${OUT}/last-mcp-list-clusters.json"

# The stranger's token is valid (the sign-in above accepted it) but the API
# server grants it nothing: list_clusters is a success with no Cluster in it
# (the list is per user), and a per-Cluster tool is a tool error with no data.
mcp_call stranger tools/call '{"name":"list_clusters","arguments":{}}'
if grep -q 'ledger' "${work}/mcp.json"; then
  fail "MCP list_clusters as stranger names a Cluster: $(head -c 300 "${work}/mcp.json")"
fi
echo "[team] ok: list_clusters as stranger names no Cluster"
mcp_call stranger tools/call '{"name":"cluster_status","arguments":{"namespace":"'"${NS}"'","name":"ledger"}}'
grep -q '"isError":true' "${work}/mcp.json" || fail "MCP cluster_status ledger as stranger is not a tool error: $(head -c 300 "${work}/mcp.json")"
if grep -qE '"(phase|primary|instances)"' "${work}/mcp.json"; then
  fail "MCP cluster_status ledger as stranger returned Cluster data: $(head -c 300 "${work}/mcp.json")"
fi
echo "[team] ok: cluster_status ledger as stranger is a tool error with no data"

# The state the replicas share: the console's one own-identity write.
# since.json holds the "since" times of what the console observed, written once
# the read model has seen the Cluster; Helm created the ConfigMap with `data: {}`.
log "the shared state ConfigMap ${CONSOLE}-state is written"
[[ -n "$(as_admin get configmap "${CONSOLE}-state" -o name --ignore-not-found)" ]] ||
  fail "ConfigMap ${CONSOLE}-state does not exist"
state_written() {
  [[ -n "$(as_admin get configmap "${CONSOLE}-state" -o jsonpath='{.data.since\.json}')" ]]
}
wait_for "ConfigMap ${CONSOLE}-state holds since.json (the console wrote it)" 180 state_written
as_admin get configmap "${CONSOLE}-state" -o jsonpath='{.data.since\.json}' | grep -q '{' ||
  fail "since.json in ${CONSOLE}-state is not a JSON object"

log "the agent served the console"
agent_served() {
  kubectl -n "${NS}" logs -l app.kubernetes.io/name=evidence-agent --tail=-1 |
    grep -E '"route":"(snapshot|backups)"' | grep -q '"status":200'
}
wait_for "the evidence agent's log shows 200 answers to the console" 120 agent_served

log "team mode e2e passed"
