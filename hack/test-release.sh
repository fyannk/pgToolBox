#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Tests of hack/release.sh with no network and no registry: the CHANGELOG
# "Unreleased" refusal and the release-candidate rules of `check`, the
# anonymous-pull gate (`check-public`, with a curl stub) and the scan outcomes
# (`scan-digests`, with a grype stub): a fixable critical is exit 1, a registry
# error is exit 2 and never reads as a finding.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
root="$(pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/ptb-release-test.XXXXXX")"
trap 'rm -rf "${work}"' EXIT
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# expect WANT_EXIT GREP_PATTERN -- command...
expect() {
  local want="$1" pattern="$2" rc=0 out
  shift 3
  out="$("$@" 2>&1)" || rc=$?
  ((rc == want)) || fail "$*: exit ${rc}, want ${want}: ${out}"
  grep -Eq -- "${pattern}" <<<"${out}" || fail "$*: output lacks /${pattern}/: ${out}"
}

# ---- check: a scratch repository with one commit, the real script and charts.
repo="${work}/repo"
mkdir -p "${repo}/hack" "${repo}/charts"
cp hack/release.sh "${repo}/hack/"
cp -r charts/pgtoolbox charts/pgtools "${repo}/charts/"
version="$(sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' charts/pgtoolbox/Chart.yaml)"
changelog() { printf '# Changelog\n\n%s\n\nbody\n\n## [0.0.1] - 2020-01-01\n\nold\n' "$1" > "${repo}/CHANGELOG.md"; }
check() { (cd "${repo}" && ./hack/release.sh check "$@"); }

changelog "## [${version}] - Unreleased"
expect 1 'still says Unreleased' -- check "v${version}"
changelog "## [${version}] - 2026-11-09"
expect 0 'ok: semver' -- check "v${version}"
changelog "## [${version}]"
expect 0 'ok: semver' -- check "v${version}"
changelog "## [9.9.9] - 2026-11-09"
expect 1 'no .## \[' -- check "v${version}"
# A release candidate may use the open section of its final version ...
changelog "## [${version}] - Unreleased"
expect 0 'ok: semver' -- check "v${version}-rc.1"
# ... but not an Unreleased section of its own.
changelog "## [${version}-rc.1] - Unreleased"
expect 1 'still says Unreleased' -- check "v${version}-rc.1"
changelog "## [${version}-rc.1] - 2026-11-09"
expect 0 'ok: semver' -- check "v${version}-rc.1"
# Charts that match neither the rc nor its core version are refused.
expect 1 'tag says' -- check "v9.9.9-rc.1"
expect 1 'semantic' -- check "0.1.0"

# ---- check-public and scan-digests: stubs first on PATH.
stubs="${work}/stubs"
mkdir -p "${stubs}"
cat > "${stubs}/curl" <<'STUB'
#!/usr/bin/env bash
# The token endpoint answers; manifests of "private" packages answer 404.
for a in "$@"; do
  case "${a}" in
    *"/token?"*) echo '{"token":"t"}'; exit 0 ;;
    *"/manifests/"*)
      case "${a}" in *private*) echo 404 ;; *) echo 200 ;; esac
      exit 0 ;;
  esac
done
exit 22
STUB
cat > "${stubs}/grype" <<'STUB'
#!/usr/bin/env bash
case "${GRYPE_STUB}" in
  unauthorized) echo "failed to get image: UNAUTHORIZED" >&2; exit 1 ;;
  critical) echo '{"matches":[{"vulnerability":{"id":"CVE-1","severity":"Critical","fix":{"state":"fixed"}},"artifact":{"name":"a","version":"1"}}]}' ;;
  unfixed) echo '{"matches":[{"vulnerability":{"id":"CVE-2","severity":"Critical","fix":{"state":"not-fixed"}},"artifact":{"name":"a","version":"1"}}]}' ;;
  clean) echo '{"matches":[]}' ;;
esac
STUB
chmod +x "${stubs}/curl" "${stubs}/grype"

printf 'ghcr.io/o/pgtoolbox-operator@sha256:%064d\nghcr.io/o/charts/pgtools@sha256:%064d\n' 1 2 > "${work}/ok.txt"
printf 'ghcr.io/o/pgtoolbox-operator@sha256:%064d\nghcr.io/o/private-thing@sha256:%064d\nghcr.io/o/charts/privatechart@sha256:%064d\n' 1 2 3 > "${work}/mixed.txt"
export PATH="${stubs}:${PATH}"
expect 0 'every digest is publicly pullable' -- ./hack/release.sh check-public "${work}/ok.txt"
expect 1 'private-thing' -- ./hack/release.sh check-public "${work}/mixed.txt"
expect 1 'charts/privatechart' -- ./hack/release.sh check-public "${work}/ok.txt" "${work}/mixed.txt"
expect 1 'Re-run failed jobs' -- ./hack/release.sh check-public "${work}/mixed.txt"

export GITHUB_STEP_SUMMARY="${work}/summary.md"
GRYPE_STUB=clean expect 0 'no fixable critical' -- ./hack/release.sh scan-digests "${work}/ok.txt"
GRYPE_STUB=unfixed expect 0 'no fixable critical' -- ./hack/release.sh scan-digests "${work}/ok.txt"
GRYPE_STUB=critical expect 1 'fixable critical' -- ./hack/release.sh scan-digests "${work}/ok.txt"
GRYPE_STUB=unauthorized expect 2 'scan error, not a finding' -- ./hack/release.sh scan-digests "${work}/ok.txt"
# An unauthorized scan must never be reported as a vulnerability.
out="$(GRYPE_STUB=unauthorized ./hack/release.sh scan-digests "${work}/ok.txt" 2>&1 || true)"
! grep -q 'fixable critical vulnerabilit' <<<"${out}" || fail "a registry error was reported as a vulnerability"
# Both platforms are scanned.
out="$(GRYPE_STUB=clean ./hack/release.sh scan-digests "${work}/ok.txt" 2>&1)"
grep -q 'linux/amd64' <<<"${out}" && grep -q 'linux/arm64' <<<"${out}" || fail "scan did not cover both platforms"

echo "ok: hack/release.sh"
cd "${root}"
