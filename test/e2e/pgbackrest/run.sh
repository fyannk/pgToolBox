#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# pgBackRest on kind: one cell per CNPG-I plugin, both on one
# CloudNativePG 1.30 cluster, both archiving to MinIO over TLS with a CA the
# test creates (the agent verifies against it; it trusts nothing else):
#
#   cell dalibo  pgbackrest.dalibo.com v0.0.3, a Stanza  (namespace pgbr-dalibo)
#   cell opera   pgbackrest.cnpg.opera.com v0.9.0, an Archive  (namespace pgbr-opera)
#
# Per cell, a real full backup, a real differential backup and switched WAL;
# then both agent modes read the repository:
#
#   * an operator-managed EvidenceAgent (the pgtoolbox operator, chart
#     charts/pgtoolbox) read by an operator-managed PgToolBoxConsole;
#   * a self-resolving agent installed by charts/pgtools as a namespace admin,
#     read by the chart's console.
#
# and the same three things are asserted about each (test/e2e/pgbackrest/
# check_test.go, run once per assertion):
#
#   1. the agent publishes pgbackrest/v1alpha1 evidence on the evidence/v1alpha1
#      channel: stanza, backups and their chain, WAL ranges, recovery paths and
#      the recoverability window (read straight from the agent);
#   2. the console's cluster backups API shows the stanza, the recovery window
#      and the Backup objects joined to the repository's labels;
#   3. broken cases are reported, not hidden: a CA the repository's certificate
#      does not chain to (TLS verification fails; the evidence is stale, no
#      window is shown, and the agent says x509), and a missing WAL segment (a
#      confirmed gap, critical, with the window ending before it).
#
# A restore drill runs in each cell, once through the PgToolBoxConsole
# (the operator mirrors the CAs) and once through the chart's console (drills
# signed by the source's server CA, RESTORE_DRILL_SERVER_CA=source, which the
# admin-filled instance CA bundle already holds), and must pass in both.
#
#   CLUSTER_NAME=pgbr-e2e-local ./test/e2e/pgbackrest/run.sh
#   KEEP_CLUSTER=1   leave the cluster up.
#   REUSE_CLUSTER=1  use the cluster KUBECONFIG points at (CLUSTER_NAME names it
#                    for the image load): it is neither created nor deleted;
#                    the scenario's own namespaces and releases are removed first.
#   SKIP_BUILD=1     use the images CLUSTER's tags already name (local reruns).
#   Failure dumps land in artifacts/e2e-pgbackrest/.
#
# Heavy tier only (ci.yml e2e-pgbackrest: push to main, nightly, dispatch,
# ci:full). It needs a kind cluster to itself; do not run it on a loaded host.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "${repo_root}"
# The script's own stdout and stderr, for the EXIT trap: a fail inside a
# wait_for predicate exits with that predicate's output redirected, and the
# failure dump must still reach the job log.
exec 3>&1 4>&2

CLUSTER_NAME="${CLUSTER_NAME:-pgbr-e2e-local}"
# shellcheck source=hack/e2e-cnpg-stack.sh
source "${repo_root}/hack/e2e-cnpg-stack.sh"
# shellcheck source=hack/e2e-pgbackrest-stack.sh
source "${repo_root}/hack/e2e-pgbackrest-stack.sh"
MANAGER_IMG="${MANAGER_IMG:-pgtoolbox-operator:e2e-pgbackrest}"
CONSOLE_IMG="${CONSOLE_IMG:-pgtoolbox-console:e2e-pgbackrest}"
EVIDENCE_IMG="${EVIDENCE_IMG:-pgtoolbox-evidence-agent:e2e-pgbackrest}"
OUT="${E2E_ARTIFACTS:-artifacts/e2e-pgbackrest}"
ADMIN=team-admin
CLUSTER=ledger
RELEASE=team

if [[ -n "${E2E_REQUIRE_KUBECONFIG:-}" ]]; then
  case "${KUBECONFIG:-}" in
    "" | *:* | "${HOME}/.kube/config")
      echo "[pgbackrest] E2E_REQUIRE_KUBECONFIG needs KUBECONFIG set to one private file, got '${KUBECONFIG:-}'" >&2
      exit 1 ;;
  esac
