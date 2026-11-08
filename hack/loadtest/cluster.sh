#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The load-test cluster: one kind node with the real workloads (the console, the
# operator, MinIO, the instance simulator) and KWOK fake nodes whose Pods get
# Running status and an IP without a kubelet. CloudNativePG's CRDs are
# installed, its operator is not: loadgen writes the Clusters and their status.
#
#   cluster.sh up        create everything (idempotent on a half-built cluster)
#   cluster.sh down      delete the kind cluster pgtoolbox-load and its state
#   cluster.sh status    what runs, and the endpoints
#   cluster.sh console   helm upgrade the console with the current settings
#   cluster.sh routes    re-route the KWOK Pod CIDRs to the node (after a node restart)
#
# Settings (environment), with their defaults:
#   NAMESPACES=4              namespaces lt-01.. the console watches (at most 16)
#   KWOK_NODES=10             fake nodes (at most 32)
#   CONSOLE_IMG=pgtoolbox-console:load OPERATOR_IMG=pgtoolbox:load   built from this tree when absent
#   BUILD_IMAGES=auto         auto | always | never
#   CONSOLE_CPU_LIMIT=2 CONSOLE_MEM_LIMIT=2Gi OPERATOR_CPU_LIMIT=2 OPERATOR_MEM_LIMIT=1Gi
#   INSTANCE_STATUS_INTERVAL=30s   the console's status sweep period
#   ALLOW_BUSY_HOST=0         up refuses to start on a busy host (a running e2e kind cluster
#                             or a high load average): the numbers would be wrong
#
# It touches only the kind cluster "pgtoolbox-load", with a private kubeconfig
# in hack/loadtest/.state/: never ~/.kube/config, never another kind cluster.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
STATE="${HERE}/.state"
CLUSTER_NAME="pgtoolbox-load"
NODE_CONTAINER="${CLUSTER_NAME}-control-plane"
export KUBECONFIG="${STATE}/kubeconfig"

NAMESPACES="${NAMESPACES:-4}"
KWOK_NODES="${KWOK_NODES:-10}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
KIND_IMAGE_PLATFORM="${KIND_IMAGE_PLATFORM:-linux/amd64}"
KWOK_VERSION="${KWOK_VERSION:-v0.8.0}"
METRICS_SERVER_VERSION="${METRICS_SERVER_VERSION:-v0.8.0}"
CONSOLE_IMG="${CONSOLE_IMG:-pgtoolbox-console:load}"
OPERATOR_IMG="${OPERATOR_IMG:-pgtoolbox-operator:load}"
SIM_IMG="${SIM_IMG:-pgtoolbox-loadgen:load}"
BUILD_IMAGES="${BUILD_IMAGES:-auto}"
CONSOLE_CPU_LIMIT="${CONSOLE_CPU_LIMIT:-2}"
CONSOLE_MEM_LIMIT="${CONSOLE_MEM_LIMIT:-2Gi}"
OPERATOR_CPU_LIMIT="${OPERATOR_CPU_LIMIT:-2}"
OPERATOR_MEM_LIMIT="${OPERATOR_MEM_LIMIT:-1Gi}"
INSTANCE_STATUS_INTERVAL="${INSTANCE_STATUS_INTERVAL:-30s}"
# The same frozen MinIO build the evidence agent's S3 journey uses
# (hack/evidence/test-s3.sh explains why this one image).
MINIO_IMAGE="${MINIO_IMAGE:-bitnamilegacy/minio@sha256:451fe6858cb770cc9d0e77ba811ce287420f781c7c1b806a386f6896471a349c}"
MINIO_LOCAL="pgtoolbox-loadtest/minio:frozen"
KWOK_IMAGE="registry.k8s.io/kwok/kwok:${KWOK_VERSION}"
METRICS_SERVER_IMAGE="registry.k8s.io/metrics-server/metrics-server:${METRICS_SERVER_VERSION}"
# NodePorts the host reaches through the kind node's address.
PORT_CONSOLE=30300 PORT_PROBES=30301 PORT_OPERATOR=30302 PORT_S3=30303
# Fake node i owns the Pod CIDR 10.(192+i).0.0/16 (65k Pods; KWOK's pool is
# patched to the whole 10.192.0.0/11 as well).
POD_SUPERNET="10.192.0.0/11"

