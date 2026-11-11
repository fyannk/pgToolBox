#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Tests of hack/changed-paths.sh: the path -> job families table (a gate input
# or an e2e input must never skip the job that exercises it), and a check that
# every tracked path reaches a named arm unless it is listed below as
# deliberately falling through to the fail-open default (every family on).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

classify() { printf '%s\n' "$1" | ./hack/changed-paths.sh --classify | cut -f2; }

# want PATH FAMILY...: the path turns exactly these families on (after the
# go->docker and operator->charts propagation).
want() {
  local path=$1 got expect
  shift
  expect="$(printf '%s\n' "$@" | sort | paste -sd, -)"
  got="$(classify "${path}" | tr ',' '\n' | sort | paste -sd, -)"
  [[ "${got}" == "${expect}" ]] || fail "${path}: families ${got}, want ${expect}"
}

# The kind e2e jobs and the packaging gates.
want internal/backupresolve/pgbackrest.go charts docker evidence go operator
want hack/lib/net.sh charts console docker evidence go operator
want hack/scorecard.sh charts
want hack/chart-gates.sh charts
want hack/chart-values-coverage.py charts
want test/e2e/console/run.sh console docker go
want test/e2e/scope/run.sh charts docker go operator
# check-repo and check-toolchain run in the go family's lint job.
want Dockerfile docker go
want deploy/olm/catalog.Dockerfile charts docker go
want deploy/olm/bundle/manifests/pgtoolbox.clusterserviceversion.yaml charts docker go
want ui/package.json console docker go ui
want ui/e2e.Dockerfile console docker go ui
want hack/evidence/pgbackrest-generator/Dockerfile docker evidence go
want hack/check-toolchain.sh docker go
# Unmapped paths turn everything on.
all="charts console docker docs evidence go headlamp operator ui workflows"
# shellcheck disable=SC2086
want test/crds/cnpg-1.30/clusters.yaml ${all}
# shellcheck disable=SC2086
want deploy/insight-pack/insight-pack.yaml ${all}
# shellcheck disable=SC2086
want .gitleaks.toml ${all}
# shellcheck disable=SC2086
want PROJECT ${all}
# shellcheck disable=SC2086
want some/new/file.txt ${all}
want docs/site/docs/index.md docs
want .github/workflows/release.yml workflows
# check-repo-files (action pins, toolchain pins) and test-hack run in lint-files,
# the `workflows` and `docker` families; every input of those must turn one on.
want .github/workflows/security.yml workflows
want .github/dependabot.yml workflows
want docs/site/docs/development/repository-settings.md docs workflows
want hack/check-action-pins.sh docker go
want hack/test-release.sh docker go
want hack/lib/net.sh charts console docker evidence go operator
want hack/security_report_test.py workflows
want hack/release.sh docker

# Every tracked path reaches a named arm, or is listed here on purpose.
deliberate='^(AUTHORS|LICENSE|NOTICE|PROJECT|\.editorconfig|\.gitignore|\.gitleaks\.toml|deploy/insight-pack/.*|test/crds/.*)$'
bad="$(git ls-files | ./hack/changed-paths.sh --classify | awk -F'\t' '$3 == "default" { print $1 }' | grep -Ev "${deliberate}" || true)"
[[ -z "${bad}" ]] || fail "tracked paths reach only the fail-open default (map them in hack/changed-paths.sh or list them here):
${bad}"

echo "changed-paths: table ok"