fi
for tool in kind kubectl helm docker curl openssl make go sha256sum base64; do
  command -v "${tool}" >/dev/null || { echo "missing required tool: ${tool}" >&2; exit 1; }
done

mkdir -p "${OUT}"
work="$(mktemp -d "${TMPDIR:-/tmp}/ptb-pgbackrest-e2e.XXXXXX")"
sampler_pid=""

# ---- timing and resource use ---------------------------------------------

started=${SECONDS}
phase_name=""
phase_start=${SECONDS}
: >"${work}/phases"
# phase <name>: close the previous phase, open this one.
phase() {
  local now=${SECONDS}
  if [[ -n "${phase_name}" ]]; then
    printf '%5ds  %s\n' "$((now - phase_start))" "${phase_name}" >>"${work}/phases"
  fi
  phase_name="$*"
  phase_start=${now}
  log "$*"
}

# sample_node_usage records the kind node's memory and CPU every 20 s: the
# control plane and every Pod of the scenario are in that one container.
sample_node_usage() {
  while :; do
    docker stats --no-stream --format '{{.MemUsage}} {{.CPUPerc}}' "${CLUSTER_NAME}-control-plane" 2>/dev/null |
      awk -v t="$(date +%s)" '{ print t, $0 }' >>"${work}/usage" || true
    sleep 20
  done
}

# report_usage prints the phase table and the peak of the samples.
report_usage() {
  [[ -n "${phase_name}" ]] && printf '%5ds  %s\n' "$((SECONDS - phase_start))" "${phase_name}" >>"${work}/phases"
  phase_name=""
  echo
  echo "==> wall time by phase (total $((SECONDS - started))s)"
  cat "${work}/phases"
  if [[ -s "${work}/usage" ]]; then
    # "1.234GiB / 27.9GiB 12.3%": MiB of the first figure, and the CPU percent.
    awk '{
      v = $2; u = v; gsub(/[0-9.]/, "", u); sub(u "$", "", v)
      mib = (u == "GiB") ? v * 1024 : (u == "MiB") ? v : (u == "KiB") ? v / 1024 : v / 1048576
      cpu = $5; sub(/%/, "", cpu)
      if (mib > peak) peak = mib
      if (cpu + 0 > cpupeak) cpupeak = cpu + 0
      n++; total += mib; cputotal += cpu
    } END {
      if (n) printf "kind node: peak memory %.0f MiB (mean %.0f), peak CPU %.0f%% of one core (mean %.0f%%), %d samples\n", peak, total / n, cpupeak, cputotal / n, n
    }' "${work}/usage"
  fi
}

# ---- failure dump and cleanup --------------------------------------------