log() { echo "[loadtest] $*" >&2; }
die() { log "FAIL: $*"; exit 1; }

# Whatever the settings, never anything but our own cluster.
case "${CLUSTER_NAME}" in
  *e2e* | pgtoolbox-lab) die "refusing to touch a cluster named ${CLUSTER_NAME}" ;;
esac

require() {
  local tool
  for tool in "$@"; do
    command -v "${tool}" >/dev/null || die "${tool} is required (make doctor lists the pinned versions)"
  done
}

node_ip() {
  docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${NODE_CONTAINER}"
}

# kind_load loads images into the cluster. With Docker's containerd image store
# `kind load docker-image` exports every platform a multi-arch index names, and
# the node's `ctr import` fails on the ones never pulled; saving the one
# platform the node runs avoids it (the same workaround as hack/e2e-chart.sh).
kind_load() {
  local archive status=0
  if ! docker save --help 2>/dev/null | grep -q -- '--platform'; then
    kind load docker-image "$@" --name "${CLUSTER_NAME}"
    return
  fi
  archive="$(mktemp "${TMPDIR:-/tmp}/kind-load.XXXXXX")"
  if ! { docker save --platform "${KIND_IMAGE_PLATFORM}" "$@" >"${archive}" &&
    kind load image-archive "${archive}" --name "${CLUSTER_NAME}" >/dev/null; }; then
    status=1
  fi
  rm -f "${archive}"
  return "${status}"
}

ensure_image() { # pull only what the host does not have
  docker image inspect "$1" >/dev/null 2>&1 || docker pull --quiet --platform "${KIND_IMAGE_PLATFORM}" "$1" >/dev/null
}

fetch() { # fetch <url> <file>: cached under .state/cache
  mkdir -p "${STATE}/cache"
  [[ -s "${STATE}/cache/$2" ]] || curl --fail --silent --show-error --location --retry 3 -o "${STATE}/cache/$2" "$1"
}

host_busy_check() {
  local busy="" ci load cpus
  ci="$(docker ps --format '{{.Names}}' | grep -E 'e2e|buildx_buildkit_.*-(ci|rel)-' | grep -v "^${NODE_CONTAINER}$" | tr '\n' ' ' || true)"
  [[ -z "${ci}" ]] || busy="a CI e2e or build container is running (${ci% })"
  load="$(cut -d' ' -f1 /proc/loadavg)"
  cpus="$(nproc)"
  if awk -v l="${load}" -v c="${cpus}" 'BEGIN { exit !(l > c / 4) }'; then
    busy="${busy:+${busy}; }load average ${load} on ${cpus} CPUs"
  fi
  if [[ -n "${busy}" ]]; then
    if [[ "${ALLOW_BUSY_HOST:-0}" == 1 ]]; then
      log "WARNING: the host is busy (${busy}); ALLOW_BUSY_HOST=1, so measurements taken now are not trustworthy"
    else
      die "the host is busy (${busy}). Measure on an idle host, or ALLOW_BUSY_HOST=1 for a smoke test."
    fi
  fi
}

build_images() {
  local want=()
  case "${BUILD_IMAGES}" in
    always) want=(console operator) ;;
    never) ;;
    auto)
      docker image inspect "${CONSOLE_IMG}" >/dev/null 2>&1 || want+=(console)
      docker image inspect "${OPERATOR_IMG}" >/dev/null 2>&1 || want+=(operator)
      ;;
    *) die "BUILD_IMAGES must be auto, always or never" ;;
  esac
  local target
  for target in "${want[@]}"; do
    log "building the ${target} image from ${ROOT} (a few minutes; set CONSOLE_IMG/OPERATOR_IMG to skip)"
    case "${target}" in
      console) make -C "${ROOT}" docker-build-console CONSOLE_IMG="${CONSOLE_IMG}" ;;
      operator) make -C "${ROOT}" docker-build OPERATOR_IMG="${OPERATOR_IMG}" ;;
    esac
  done
  docker image inspect "${CONSOLE_IMG}" "${OPERATOR_IMG}" >/dev/null || die "the console and operator images must exist locally"
}

