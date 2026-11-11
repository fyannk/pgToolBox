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
# The dry run reports an open section instead of refusing it, but a real
# release (REQUIRE_TAG=1) still refuses it.
expect 0 'note: .*still says Unreleased' -- env ALLOW_UNRELEASED=1 bash -c 'cd "$1" && ./hack/release.sh check "$2"' _ "${repo}" "v${version}"
expect 1 'still says Unreleased' -- env ALLOW_UNRELEASED=1 REQUIRE_TAG=1 bash -c 'cd "$1" && ./hack/release.sh check "$2"' _ "${repo}" "v${version}"
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
# A candidate that is really cut (REQUIRE_TAG) or preflighted (REQUIRE_DATED)
# needs the dated section too: rc and final share one commit, so the date is
# in the tree before rc.1, never in a commit between them.
changelog "## [${version}] - Unreleased"
expect 1 "date it \\('## \\[${version//./\\.}\\] - " -- env REQUIRE_DATED=1 bash -c 'cd "$1" && ./hack/release.sh check "$2"' _ "${repo}" "v${version}-rc.1"
# ... and the message names the core heading, never the candidate's own.
out="$(cd "${repo}" && REQUIRE_DATED=1 ./hack/release.sh check "v${version}-rc.1" 2>&1 || true)"
! grep -q "date it ('## \[${version}-rc" <<<"${out}" || fail "the Unreleased message tells the owner to date the rc heading: ${out}"
changelog "## [${version}] - 2026-11-09"
expect 0 'ok: semver' -- env REQUIRE_DATED=1 bash -c 'cd "$1" && ./hack/release.sh check "$2"' _ "${repo}" "v${version}-rc.1"
# Charts that match neither the rc nor its core version are refused.
expect 1 'tag says' -- check "v9.9.9-rc.1"
expect 1 'semantic' -- check "0.1.0"

# A failed release candidate is never moved or deleted (the tag ruleset allows
# neither): it is superseded by the next one. With rc.1 already tagged, rc.2 of
# the same tree passes the check, and so does the final version.
git -C "${repo}" init -q
git -C "${repo}" config user.email t@example.com
git -C "${repo}" config user.name t
git -C "${repo}" config commit.gpgsign false
git -C "${repo}" config tag.gpgsign false
changelog "## [${version}] - Unreleased"
git -C "${repo}" add -A
git -C "${repo}" commit -q -m "chore: scratch"
git -C "${repo}" tag -a "v${version}-rc.1" -m rc1
expect 0 'ok: semver' -- check "v${version}-rc.2"
expect 0 'is an annotated tag' -- check "v${version}-rc.1"
git -C "${repo}" tag "v${version}-rc.3" # lightweight: refused
expect 1 'must be annotated' -- check "v${version}-rc.3"
git -C "${repo}" tag -d "v${version}-rc.3" >/dev/null

# ---- commits: the notes of a final tag cover everything since the previous
# FINAL tag (release candidates included); a candidate starts at the previous
# tag of any kind.
crepo="${work}/commits"
mkdir -p "${crepo}/hack"
cp hack/release.sh "${crepo}/hack/"
git -C "${crepo}" init -q
git -C "${crepo}" config user.email t@example.com
git -C "${crepo}" config user.name t
git -C "${crepo}" config tag.gpgsign false
git -C "${crepo}" config commit.gpgsign false
cc() { git -C "${crepo}" commit -q --allow-empty -m "$1"; }
cc "feat: before everything"; git -C "${crepo}" tag -a v0.0.1 -m x
cc "feat: in the first candidate"; git -C "${crepo}" tag -a v0.1.0-rc.1 -m x
cc "fix: between the candidates"; git -C "${crepo}" tag -a v0.1.0-rc.2 -m x
cc "docs: after the last candidate"; git -C "${crepo}" tag -a v0.1.0 -m x
commits() { (cd "${crepo}" && ./hack/release.sh commits "$1"); }
out="$(commits v0.1.0)"
for want in "in the first candidate" "between the candidates" "after the last candidate"; do
  grep -q "${want}" <<<"${out}" || fail "notes of v0.1.0 lack '${want}': ${out}"
done
! grep -q "before everything" <<<"${out}" || fail "notes of v0.1.0 reach back past v0.0.1: ${out}"
out="$(commits v0.1.0-rc.2)"
grep -q "between the candidates" <<<"${out}" || fail "notes of rc.2 lack its own commit: ${out}"
! grep -q "in the first candidate" <<<"${out}" || fail "notes of rc.2 repeat rc.1's commits: ${out}"
out="$(commits v0.1.0-rc.1)"
grep -q "in the first candidate" <<<"${out}" && ! grep -q "before everything" <<<"${out}" || fail "notes of rc.1 start at the wrong tag: ${out}"