dump() {
  log "failed: dumping state to ${OUT}"
  kubectl get pods,deploy,svc,pvc,cluster,backup,stanza,archive,evidenceagent,pgtoolboxconsole -A -o wide >"${OUT}/objects.txt" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp >"${OUT}/events.txt" 2>&1 || true
  local ns
  for ns in pgbr-dalibo pgbr-opera; do
    kubectl -n "${ns}" get cluster "${CLUSTER}" -o yaml >"${OUT}/${ns}-cluster.yaml" 2>&1 || true
    kubectl -n "${ns}" get stanza,archive,evidenceagent,pgtoolboxconsole -o yaml >"${OUT}/${ns}-objects.yaml" 2>&1 || true
    kubectl -n "${ns}" describe pods >"${OUT}/${ns}-describe-pods.txt" 2>&1 || true
    kubectl -n "${ns}" logs -l app.kubernetes.io/component=evidence-agent --all-containers --tail=-1 --prefix >"${OUT}/${ns}-agents.log" 2>&1 || true
    kubectl -n "${ns}" logs -l app.kubernetes.io/component=console --all-containers --tail=-1 --prefix >"${OUT}/${ns}-consoles.log" 2>&1 || true
    kubectl -n "${ns}" logs "${CLUSTER}-1" --all-containers --tail=300 --prefix >"${OUT}/${ns}-postgres.log" 2>&1 || true
  done
  kubectl -n cnpg-system logs -l app=pgbackrest --all-containers --tail=200 >"${OUT}/plugin-opera.log" 2>&1 || true
  kubectl -n cnpg-system logs -l app=pgbackrest-controller --all-containers --tail=200 >"${OUT}/plugin-dalibo.log" 2>&1 || true
  kubectl -n cnpg-system logs deployment/cnpg-controller-manager --tail=300 >"${OUT}/cnpg.log" 2>&1 || true
  kubectl -n pgtoolbox logs deployment/pgtoolbox --tail=300 >"${OUT}/operator.log" 2>&1 || true
  kubectl -n minio logs deployment/minio --tail=100 >"${OUT}/minio.log" 2>&1 || true
  if [[ "${REUSE_CLUSTER:-}" != 1 ]]; then
    kind export logs --name "${CLUSTER_NAME}" "${OUT}/kind" >/dev/null 2>&1 || true
  fi
  tail -n 5 "${work}/wait.err" 2>/dev/null || true
  # The runner keeps no artifacts: print what usually explains a failure.
  tail -n 25 "${OUT}"/*-agents.log 2>/dev/null || true
  echo "==> pod states"
  grep -E 'pgbr-|cnpg-system|minio|pgtoolbox' "${OUT}/objects.txt" 2>/dev/null | head -n 60 || true
  tail -n 30 "${OUT}/events.txt" 2>/dev/null || true
}

cleanup() {
  local status=$?
  exec 1>&3 2>&4
  [[ -z "${sampler_pid}" ]] || kill "${sampler_pid}" 2>/dev/null || true
  ((status == 0)) || dump
  report_usage || true
  rm -rf "${work:?}"
  if [[ "${KEEP_CLUSTER:-}" == "1" || "${REUSE_CLUSTER:-}" == "1" ]]; then
    log "leaving cluster ${CLUSTER_NAME} up (KEEP_CLUSTER / REUSE_CLUSTER)"
  else
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  exit "${status}"
}
trap cleanup EXIT

fail() { echo "[pgbackrest] FAIL: $*" >&2; exit 1; }

# wait_for <what> <seconds> <command...>: poll until the command succeeds.
wait_for() {
  local what=$1 limit=$2 i=0
  shift 2
  until "$@" >/dev/null 2>>"${work}/wait.err"; do
    i=$((i + 3))
    ((i <= limit)) || fail "${what}: not true after ${limit}s"
    sleep 3
  done
  echo "[pgbackrest] ok: ${what}"
}

# retry <attempts> <command...> is in hack/lib/net.sh: CloudNativePG's webhook can
# be briefly away (a manager restart on a loaded host), so the applies below use it.

as_admin() { kubectl --as="${ADMIN}" -n "${NS}" "$@"; }
helm_admin() { helm --kube-as-user "${ADMIN}" --namespace "${NS}" "$@"; }

# check <assertion flags...>: one assertion of check_test.go, fatal when it does
# not hold in time. Its output is the evidence of the run.
check() {
  if ! "${work}/check.test" -test.run '^TestCheck$' -test.v -namespace "${NS}" -plugin "${FLAVOR}" "$@" >"${work}/check.out" 2>&1; then
    cat "${work}/check.out" >&2
    fail "assertion failed: $*"
  fi
  grep -E 'check_test.go' "${work}/check.out" | sed 's/^ *check_test.go:[0-9]*: /[pgbackrest] /' || true
}

# ---- the cluster ----------------------------------------------------------

if [[ "${SKIP_BUILD:-}" != 1 ]]; then
  phase "building the operator, console and evidence agent images"
  make docker-build docker-build-console docker-build-evidence \
    OPERATOR_IMG="${MANAGER_IMG}" CONSOLE_IMG="${CONSOLE_IMG}" EVIDENCE_IMG="${EVIDENCE_IMG}" VERSION=e2e
fi

phase "building the assertions (test/e2e/pgbackrest)"
go test -c -tags e2epgbackrest -o "${work}/check.test" ./test/e2e/pgbackrest

if [[ "${REUSE_CLUSTER:-}" == 1 ]]; then
  phase "using the existing cluster ${CLUSTER_NAME}: removing this scenario's namespaces and releases"
  helm uninstall "${RELEASE}" -n pgbr-dalibo >/dev/null 2>&1 || true
  helm uninstall "${RELEASE}" -n pgbr-opera >/dev/null 2>&1 || true
  # The Clusters first, while their Archive or Stanza and Secrets still exist:
  # PostgreSQL archives its last WAL at shutdown through the plugin sidecar, and
  # a Pod that loses them first waits out its whole stopDelay.
  for ns in pgbr-dalibo pgbr-opera; do
    kubectl -n "${ns}" delete clusters.postgresql.cnpg.io --all --ignore-not-found --wait=true --timeout=180s 2>/dev/null || true
  done
  kubectl delete namespace pgbr-dalibo pgbr-opera minio --ignore-not-found --wait=true --timeout=300s
else
  phase "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE})"
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  hack/kind-create.sh --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config hack/kind/ci.yaml --wait 300s
fi
sample_node_usage &
sampler_pid=$!

phase "CloudNativePG ${CNPG_VERSION} and cert-manager"
if [[ "${REUSE_CLUSTER:-}" == 1 ]] && kubectl -n cnpg-system get deployment cnpg-controller-manager >/dev/null 2>&1; then
  echo "[pgbackrest] CloudNativePG is already installed in ${CLUSTER_NAME}"
else
  install_cnpg_base
fi
kind_load "${MANAGER_IMG}" "${CONSOLE_IMG}" "${EVIDENCE_IMG}"

phase "the pgBackRest plugins (Dalibo ${DALIBO_PLUGIN_VERSION}, Opera ${OPERA_PLUGIN_VERSION}) and MinIO over TLS"
install_pgbackrest_plugins "${work}/plugins"
kubectl apply -f test/e2e/pgbackrest/minio-tls.yaml
kubectl -n minio wait --for=condition=Ready certificate/minio-tls --timeout=180s
kubectl -n minio rollout status deployment/minio --timeout=300s

phase "the pgtoolbox operator"
helm upgrade --install pgtoolbox charts/pgtoolbox \
  --namespace pgtoolbox --create-namespace \
  --set image.repository="${MANAGER_IMG%%:*}" --set image.tag="${MANAGER_IMG##*:}" --set image.pullPolicy=Never \
  --set operator.enabled=true --set replicaCount=1 \
  --set operator.evidenceAgent.image="${EVIDENCE_IMG}" \
  --set operator.console.image.repository="${CONSOLE_IMG%%:*}" --set operator.console.image.tag="${CONSOLE_IMG##*:}" \
  --wait --timeout 300s

# The CA the agent must NOT trust: a self-signed root that signed nothing.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout /dev/null \
  -subj "/CN=not-the-minio-ca" -days 1 -out "${work}/wrong-ca.crt" 2>/dev/null

access_key="$(kubectl -n minio get secret minio-credentials -o jsonpath='{.data.ACCESS_KEY_ID}' | base64 -d)"
secret_key="$(kubectl -n minio get secret minio-credentials -o jsonpath='{.data.ACCESS_SECRET_KEY}' | base64 -d)"
minio_ca="$(kubectl -n minio get secret minio-ca -o jsonpath='{.data.ca\.crt}' | base64 -d)"

# ---- one cell --------------------------------------------------------------

# cell_objects <flavor>: the plugin's own object and the Cluster that archives
# through it, as the team admin.
cell_objects() {
  if [[ "${FLAVOR}" == dalibo ]]; then
    as_admin apply -f - <<YAML
apiVersion: pgbackrest.dalibo.com/v1
kind: Stanza
metadata: {name: store}
spec:
  stanzaConfiguration:
    name: ${CLUSTER}
    s3Repositories:
      - bucket: pgbr-dalibo
        endpoint: minio.minio.svc:9000
        region: us-east-1
        repoPath: /repo
        uriStyle: path
        # Dalibo's plugin has no CA field: it verifies nothing here. The agent
        # verifies against the CA it is given, whatever this says.
        verifyTLS: false
        retentionPolicy: {full: 2, fullType: count}
        secretRef:
          accessKeyId: {name: s3, key: ACCESS_KEY_ID}
          secretAccessKey: {name: s3, key: ACCESS_SECRET_KEY}
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: ${CLUSTER}}
spec:
  instances: 1
  storage: {size: 1Gi}
  plugins:
    - name: pgbackrest.dalibo.com
      enabled: true
      isWALArchiver: true
      parameters: {stanzaRef: store}
YAML
  else
    as_admin apply -f - <<YAML
apiVersion: pgbackrest.cnpg.opera.com/v1
kind: Archive
metadata: {name: store}
spec:
  configuration:
    compression: gz
    repositories:
      - destinationPath: /repo
        bucket: pgbr-opera
        endpointURL: minio.minio.svc:9000
        # Opera's plugin verifies the endpoint against this CA.
        endpointCA: {name: minio-ca, key: ca.crt}
        s3Credentials:
          region: us-east-1
          uriStyle: path
          accessKeyId: {name: s3, key: ACCESS_KEY_ID}
          secretAccessKey: {name: s3, key: ACCESS_SECRET_KEY}
        retention: {full: 2}
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: ${CLUSTER}}
spec:
  instances: 1
  storage: {size: 1Gi}
  plugins:
    - name: pgbackrest.cnpg.opera.com
      isWALArchiver: true
      parameters: {pgbackrestObjectName: store}
YAML
  fi
}

psql_primary() { as_admin exec "${CLUSTER}-1" -c postgres -- psql -U postgres -qAt -c "$1"; }

# take_backup <name> <full|diff>: a real backup through the plugin.
take_backup() {
  local name=$1 type=$2 key=type
  [[ "${FLAVOR}" == dalibo ]] && key=backupType
  retry 6 as_admin apply -f - <<YAML
apiVersion: postgresql.cnpg.io/v1
kind: Backup
metadata: {name: ${name}}
spec:
  method: plugin
  cluster: {name: ${CLUSTER}}
  pluginConfiguration:
    name: ${PLUGIN}
    parameters: {${key}: ${type}}
YAML
  # Done is completed or failed: the verdict is read after the wait, so a
  # failure is reported with the Backup's own error.
  backup_done() { [[ "$(as_admin get backup "${name}" -o jsonpath='{.status.phase}')" =~ ^(completed|failed)$ ]]; }
  wait_for "Backup ${name} (${type}) finished" 600 backup_done
  [[ "$(as_admin get backup "${name}" -o jsonpath='{.status.phase}')" == completed ]] ||
    fail "Backup ${name} failed: $(as_admin get backup "${name}" -o jsonpath='{.status.error}')"
}

# switch_wal <n>: n real segments, each archived through the plugin. The last
# switch returns the start of the next segment: the one before it is the last
# segment that must reach the repository.
switch_wal() {
  local i end
  for ((i = 0; i < $1; i++)); do
    end="$(psql_primary "insert into e2e_rows select g from generate_series(1, 2000) g; select pg_switch_wal();" | tail -n 1)"
    sleep 2
  done
  archived() { [[ "$(psql_primary "select coalesce(last_archived_wal >= pg_walfile_name('${end}'::pg_lsn - 1), false) from pg_stat_archiver")" == t ]]; }
  wait_for "the switched WAL is archived" 180 archived
}

run_cell() {
  FLAVOR=$1
  NS="pgbr-${FLAVOR}"
  local object_kind=Stanza bucket="pgbr-${FLAVOR}"
  PLUGIN=pgbackrest.dalibo.com
  if [[ "${FLAVOR}" == opera ]]; then
    PLUGIN=pgbackrest.cnpg.opera.com
    object_kind=Archive
  fi

  phase "cell ${FLAVOR}: the namespace, its team admin and the Cluster archiving through ${PLUGIN}"
  kubectl create namespace "${NS}"
  # The credentials the plugin object names, and the CA. Values are never printed.
  # The keys come through a process substitution (printf is a builtin), so they
  # never appear in the host's kubectl argv.
  kubectl -n "${NS}" create secret generic s3 --from-env-file=<(printf 'ACCESS_KEY_ID=%s\nACCESS_SECRET_KEY=%s\n' "${access_key}" "${secret_key}") >/dev/null
  kubectl -n "${NS}" create secret generic minio-ca --from-literal=ca.crt="${minio_ca}" >/dev/null
  kubectl -n "${NS}" create secret generic wrong-ca --from-file=ca.crt="${work}/wrong-ca.crt" >/dev/null
  # The team admin: the built-in admin role, plus the groups CloudNativePG, the
  # plugins and barman-cloud own (none aggregates into admin). The pgtools chart
  # renders Roles that read them, which an admin may grant only if it holds them.
  kubectl apply -f - <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: ${ADMIN}-admin, namespace: ${NS}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: admin}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: User, name: ${ADMIN}}]
---
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
---
# The people: viewer reads; dba reads and may create a drill Cluster; evidence-reader
# may get the EvidenceAgent, which is what the agent's SubjectAccessReview asks.
apiVersion: v1
kind: ServiceAccount
metadata: {name: viewer, namespace: ${NS}}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: dba, namespace: ${NS}}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: evidence-reader, namespace: ${NS}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: console-viewer, namespace: ${NS}}
rules:
  - {apiGroups: [postgresql.cnpg.io], resources: [backups, clusters, databaseroles, databases, failoverquorums, imagecatalogs, poolers, publications, scheduledbackups, subscriptions], verbs: [get, list, watch]}
  - {apiGroups: [""], resources: [events, persistentvolumeclaims, pods, services], verbs: [get, list, watch]}
  - {apiGroups: [""], resources: [pods/log], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: drill-runner, namespace: ${NS}}
rules:
  - {apiGroups: [postgresql.cnpg.io], resources: [clusters], verbs: [create]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: evidence-reader, namespace: ${NS}}
rules:
  - {apiGroups: [pgtoolbox.fyannk.dev], resources: [evidenceagents], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: console-viewer, namespace: ${NS}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: console-viewer}
subjects:
  - {kind: ServiceAccount, name: viewer, namespace: ${NS}}
  - {kind: ServiceAccount, name: dba, namespace: ${NS}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: drill-runner, namespace: ${NS}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: drill-runner}
subjects: [{kind: ServiceAccount, name: dba, namespace: ${NS}}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: evidence-reader, namespace: ${NS}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: evidence-reader}
subjects: [{kind: ServiceAccount, name: evidence-reader, namespace: ${NS}}]
YAML
  cell_objects
  as_admin wait --for=condition=Ready "cluster/${CLUSTER}" --timeout=900s

  phase "cell ${FLAVOR}: a real full backup, a real differential backup and switched WAL"
  psql_primary "create table e2e_rows as select g from generate_series(1, 5000) g" >/dev/null
  # A Cluster is Ready before its first WAL segment reaches the repository (and,
  # with the plugin, before the stanza is usable): a backup taken then fails.
  # CloudNativePG sets ContinuousArchiving once wal-archive succeeds, whatever
  # the archiving method.
  psql_primary "select pg_switch_wal()" >/dev/null
  as_admin wait --for=condition=ContinuousArchiving "cluster/${CLUSTER}" --timeout=600s
  take_backup full-1 full
  psql_primary "insert into e2e_rows select g from generate_series(1, 5000) g" >/dev/null
  switch_wal 2
  take_backup diff-1 diff
  # Segments after the newest backup: the missing-WAL case removes one of them.
  switch_wal 4

  phase "cell ${FLAVOR}: the operator-managed EvidenceAgent and PgToolBoxConsole"
  # The drill runs through the PgToolBoxConsole: the operator mirrors each Cluster's
  # CA, the drill Cluster's own included, into the console's instance CA
  # bundle (RESTORE_DRILL_SERVER_CA=own). A pgtools console signs its drills
  # with the source's server CA instead (RESTORE_DRILL_SERVER_CA=source), which
  # its admin-filled bundle already holds (console/restore-drills.md, "Which CA
  # signs a drill").
  local restore_drills='{"enabled": true, "networkPolicy": {"objectStore": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "minio"}}}], "objectStorePorts": [9000]}}'
  kubectl -n "${NS}" apply -f - <<YAML
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: EvidenceAgent
metadata: {name: ${CLUSTER}}
spec:
  clusterRef: {name: ${CLUSTER}}
  # TLS is verified against this CA, whatever the plugin object says.
  allowedSecrets: [s3, minio-ca, wrong-ca]
  refreshInterval: 30s
  pgbackrest:
    endpointCA: {name: minio-ca, key: ca.crt}
---
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxConsole
metadata: {name: console}
spec:
  replicas: 1
  history: {persistent: false}
  auth: {tokenLogin: true}
  evidence: {interval: 30s}
  restoreDrills: ${restore_drills}
YAML
  kubectl -n "${NS}" wait --for=condition=Ready "evidenceagent/${CLUSTER}" --timeout=300s
  kubectl -n "${NS}" wait --for=condition=Ready pgtoolboxconsole/console --timeout=300s
  [[ "$(kubectl -n "${NS}" get evidenceagent "${CLUSTER}" -o jsonpath='{.status.pgbackrest.kind}/{.status.pgbackrest.objectName}/{.status.pgbackrest.stanza}')" == "${object_kind}/store/${CLUSTER}" ]] ||
    fail "EvidenceAgent.status.pgbackrest does not name ${object_kind}/store/${CLUSTER}"

  phase "cell ${FLAVOR}: the self-resolving agent and the console of charts/pgtools, installed by ${ADMIN}"
  local agent_key=stanza
  [[ "${FLAVOR}" == opera ]] && agent_key=archive
  # The chart's instance CA bundle is the admin's to fill (the console reads no
  # Secret): the source Cluster's server CA. Drills run in the source's
  # namespace and are signed by that CA (restoreDrills.serverCA.mode auto =
  # source), so the console can verify them without a Secret verb.
  as_admin get secret "${CLUSTER}-ca" -o jsonpath='{.data.ca\.crt}' | base64 -d >"${work}/instance-ca-${FLAVOR}.crt"
  [[ -s "${work}/instance-ca-${FLAVOR}.crt" ]] || fail "the source Cluster's server CA (${CLUSTER}-ca) is empty"
  as_admin create configmap pgtoolbox-console-instance-ca --from-file=ca.crt="${work}/instance-ca-${FLAVOR}.crt" >/dev/null
  cat >"${work}/values-${FLAVOR}.yaml" <<YAML
console:
  replicas: 1
  restoreDrills:
    enabled: true
    networkPolicy:
      objectStore:
        to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: minio}}}]
        ports: [9000]
  cookieSecure: "false"
  history: {persistent: false}
  evidence: {interval: 30s}
  image: {repository: ${CONSOLE_IMG%:*}, tag: "${CONSOLE_IMG##*:}", pullPolicy: Never}
evidence:
  image: {repository: ${EVIDENCE_IMG%:*}, tag: "${EVIDENCE_IMG##*:}", pullPolicy: Never}
  agents:
    - cluster: ${CLUSTER}
      ${agent_key}: store
      secrets: [s3, minio-ca]
      pgbackrest:
        endpointCA: {name: minio-ca, key: ca.crt}
YAML
  helm_admin install "${RELEASE}" charts/pgtools -f "${work}/values-${FLAVOR}.yaml" --wait --timeout 600s

  phase "cell ${FLAVOR}: both agents publish pgbackrest/v1alpha1 evidence, both consoles show the stanza and the window"
  check -check agent-live -agent "${CLUSTER}" -agent-auth "operator:${CLUSTER}:evidence-reader"
  check -check agent-live -agent "${RELEASE}" -agent-auth "shared:${RELEASE}-evidence-token"
  check -check console-live -console console
  check -check console-live -console "${RELEASE}"

  phase "cell ${FLAVOR}: a CA the repository does not chain to is reported (TLS verification)"
  kubectl -n "${NS}" patch evidenceagent "${CLUSTER}" --type merge -p '{"spec":{"pgbackrest":{"endpointCA":{"name":"wrong-ca","key":"ca.crt"}}}}'
  check -check console-unavailable -console console -timeout 6m
  tls_reported() {
    kubectl -n "${NS}" logs "deployment/${CLUSTER}-evidence-agent" --tail=-1 | grep -q 'x509: certificate signed by unknown authority'
  }
  wait_for "the agent's log names the failed verification (x509: certificate signed by unknown authority)" 120 tls_reported
  kubectl -n "${NS}" patch evidenceagent "${CLUSTER}" --type merge -p '{"spec":{"pgbackrest":{"endpointCA":{"name":"minio-ca","key":"ca.crt"}}}}'
  kubectl -n "${NS}" wait --for=condition=Ready "evidenceagent/${CLUSTER}" --timeout=300s
  check -check console-live -console console -timeout 6m
  check -check agent-live -agent "${CLUSTER}" -agent-auth "operator:${CLUSTER}:evidence-reader"

  phase "cell ${FLAVOR}: a restore drill through the plugin, from the newest backup of the agent's window"
  check -check console-drill -console console -timeout 12m

  phase "cell ${FLAVOR}: a restore drill through the chart's console (server CA of the source, admin-filled bundle)"
  check -check console-drill -console "${RELEASE}" -timeout 12m

  phase "cell ${FLAVOR}: a missing WAL segment is reported (confirmed gap, critical, the window ends before it)"
  # The second newest archived segment: WAL after it exists, the backups' own
  # WAL does not depend on it.
  local segment
  # (the MinIO image has no grep: list there, choose here)
  segment="$(kubectl -n minio exec deploy/minio -- sh -c "ls /data/${bucket}/repo/archive/${CLUSTER}/*/*/" |
    grep -E '^[0-9A-F]{24}-' | sort | tail -n 2 | head -n 1)"
  [[ "${segment}" =~ ^[0-9A-F]{24}- ]] || fail "no WAL segment to remove in ${bucket} (got '${segment}')"
  kubectl -n minio exec deploy/minio -- sh -c "rm -rf /data/${bucket}/repo/archive/${CLUSTER}/*/*/${segment}"
  echo "[pgbackrest] removed WAL segment ${segment%%-*} from ${bucket}"
  # A gap is a candidate until two complete scans saw it. The operator's agent
  # scans every 30 s; the chart's every 5 minutes (CATALOG_REFRESH_INTERVAL), so
  # its answer comes up to ten minutes later: those checks run after both cells,
  # when the first cell's has long been due and the second's overlaps nothing.
  check -check agent-gap -agent "${CLUSTER}" -agent-auth "operator:${CLUSTER}:evidence-reader" -timeout 5m
  check -check console-gap -console console -timeout 5m
}

