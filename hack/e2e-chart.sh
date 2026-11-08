#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The chart e2e: the namespace console installed from
# charts/pgtools on a real kind cluster, asserted by test/e2e/chart.
#
#   hack/e2e-chart.sh scope    two team consoles in two namespaces;
#                              each holds no cluster-scoped grant but the opt-in
#                              ClusterImageCatalog read (proved by SubjectAccessReview
#                              against the console's own ServiceAccount); the
#                              availability rule on a real
#                              API server; SSO against an in-cluster dex the API
#                              server trusts (structured AuthenticationConfiguration),
#                              the cluster list a DBA may see, the honest overview,
#                              the revocation ConfigMap and sign-out
#   hack/e2e-chart.sh netpol   the same chart on Calico with networkPolicy.enabled:
#                              the console still works and cannot reach the other
#                              namespace's instance pods or evidence agent
#
# Provisioning only: the CNPG CRDs (no operator, no PostgreSQL), the console image
# built from the working tree, and for `netpol` the Calico CNI. Everything else is
# Go (test/e2e/chart) and runs against the cluster this script owns.
#
# Environment:
#   DEX_REF       the dex image, by digest (Dependabot cannot track it: bump it by hand)
#   CONSOLE_IMG   image to deploy, built by `make docker-build-console`   (pgtoolbox-console:dev)
#   OPERATOR_IMG  the operator image for the `ct install` cases, built by
#                 `make docker-build` (pgtoolbox-operator:dev); scope mode only
#   SKIP_CT_INSTALL=1  skip the `ct install` stage (needs ct)
#   CLUSTER_NAME  default ptb-e2e-$GITHUB_RUN_ID in CI (netpol-e2e-$GITHUB_RUN_ID in
#                 netpol mode: the two chart jobs of a run must not share a name),
#                 ptb-chart-$$ locally
#   KUBECONFIG    a private file (default: a new temp file); never ~/.kube/config
#   E2E_ARTIFACTS where failure dumps and `kind export logs` go (artifacts/e2e-chart)
#   KEEP_CLUSTER=1 leaves the cluster up
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

mode="${1:-}"
case "${mode}" in scope | netpol) ;; *) echo "usage: $0 scope|netpol" >&2; exit 2 ;; esac

# Pinned so a failure is the chart's, never a moving platform underneath.
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
CNPG_VERSION="${CNPG_VERSION:-1.30.0}"
CNPG_MANIFEST="${CNPG_MANIFEST:-https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-${CNPG_VERSION}.yaml}"
CALICO_VERSION="${CALICO_VERSION:-v3.32.2}"
CALICO_MANIFEST="${CALICO_MANIFEST:-https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml}"
PROBE_IMAGE="${PROBE_IMAGE:-busybox:1.37}"
# dex v2.45.1, the multi-arch index digest. Pulled, then tagged with a local
# name so that the node loads it without a registry (imagePullPolicy: Never).
DEX_REF="${DEX_REF:-dexidp/dex@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462}"
DEX_LOCAL="e2e.local/dex:pinned"
# The dex Service keeps this ClusterIP (inside kind's default service subnet) so
# that the issuer URL is an address the API server, which runs on the node and
# cannot resolve cluster DNS, can dial: https://${DEX_IP}:5556/dex.
DEX_IP="10.96.200.10"
CONSOLE_IMG="${CONSOLE_IMG:-pgtoolbox-console:dev}"
OPERATOR_IMG="${OPERATOR_IMG:-pgtoolbox-operator:dev}"
KIND_IMAGE_PLATFORM="${KIND_IMAGE_PLATFORM:-linux/amd64}"
ARTIFACTS="${E2E_ARTIFACTS:-artifacts/e2e-chart}"

if [[ -n "${GITHUB_RUN_ID:-}" ]]; then
  prefix=ptb
  [[ "${mode}" == netpol ]] && prefix=netpol
  CLUSTER_NAME="${CLUSTER_NAME:-${prefix}-e2e-${GITHUB_RUN_ID}}"
else
  CLUSTER_NAME="${CLUSTER_NAME:-ptb-chart-$$}"
fi

# A job on someone's own machine must never read or write the owner's kubeconfig.
if [[ -z "${KUBECONFIG:-}" ]]; then
  KUBECONFIG="$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ptb-chart-e2e.XXXXXX.kubeconfig")"
  own_kubeconfig=1
fi
case "${KUBECONFIG}" in
  *:* | "${HOME}/.kube/config")
    echo "[e2e-chart] KUBECONFIG must be one private file, got '${KUBECONFIG}'" >&2
    exit 1 ;;
esac
export KUBECONFIG

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