# commits-link: a link, never the log; the first release links the history.
link() { (cd "${crepo}" && ./hack/release.sh commits-link "$1"); }
out="$(link v0.1.0)"
grep -q 'compare/v0.0.1...v0.1.0' <<<"${out}" && ! grep -q "feat:" <<<"${out}" || fail "commits-link of v0.1.0: ${out}"
out="$(link v0.0.1)"
grep -q 'commits/v0.0.1' <<<"${out}" || fail "commits-link of the first tag: ${out}"

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
cat > "${stubs}/gh" <<'STUB'
#!/usr/bin/env bash
# The check-runs listing, already reduced by --jq: name, status, conclusion,
# started_at, id (tab separated). GH_STUB_RUNS names the file that holds it.
case "$1" in
  api)
    # actions/runs: path, event, status, conclusion, id (GH_STUB_WF); the
    # check-runs listing otherwise (GH_STUB_RUNS).
    case "$*" in
      *actions/runs*) [[ -z "${GH_STUB_WF:-}" ]] || cat "${GH_STUB_WF}" ;;
      *) [[ -z "${GH_STUB_RUNS:-}" ]] || cat "${GH_STUB_RUNS}" ;;
    esac ;;
  attestation)
    # Fails the first GH_STUB_FAILS calls (an attestation that lags the push).
    n="$(cat "${GH_STUB_COUNT}" 2>/dev/null || echo 0)"
    echo $((n + 1)) > "${GH_STUB_COUNT}"
    ((n >= ${GH_STUB_FAILS:-0})) || exit 1 ;;
  *) exit 1 ;;
esac
STUB
cat > "${stubs}/docker" <<'STUB'
#!/usr/bin/env bash
[[ -z "${DOCKER_STUB_FAIL:-}" ]] || { echo "denied" >&2; exit 1; }
printf '"sha256:%064d"\n' 7
STUB
cat > "${stubs}/helm" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == pull ]]; then printf 'Pulled: x\nDigest: sha256:%064d\n' 8; exit 0; fi
# Everything else (template, package) is the real helm.
PATH="${PATH#*:}" exec helm "$@"
STUB
cat > "${stubs}/cosign" <<'STUB'
#!/usr/bin/env bash
[[ -z "${COSIGN_STUB_FAIL:-}" ]] || { echo "no matching signatures" >&2; exit 1; }
STUB
chmod +x "${stubs}/curl" "${stubs}/grype" "${stubs}/gh" "${stubs}/docker" "${stubs}/helm" "${stubs}/cosign"

printf 'ghcr.io/o/pgtoolbox-operator@sha256:%064d\nghcr.io/o/charts/pgtools@sha256:%064d\n' 1 2 > "${work}/ok.txt"
printf 'ghcr.io/o/pgtoolbox-operator@sha256:%064d\nghcr.io/o/private-thing@sha256:%064d\nghcr.io/o/charts/privatechart@sha256:%064d\n' 1 2 3 > "${work}/mixed.txt"
export PATH="${stubs}:${PATH}"
export CHECK_PUBLIC_ATTEMPTS=1 CHECK_PUBLIC_SLEEP=0
expect 0 'every digest is publicly pullable' -- ./hack/release.sh check-public "${work}/ok.txt"
expect 1 'private-thing' -- ./hack/release.sh check-public "${work}/mixed.txt"
expect 1 'charts/privatechart' -- ./hack/release.sh check-public "${work}/ok.txt" "${work}/mixed.txt"
expect 1 'Re-run failed jobs' -- ./hack/release.sh check-public "${work}/mixed.txt"
# While it waits, one ::notice names the settings page of each private package.
expect 1 '::notice title=Make these GHCR packages public::.*packages/container/private-thing/settings' -- ./hack/release.sh check-public "${work}/mixed.txt"

