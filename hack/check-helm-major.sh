#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Fail early, with a plain message, when the helm on PATH has another major
# version than mise.toml pins. helm 3 and helm 4 render and serialise
# differently, so `make verify-manifests` against the wrong major reports a
# diff that is the tool's, not the tree's.
#
#   hack/check-helm-major.sh           (ROOT and HELM override the defaults; tests)
set -euo pipefail

root="${ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
helm="${HELM:-helm}"

want="$(awk -F'"' '$2 == "helm" { print $4; exit }' "${root}/mise.toml")"
[[ -n "${want}" ]] || { echo "check-helm-major: mise.toml pins no helm" >&2; exit 1; }
command -v "${helm}" >/dev/null 2>&1 || { echo "check-helm-major: helm is not installed; mise.toml pins ${want} ('mise install')" >&2; exit 1; }
have="$("${helm}" version --short 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1 || true)"
if [[ "${have%%.*}" != "${want%%.*}" ]]; then
  echo "check-helm-major: helm ${have:-unknown} is on PATH but mise.toml pins ${want}; the major differs, so rendered manifests would differ for reasons that are not in the tree. Run 'mise install' (or 'mise exec -- make ...')." >&2
  exit 1
fi
