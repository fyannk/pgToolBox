#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Sourced, not run: what the kind scenarios that need a real CloudNativePG share
# (test/e2e/evidenceagent.sh, test/e2e/team/run.sh) -- the pins, the one-platform
# image load, and the stack a Cluster archiving to object storage needs:
# CloudNativePG, cert-manager, the Barman Cloud Plugin and MinIO
# (test/e2e/testdata/minio.yaml, bucket e2e-backups, s3://, plain HTTP).
#
# The pins live here, under hack/, in one place a bump touches. The caller sets CLUSTER_NAME, runs from the repository root and
# creates the kind cluster itself.

# shellcheck source=hack/lib/net.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/net.sh"

KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
# The CloudNativePG and cert-manager defaults, and the sha256 each is checked against, are in hack/lib/net.sh.
CNPG_VERSION="${CNPG_VERSION:-${CNPG_DEFAULT_VERSION}}"
CNPG_MANIFEST="${CNPG_MANIFEST:-$(cnpg_manifest_url "${CNPG_VERSION}")}"
BARMAN_PLUGIN_MANIFEST="${BARMAN_PLUGIN_MANIFEST:-https://github.com/cloudnative-pg/plugin-barman-cloud/releases/latest/download/manifest.yaml}"
CERT_MANAGER_MANIFEST="${CERT_MANAGER_MANIFEST:-$(cert_manager_manifest_url "${CERT_MANAGER_DEFAULT_VERSION}")}"
KIND_IMAGE_PLATFORM="${KIND_IMAGE_PLATFORM:-linux/amd64}"

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

# kind_load saves the one platform the node runs: see kind_load in hack/e2e-chart.sh.
kind_load() {
  local archive status=0
  archive="$(mktemp "${TMPDIR:-/tmp}/kind-load.XXXXXX")"
  if ! { docker save --platform "${KIND_IMAGE_PLATFORM}" "$@" >"${archive}" &&
    kind load image-archive "${archive}" --name "${CLUSTER_NAME}"; }; then
    status=1
  fi
  rm -f "${archive}"
  return "${status}"
}

# install_cnpg_base installs CloudNativePG ${CNPG_VERSION} and cert-manager into
# the current kubeconfig's cluster: what every CNPG-I plugin needs under it
# (test/e2e/pgbackrest/run.sh installs the pgBackRest plugins on top).
install_cnpg_base() {
  log "installing CloudNativePG ${CNPG_VERSION} and cert-manager"
  apply_manifest "${CNPG_MANIFEST}" --server-side
  # Longer leader-election leases (integer seconds; CloudNativePG 1.30
  # internal/cmd/manager/controller/cmd.go): on a loaded CI
  # runner the default 15 s lease is lost, the manager restarts, and its webhook
  # refuses connections in the middle of a run.
  local args flag
  args="$(kubectl -n cnpg-system get deployment cnpg-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}')"
  for flag in "leader-lease-duration=${CNPG_LEASE_DURATION:-60}" "leader-renew-deadline=${CNPG_LEASE_RENEW:-45}"; do
    if [[ "${args}" != *"--${flag%%=*}="* ]]; then
      kubectl -n cnpg-system patch deployment cnpg-controller-manager --type json \
        -p "[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--${flag}\"}]" >/dev/null
    fi
  done
  kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=300s
  kubectl -n cnpg-system wait --for=condition=Available deployment/cnpg-controller-manager --timeout=300s
  apply_manifest "${CERT_MANAGER_MANIFEST}" --server-side
  local deployment
  for deployment in cert-manager cert-manager-webhook cert-manager-cainjector; do
    kubectl -n cert-manager rollout status "deployment/${deployment}" --timeout=300s
  done
}

# install_cnpg_stack installs CloudNativePG ${CNPG_VERSION}, cert-manager, the
# Barman Cloud Plugin and MinIO into the current kubeconfig's cluster.
install_cnpg_stack() {
  install_cnpg_base
  log "installing the Barman Cloud Plugin and MinIO"
  # cert-manager's webhook answers a little after its Deployment is Available,
  # and the plugin manifest carries cert-manager objects: retry until it does.
  # The manifest is "latest", so there is no checksum to pin; it is fetched once, with retries.
  local i barman_file
  barman_file="$(mktemp "${TMPDIR:-/tmp}/barman-plugin.XXXXXX")"
  fetch_url "${BARMAN_PLUGIN_MANIFEST}" "${barman_file}" || return 1
  for i in $(seq 1 12); do
    kubectl apply --server-side -f "${barman_file}" && break
    if ((i == 12)); then
      log "the Barman Cloud plugin manifest did not apply: cert-manager's webhook never answered"
      rm -f "${barman_file}"
      return 1
    fi
    sleep 10
  done
  rm -f "${barman_file}"
  kubectl -n cnpg-system rollout status deployment/barman-cloud --timeout=300s
  kubectl apply --server-side -f test/e2e/testdata/minio.yaml
  kubectl -n minio rollout status deployment/minio --timeout=300s
}