# ---- wait-ci: the newest run of a check counts, whatever order it is listed
# in; a commit with no check run at all fails fast with the dispatch hint.
runs="${work}/runs.tsv"
tab() { printf '%s\t%s\t%s\t%s\t%s\n' "$@"; }
wfruns="${work}/wf.tsv"
# Full-tier runs of both workflows (a dispatch), unless a test says otherwise.
{ tab ci.yml workflow_dispatch completed success 5; tab security.yml schedule completed success 6; } > "${wfruns}"
export GH_STUB_RUNS="${runs}" GH_STUB_WF="${wfruns}" CI_WAIT_SLEEP=0 CI_WAIT_ATTEMPTS=3 CI_WAIT_EMPTY_ATTEMPTS=2
# An old failure followed (by id) by a green re-run, listed newest first.
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; tab ci-ok completed failure 2026-11-10T09:00:00Z 10; tab security-gate completed success 2026-11-10T09:00:00Z 11; } > "${runs}"
expect 0 'green on' -- ./hack/release.sh wait-ci deadbeef
# An old success followed by a newer failure, listed oldest first.
{ tab ci-ok completed success 2026-11-10T09:00:00Z 10; tab ci-ok completed failure 2026-11-10T10:00:00Z 20; tab security-gate completed success 2026-11-10T09:00:00Z 11; } > "${runs}"
expect 1 'CI failed on deadbeef: ci-ok=failure' -- ./hack/release.sh wait-ci deadbeef
# A newer run still in progress hides the older green one.
{ tab ci-ok in_progress "" 2026-11-10T10:00:00Z 20; tab ci-ok completed success 2026-11-10T09:00:00Z 10; tab security-gate completed success 2026-11-10T09:00:00Z 11; } > "${runs}"
expect 1 'did not finish' -- ./hack/release.sh wait-ci deadbeef
# No check run at all: fail fast, with the command that fixes it.
: > "${runs}"
expect 1 'gh workflow run security.yml --ref main' -- ./hack/release.sh wait-ci deadbeef
# preflight: one poll, no waiting; a green pair on a dated CHANGELOG passes,
# a missing check run or an Unreleased heading fails before any tag is pushed.
pre() { (cd "${repo}" && ./hack/release.sh preflight "$@"); }
head="$(git -C "${repo}" rev-parse HEAD)"
git -C "${repo}" update-ref refs/remotes/origin/main "${head}"
changelog "## [${version}] - 2026-11-09"
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; tab security-gate completed success 2026-11-10T10:00:00Z 21; } > "${runs}"
expect 0 'ok: preflight' -- pre "v${version}" "${head}"
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; } > "${runs}"
expect 1 'did not finish' -- pre "v${version}" "${head}"
: > "${runs}"
expect 1 'no check run exists for' -- pre "v${version}" "${head}"
expect 1 'gh workflow run security.yml --ref main' -- pre "v${version}" "${head}"
expect 1 'make release-preflight TAG=<tag>' -- pre "v${version}" "${head}"
# The tagged SHA must be an ancestor of origin/main: refused before any check-run call.
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; tab security-gate completed success 2026-11-10T10:00:00Z 21; } > "${runs}"
stray="$(git -C "${repo}" commit-tree -p "${head}" -m stray "$(git -C "${repo}" rev-parse "${head}^{tree}")")"
expect 1 'not an ancestor of origin/main' -- pre "v${version}" "${stray}"
git -C "${repo}" update-ref -d refs/remotes/origin/main
expect 1 'origin/main is not fetched' -- pre "v${version}" "${head}"
git -C "${repo}" update-ref refs/remotes/origin/main "${head}"
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; tab security-gate completed success 2026-11-10T10:00:00Z 21; } > "${runs}"
changelog "## [${version}] - Unreleased"
expect 1 'still says Unreleased' -- pre "v${version}" "${head}"
expect 1 'still says Unreleased' -- pre "v${version}-rc.1" deadbeef
unset GH_STUB_RUNS CI_WAIT_SLEEP CI_WAIT_ATTEMPTS CI_WAIT_EMPTY_ATTEMPTS

# ---- verify-published: anonymous pulls, then signature and attestation of
# every image and chart, the attestation retried while it lags the push.
export GH_STUB_COUNT="${work}/gh-count" VERIFY_SLEEP=0 VERIFY_ATTEMPTS=3
rm -f "${GH_STUB_COUNT}"
expect 0 'ok: ghcr.io/fyannk/charts/pgtools:0.1.0' -- ./hack/release.sh verify-published v0.1.0
rm -f "${GH_STUB_COUNT}"
GH_STUB_FAILS=2 expect 0 'ok: ghcr.io/fyannk/pgtoolbox-catalog:0.1.0' -- ./hack/release.sh verify-published v0.1.0
rm -f "${GH_STUB_COUNT}"
GH_STUB_FAILS=99 expect 1 'does not verify' -- ./hack/release.sh verify-published v0.1.0
COSIGN_STUB_FAIL=1 expect 1 'does not verify' -- ./hack/release.sh verify-published v0.1.0
DOCKER_STUB_FAIL=1 expect 1 'cannot be pulled anonymously' -- ./hack/release.sh verify-published v0.1.0
expect 1 'not a release tag' -- ./hack/release.sh verify-published 0.1.0
unset GH_STUB_COUNT VERIFY_SLEEP VERIFY_ATTEMPTS

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