build_loadgen() {
  log "building loadgen (static) and its image"
  mkdir -p "${STATE}/image"
  (cd "${ROOT}" && CGO_ENABLED=0 GOOS=linux GOARCH="${KIND_IMAGE_PLATFORM#*/}" go build -trimpath -ldflags "-s -w" -o "${STATE}/image/loadgen" ./hack/loadtest/cmd/loadgen)
  cp "${STATE}/image/loadgen" "${STATE}/loadgen"
  docker build --quiet -t "${SIM_IMG}" -f "${HERE}/Dockerfile" "${STATE}/image" >/dev/null
}

# wait_for <description> <command...>: poll up to 3 minutes.
wait_for() {
  local what="$1" i
  shift
  for i in $(seq 1 90); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  die "timed out waiting for ${what}"
}

namespaces() {
  local i
  for i in $(seq 1 "${NAMESPACES}"); do printf 'lt-%02d\n' "${i}"; done
}

yaml_list() { # yaml_list a b c -> [a, b, c]
  local out="" item
  for item in "$@"; do out+="${out:+, }${item}"; done
  echo "[${out}]"
}

create_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    log "kind cluster ${CLUSTER_NAME} already exists, reusing it"
    kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" >/dev/null
    return
  fi
  log "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE})"
  kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config "${HERE}/manifests/kind.yaml" \
    --kubeconfig "${KUBECONFIG}" --wait 180s >/dev/null
  chmod 600 "${KUBECONFIG}"
}

install_kwok() {
  log "installing KWOK ${KWOK_VERSION} and ${KWOK_NODES} fake nodes"
  ensure_image "${KWOK_IMAGE}"
  kind_load "${KWOK_IMAGE}"
  local base="https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}"
  fetch "${base}/kwok.yaml" "kwok-${KWOK_VERSION}.yaml"
  fetch "${base}/stage-fast.yaml" "kwok-stage-fast-${KWOK_VERSION}.yaml"
  # One address pool wide enough for every fake Pod, whichever way KWOK
  # allocates (its default, 10.0.0.0/24, holds 254 Pods in total).
  sed "s#cidr: 10.0.0.0/24#cidr: ${POD_SUPERNET}#" "${STATE}/cache/kwok-${KWOK_VERSION}.yaml" |
    kubectl apply --server-side -f - >/dev/null
  kubectl -n kube-system rollout status deployment/kwok-controller --timeout=180s >/dev/null
  # kindnet watches every Pod; its 50Mi limit is OOM-killed past a few
  # thousand fake ones.
  kubectl -n kube-system set resources daemonset kindnet --limits=memory=256Mi >/dev/null
  kubectl apply --server-side -f "${STATE}/cache/kwok-stage-fast-${KWOK_VERSION}.yaml" >/dev/null
  local i name
  for i in $(seq 0 $((KWOK_NODES - 1))); do
    name="$(printf 'kwok-node-%02d' "${i}")"
    sed -e "s#__NAME__#${name}#g" -e "s#__CIDR__#10.$((192 + i)).0.0/16#" "${HERE}/manifests/kwok-node.yaml"
    echo '---'
  done | kubectl apply -f - >/dev/null
  kubectl label node "${NODE_CONTAINER}" type=real --overwrite >/dev/null
  wait_for "the fake nodes to be Ready" bash -c "[ \"\$(kubectl get nodes -l type=kwok --no-headers 2>/dev/null | grep -c ' Ready')\" -ge ${KWOK_NODES} ]"
}