# chart_gap_checks <flavor>: the self-resolving agent and the chart's console.
chart_gap_checks() {
  FLAVOR=$1
  NS="pgbr-${FLAVOR}"
  PLUGIN=pgbackrest.dalibo.com
  [[ "${FLAVOR}" == opera ]] && PLUGIN=pgbackrest.cnpg.opera.com
  check -check agent-gap -agent "${RELEASE}" -agent-auth "shared:${RELEASE}-evidence-token" -timeout 12m
  check -check console-gap -console "${RELEASE}" -timeout 5m
}

# PGBR_FLAVORS (or E2E_VARIANT, which CI sets) picks the cells: CI runs one per
# job, in parallel; by hand both run, one after the other, on one kind cluster.
read -r -a flavors <<<"${PGBR_FLAVORS:-${E2E_VARIANT:-dalibo opera}}"
for flavor in "${flavors[@]}"; do
  [[ "${flavor}" =~ ^(dalibo|opera)$ ]] || fail "PGBR_FLAVORS: unknown cell '${flavor}' (dalibo, opera)"
done
for flavor in "${flavors[@]}"; do run_cell "${flavor}"; done
phase "the chart agents see the missing WAL too (scan every 5 minutes, a gap needs two scans)"
for flavor in "${flavors[@]}"; do chart_gap_checks "${flavor}"; done

phase "done"
log "pgBackRest e2e passed (${flavors[*]}): Dalibo ${DALIBO_PLUGIN_VERSION}, Opera ${OPERA_PLUGIN_VERSION}"