# ---- pin-refs, install-manifests and the bundle/catalog pinning, for the
# final version and for a release candidate cut from the same tree (which
# carries the core version): fake digests, the real committed files.
digests="${work}/digests.txt"
: > "${digests}"
for n in operator console evidence-agent bundle catalog; do
  printf 'ghcr.io/fyannk/pgtoolbox-%s@sha256:%064d\n' "${n}" "${#n}" >> "${digests}"
done
tree="$(sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' charts/pgtoolbox/Chart.yaml)"
for v in "${tree}" "${tree}-rc.1"; do
  d="${work}/pin-${v}"
  rm -rf "${d}"
  mkdir -p "${d}"
  cp -r deploy/olm/bundle "${d}/bundle"
  cp -r deploy/olm/catalog "${d}/catalog"
  while IFS= read -r f; do ./hack/release.sh pin-refs "${f}" "${digests}" "${v}"; done \
    < <(grep -rlE 'ghcr\.io/[^/]+/pgtoolbox-' "${d}/bundle")
  ! grep -rEq 'ghcr\.io/[^/ ]+/pgtoolbox-[a-z-]+:[0-9]' "${d}/bundle" || fail "bundle for ${v} still has a tag reference"
  grep -rq 'pgtoolbox-operator@sha256:' "${d}/bundle" || fail "bundle for ${v} pins no operator digest"
  # The catalog names the bundle, pinned with the digests so far.
  while IFS= read -r f; do ./hack/release.sh pin-refs "${f}" "${digests}" "${v}"; done \
    < <(grep -rlE 'ghcr\.io/[^/]+/pgtoolbox-' "${d}/catalog")
  grep -rq 'pgtoolbox-bundle@sha256:' "${d}/catalog" || fail "catalog for ${v} pins no bundle digest"
  if command -v kubectl >/dev/null; then
    OUT_DIR="${d}/out" ./hack/release.sh install-manifests "${digests}" "${v}" >/dev/null \
      || fail "install-manifests failed for ${v}"
    grep -q 'pgtoolbox-console@sha256:' "${d}/out/install-console.yaml" || fail "install-console.yaml for ${v} is not pinned"
  else
    echo "note: kubectl is not on PATH; install-manifests not exercised for ${v}"
  fi
done
# Negative: a tree on another version pins nothing, and that is an error.
printf 'image: ghcr.io/fyannk/pgtoolbox-operator:9.9.9\n' > "${work}/other.yaml"
expect 1 'still references' -- ./hack/release.sh pin-refs "${work}/other.yaml" "${digests}" "${tree}-rc.1"
printf 'image: docker.io/library/busybox:1\n' > "${work}/none.yaml"
expect 1 'nothing was pinned' -- ./hack/release.sh pin-refs "${work}/none.yaml" "${digests}" "${tree}-rc.1"
# Negative: a reference left on another tag beside a pinned one is an error.
printf 'a: ghcr.io/fyannk/pgtoolbox-operator:%s\nb: ghcr.io/fyannk/pgtoolbox-operator:latest\n' "${tree}" > "${work}/left.yaml"
expect 1 'still references' -- ./hack/release.sh pin-refs "${work}/left.yaml" "${digests}" "${tree}-rc.1"
# The dry run rehearses an rc end to end with placeholder digests.
if command -v helm >/dev/null && command -v jq >/dev/null; then
  # The bundle and the catalog are pinned (on copies) in fake-digest mode too.
  dry="$(OUT_DIR="${work}/dry" DRY_RUN_FAKE_DIGESTS=1 DRY_RUN_TAG="v${tree}-rc.1" ./hack/release.sh dry-run 2>&1)" \
    || fail "dry-run of v${tree}-rc.1 failed: ${dry}"
  grep -q 'DRY-RUN pinned bundle' <<<"${dry}" && grep -q 'DRY-RUN pinned catalog' <<<"${dry}" \
    || fail "the fake-digest dry run did not pin the bundle and the catalog: ${dry}"
  [[ -f "${work}/dry/pgtoolbox-${tree}-rc.1.tgz" ]] || fail "dry-run did not package the rc chart"
else
  echo "note: helm or jq missing; the rc dry run was not exercised"
fi
# SHA256SUMS covers chart-digests.txt.
mkdir -p "${work}/sums"
printf x > "${work}/sums/digests.txt"
printf y > "${work}/sums/chart-digests.txt"
OUT_DIR="${work}/sums" ./hack/release.sh sums >/dev/null
grep -q ' chart-digests.txt$' "${work}/sums/SHA256SUMS" || fail "SHA256SUMS lacks chart-digests.txt"

echo "ok: hack/release.sh"
cd "${root}"

