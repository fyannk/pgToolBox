#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The static gates of the two charts, charts/pgtoolbox (the operator) and
# charts/pgtools (the team tools), one per
# subcommand so CI and a developer run the same thing:
#
#   hack/chart-gates.sh unittest      helm-unittest suites in charts/*/tests
#   hack/chart-gates.sh lint          ct lint, once per charts/*/ci/*-values.yaml
#   hack/chart-gates.sh kubeconform   every rendering against the Kubernetes schemas
#   hack/chart-gates.sh kube-linter   every rendering against kube-linter's defaults
#   hack/chart-gates.sh kube-linter-sarif DIR
#                                     the same kube-linter run as SARIF, one file per chart in DIR, for code scanning (never fails on findings)
#   hack/chart-gates.sh docs          README.md of each chart is what helm-docs renders, and every value is described
#   hack/chart-gates.sh docs-write    regenerate charts/*/README.md (make chart-docs)
#   hack/chart-gates.sh all
#
# Tools come from mise.toml (ct, kubeconform, kube-linter, helm-unittest's untt, helm-docs);
# `make doctor` says which is missing. Needs network for kubeconform's schemas.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${root}"

charts=(charts/pgtoolbox charts/pgtools)
# The Kubernetes version the schemas are taken from: the kind node image the e2e
# uses (hack/e2e-chart.sh KIND_NODE_IMAGE) is the oldest the chart is exercised on.
k8s_version="${K8S_SCHEMA_VERSION:-1.34.0}"
# Route (OpenShift) has no published schema; HTTPRoute is validated against the
# Gateway API schema from the datree catalogue.
crd_catalog='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "chart-gates: $1 is not on PATH (mise install; see mise.toml)" >&2; exit 2; }
}

# renderings prints one values file per line, charts/<chart>/ci/*-values.yaml.
renderings() {
  local c f
  for c in "${charts[@]}"; do
    for f in "${c}"/ci/*-values.yaml; do
      printf '%s\n' "${f}"
    done
  done
}

# render renders the chart a ci values file belongs to.
render() { helm template t "${1%/ci/*}" --namespace team-a -f "$1"; }

unittest() {
  need untt
  untt "${charts[@]}"
}

lint() {
  need ct
  need yamllint
  ct lint --config charts/ct.yaml
}

kubeconform() {
  need kubeconform
  local f fail=0
  mkdir -p "${TMPDIR:-/tmp}/kubeconform-cache"
  while read -r f; do
    echo "== kubeconform ${f}"
    render "${f}" | command kubeconform -strict -summary -cache "${TMPDIR:-/tmp}/kubeconform-cache" -kubernetes-version "${k8s_version}" \
      -skip Route,CustomResourceDefinition \
      -schema-location default -schema-location "${crd_catalog}" || fail=1
  done < <(renderings)
  return "${fail}"
}

kube_linter() {
  need kube-linter
  local f fail=0 extra
  while read -r f; do
    echo "== kube-linter ${f}"
    # The operator keeps its single anti-affinity-less Deployment; the team
    # tools are held to the defaults.
    extra=()
    [[ "${f}" != charts/pgtoolbox/* ]] || extra=(--config charts/kube-linter-operator.yaml)
    render "${f}" | command kube-linter lint ${extra[@]+"${extra[@]}"} - || fail=1
  done < <(renderings)
  return "${fail}"
}

# kube_linter_sarif DIR writes DIR/kube-linter-<chart>.sarif: every rendering of
# a chart linted together, with the chart's own configuration. It reports; the
# `kube-linter` subcommand above is the gate. A failure to lint at all (no
# output) is an error.
kube_linter_sarif() {
  need kube-linter
  local out="${1:?usage: $0 kube-linter-sarif DIR}" c f n=0 dir extra
  mkdir -p "${out}"
  for c in "${charts[@]}"; do
    dir="${out}/render-$(basename "${c}")"
    rm -rf "${dir}"
    mkdir -p "${dir}"
    n=0
    for f in "${c}"/ci/*-values.yaml; do
      n=$((n + 1))
      render "${f}" > "${dir}/$(basename "${f}" .yaml).yaml"
    done
    extra=()
    [[ "${c}" != charts/pgtoolbox ]] || extra=(--config charts/kube-linter-operator.yaml)
    command kube-linter lint "${dir}" --format sarif ${extra[@]+"${extra[@]}"} > "${out}/kube-linter-$(basename "${c}").sarif" || true
    [[ -s "${out}/kube-linter-$(basename "${c}").sarif" ]] || { echo "chart-gates: kube-linter produced no SARIF for ${c}" >&2; return 1; }
    echo "kube-linter: ${n} rendering(s) of ${c}"
  done
}

# helm-docs renders README.md from README.md.gotmpl and the `# --` comments of
# values.yaml. The check renders into a copy and compares, so it never touches the
# working tree (and works on a dirty one); coverage fails on a value without a
# description, which helm-docs would drop from the table without a word.
docs_render() { helm-docs --chart-search-root "$1" --template-files README.md.gotmpl --sort-values-order file --log-level warning; }

docs_write() {
  need helm-docs
  docs_render charts
}

docs() {
  need helm-docs
  need python3
  python3 -I hack/chart-values-coverage.py charts/*/values.yaml || return 1
  local tmp c fail=0
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'" RETURN
  cp -R charts "${tmp}/charts"
  docs_render "${tmp}/charts"
  for c in "${charts[@]}"; do
    if ! diff -u "${c}/README.md" "${tmp}/${c}/README.md"; then
      echo "chart-docs: ${c}/README.md is stale; run 'make chart-docs' and commit the result" >&2
      fail=1
    fi
  done
  return "${fail}"
}

case "${1:-all}" in
  unittest) unittest ;;
  lint) lint ;;
  kubeconform) kubeconform ;;
  kube-linter) kube_linter ;;
  kube-linter-sarif) shift; kube_linter_sarif "$@" ;;
  docs) docs ;;
  docs-write) docs_write ;;
  all) unittest && lint && kubeconform && kube_linter && docs ;;
  *) echo "usage: $0 unittest|lint|kubeconform|kube-linter|docs|docs-write|all" >&2; exit 2 ;;
esac
