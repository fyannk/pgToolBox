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

declare -A on
for f in "${families[@]}"; do on[$f]=false; done
mark() { on[$1]=true; }

while IFS= read -r path; do
  [[ -n "${path}" ]] || continue
  case "${path}" in
    .github/workflows/ci.yml | .github/workflows/e2e-kind.yml | .github/actions/* | mise.toml | go.mod | go.sum | go.work | go.work.sum | Makefile | hack/common.mk | .golangci.yml | hack/changed-paths.sh)
      for f in "${families[@]}"; do mark "$f"; done ;;
    # The embedded documentation bundle is generated from the site's build
    # (make docs-bundle, the Dockerfile's docs stages) and carries the chart's
    # version; only its loader and the placeholder are in the tree.
    internal/console/docsbundle/*) mark go; mark console; mark docs ;;
    hack/docsbundle/* | hack/mk/docs.mk) mark go; mark docs ;;
    charts/pgtoolbox/Chart.yaml) mark charts; mark docs ;;
    api/* | config/*) mark go; mark operator; mark charts ;;
    internal/operator/* | cmd/operator/* | test/e2e/*) mark go; mark operator ;;
    hack/release.sh | hack/image-profile.sh) mark docker ;;
    internal/console/* | cmd/console/* | hack/console/*)
      mark go; mark console ;;
    pkg/evidence/* | internal/evidence/* | cmd/evidence-agent/* | hack/evidence/* | deploy/evidence/*)
      mark go; mark evidence ;;
    pkg/*) mark go; mark console; mark operator ;;
    charts/* | hack/rules/* | deploy/olm/* | hack/validate-packaging.sh | hack/sync-manifests.sh) mark charts ;;
    Dockerfile | .dockerignore) mark docker ;;
    docs/site/* | *.md | .codespellrc | lychee.toml) mark docs ;;
    # The docs site copies the console tour's screenshots from these baselines
    # at build time (docs/site/scripts/copy-screens.mjs): the site, so `make
    # docs`, must run when one changes, besides the UI gates below.
    ui/e2e/__screenshots__/screens.spec.ts/*) mark ui; mark console; mark docker; mark docs ;;
    # The console image embeds the SPA built from ui/.
    ui/*) mark ui; mark console; mark docker ;;
    headlamp/* | hack/mk/headlamp.mk) mark headlamp ;;
    .github/*) mark workflows ;;
    hack/security_report* | hack/lint-workflows.sh) mark workflows ;;
    hack/*) mark go ;;
    *.go) mark go ;;
  esac
done <<<"${changed}"

# Anything that builds an image or ships through the chart follows the code it
# packages.
if [[ "${on[go]}" == true ]]; then on[docker]=true; fi
if [[ "${on[operator]}" == true ]]; then on[charts]=true; fi

for f in "${families[@]}"; do echo "${f}=${on[$f]}"; done