# ---- prune-caches: a recorded (anonymised) listing of the live repository is
# the golden input; the selection must keep the newest member of every family.
live="${root}/hack/testdata/caches-live.json"
echo 27 > "${work}/open.txt"
prune_ids() { ./hack/prune-caches.sh select "$1" "${work}/open.txt" | sort -n | tr '\n' ' '; }
sel() { # sel CACHES.json ids...: the summary of keys/ids for the assertions
  jq -r --argjson ids "$(printf '%s\n' "${@:2}" | jq -Rn '[inputs | select(length > 0) | tonumber]')" '.[] | select(.id as $i | $ids | index($i)) | .key' "$1"
}
got="$(prune_ids "${live}")"
# yesterday's gobuild copies (10 of them) go, nothing else does
[[ "$(wc -w <<<"${got}")" == 10 ]] || fail "prune-caches (live listing) selected '${got}', want exactly the 10 gobuild copies of the previous day"
while read -r key; do
  [[ "${key}" == gobuild-*-2026-11-10 ]] || fail "prune-caches selected '${key}' from the live listing; only previous-day gobuild copies may go"
done < <(sel "${live}" ${got})
# every family keeps its newest member: all 12 mise entries (one family per job hash), node, gomod, codeql, today's gobuild
for k in mise-v1 node-Linux gomod-Linux codeql-trap gobuild-Linux-test-race gobuild-Linux-e2e; do
  jq -e --arg k "${k}" --argjson ids "$(printf '%s\n' ${got} | jq -Rn '[inputs|tonumber]')" \
    '[.[] | select(.key | startswith($k)) | select(.id as $i | $ids | index($i) | not)] | length > 0' "${live}" > /dev/null || fail "prune-caches deleted every ${k}* cache"
done
# closed-PR caches, a replaced hash, a 404-prone id list and the budget
jq '. + [
  {"id":901,"key":"gobuild-Linux-test-0909090909090909090909090909090909090909090909090909090909090909-2026-11-11","ref":"refs/pull/9/merge","size_in_bytes":10,"created_at":"2026-11-11T00:00:00Z","last_accessed_at":"2026-11-11T00:00:00Z"},
  {"id":902,"key":"gobuild-Linux-test-0909090909090909090909090909090909090909090909090909090909090909-2026-11-11","ref":"refs/pull/27/merge","size_in_bytes":10,"created_at":"2026-11-11T00:00:00Z","last_accessed_at":"2026-11-11T00:00:00Z"},
  {"id":903,"key":"gomod-Linux-0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a","ref":"refs/heads/main","size_in_bytes":10,"created_at":"2026-10-01T00:00:00Z","last_accessed_at":"2026-11-11T01:00:00Z"}]' "${live}" > "${work}/live2.json"
