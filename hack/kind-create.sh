#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# `kind create cluster` with one retry, for the e2e jobs.
#
#   hack/kind-create.sh --name ptb-e2e --image kindest/node:v1.34.0 --wait 300s --config - <<YAML
#
# Under a heavy load kubeadm can give up on a control plane that would
# have come up ("kube-apiserver is not healthy after 4m0s"). That failure says nothing about the code under test,
# so the cluster is deleted and created once more. A config read from stdin
# (--config -) is kept for the second attempt.
#
# Environment: KIND_CREATE_ATTEMPTS (2).
set -euo pipefail

attempts="${KIND_CREATE_ATTEMPTS:-2}"
name=kind
args=("$@")
config=""
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    --name) name="${args[i + 1]}" ;;
    --name=*) name="${args[i]#--name=}" ;;
    --config)
      if [[ "${args[i + 1]}" == - ]]; then
        config="$(mktemp)"
        trap 'rm -f "${config}"' EXIT
        cat >"${config}"
        args[i + 1]="${config}"
      fi
      ;;
  esac
done

for ((n = 1; ; n++)); do
  if kind create cluster "${args[@]}"; then
    exit 0
  fi
  if ((n >= attempts)); then
    echo "[kind-create] ${name}: creation failed ${n} time(s), giving up" >&2
    exit 1
  fi
  echo "[kind-create] ${name}: creation failed (attempt ${n}/${attempts}); deleting and retrying" >&2
  kind delete cluster --name "${name}" >/dev/null 2>&1 || true
  sleep "${KIND_CREATE_PAUSE:-15}"
done
