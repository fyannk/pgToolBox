#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# make doctor: compare the tools on this host with mise.toml, the single source
# of tool versions, and say how to fix each mismatch. Read-only; exits non-zero
# when a tool is missing or at another version, so CI could call it too.
#
# It needs no mise: mise.toml is read with awk (keys are quoted for that), so a
# machine without mise gets a useful report whose first line is the install
# hint.
set -euo pipefail

root="${ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "${root}"

want() { awk -F'"' -v k="$1" '$2 == k { print $4; exit }' mise.toml; }

status=0
row() { # tool wanted found
  if [[ -z "$3" ]]; then
    printf '  %-14s %-34s MISSING\n' "$1" "wants $2"
    status=1
  elif [[ "$3" != "$2" ]]; then
    printf '  %-14s %-34s found %s\n' "$1" "wants $2" "$3"
    status=1
  else
    printf '  %-14s %-34s ok\n' "$1" "wants $2"
  fi
}

first_version() { grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1; }

if ! command -v mise >/dev/null 2>&1; then
  echo "mise is not installed (https://mise.jdx.dev/getting-started.html); then: mise install"
fi
echo "tools pinned by mise.toml:"

found() { command -v "$1" >/dev/null 2>&1 && "${@:2}" 2>&1 | first_version || true; }

row go "$(want go)" "$(found go go version)"
row node "$(want node)" "$(found node node --version)"
row pnpm "$(want pnpm)" "$(found pnpm pnpm --version)"
row helm "$(want helm)" "$(found helm helm version --short)"
row kind "$(want kind)" "$(found kind kind version)"
row kubectl "$(want kubectl)" "$(found kubectl kubectl version --client)"
row actionlint "$(want actionlint)" "$(found actionlint actionlint -version)"
row hadolint "$(want hadolint)" "$(found hadolint hadolint --version)"
row zizmor "$(want zizmor)" "$(found zizmor zizmor --version)"
row shellcheck "$(want aqua:koalaman/shellcheck)" "$(found shellcheck shellcheck --version)"
row ct "$(want aqua:helm/chart-testing)" "$(found ct ct version)"
row kubeconform "$(want aqua:yannh/kubeconform)" "$(found kubeconform kubeconform -v)"
row kube-linter "$(want github:stackrox/kube-linter)" "$(found kube-linter kube-linter version)"
row helm-docs "$(want go:github.com/norwoodj/helm-docs/cmd/helm-docs)" "$(found helm-docs helm-docs --version)"
row golangci-lint "$(want go:github.com/golangci/golangci-lint/v2/cmd/golangci-lint)" "$(found golangci-lint golangci-lint --version)"
row govulncheck "$(want go:golang.org/x/vuln/cmd/govulncheck)" "$(found govulncheck govulncheck -version)"
row gitleaks "$(want aqua:gitleaks/gitleaks)" "$(found gitleaks gitleaks version)"
row grype "$(want aqua:anchore/grype)" "$(found grype grype version)"
row osv-scanner "$(want aqua:google/osv-scanner)" "$(found osv-scanner osv-scanner --version)"
row cosign "$(want aqua:sigstore/cosign)" "$(found cosign cosign version)"
row controller-gen "$(want go:sigs.k8s.io/controller-tools/cmd/controller-gen)" "$(found controller-gen controller-gen --version)"
# setup-envtest prints no version; presence is what matters.
if command -v setup-envtest >/dev/null 2>&1; then
  printf '  %-14s %-34s ok (version not reported)\n' setup-envtest "wants $(want go:sigs.k8s.io/controller-runtime/tools/setup-envtest)"
else
  printf '  %-14s %-34s MISSING\n' setup-envtest "wants $(want go:sigs.k8s.io/controller-runtime/tools/setup-envtest)"
  status=1
fi

row operator-sdk "$(want github:operator-framework/operator-sdk)" "$(found operator-sdk operator-sdk version)"
# crd-ref-docs (make crd-ref-docs falls back to `go run` of the pinned version,
# so a missing binary is only slower, but a different one would render another page).
row crd-ref-docs "$(want go:github.com/elastic/crd-ref-docs)" "$(found crd-ref-docs crd-ref-docs --version)"
row lychee "$(want aqua:lycheeverse/lychee)" "$(found lychee lychee --version)"
row codespell "$(want pipx:codespell)" "$(found codespell codespell --version)"

# untt (helm-unittest) prints no version either; presence is what matters.
if command -v untt >/dev/null 2>&1; then
  printf '  %-14s %-34s ok (version not reported)\n' untt "wants $(want github:helm-unittest/helm-unittest)"
else
  printf '  %-14s %-34s MISSING\n' untt "wants $(want github:helm-unittest/helm-unittest)"
  status=1
fi

echo "other tools (not pinned here; the e2e, image and release targets need them):"
for t in docker git make jq curl python3 gh; do
  if command -v "${t}" >/dev/null 2>&1; then
    printf '  %-14s ok\n' "${t}"
  else
    printf '  %-14s MISSING\n' "${t}"
    status=1
  fi
done

# go.mod's toolchain line must follow mise.toml; a drift means CI builds with
# one Go and a developer's go.work with another.
mod_tc="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' go.mod)"
if [[ "${mod_tc}" != "$(want go)" ]]; then
  echo "go.mod says toolchain go${mod_tc}; mise.toml pins go $(want go) — move them together"
  status=1
fi

if (( status )); then
  echo "doctor: mismatches above; 'mise install' fixes the pinned tools."
else
  echo "doctor: all good."
fi
exit "${status}"
