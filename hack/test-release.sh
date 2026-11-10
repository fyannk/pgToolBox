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
  api) [[ -z "${GH_STUB_RUNS:-}" ]] || cat "${GH_STUB_RUNS}" ;;
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
expect 0 'every digest is publicly pullable' -- ./hack/release.sh check-public "${work}/ok.txt"
expect 1 'private-thing' -- ./hack/release.sh check-public "${work}/mixed.txt"
expect 1 'charts/privatechart' -- ./hack/release.sh check-public "${work}/ok.txt" "${work}/mixed.txt"
expect 1 'Re-run failed jobs' -- ./hack/release.sh check-public "${work}/mixed.txt"

# ---- wait-ci: the newest run of a check counts, whatever order it is listed
# in; a commit with no check run at all fails fast with the dispatch hint.
runs="${work}/runs.tsv"
tab() { printf '%s\t%s\t%s\t%s\t%s\n' "$@"; }
export GH_STUB_RUNS="${runs}" CI_WAIT_SLEEP=0 CI_WAIT_ATTEMPTS=3 CI_WAIT_EMPTY_ATTEMPTS=2
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
pre() { (cd "${repo}" && GH_STUB_RUNS="${runs}" ./hack/release.sh preflight "$@"); }
changelog "## [${version}] - 2026-11-09"
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; tab security-gate completed success 2026-11-10T10:00:00Z 21; } > "${runs}"
expect 0 'ok: preflight' -- pre "v${version}" deadbeef
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; } > "${runs}"
expect 1 'did not finish' -- pre "v${version}" deadbeef
: > "${runs}"
expect 1 'no check run exists for deadbeef' -- pre "v${version}" deadbeef
expect 1 'gh workflow run security.yml --ref main' -- pre "v${version}" deadbeef
expect 1 'make release-preflight TAG=<tag>' -- pre "v${version}" deadbeef
{ tab ci-ok completed success 2026-11-10T10:00:00Z 20; tab security-gate completed success 2026-11-10T10:00:00Z 21; } > "${runs}"
changelog "## [${version}] - Unreleased"
expect 1 'still says Unreleased' -- pre "v${version}" deadbeef
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

# ---- retry: the bounded loop the workflow's verify step shares with
# verify-published; a command that never succeeds fails after VERIFY_ATTEMPTS.
export VERIFY_SLEEP=0 VERIFY_ATTEMPTS=2
expect 0 '' -- ./hack/release.sh retry true
expect 1 'attempt 2/2 failed' -- ./hack/release.sh retry false

# ---- release.yml: the nested module's release is never "latest"; the product
# release is latest only for a stable tag. (A release that takes the badge from
# the product is what `gh release create` does by default for the newest one.)
wf="${root}/.github/workflows/release.yml"
grep -q -- 'flags+=(--latest=false)' "${wf}" || fail "release.yml: module release does not pass --latest=false"
grep -q -- 'flags+=(--prerelease --latest=false)' "${wf}" || fail "release.yml: product rc does not pass --latest=false"
grep -q -- 'flags+=(--latest)' "${wf}" || fail "release.yml: stable product release does not pass --latest"
grep -q -- 'gh release edit "${TAG}" --draft=false --latest$' "${wf}" || fail "release.yml: publishing a stable release does not pass --latest"
echo "ok: release.yml latest flags"

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
