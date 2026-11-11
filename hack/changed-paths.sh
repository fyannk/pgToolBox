#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Which families of CI jobs does this change need? Prints `name=true|false`
# lines for $GITHUB_OUTPUT. Path filters live here, in one reviewable place,
# because `on.<event>.paths` applies to a whole workflow, not to jobs, and a
# required check that a path filter skips never reports and blocks the merge.
#
# Fail-open: an event with no meaningful base (schedule, dispatch, tag push), a
# base the clone cannot see, or a change to anything that affects every family
# (the pipeline itself, the toolchain, the module files, the Makefile) turns
# every family on. A skipped job is a decision this script made, visible in
# the job list as skipped; the `ci-ok` job treats skipped as passed and failed
# or cancelled as not.
#
# Environment: EVENT, BASE_SHA (pull request base or push "before"), HEAD_SHA.
set -euo pipefail

families=(go operator console evidence charts docs ui headlamp workflows docker)

emit_all() {
  local value=$1 f
  for f in "${families[@]}"; do echo "${f}=${value}"; done
}

declare -A on
reset() { local f; for f in "${families[@]}"; do on[$f]=false; done; defaulted=false; }
mark() { on[$1]=true; }
mark_all() { local f; for f in "${families[@]}"; do mark "$f"; done; }
mark_kind() { mark go; mark operator; mark console; mark evidence; mark charts; }

# classify PATH marks the families one changed path needs. The first matching
# arm wins; the last arm is the fail-open default: a path no arm names turns
# every family on, so a new kind of file is never silently untested.
# hack/test-changed-paths.sh pins the table and fails on a tracked path that
# only reaches the default without being listed there.
classify() {
  case "$1" in
    .github/workflows/ci.yml | .github/workflows/e2e-kind.yml | .github/actions/* | mise.toml | go.mod | go.sum | go.work | go.work.sum | Makefile | hack/common.mk | .golangci.yml | hack/changed-paths.sh)
      mark_all ;;
    # The embedded documentation bundle is generated from the site's build
    # (make docs-bundle, the Dockerfile's docs stages) and carries the chart's
    # version; only its loader and the placeholder are in the tree.
    internal/console/docsbundle/*) mark go; mark console; mark docs ;;
    hack/docsbundle/* | hack/mk/docs.mk) mark go; mark docs ;;
    charts/pgtoolbox/Chart.yaml) mark charts; mark docs ;;
    api/* | config/*) mark go; mark operator; mark charts ;;
    # The operator, the evidence agent (selfresolve) and the chart tests import
    # the backup-repository resolver: everything that exercises them runs.
    internal/backupresolve/*) mark go; mark operator; mark evidence ;;
    internal/operator/* | cmd/operator/*) mark go; mark operator ;;
    # The console's own kind scenario.
    test/e2e/console/*) mark go; mark console ;;
    test/e2e/*) mark go; mark operator ;;
    # Live-fire scripts against a running stack (make pentest): linted, not run in CI.
    test/pentest/*) mark go ;;
    hack/release.sh | hack/image-profile.sh) mark docker ;;
    internal/console/* | cmd/console/* | hack/console/*)
      mark go; mark console ;;
    pkg/evidence/* | internal/evidence/* | cmd/evidence-agent/* | hack/evidence/* | deploy/evidence/*)
      mark go; mark evidence ;;
    pkg/*) mark go; mark console; mark operator ;;
    # The OLM bundle and catalog are checked by check-repo (the toolchain pins in
    # catalog.Dockerfile, opm's go.mod) as well as packaged.
    deploy/olm/*) mark go; mark charts ;;
    charts/* | hack/rules/* | hack/validate-packaging.sh | hack/sync-manifests.sh | hack/scorecard.sh | hack/chart-gates.sh | hack/chart-values-coverage.py) mark charts ;;
    Dockerfile | .dockerignore) mark docker ;;
    docs/site/* | *.md | .codespellrc | lychee.toml) mark docs ;;
    # The docs site copies the console tour's screenshots from these baselines
    # at build time (docs/site/scripts/copy-screens.mjs): the site, so `make
    # docs`, must run when one changes, besides the UI gates below.
    ui/e2e/__screenshots__/screens.spec.ts/*) mark ui; mark console; mark docker; mark docs ;;
    # check-toolchain reads ui/package.json (Playwright) and ui/e2e.Dockerfile.
    ui/package.json | ui/e2e.Dockerfile) mark go; mark ui; mark console; mark docker ;;
    # The console image embeds the SPA built from ui/.
    ui/*) mark ui; mark console; mark docker ;;
    headlamp/* | hack/mk/headlamp.mk) mark headlamp ;;
    # The kind e2e jobs (operator, console, evidence agent, chart) are driven by
    # these scripts, the shared Makefile fragments and the pins in hack/lib
    # (CNPG, cert-manager); a change to one must run them.
    hack/e2e-* | hack/kind* | hack/matrix.sh | hack/mk/*.mk | hack/test-multiarch.sh | hack/lib/*) mark_kind ;;
    .github/*) mark workflows ;;
    hack/security_report* | hack/lint-workflows.sh) mark workflows ;;
    hack/*) mark go ;;
    *.go) mark go ;;
    *) mark_all; defaulted=true ;;
  esac
  # check-repo (lint job) reads every Dockerfile's FROM lines.
  case "$1" in
    *Dockerfile | */Dockerfile.*) mark go ;;
  esac
}

# Anything that builds an image or ships through the chart follows the code it
# packages.
propagate() {
  if [[ "${on[go]}" == true ]]; then on[docker]=true; fi
  if [[ "${on[operator]}" == true ]]; then on[charts]=true; fi
}

# --classify reads paths on stdin and prints "path<TAB>families<TAB>default|ok"
# (for hack/test-changed-paths.sh); it needs no git state.
if [[ "${1:-}" == "--classify" ]]; then
  while IFS= read -r path; do
    [[ -n "${path}" ]] || continue
    reset
    classify "${path}"
    propagate
    list=""
    for f in "${families[@]}"; do [[ "${on[$f]}" == true ]] && list+="${list:+,}${f}"; done
    state=ok
    [[ "${defaulted}" == true ]] && state=default
    printf '%s\t%s\t%s\n' "${path}" "${list}" "${state}"
  done
  exit 0
fi

event="${EVENT:-}"
base="${BASE_SHA:-}"
head="${HEAD_SHA:-HEAD}"

if [[ "${event}" != "pull_request" && "${event}" != "push" ]] \
  || [[ -z "${base}" || "${base}" =~ ^0+$ ]]; then
  emit_all true
  exit 0
fi

if ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
  git fetch --quiet --no-tags --depth=1 origin "${base}" 2>/dev/null || true
fi
if ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
  echo "::notice::base ${base} is not reachable; running every job family" >&2
  emit_all true
  exit 0
fi

changed="$(git diff --name-only "${base}" "${head}")"
echo "::group::changed files" >&2
printf '%s\n' "${changed}" >&2
echo "::endgroup::" >&2

reset
while IFS= read -r path; do
  [[ -n "${path}" ]] || continue
  classify "${path}"
done <<<"${changed}"
propagate

for f in "${families[@]}"; do echo "${f}=${on[$f]}"; done