got="$(prune_ids "${work}/live2.json")"
[[ " ${got}" == *" 901 "* ]] || fail "prune-caches kept the cache of closed PR 9: ${got}"
[[ " ${got}" != *" 902 "* ]] || fail "prune-caches deleted the cache of open PR 27: ${got}"
[[ " ${got}" == *" 903 "* ]] || fail "prune-caches kept a superseded gomod cache: ${got}"
# budget: only newest members remain (6.8 GB); a 5 GB budget evicts rebuildable gobuild caches, largest first, before any toolchain cache
got="$(BUDGET_MB=5000 prune_ids "${live}")"
left="$(jq --argjson ids "$(printf '%s\n' ${got} | jq -Rn '[inputs|tonumber]')" '[.[] | select(.id as $i | $ids | index($i) | not) | .size_in_bytes] | add / 1048576' "${live}")"
(( ${left%.*} <= 5000 )) || fail "prune-caches left ${left} MiB with a 5000 MiB budget"
! sel "${live}" ${got} | grep -Eq '^(mise|node|gomod|codeql)' || fail "prune-caches evicted a toolchain cache before the rebuildable ones: $(sel "${live}" ${got} | grep -E '^(mise|node|gomod)')"
# a 404 on one DELETE (already evicted) does not stop the loop; another error does not either, but fails the run
mkdir -p "${work}/prune-stub"
cat > "${work}/prune-stub/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "api --paginate"*) jq -c '.[]' "${PRUNE_LIVE}" | jq -c '{actions_caches: [.]}' | jq -c --slurp '.[]' | jq '.actions_caches[]' ;;
  "pr list"*) echo 27 ;;
  "api -X DELETE"*) id="${4##*/}"; echo "${id}" >> "${PRUNE_LOG}"
    [[ "${id}" == "${PRUNE_404}" ]] && { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    [[ "${id}" == "${PRUNE_500}" ]] && { echo "gh: boom (HTTP 500)" >&2; exit 1; }
    exit 0 ;;
esac
STUB
chmod +x "${work}/prune-stub/gh"
first="$(prune_ids "${live}" | cut -d' ' -f1)"
: > "${work}/prune.log"
expect 0 'already gone \(404\)' -- env PATH="${work}/prune-stub:${PATH}" PRUNE_LIVE="${live}" PRUNE_LOG="${work}/prune.log" PRUNE_404="${first}" PRUNE_500=0 GITHUB_REPOSITORY=o/r ./hack/prune-caches.sh
[[ "$(wc -l < "${work}/prune.log")" == 10 ]] || fail "prune-caches stopped after a 404: $(wc -l < "${work}/prune.log") of 10 deletions attempted"
: > "${work}/prune.log"
expect 1 '1 deletion\(s\) failed' -- env PATH="${work}/prune-stub:${PATH}" PRUNE_LIVE="${live}" PRUNE_LOG="${work}/prune.log" PRUNE_404=0 PRUNE_500="${first}" GITHUB_REPOSITORY=o/r ./hack/prune-caches.sh
[[ "$(wc -l < "${work}/prune.log")" == 10 ]] || fail "prune-caches stopped after a 500"
echo "ok: prune-caches"

# ---- retry: the bounded loop the workflow's verify step shares with
# verify-published; a command that never succeeds fails after VERIFY_ATTEMPTS.
export VERIFY_SLEEP=0 VERIFY_ATTEMPTS=2
expect 0 '' -- ./hack/release.sh retry true
expect 1 'attempt 2/2 failed' -- ./hack/release.sh retry false

# ---- release.yml: the nested module's release is never "latest"; the product
# release is latest only for a stable tag. (A release that takes the badge from
# the product is what `gh release create` does by default for the newest one.)
wf="${root}/.github/workflows/release.yml"
# wait-ci (full_tier_state) calls the actions runs API in the prepare job.
awk '/^  prepare:/ {on=1} on && /^  images:/ {exit} on && /actions: read/ {f=1} END {exit !f}' "${wf}" || fail "release.yml: prepare lacks actions: read (hack/release.sh full_tier_state lists workflow runs)"
grep -q 'docs_ref=refs/tags/' "${wf}" || fail "release.yml: the Pages dispatch does not build the release tag"
! grep -q 'release.sh commits "${TAG}"' <(sed -n '/Write the release notes/,/Create the draft/p' "${wf}") || fail "release.yml: product notes paste the whole commit log"
grep -q -- 'flags+=(--latest=false)' "${wf}" || fail "release.yml: module release does not pass --latest=false"
grep -q -- 'flags+=(--prerelease --latest=false)' "${wf}" || fail "release.yml: product rc does not pass --latest=false"
grep -q -- 'flags+=(--latest)' "${wf}" || fail "release.yml: stable product release does not pass --latest"
grep -q -- 'gh release edit "${TAG}" --draft=false --latest$' "${wf}" || fail "release.yml: publishing a stable release does not pass --latest"
echo "ok: release.yml latest flags"

# ---- pages.yml: which documentation the site shows. The step's script is run
# as written with a gh stub.
awk '/- name: Choose the documentation to build/ {on=1; next} on && /^        run: \|/ {r=1; next} on && r && /^      - name:/ {exit} on && r {sub(/^          /, ""); print}' "${root}/.github/workflows/pages.yml" > "${work}/pages-choose.sh"
[[ -s "${work}/pages-choose.sh" ]] || fail "pages.yml: cannot extract the 'Choose the documentation' script"
mkdir -p "${work}/pgh"
cat > "${work}/pgh/gh" <<'STUB'
#!/usr/bin/env bash
[[ -n "${PAGES_LATEST:-}" ]] || exit 1
echo "${PAGES_LATEST}"
STUB
chmod +x "${work}/pgh/gh"
pages() { # pages EVENT INPUT LATEST -> "ref=..." or exit 1
  : > "${work}/pages-out"
  env PATH="${work}/pgh:${PATH}" REPO=o/r EVENT_NAME="$1" DOCS_REF_INPUT="$2" PAGES_LATEST="$3" DEFAULT_BRANCH=main GITHUB_OUTPUT="${work}/pages-out" bash "${work}/pages-choose.sh" > /dev/null 2>&1 || return 1
  cat "${work}/pages-out"
}
[[ "$(pages schedule "" v0.1.0)" == "ref=refs/tags/v0.1.0" ]] || fail "pages: a scheduled build does not serve the latest stable tag"
[[ "$(pages workflow_run "" v0.1.0)" == "ref=refs/tags/v0.1.0" ]] || fail "pages: a workflow_run build does not serve the latest stable tag"
[[ "$(pages schedule "" "")" == "ref=main" ]] || fail "pages: no release must fall back to main"
[[ "$(pages schedule "" v0.2.0-rc.1)" == "ref=main" ]] || fail "pages: a pre-release counts as the latest stable tag"
[[ "$(pages workflow_dispatch "" v0.1.0)" == "ref=main" ]] || fail "pages: a dispatch with no docs_ref must build the default branch"
[[ "$(pages workflow_dispatch refs/tags/v0.1.0-rc.1 v0.1.0)" == "ref=refs/tags/v0.1.0-rc.1" ]] || fail "pages: dispatch with a tag"
[[ "$(pages workflow_dispatch main v0.1.0)" == "ref=main" ]] || fail "pages: dispatch with the default branch"
for bad in refs/pull/5/merge refs/heads/evil refs/tags/not-semver 'refs/tags/v1.0.0;rm' deadbeefdeadbeef; do
  ! pages workflow_dispatch "${bad}" v0.1.0 > /dev/null || fail "pages: docs_ref '${bad}' was accepted"
done
! grep -n 'run:.*\${{' "${root}/.github/workflows/pages.yml" || fail "pages.yml: an expression inside run:"
echo "ok: pages docs ref"

# ---- Chart.yaml: helm push stamps org.opencontainers.image.source from the
# first entry of `sources`, so it must be this repository.
for c in pgtoolbox pgtools; do
  first="$(awk '/^sources:/ { on = 1; next } on && /^ *- / { print $2; exit }' "${root}/charts/${c}/Chart.yaml")"
  [[ "${first}" == "https://github.com/fyannk/pgToolBox" ]] || fail "charts/${c}/Chart.yaml: first source is '${first}', want the repository (OCI image.source annotation)"
done
# ---- Dockerfile: network installs are retried like go mod download.
grep -q 'pnpm install --frozen-lockfile && exit 0' "${root}/Dockerfile" || fail "Dockerfile: pnpm install is not retried"
grep -q 'npm ci --ignore-scripts --no-audit --no-fund && exit 0' "${root}/Dockerfile" || fail "Dockerfile: npm ci is not retried"
for d in docs/site headlamp; do
  grep -q '^ignore-scripts=true' "${root}/${d}/.npmrc" || fail "${d}/.npmrc must set ignore-scripts=true (install scripts of npm dependencies)"
done
grep -q 'docs/site/.npmrc' "${root}/Dockerfile" || fail "Dockerfile: docs-site must copy .npmrc before npm ci"
grep -q "vars.PRUNE_GHCR == 'true'" "${root}/.github/workflows/release.yml" || fail "release.yml: prune must stay opt-in (PRUNE_GHCR)"
if grep -rn "release.md.*When a release fails" "${root}/.github/workflows"; then fail "workflows point at release.md for 'When a release fails'; it lives in development/ci.md"; fi
echo "ok: chart sources, Dockerfile retries"

# ---- full tier: a green ci-ok from a push to main (path-filtered, so a docs-only
# commit skipped the images, e2e and OLM jobs) is not enough; the tagged commit
# needs a workflow_dispatch or schedule run of both workflows.
export GH_STUB_RUNS="${runs}" GH_STUB_WF="${wfruns}" CI_WAIT_SLEEP=0 CI_WAIT_ATTEMPTS=3 CI_WAIT_EMPTY_ATTEMPTS=2
changelog "## [${version}] - 2026-11-09"
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; tab security-gate completed success 2026-11-10T10:00:00Z 21; } > "${runs}"
{ tab ci.yml push completed success 5; tab security.yml push completed success 6; } > "${wfruns}"
expect 1 'no full-tier run of ci.yml' -- ./hack/release.sh wait-ci deadbeef
expect 1 'gh workflow run ci.yml --ref main' -- ./hack/release.sh wait-ci deadbeef
expect 1 'no full-tier run of ci.yml' -- pre "v${version}" "${head}"
{ tab ci.yml workflow_dispatch completed success 5; tab security.yml push completed success 6; } > "${wfruns}"
expect 1 'no full-tier run of security.yml' -- ./hack/release.sh wait-ci deadbeef
# A dispatch that failed after an earlier green one: the newest full-tier run counts.
{ tab ci.yml workflow_dispatch completed success 5; tab ci.yml workflow_dispatch completed failure 9; tab security.yml schedule completed success 6; } > "${wfruns}"
expect 1 'full-tier run of ci.yml on deadbeef ended failure' -- ./hack/release.sh wait-ci deadbeef
# A dispatch still running keeps wait-ci waiting.
{ tab ci.yml workflow_dispatch in_progress "" 9; tab ci.yml workflow_dispatch completed success 5; tab security.yml schedule completed success 6; } > "${wfruns}"
expect 1 'did not finish' -- ./hack/release.sh wait-ci deadbeef
{ tab ci.yml workflow_dispatch completed success 5; tab security.yml schedule completed success 6; } > "${wfruns}"
expect 0 'full-tier .* green on deadbeef' -- ./hack/release.sh wait-ci deadbeef
expect 0 'ok: preflight' -- pre "v${version}" "${head}"