install_metrics_server() {
  log "installing metrics-server ${METRICS_SERVER_VERSION}"
  ensure_image "${METRICS_SERVER_IMAGE}"
  kind_load "${METRICS_SERVER_IMAGE}"
  fetch "https://github.com/kubernetes-sigs/metrics-server/releases/download/${METRICS_SERVER_VERSION}/components.yaml" "metrics-server-${METRICS_SERVER_VERSION}.yaml"
  kubectl apply -f "${STATE}/cache/metrics-server-${METRICS_SERVER_VERSION}.yaml" >/dev/null
  # kind's kubelet serves a self-signed certificate.
  if ! kubectl -n kube-system get deployment metrics-server -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q kubelet-insecure-tls; then
    kubectl -n kube-system patch deployment metrics-server --type=json \
      -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' >/dev/null
  fi
  kubectl -n kube-system rollout status deployment/metrics-server --timeout=180s >/dev/null
}

install_crds() {
  log "installing the CloudNativePG 1.30 CRDs and the Barman ObjectStore CRD (no CloudNativePG operator)"
  kubectl apply --server-side -f "${ROOT}/test/crds/cnpg-1.30/" >/dev/null
  kubectl apply --server-side -f "${ROOT}/internal/operator/evidenceagent/testdata/crd/objectstores.barmancloud.cnpg.io.yaml" >/dev/null
}

load_images() {
  log "loading images into the node"
  ensure_image "${MINIO_IMAGE}"
  # A digest reference loses its name inside the node's containerd, so the
  # image is loaded (and run) under a tag.
  docker tag "${MINIO_IMAGE}" "${MINIO_LOCAL}"
  kind_load "${CONSOLE_IMG}" "${OPERATOR_IMG}" "${SIM_IMG}" "${MINIO_LOCAL}"
}

install_operator() {
  log "installing the operator chart (charts/pgtoolbox)"
  cat >"${STATE}/operator-values.yaml" <<EOF
replicaCount: 1
image: {repository: ${OPERATOR_IMG%:*}, tag: "${OPERATOR_IMG##*:}", pullPolicy: Never}
resources:
  requests: {cpu: 100m, memory: 128Mi}
  limits: {cpu: "${OPERATOR_CPU_LIMIT}", memory: ${OPERATOR_MEM_LIMIT}}
operator:
  nodeSelector: {type: real}
  podDisruptionBudget: {enabled: false}
  console:
    image: {repository: ${CONSOLE_IMG%:*}, tag: "${CONSOLE_IMG##*:}"}
  metrics:
    readerClusterRole: true
    readerSubjects: [{name: loadtest, namespace: loadtest}]
EOF
  helm upgrade --install pgtoolbox "${ROOT}/charts/pgtoolbox" --namespace pgtoolbox --create-namespace \
    -f "${STATE}/operator-values.yaml" --wait --timeout 5m >/dev/null
}

setup_loadtest_namespace() {
  log "namespaces ${NAMESPACES} x lt-NN, the loadtest ServiceAccount and its bindings"
  kubectl create namespace loadtest --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n loadtest create serviceaccount loadtest --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  local ns
  for ns in $(namespaces); do
    kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    # Viewer in every watched namespace: the ServiceAccount loadtest, and the
    # group of all ServiceAccounts of the namespace loadtest, which is whom
    # `loadgen http` signs in as (one ServiceAccount per virtual user).
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: loadtest-viewer, namespace: ${ns}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: pgtoolbox-console-viewer}
subjects:
  - {kind: ServiceAccount, name: loadtest, namespace: loadtest}
  - {apiGroup: rbac.authorization.k8s.io, kind: Group, name: "system:serviceaccounts:loadtest"}
EOF
  done
}