for tool in kind kubectl helm docker go awk curl openssl; do
  command -v "${tool}" >/dev/null || { echo "[e2e-chart] missing required tool: ${tool}" >&2; exit 1; }
done

cleanup() {
  local status=$?
  [[ -z "${authn_dir:-}" ]] || rm -rf "${authn_dir}"
  if ((status != 0)); then
    log "failed (${status}): exporting kind logs to ${ARTIFACTS}/kind"
    mkdir -p "${ARTIFACTS}"
    kind export logs --name "${CLUSTER_NAME}" "${ARTIFACTS}/kind" >/dev/null 2>&1 || true
  fi
  if [[ "${KEEP_CLUSTER:-}" == "1" ]]; then
    log "leaving cluster ${CLUSTER_NAME} up (KEEP_CLUSTER=1); KUBECONFIG=${KUBECONFIG}"
    return
  fi
  log "deleting cluster ${CLUSTER_NAME}"
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  [[ -z "${own_kubeconfig:-}" ]] || rm -f "${KUBECONFIG}"
}
trap cleanup EXIT

# kind_load loads images into the kind cluster. With Docker's containerd image
# store, `kind load docker-image` exports every platform a pulled multi-arch image's
# index names, including the ones never pulled, and the node's `ctr import` then
# fails with "content digest ... not found". Saving the one platform the node runs
# avoids that.
kind_load() {
  local archive status=0
  if ! docker save --help 2>/dev/null | grep -q -- '--platform'; then
    kind load docker-image "$@" --name "${CLUSTER_NAME}"
    return
  fi
  archive="$(mktemp "${TMPDIR:-/tmp}/kind-load.XXXXXX")"
  if ! { docker save --platform "${KIND_IMAGE_PLATFORM}" "$@" >"${archive}" &&
    kind load image-archive "${archive}" --name "${CLUSTER_NAME}"; }; then
    status=1
  fi
  rm -f "${archive}"
  return "${status}"
}

ensure_image() { # pull only what the host does not have
  docker image inspect "$1" >/dev/null 2>&1 || docker pull --quiet --platform "${KIND_IMAGE_PLATFORM}" "$1" >/dev/null
}

# pull_dex pulls the pinned dex and tags it ${DEX_LOCAL}.
pull_dex() {
  docker pull --quiet --platform "${KIND_IMAGE_PLATFORM}" "${DEX_REF}" >/dev/null
  docker tag "${DEX_REF}" "${DEX_LOCAL}"
}