# ---- check-public polls: a package made public while the run waits goes green.
cat > "${stubs}/curl" <<'STUB'
#!/usr/bin/env bash
# Manifests answer 404 until the counter file reaches CURL_STUB_FLIP polls.
for a in "$@"; do
  case "${a}" in
    *"/token?"*) echo '{"token":"t"}'; exit 0 ;;
    *"/manifests/"*)
      n="$(cat "${CURL_STUB_COUNT}" 2>/dev/null || echo 0)"; echo $((n + 1)) > "${CURL_STUB_COUNT}"
      if ((n >= ${CURL_STUB_FLIP:-0})); then echo 200; else echo 404; fi
      exit 0 ;;
  esac
done
exit 22
STUB
printf 'ghcr.io/o/pgtoolbox-operator@sha256:%064d\n' 1 > "${work}/one.txt"
export CURL_STUB_COUNT="${work}/curl-count" CHECK_PUBLIC_SLEEP=0 CHECK_PUBLIC_ATTEMPTS=3
echo 0 > "${CURL_STUB_COUNT}"
CURL_STUB_FLIP=1 expect 0 'every digest is publicly pullable' -- ./hack/release.sh check-public "${work}/one.txt"
echo 0 > "${CURL_STUB_COUNT}"
CURL_STUB_FLIP=99 expect 1 'Re-run failed jobs' -- ./hack/release.sh check-public "${work}/one.txt"
echo 0 > "${CURL_STUB_COUNT}"
CURL_STUB_FLIP=99 expect 1 'packages/container/pgtoolbox-operator/settings' -- ./hack/release.sh check-public "${work}/one.txt"
# A registry outage (5xx, timeout) is not "private": different text, no settings link.
cat > "${stubs}/curl" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do
  case "${a}" in
    *"/token?"*) echo '{"token":"t"}'; exit 0 ;;
    *"/manifests/"*) echo "${CURL_STUB_STATUS}"; exit 0 ;;
  esac