install_instance_ca_and_sim() {
  log "instance simulator: CA, routes for the KWOK Pod CIDRs, deployment"
  local dir="${STATE}/ca"
  if [[ ! -s "${dir}/ca.key" ]]; then
    mkdir -p "${dir}"
    openssl ecparam -name prime256v1 -genkey -noout -out "${dir}/ca.key" 2>/dev/null
    openssl req -x509 -new -key "${dir}/ca.key" -sha256 -days 30 -subj "/CN=loadtest-instance-ca" \
      -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -out "${dir}/ca.crt" 2>/dev/null
    chmod 600 "${dir}/ca.key"
  fi
  kubectl create namespace pgtoolbox-console --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n pgtoolbox-console create configmap pgtoolbox-console-instance-ca --from-file=ca.crt="${dir}/ca.crt" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n loadtest create secret generic instance-sim-ca --from-file=tls.crt="${dir}/ca.crt" --from-file=tls.key="${dir}/ca.key" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  routes
  sed "s#__SIM_IMAGE__#${SIM_IMG}#" "${HERE}/manifests/instance-sim.yaml" | kubectl apply -f - >/dev/null
  kubectl -n loadtest rollout status deployment/instance-sim --timeout=180s >/dev/null
}

routes() {
  # A connection to a KWOK Pod's address is delivered to the node itself,
  # where instance-sim (hostNetwork) listens on :8000 and :9187. kindnet
  # routes each fake node's /16 through the KWOK controller Pod, so the local
  # routes are the two /17 halves of each: more specific, they win.
  local script="" i
  for i in $(seq 0 31); do
    script+="ip route replace local 10.$((192 + i)).0.0/17 dev lo; ip route replace local 10.$((192 + i)).128.0/17 dev lo; "
  done
  docker exec "${NODE_CONTAINER}" sh -c "${script}"
}

install_minio() {
  log "MinIO on the real node"
  sed -e "s#__MINIO_IMAGE__#${MINIO_LOCAL}#" -e "s#__S3_NODEPORT__#${PORT_S3}#" "${HERE}/manifests/minio.yaml" | kubectl apply -f - >/dev/null
  kubectl -n loadtest rollout status deployment/minio --timeout=180s >/dev/null
}

install_console() {
  local nss values="${STATE}/console-values.yaml"
  # shellcheck disable=SC2207
  nss=($(namespaces))
  log "installing the console chart (charts/pgtools) watching $(yaml_list "${nss[@]}")"
  cat >"${values}" <<EOF
console:
  replicas: 1
  image: {repository: ${CONSOLE_IMG%:*}, tag: "${CONSOLE_IMG##*:}", pullPolicy: Never}
  watchNamespaces: $(yaml_list "${nss[@]}")
  cookieSecure: "false"
  logs: {streamEnabled: false}
  podDisruptionBudget: {enabled: false}
  nodeSelector: {type: real}
  resources:
    requests: {cpu: 100m, memory: 256Mi}
    limits: {cpu: "${CONSOLE_CPU_LIMIT}", memory: ${CONSOLE_MEM_LIMIT}}
  metrics:
    readerClusterRole: true
    readerSubjects: [{name: loadtest, namespace: loadtest}]
  instanceStatus: {caBundleConfigMap: pgtoolbox-console-instance-ca}
  extraEnv:
    - {name: INSTANCE_STATUS_INTERVAL, value: "${INSTANCE_STATUS_INTERVAL}"}
EOF
  helm upgrade --install pgtoolbox-console "${ROOT}/charts/pgtools" --namespace pgtoolbox-console --create-namespace \
    -f "${values}" --wait --timeout 5m >/dev/null
}

install_access() {
  log "NodePort access to the console, the operator metrics and MinIO"
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata: {name: pgtoolbox-console-load-access, namespace: pgtoolbox-console}
spec:
  type: NodePort
  selector: {app.kubernetes.io/name: pgtoolbox-console, app.kubernetes.io/component: console}
  ports:
    - {name: http, port: 3000, targetPort: 3000, nodePort: ${PORT_CONSOLE}}
    - {name: probes, port: 3001, targetPort: 3001, nodePort: ${PORT_PROBES}}
---
apiVersion: v1
kind: Service
metadata: {name: pgtoolbox-load-access, namespace: pgtoolbox}
spec:
  type: NodePort
  selector: {app.kubernetes.io/name: pgtoolbox, app.kubernetes.io/instance: pgtoolbox}
  ports:
    - {name: metrics, port: 8080, targetPort: 8080, nodePort: ${PORT_OPERATOR}}
EOF
}

