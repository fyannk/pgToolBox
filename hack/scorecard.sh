#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The OLM scorecard: `operator-sdk scorecard` with the
# basic and olm suites of deploy/olm/bundle/tests/scorecard/config.yaml, on a kind
# cluster this script owns (or the one KUBECONFIG already points at with
# USE_EXISTING_CLUSTER=1).
#
#   hack/scorecard.sh
#
# Gating (a failure fails the script; names are the results' .name): basic-check-spec, olm-bundle-validation,
# olm-crds-have-validation, olm-crds-have-resources. Reported, not gating, until
# the CSV carries spec descriptors (olm-spec-descriptors fails today: no specDescriptors for any PgAdmin or EvidenceAgent field): olm-spec-descriptors, olm-status-descriptors.
# Moving a test between the lists is a one-line change below, and a
# reviewed one.
#
# Environment:
#   KIND_NODE_IMAGE   default kindest/node:v1.34.0 (as hack/e2e-chart.sh)
#   CLUSTER_NAME      default ptb-e2e-$GITHUB_RUN_ID in CI, ptb-scorecard-$$ locally
#   KUBECONFIG        a private file (default: a new temp file)
#   USE_EXISTING_CLUSTER=1   run on the current KUBECONFIG, create and delete nothing
#   SCORECARD_ARTIFACTS  where the JSON result goes (artifacts/scorecard)
#   KEEP_CLUSTER=1    leave the cluster up
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

# shellcheck source=hack/lib/net.sh
source hack/lib/net.sh

gating=(basic-check-spec olm-bundle-validation olm-crds-have-validation olm-crds-have-resources)
reporting=(olm-spec-descriptors olm-status-descriptors)

KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
ARTIFACTS="${SCORECARD_ARTIFACTS:-artifacts/scorecard}"
if [[ -n "${GITHUB_RUN_ID:-}" ]]; then
  CLUSTER_NAME="${CLUSTER_NAME:-ptb-e2e-${GITHUB_RUN_ID}}"
else
  CLUSTER_NAME="${CLUSTER_NAME:-ptb-scorecard-$$}"
fi

for tool in operator-sdk kubectl jq docker; do
  command -v "${tool}" >/dev/null || { echo "[scorecard] missing required tool: ${tool} (mise install)" >&2; exit 2; }
done

own_cluster=1
[[ "${USE_EXISTING_CLUSTER:-}" != 1 ]] || own_cluster=0
if ((own_cluster)); then
  command -v kind >/dev/null || { echo "[scorecard] missing required tool: kind" >&2; exit 2; }
  if [[ -z "${KUBECONFIG:-}" ]]; then
    KUBECONFIG="$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ptb-scorecard.XXXXXX.kubeconfig")"
    own_kubeconfig=1
  fi
  case "${KUBECONFIG}" in
    *:* | "${HOME}/.kube/config") echo "[scorecard] KUBECONFIG must be one private file, got '${KUBECONFIG}'" >&2; exit 1 ;;
  esac
  export KUBECONFIG
fi

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

cleanup() {
  local status=$?
  if ((own_cluster)); then
    if ((status != 0)); then
      mkdir -p "${ARTIFACTS}"
      kind export logs --name "${CLUSTER_NAME}" "${ARTIFACTS}/kind" >/dev/null 2>&1 || true
    fi
    if [[ "${KEEP_CLUSTER:-}" == 1 ]]; then
      log "leaving cluster ${CLUSTER_NAME} up (KEEP_CLUSTER=1); KUBECONFIG=${KUBECONFIG}"
    else
      kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
      [[ -z "${own_kubeconfig:-}" ]] || rm -f "${KUBECONFIG}"
    fi
  fi
}
trap cleanup EXIT

if ((own_cluster)); then
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  log "creating kind cluster ${CLUSTER_NAME} (${KIND_NODE_IMAGE})"
  hack/kind-create.sh --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config hack/kind/ci.yaml --wait 300s
  # Side-load the scorecard image: the node needs no route to quay.io, and the
  # host's Docker cache makes a re-run fast.
  images="$(awk '$1 == "image:" { print $2 }' deploy/olm/bundle/tests/scorecard/config.yaml | sort -u)"
  for image in ${images}; do
    docker image inspect "${image}" >/dev/null 2>&1 || pull_image linux/amd64 "${image}"
    docker save --platform linux/amd64 "${image}" 2>/dev/null >"${TMPDIR:-/tmp}/scorecard-image.$$" \
      || docker save "${image}" >"${TMPDIR:-/tmp}/scorecard-image.$$"
    kind load image-archive "${TMPDIR:-/tmp}/scorecard-image.$$" --name "${CLUSTER_NAME}"
    rm -f "${TMPDIR:-/tmp}/scorecard-image.$$"
  done
fi

mkdir -p "${ARTIFACTS}"
log "operator-sdk scorecard deploy/olm/bundle"
# --pod-security=restricted: the scorecard pods run under the profile OpenShift applies.
# operator-sdk exits non-zero when ANY test fails, the reporting ones included, so
# its status is not the verdict: a result file that parses is, judged below.
set +e
operator-sdk scorecard deploy/olm/bundle --output json --wait-time 180s \
  --service-account default --pod-security=restricted >"${ARTIFACTS}/scorecard.json"
sdk_status=$?
set -e
jq -e '.items | length > 0' "${ARTIFACTS}/scorecard.json" >/dev/null 2>&1 \
  || { cat "${ARTIFACTS}/scorecard.json" >&2 2>/dev/null || true; echo "[scorecard] operator-sdk scorecard produced no results (exit ${sdk_status})" >&2; exit 1; }

log "results"
jq -r '.items[].status.results[] | "\(.state)\t\(.name)"' "${ARTIFACTS}/scorecard.json" | sort

fail=0
result_of() { jq -r --arg n "$1" '[.items[].status.results[] | select(.name == $n)][0].state // "missing"' "${ARTIFACTS}/scorecard.json"; }
for t in "${gating[@]}"; do
  state="$(result_of "${t}")"
  if [[ "${state}" != pass ]]; then
    echo "FAIL ${t}: ${state}" >&2
    jq -r --arg n "${t}" '[.items[].status.results[] | select(.name == $n)][0] | (.log // "", (.errors // [] | join("\n")), (.suggestions // [] | join("\n")))' "${ARTIFACTS}/scorecard.json" >&2
    fail=1
  fi
done
for t in "${reporting[@]}"; do
  state="$(result_of "${t}")"
  [[ "${state}" == pass ]] || echo "note: ${t} is ${state} (reported, not gating)"
done
((fail == 0)) || exit 1
echo "scorecard: ${#gating[@]} gating tests pass"