# make_authn writes what the scope mode needs before the cluster exists: a CA and
# the dex serving certificate (an IP SAN: the issuer is an IP, see DEX_IP), and the
# AuthenticationConfiguration that makes the API server accept dex ID tokens.
make_authn() {
  authn_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ptb-chart-authn.XXXXXX")"
  chmod 755 "${authn_dir}"
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=pgtoolbox-e2e-dex-ca" \
    -keyout "${authn_dir}/ca.key" -out "${authn_dir}/ca.crt" 2>/dev/null
  openssl req -newkey rsa:2048 -nodes -subj "/CN=dex" -keyout "${authn_dir}/dex.key" -out "${authn_dir}/dex.csr" 2>/dev/null
  printf 'subjectAltName=IP:%s,DNS:dex.sso.svc\nextendedKeyUsage=serverAuth\n' "${DEX_IP}" >"${authn_dir}/ext.cnf"
  openssl x509 -req -in "${authn_dir}/dex.csr" -CA "${authn_dir}/ca.crt" -CAkey "${authn_dir}/ca.key" -CAcreateserial \
    -days 2 -extfile "${authn_dir}/ext.cnf" -out "${authn_dir}/dex.crt" 2>/dev/null
  {
    echo "apiVersion: apiserver.config.k8s.io/v1"
    echo "kind: AuthenticationConfiguration"
    echo "jwt:"
    echo "- issuer:"
    echo "    url: https://${DEX_IP}:5556/dex"
    # The console's client and its grant client (OIDC_GRANT_CLIENT_ID,
    # test/e2e/chart/dex_test.go): either audience is accepted.
    echo "    audiences: [pgtoolbox-console, pgtoolbox-console-grants]"
    echo "    audienceMatchPolicy: MatchAny"
    echo "    certificateAuthority: |"
    sed 's/^/      /' "${authn_dir}/ca.crt"
    echo "  claimMappings:"
    echo "    username:"
    echo "      claim: email"
    echo "      prefix: 'oidc:'"
  } >"${authn_dir}/authn.yaml"
  chmod 644 "${authn_dir}"/*
}

# wait_control_plane gates on a control plane that answers. kind create --wait
# only warns "Timed out waiting for Ready" and exits 0 when a loaded host makes
# etcd slow, and the next write then fails on "etcdserver: request timed out"
# far from the cause. With a CNI to install (netpol) the node is NotReady until
# Calico runs, so the node wait is the caller's, after the CNI.
wait_control_plane() {
  local deadline=$((SECONDS + ${CONTROL_PLANE_WAIT:-600})) pod
  until kubectl get --raw /readyz >/dev/null 2>&1; do
    ((SECONDS < deadline)) || { echo "[e2e-chart] the API server never became ready (/readyz)" >&2; kubectl get --raw '/readyz?verbose' >&2 || true; exit 1; }
    sleep 3
  done
  for pod in etcd kube-apiserver kube-controller-manager kube-scheduler; do
    until kubectl -n kube-system wait --for=condition=Ready pod -l "component=${pod}" --timeout=10s >/dev/null 2>&1; do
      ((SECONDS < deadline)) || { echo "[e2e-chart] ${pod} never became Ready" >&2; kubectl -n kube-system get pods -o wide >&2 || true; exit 1; }
    done
  done
  # A write round-trip through etcd, the thing that failed: a ConfigMap create and delete.
  until kubectl -n kube-system create configmap ptb-e2e-probe --from-literal=k=v >/dev/null 2>&1; do
    ((SECONDS < deadline)) || { echo "[e2e-chart] etcd never accepted a write" >&2; exit 1; }
    sleep 3
  done
  kubectl -n kube-system delete configmap ptb-e2e-probe >/dev/null
}

kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true

log "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE}, mode ${mode})"
if [[ "${mode}" == netpol ]]; then
  # A policy-enforcing CNI needs the default one off. Calico's default pool is
  # 192.168.0.0/16, so the pod subnet follows it.
  hack/kind-create.sh --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --wait 300s --config - <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 192.168.0.0/16
# etcd on the node's tmpfs, as in hack/kind/ci.yaml.
kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    etcd:
      local:
        dataDir: /tmp/etcd
YAML
  wait_control_plane
else
  pull_dex
  make_authn
  # kind 0.33 with kindest/node v1.34.0 still takes the kubeadm v1beta3 form:
  # extraArgs is a map (a v1beta4 list fails "cannot unmarshal array"). The
  # API server starts before dex exists; its JWT authenticator initialises lazily.
  hack/kind-create.sh --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --wait 300s --config - <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  serviceSubnet: 10.96.0.0/16
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: ${authn_dir}
        containerPath: /etc/kubernetes/authn
        readOnly: true
kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    etcd:
      local:
        dataDir: /tmp/etcd
    apiServer:
      extraArgs:
        authentication-config: /etc/kubernetes/authn/authn.yaml
      extraVolumes:
        - name: authn
          hostPath: /etc/kubernetes/authn
          mountPath: /etc/kubernetes/authn
          readOnly: true
          pathType: DirectoryOrCreate
YAML
  wait_control_plane
  kubectl wait --for=condition=Ready nodes --all --timeout=300s
fi

if [[ "${mode}" == netpol ]]; then
  log "installing Calico ${CALICO_VERSION}"
  work="$(mktemp -d "${TMPDIR:-/tmp}/ptb-chart-e2e.XXXXXX")"
  curl --fail --silent --show-error --location --retry 3 -o "${work}/calico.yaml" "${CALICO_MANIFEST}"
  images="$(awk '$1 == "image:" { print $2 }' "${work}/calico.yaml" | sort -u)"
  # Side-loaded: the node needs no registry route, and the host's Docker cache
  # makes a re-run fast.
  for image in ${images}; do ensure_image "${image}"; done
  # shellcheck disable=SC2086 # image references never contain spaces
  kind_load ${images}
  kubectl apply -f "${work}/calico.yaml" >/dev/null
  rm -rf "${work}"
  kubectl -n kube-system rollout status daemonset/calico-node --timeout=300s
  kubectl -n kube-system rollout status deployment/calico-kube-controllers --timeout=300s
  kubectl wait --for=condition=Ready nodes --all --timeout=120s
fi

log "installing the CloudNativePG CRDs ${CNPG_VERSION} (no operator: the console only reads them)"
work="$(mktemp -d "${TMPDIR:-/tmp}/ptb-chart-e2e.XXXXXX")"
curl --fail --silent --show-error --location --retry 3 -o "${work}/cnpg.yaml" "${CNPG_MANIFEST}"
awk -v RS='\n---\n' -v ORS='\n---\n' '/(^|\n)kind: CustomResourceDefinition(\n|$)/' "${work}/cnpg.yaml" >"${work}/crds.yaml"
crds="$(grep -c '^kind: CustomResourceDefinition$' "${work}/crds.yaml" || true)"
((crds >= 10)) || { echo "[e2e-chart] expected the CNPG CRDs in ${CNPG_MANIFEST}, found ${crds}" >&2; exit 1; }
kubectl apply --server-side -f "${work}/crds.yaml" >/dev/null
rm -rf "${work}"

log "loading images"
ensure_image "${PROBE_IMAGE}"
if [[ "${mode}" == netpol ]]; then kind_load "${CONSOLE_IMG}" "${PROBE_IMAGE}"; else kind_load "${CONSOLE_IMG}" "${PROBE_IMAGE}" "${DEX_LOCAL}" "${OPERATOR_IMG}"; fi

log "running test/e2e/chart (${mode})"
case "${mode}" in
  scope) run='^TestTeamConsoles$' ;;
  netpol) run='^TestNetworkPolicy$' ;;
esac
mkdir -p "${ARTIFACTS}"
E2E_ARTIFACTS="${ARTIFACTS}" CONSOLE_IMG="${CONSOLE_IMG}" PROBE_IMAGE="${PROBE_IMAGE}" \
  E2E_DEX_IMAGE="${DEX_LOCAL}" E2E_DEX_IP="${DEX_IP}" E2E_DEX_DIR="${authn_dir:-}" \
  go test -tags e2echart ./test/e2e/chart/... -run "${run}" -count=1 -timeout 33m -v

# ct install: the charts installed the way a user would, by
# chart-testing, in the cluster this script already owns (no second kind). One ct
# run per case, "<chart>/<case>" for charts/<chart>/ci-install/<case>-values.yaml;
# ct reads the values from the chart's ci/ directory, so each case gets a copy of
# the chart whose ci/ holds only it. The images are the ones loaded above; the
# chart's own repository and tag are overridden, pullPolicy Never.
ct_install() {
  local entry chart case_name copy sets
  command -v ct >/dev/null || { echo "[e2e-chart] ct is required for the ct install stage (mise install; SKIP_CT_INSTALL=1 skips it)" >&2; exit 1; }
  for entry in "$@"; do
    chart="${entry%%/*}"
    case_name="${entry#*/}"
    case "${chart}" in
      pgtools) sets="--set=console.image.repository=${CONSOLE_IMG%:*} --set=console.image.tag=${CONSOLE_IMG##*:} --set=console.image.pullPolicy=Never" ;;
      pgtoolbox) sets="--set=image.repository=${OPERATOR_IMG%:*} --set=image.tag=${OPERATOR_IMG##*:} --set=image.pullPolicy=Never --set=operator.console.image.repository=${CONSOLE_IMG%:*} --set=operator.console.image.tag=${CONSOLE_IMG##*:}" ;;
      *) echo "[e2e-chart] unknown chart ${chart}" >&2; exit 1 ;;
    esac
    copy="$(mktemp -d "${TMPDIR:-/tmp}/ptb-ct-install.XXXXXX")"
    mkdir -p "${copy}/charts"
    cp -r "charts/${chart}" "${copy}/charts/${chart}"
    rm -rf "${copy}/charts/${chart}/ci" "${copy}/charts/${chart}/ci-install"
    mkdir "${copy}/charts/${chart}/ci"
    cp "charts/${chart}/ci-install/${case_name}-values.yaml" "${copy}/charts/${chart}/ci/${case_name}-values.yaml"
    log "ct install: ${chart} ${case_name}"
    # ct installs into --namespace without creating it (the namespaces of the chart's
    # own watched list, ct-team-a and ct-team-b, are created above).
    kubectl create namespace "ct-${case_name}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    ct install --charts "${copy}/charts/${chart}" --chart-dirs "${copy}/charts" \
      --namespace "ct-${case_name}" --release-name "ct-${case_name}" \
      --helm-extra-args "--timeout 300s" \
      --helm-extra-set-args "${sets}" \
      || { rm -rf "${copy}"; echo "[e2e-chart] ct install failed for ${entry}" >&2; exit 1; }
    rm -rf "${copy}"
    kubectl delete namespace "ct-${case_name}" --wait=false >/dev/null 2>&1 || true
  done
}

if [[ "${SKIP_CT_INSTALL:-}" != 1 ]]; then
  for ns in ct-team-a ct-team-b; do
    kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  done
  case "${mode}" in
    scope) ct_install pgtools/console-default pgtools/console-multi-namespace pgtoolbox/operator-only ;;
    netpol) ct_install pgtools/console-networkpolicy ;;
  esac
fi