write_env() {
  local ip
  ip="$(node_ip)"
  cat >"${STATE}/env" <<EOF
# Source this: . hack/loadtest/.state/env   (hack/loadtest/loadgen.sh does it for you)
export LOADTEST_KUBECONFIG="${KUBECONFIG}"
export LOADTEST_CONSOLE_URL="http://${ip}:${PORT_CONSOLE}"
export LOADTEST_CONSOLE_PROBE_URL="http://${ip}:${PORT_PROBES}"
export LOADTEST_OPERATOR_METRICS_URL="https://${ip}:${PORT_OPERATOR}"
# The operator of this throwaway cluster serves a self-signed certificate.
export LOADTEST_OPERATOR_SKIP_VERIFY=1
export LOADTEST_S3_ENDPOINT="http://${ip}:${PORT_S3}"
export LOADTEST_S3_ACCESS_KEY="loadtest"
export LOADTEST_S3_SECRET_KEY="loadtest-secret"
export LOADTEST_S3_BUCKET="barman"
export LOADTEST_NAMESPACES="${NAMESPACES}"
EOF
}

cmd_up() {
  require kind kubectl helm docker go curl openssl awk sed
  host_busy_check
  mkdir -p "${STATE}"
  [[ "${NAMESPACES}" =~ ^[0-9]+$ ]] && ((NAMESPACES >= 1 && NAMESPACES <= 16)) || die "NAMESPACES must be 1..16 (the console watches at most 16)"
  [[ "${KWOK_NODES}" =~ ^[0-9]+$ ]] && ((KWOK_NODES >= 1 && KWOK_NODES <= 32)) || die "KWOK_NODES must be 1..32"
  build_images
  build_loadgen
  create_cluster
  install_kwok
  install_metrics_server
  install_crds
  load_images
  install_operator   # first: it creates the pgtoolbox-console-viewer ClusterRole
  setup_loadtest_namespace
  install_instance_ca_and_sim
  install_minio
  install_console
  install_access
  write_env
  cmd_status
  log "ready. Next: hack/loadtest/loadgen.sh populate --namespaces ${NAMESPACES} --clusters 20 --instances 3"
}

cmd_console() {
  require kubectl helm
  [[ -s "${KUBECONFIG}" ]] || die "no cluster: run cluster.sh up"
  install_console
}

cmd_status() {
  [[ -s "${KUBECONFIG}" ]] || die "no cluster: run cluster.sh up"
  kubectl get nodes -L type
  kubectl get pods -A -o wide --field-selector spec.nodeName="${NODE_CONTAINER}" | head -40
  echo
  cat "${STATE}/env" 2>/dev/null | grep '^export' || true
}

cmd_down() {
  require kind
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    log "deleting kind cluster ${CLUSTER_NAME}"
    kind delete cluster --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" >/dev/null
  else
    log "no kind cluster ${CLUSTER_NAME}"
  fi
  # Keep the download cache and the built binary; drop what names the cluster.
  rm -rf "${STATE}/kubeconfig" "${STATE}/env" "${STATE}/ca" "${STATE}/operator-values.yaml" "${STATE}/console-values.yaml"
  if [[ "${PURGE:-0}" == 1 ]]; then rm -rf "${STATE}"; fi
  log "down. The images ${CONSOLE_IMG}, ${OPERATOR_IMG}, ${SIM_IMG} stay in the Docker store (docker rmi to drop them)."
}

case "${1:-}" in
  up) cmd_up ;;
  down) cmd_down ;;
  status) cmd_status ;;
  console) cmd_console ;;
  routes) routes ;;
  *) awk 'NR > 1 && /^set -euo/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}" >&2; exit 2 ;;
esac
