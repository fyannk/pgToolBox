#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The kind x CloudNativePG matrix: where its cells are defined, and how one cell runs. .github/workflows/matrix.yml and
# `make test-e2e-matrix` both use this file, so a cell run by hand is the cell CI
# runs.
#
#   hack/matrix.sh plan                  print the matrix as {"include":[...]} JSON
#   hack/matrix.sh run <scenario>        run one scenario for the cell in the environment
#
# Environment for `plan` (strings, never interpolated by the workflow):
#   KUBERNETES   all (default) | 1.34 | 1.36
#   CNPG         all (default) | 1.30.1 | main
#   NIGHTLY      true adds the `main` CNPG cell to `all` (non-blocking)
# Environment for `run`: NODE_IMAGE and CNPG_MANIFEST (printed by `plan`, or give
# K8S and CNPG and they are derived), plus what the scenario's own script reads.
#
# The axes:
#   Kubernetes  1.34 is the oldest minor CloudNativePG 1.30.x lists (1.34-1.36,
#               supported_releases.md of cloudnative-pg, 2026-11-06) and the OLM
#               bundle's tests start at; 1.36 is the current minor kind 0.33.0
#               ships an image for that CNPG 1.30 supports.
#   CNPG        N = 1.30.1, the only supported minor: pgToolBox supports
#               CloudNativePG 1.30 and later (1.29 reached end of life upstream
#               on 2026-09-29 and was dropped on 2026-11-06). A second blocking
#               minor comes back with 1.31.0 (planned upstream for December
#               2026, not released). `main` runs nightly, non-blocking.
# The node images are the kind 0.33.0 release's, by digest.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

declare -A node_image=(
  [1.34]="kindest/node:v1.34.11@sha256:44e222ee2132dab25ff87301682f89eb82c7880ea3a1bf543bfe9708fd08d67d"
  [1.36]="kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"
)
k8s_versions=(1.34 1.36)
cnpg_versions=(1.30.1)
# The scenarios a cell runs, in order; each is a Makefile target that provisions
# its own kind cluster and honours KIND_NODE_IMAGE and CNPG_MANIFEST.
scenarios=(console evidenceagent pgadmin)
declare -A scenario_target=(
  [console]=test-e2e-console
  [evidenceagent]=test-e2e-evidenceagent
  [pgadmin]=test-e2e-pgadmin
)

cnpg_manifest() { # version | main
  local v=$1
  if [[ "${v}" == main ]]; then
    echo "main"  # materialised by `run`: upstream publishes no manifest for main
  else
    echo "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${v%.*}/releases/cnpg-${v}.yaml"
  fi
}

die() { echo "matrix: $*" >&2; exit 2; }

cmd_plan() {
  local want_k="${KUBERNETES:-all}" want_c="${CNPG:-all}" nightly="${NIGHTLY:-false}"
  local k c rows=() blocking
  [[ "${want_k}" == all || -n "${node_image[${want_k}]:-}" ]] || die "KUBERNETES must be all or one of ${k8s_versions[*]}, got '${want_k}'"
  [[ "${want_c}" =~ ^(all|main|[0-9]+\.[0-9]+\.[0-9]+)$ ]] || die "CNPG must be all, main or X.Y.Z, got '${want_c}'"
  local cnpgs=("${cnpg_versions[@]}")
  if [[ "${want_c}" != all ]]; then cnpgs=("${want_c}"); fi
  if [[ "${want_c}" == all && "${nightly}" == true ]]; then cnpgs+=(main); fi
  for k in "${k8s_versions[@]}"; do
    [[ "${want_k}" == all || "${want_k}" == "${k}" ]] || continue
    for c in "${cnpgs[@]}"; do
      blocking=true
      # main is a moving target and is checked on the current Kubernetes only.
      if [[ "${c}" == main ]]; then
        blocking=false
        [[ "${k}" == "${k8s_versions[-1]}" || "${want_k}" != all ]] || continue
      fi
      rows+=("{\"name\":\"k8s-${k}-cnpg-${c}\",\"k8s\":\"${k}\",\"node_image\":\"${node_image[${k}]}\",\"cnpg\":\"${c}\",\"cnpg_manifest\":\"$(cnpg_manifest "${c}")\",\"experimental\":$([[ ${blocking} == true ]] && echo false || echo true)}")
    done
  done
  ((${#rows[@]})) || die "no cell selected"
  local IFS=,
  printf '{"include":[%s]}\n' "${rows[*]}"
}

cmd_run() {
  local scenario="${1:?usage: matrix.sh run <console|evidenceagent|pgadmin|all>}"
  local k="${K8S:-}" c="${CNPG:-}"
  local image="${NODE_IMAGE:-}" manifest="${CNPG_MANIFEST:-}"
  if [[ -z "${image}" ]]; then
    [[ -n "${node_image[${k}]:-}" ]] || die "set NODE_IMAGE, or K8S to one of ${k8s_versions[*]}"
    image="${node_image[${k}]}"
  fi
  if [[ -z "${manifest}" ]]; then
    [[ -n "${c}" ]] || die "set CNPG_MANIFEST, or CNPG to X.Y.Z or main"
    manifest="$(cnpg_manifest "${c}")"
  fi
  if [[ "${manifest}" == main ]]; then
    # Upstream's kustomize base at main, running the image its CI publishes for
    # main. Best effort, which is why the cell is non-blocking.
    command -v kubectl >/dev/null || die "kubectl is needed to render CNPG main"
    manifest="${TMPDIR:-/tmp}/cnpg-main-$$.yaml"
    kubectl kustomize "https://github.com/cloudnative-pg/cloudnative-pg//config/default?ref=main" \
      | sed 's#image: controller:latest#image: ghcr.io/cloudnative-pg/cloudnative-pg-testing:main#' > "${manifest}"
    trap 'rm -f "${manifest}"' EXIT
  fi
  local todo=("${scenario}")
  [[ "${scenario}" != all ]] || todo=("${scenarios[@]}")
  local s status=0
  for s in "${todo[@]}"; do
    [[ -n "${scenario_target[${s}]:-}" ]] || die "unknown scenario '${s}'"
    echo "== cell: ${image} + ${manifest} :: ${s}"
    # The cells of one run go one after another: one kind cluster at a time.
    if ! KIND_NODE_IMAGE="${image}" CNPG_MANIFEST="${manifest}" \
      timeout "${E2E_TIMEOUT:-30m}" make "${scenario_target[${s}]}"; then
      status=1
      echo "== cell FAILED: ${s}" >&2
    fi
  done
  return "${status}"
}

case "${1:-}" in
  plan) cmd_plan ;;
  run) shift; cmd_run "$@" ;;
  *) echo "usage: $0 plan | run <scenario>" >&2; exit 2 ;;
esac