done
exit 22
STUB
out="$(CURL_STUB_STATUS=503 ./hack/release.sh check-public "${work}/one.txt" 2>&1)" && fail "check-public passed on a 503"
grep -q 'registry did not answer' <<<"${out}" && grep -q 'REGISTRY ERROR (HTTP 503)' <<<"${out}" || fail "check-public 503 text: ${out}"
! grep -Eq 'NOT PUBLIC|Make each package public|Make these GHCR' <<<"${out}" || fail "check-public calls a 503 private: ${out}"
out="$(CURL_STUB_STATUS=000 ./hack/release.sh check-public "${work}/one.txt" 2>&1)" && fail "check-public passed on a timeout"
grep -q 'registry did not answer' <<<"${out}" || fail "check-public timeout text: ${out}"
for st in 401 403; do
  out="$(CURL_STUB_STATUS=${st} ./hack/release.sh check-public "${work}/one.txt" 2>&1)" && fail "check-public passed on ${st}"
  grep -q "NOT PUBLIC (HTTP ${st})" <<<"${out}" && grep -q 'Make each package public' <<<"${out}" && ! grep -q 'registry did not answer' <<<"${out}" || fail "check-public ${st} text: ${out}"
done

# ---- image labels: metadata-action's defaults (the repository's name and
# description) replace the Dockerfile labels, so every metadata step of the
# release must set its own title and description, distinct from the others.
titles="$(awk '
  /uses: docker\/metadata-action@/ { inmeta = 1; next }
  inmeta && /org\.opencontainers\.image\.title=/ { sub(/.*image\.title=/, ""); print }
  inmeta && /^      - name:/ { inmeta = 0 }
' .github/workflows/release.yml)"
[[ "$(wc -l <<<"${titles}")" -ge 3 ]] || fail "release.yml: every metadata-action step must set org.opencontainers.image.title: ${titles}"
[[ -z "$(sort <<<"${titles}" | uniq -d)" ]] || fail "release.yml: metadata-action titles are not distinct: ${titles}"
[[ "$(grep -c 'org.opencontainers.image.description=' .github/workflows/release.yml)" -ge 3 ]] || fail "release.yml: metadata-action steps must set a description"
for t in "pgToolBox operator" "pgToolBox console" "pgToolBox evidence agent"; do
  grep -q "title: ${t}$" .github/workflows/release.yml || fail "release.yml: no matrix title '${t}'"
  grep -q "image.title=\"${t}\"" Dockerfile || fail "Dockerfile does not label '${t}'"
done
echo "release tests passed"
