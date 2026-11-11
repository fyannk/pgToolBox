#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The logic of the release, as one script so the workflow stays a list of
# steps and every piece can be run (or dry-run) on a laptop. The workflow
# (.github/workflows/release.yml) builds, pushes, signs and attests with
# actions; this script holds what is not an action: the checks, the notes, the
# digest pinning and the manifests.
#
#   release.sh check [TAG]      semver, chart versions, a dated CHANGELOG section
#                               (an "Unreleased" heading is refused for a final
#                               tag unless ALLOW_UNRELEASED=1, the dry run's
#                               rehearsal; v0.1.0-rc.N may use the open 0.1.0 section) and, when
#                               the tag exists, that it is annotated (REQUIRE_TAG=1
#                               insists it exists; REQUIRE_MAIN=1 insists its commit
#                               is on origin/main)
#   release.sh check-module TAG nested module tag (pkg/evidence/vX.Y.Z): module
#                               path, semver, annotated, zero requirements
#   release.sh wait-ci SHA      wait for the ci-ok and security-gate checks of a
#                               commit to succeed (gh api; needs GH_TOKEN)
#   release.sh preflight TAG [SHA]
#                               everything a tag needs before it is pushed: the
#                               CHANGELOG section is dated (final tag), an existing
#                               tag is annotated, and ci-ok and security-gate are
#                               already green on SHA (default: the tag's commit,
#                               else HEAD); one poll, no waiting
#   release.sh retry CMD...     run CMD, bounded by VERIFY_ATTEMPTS and VERIFY_SLEEP
#   release.sh commits-link TAG a link to the compare view (or the history) in place
#                               of the commit list, for the release notes
#   release.sh notes VERSION    the CHANGELOG section of a version, as notes
#   release.sh commits TAG     the conventional commits since the previous tag of the
#                               same line, grouped by type, as Markdown
#   release.sh pin-chart DIR DIGESTS VERSION
#                               rewrite a copy of a chart (pgtoolbox or pgtools) so
#                               its own images are referenced by digest (tag kept
#                               in a comment)
#   release.sh pin-refs FILE DIGESTS VERSION
#                               rewrite repo:VERSION (or the tree's core version) to repo@digest in a bundle,
#                               catalog or manifest file
#   release.sh verify-pins DIR DIGESTS
#                               render a pinned chart and fail on any own image not
#                               at the digest DIGESTS names
#   release.sh package-charts DIGESTS VERSION
#                               pin, verify and package both charts into OUT_DIR
#   release.sh install-manifests DIGESTS VERSION
#                               render config/default and config/console into
#                               OUT_DIR with their images pinned by digest
#   release.sh check-public FILE...
#                               every digest listed must be pullable anonymously
#                               (else exit 1, naming the packages to make public)
#   release.sh scan-digests FILE
#                               grype every digest on each platform; exit 1 for a
#                               fixable critical, exit 2 when the scan itself failed
#   release.sh verify-published TAG
#                               what a user does, with no registry credentials:
#                               pull every image and chart of the release by tag,
#                               cosign verify and gh attestation verify each by
#                               digest (retried: attestations lag the push)
#   release.sh sums             write SHA256SUMS over the release files in OUT_DIR
#   release.sh dry-run          build everything a release builds without pushing,
#                               signing or attesting; print the digests
#
# The product is released by a vX.Y.Z tag; the nested evidence module
# (pkg/evidence) by its own pkg/evidence/vX.Y.Z tag, which Go requires for a
# module below the repository root. The two version lines are independent: the
# module changes only when the wire contract does.
#
# Configuration is environment, never arguments, so a workflow cannot put an
# input on a command line:
#
#   GITHUB_REPOSITORY  owner/name (default fyannk/pgToolBox)
#   OUT_DIR            where digests, charts, manifests and checksums go
#                      (default dist/release)
#   REGISTRY           default ghcr.io
#   CI_WAIT_ATTEMPTS, CI_WAIT_SLEEP   wait-ci polling (120 x 30 s)
#   CHECK_PUBLIC_ATTEMPTS, CHECK_PUBLIC_SLEEP   check-public polling (20 x 60 s)
#   CI_WAIT_EMPTY_ATTEMPTS  wait-ci: polls with no check run at all before it
#                      fails with the dispatch hint (default 4)
#   DRY_RUN_FAKE_DIGESTS=1   dry-run: skip the image builds and use placeholder
#                            digests, to exercise the pinning and packaging only
#   DRY_RUN_TAG        dry-run: rehearse this tag (v0.1.0-rc.1) against the tree
#                      instead of the chart's version
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

# The two charts: the operator and the team tools. One version for both, the
# product's; charts/pgtoolbox/Chart.yaml is where it is read.
chart_dir=charts/pgtoolbox
chart_dirs=(charts/pgtoolbox charts/pgtools)
evidence_module_dir=pkg/evidence
semver_re='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
out_dir="${OUT_DIR:-${repo_root}/dist/release}"
repo="${GITHUB_REPOSITORY:-fyannk/pgToolBox}"
registry="${REGISTRY:-ghcr.io}"
prefix="${registry}/${repo%%/*}"
prefix="${prefix,,}"

# name | image (OCI names are lowercase) | context | Dockerfile | target | platforms
# The catalog's Dockerfile runs opm on the build platform only (see the file),
# so no stage needs the foreign architecture's emulation.
images=(
  "operator|pgtoolbox-operator|.|Dockerfile|operator|linux/amd64,linux/arm64"
  "console|pgtoolbox-console|.|Dockerfile|console|linux/amd64,linux/arm64"
  "evidence-agent|pgtoolbox-evidence-agent|.|Dockerfile|evidence-agent|linux/amd64,linux/arm64"
  "bundle|pgtoolbox-bundle|deploy/olm/bundle|deploy/olm/bundle.Dockerfile||linux/amd64,linux/arm64"
  "catalog|pgtoolbox-catalog|deploy/olm/catalog|deploy/olm/catalog.Dockerfile||linux/amd64,linux/arm64"
)

die() { echo "release: $*" >&2; exit 1; }

chart_field() { # version | appVersion
  sed -n "s/^$1: *\"\{0,1\}\([^\"]*\)\"\{0,1\} *\$/\1/p" "${chart_dir}/Chart.yaml"
}

# ---------------------------------------------------------------- check ---
cmd_check() {
  local tag="${1:-v$(chart_field version)}"
  [[ "${tag}" =~ ${semver_re} ]] || die "release tag must be semantic and start with v: ${tag}"
  local version="${tag#v}"

  # A release candidate (v0.1.0-rc.1) is cut from the tree of its final
  # version: the charts and the CHANGELOG may carry either the rc version or the
  # core version (0.1.0); helm package stamps the rc version on the archives.
  # A pushed candidate and its final tag name the same commit, so both need the
  # dated heading (see the Unreleased rule below).
  local core="${version%%-*}" chart_version app_version dir
  for dir in "${chart_dirs[@]}"; do
    chart_version="$(chart_dir="${dir}" chart_field version)"
    app_version="$(chart_dir="${dir}" chart_field appVersion)"
    if [[ ( "${chart_version}" != "${version}" && "${chart_version}" != "${core}" ) ||
      ( "${app_version}" != "${version}" && "${app_version}" != "${core}" ) ]]; then
      die "${dir}/Chart.yaml says version=${chart_version} appVersion=${app_version}; the tag says ${version}"
    fi
  done

  local heading
  heading="$(changelog_heading "${version}")"
  if [[ -z "${heading}" && "${version}" != "${core}" ]]; then
    heading="$(changelog_heading "${core}")"
  fi
  [[ -n "${heading}" ]] || die "CHANGELOG.md has no '## [${version}]' section"
  # The heading of the release being cut carries its date. Rehearsals (a dry
  # run, a plain check) let a candidate use the open section of its final
  # version; a candidate that is really cut or preflighted does not.
  # ALLOW_UNRELEASED=1 is the dry run on a pull request, where the open
  # section is the normal state: it is reported, not refused. A real release
  # (REQUIRE_TAG=1) never takes it.
  # A real release (REQUIRE_TAG=1) and the preflight (REQUIRE_DATED=1) refuse an
  # open section for a candidate too: the candidate and the final tag are cut on
  # the SAME commit (a dating commit between them would change the SHA and need
  # a full-tier run of its own), so the section is dated before rc.1.
  local strict=0
  [[ "${REQUIRE_TAG:-0}" == "1" || "${REQUIRE_DATED:-0}" == "1" ]] && strict=1
  if [[ "${heading}" == *[Uu]nreleased* ]] && { [[ "${strict}" == 1 ]] || [[ "${version}" == "${core}" ]] || [[ "${heading}" == "## [${version}]"* ]]; }; then
    if [[ "${ALLOW_UNRELEASED:-0}" == "1" && "${REQUIRE_TAG:-0}" != "1" ]]; then
      echo "note: CHANGELOG.md heading '${heading}' still says Unreleased; it must be dated before tagging ${tag}"
    else
      die "CHANGELOG.md heading '${heading}' still says Unreleased; date it ('## [${core}] - $(date -u +%F)', the core version: a candidate and the final tag share that one section) before tagging ${tag}"
    fi
  fi

  # A dated heading carries a real YYYY-MM-DD ('## [0.1.0] - TBD' is not a date).
  if [[ "${heading}" != *[Uu]nreleased* ]]; then
    local hdate
    hdate="$(sed -nE 's/^## \[[^]]+\] - ([0-9]{4}-[0-9]{2}-[0-9]{2})([[:space:]].*)?$/\1/p' <<<"${heading}")"
    [[ -n "${hdate}" ]] && [[ "$(date -u -d "${hdate}" +%F 2>/dev/null)" == "${hdate}" ]] \
      || die "CHANGELOG.md heading '${heading}' has no valid YYYY-MM-DD date ('## [${core}] - $(date -u +%F)')"
  fi

  if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
    [[ "$(git cat-file -t "refs/tags/${tag}")" == "tag" ]] \
      || die "release tag must be annotated: ${tag} (git tag -a)"
    echo "ok: ${tag} is an annotated tag on $(git rev-list -n 1 "refs/tags/${tag}")"
    check_on_main "${tag}"
  elif [[ "${REQUIRE_TAG:-0}" == "1" ]]; then
    die "tag ${tag} does not exist in this clone"
  else
    echo "ok: ${tag} is not tagged yet; create it with: git tag -a ${tag} -m 'pgToolBox ${version}'"
  fi
  echo "ok: semver, chart ${chart_version}/${app_version}, CHANGELOG section for ${version}"
}

# changelog_heading VERSION -> the '## [VERSION]...' line, empty when absent.
changelog_heading() {
  awk -v v="$1" 'index($0, "## [" v "]") == 1 { print; exit }' CHANGELOG.md
}

# A release is cut from main: the tagged commit must be reachable from it.
check_on_main() {
  [[ "${REQUIRE_MAIN:-0}" == "1" ]] || return 0
  local sha
  sha="$(git rev-list -n 1 "refs/tags/$1")"
  git rev-parse -q --verify refs/remotes/origin/main >/dev/null \
    || die "origin/main is not fetched; fetch the full history to check that $1 is on main"
  git merge-base --is-ancestor "${sha}" refs/remotes/origin/main \
    || die "$1 points at ${sha}, which is not on main"
  echo "ok: ${sha} is on main"
}

# --------------------------------------------------------- check-module ---
cmd_check_module() {
  local tag="${1:?usage: release.sh check-module pkg/evidence/vX.Y.Z}"
  local prefix_dir="${evidence_module_dir}/"
  [[ "${tag}" == "${prefix_dir}"* ]] || die "module tag must start with ${prefix_dir}: ${tag}"
  local version="${tag#"${prefix_dir}"}"
  [[ "${version}" =~ ${semver_re} ]] || die "module tag must end in a semantic version starting with v: ${tag}"

  local expected actual
  expected="$(awk '$1 == "module" { print $2; exit }' go.mod)/${evidence_module_dir}"
  actual="$(awk '$1 == "module" { print $2; exit }' "${evidence_module_dir}/go.mod")"
  [[ "${actual}" == "${expected}" ]] || die "${evidence_module_dir}/go.mod declares ${actual}, expected ${expected}"
  [[ "$(cd "${evidence_module_dir}" && GOWORK=off go list -m all | wc -l)" -eq 1 ]] \
    || die "${evidence_module_dir} must stay a zero-dependency module"

  if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
    [[ "$(git cat-file -t "refs/tags/${tag}")" == "tag" ]] || die "module tag must be annotated: ${tag} (git tag -a)"
    echo "ok: ${tag} is an annotated tag on $(git rev-list -n 1 "refs/tags/${tag}")"
    check_on_main "${tag}"
  elif [[ "${REQUIRE_TAG:-0}" == "1" ]]; then
    die "tag ${tag} does not exist in this clone"
  else
    echo "ok: ${tag} is not tagged yet; create it with: git tag -a ${tag} -m 'pkg/evidence ${version}'"
  fi
  echo "ok: ${actual} ${version}"
}

# -------------------------------------------------------------- wait-ci ---
# The two checks a release needs on the tagged commit: the pipeline's final
# job (ci-ok, which aggregates every other job) and the security gate. Either
# may have run for a push to main, a pull request's merge or a manual dispatch;
# when a check ran more than once (a re-run, a dispatch) the newest run counts:
# the highest check-run id of that name.
#
# Green is not enough: see full_tier_state, which also demands a full-tier run
# (dispatch or schedule) of both workflows on the tagged commit.
#
# A commit that carries no check run at all was merged by a token whose events
# start no workflow (the automerge token): waiting 60 minutes would not change
# that, so after CI_WAIT_EMPTY_ATTEMPTS polls (default 4) it fails with the
# command that fixes it.
cmd_wait_ci() {
  local sha="${1:?usage: release.sh wait-ci SHA}"
  command -v gh >/dev/null || die "gh is not on PATH"
  local attempts="${CI_WAIT_ATTEMPTS:-120}" empty_limit="${CI_WAIT_EMPTY_ATTEMPTS:-4}" empty=0
  local attempt runs name conclusion missing failed
  for attempt in $(seq 1 "${attempts}"); do
    runs="$(gh api --paginate "repos/${repo}/commits/${sha}/check-runs?per_page=100" --jq '.check_runs[] | [.name, .status, (.conclusion // ""), (.started_at // ""), .id] | @tsv')" \
      || die "wait-ci: \`gh api repos/${repo}/commits/${sha}/check-runs\` failed (see its message above; is GH_TOKEN set and does it read checks?)"
    if [[ -z "${runs}" ]]; then
      empty=$((empty + 1))
      if ((empty >= empty_limit)); then
        die "no check run exists for ${sha} (merged with a token that starts no workflow, or the 30-minute net has not run yet): run \`gh workflow run ci.yml --ref main\` and \`gh workflow run security.yml --ref main\`, wait for green, and run \`make release-preflight TAG=<tag>\` BEFORE pushing the tag"
      fi
    else
      empty=0
    fi
    missing="" failed=""
    for name in ci-ok security-gate; do
      # The newest by check-run id, whatever order the API listed them in.
      conclusion="$(LC_ALL=C sort -t$'\t' -k5,5n <<<"${runs}" | awk -F'\t' -v n="${name}" '$1 == n { c = ($2 == "completed" ? $3 : "pending") } END { print c }')"
      case "${conclusion}" in
        success) ;;
        "" | pending) missing+=" ${name}" ;;
        *) failed+=" ${name}=${conclusion}" ;;
      esac
    done
    [[ -z "${failed}" ]] || die "CI failed on ${sha}:${failed}"
    if [[ -z "${missing}" ]]; then
      echo "ci-ok and security-gate are green on ${sha}"
      full_tier_state "${sha}" && return 0
    fi
    echo "waiting for${missing} on ${sha} (${attempt}/${attempts}); dispatch ci.yml and security.yml on the commit if no run exists"
    sleep "${CI_WAIT_SLEEP:-30}"
  done
  die "CI did not finish on ${sha} in time"
}

# full_tier_state SHA: ci.yml and security.yml must each have a FULL-tier run
# on SHA, that is one started by workflow_dispatch or schedule. A push to main
# runs only the job families its diff touches (hack/changed-paths.sh), so the
# ci-ok of a docs-only commit (the CHANGELOG-dating one) is green with the
# images, e2e and OLM jobs skipped, and a release must never rest on it. The
# newest full-tier run of each workflow counts. Returns 0 when both succeeded,
# 1 while one is still running (the caller keeps waiting), and dies when one
# failed or none exists (waiting would not change that).
full_tier_state() {
  local sha="$1" wf runs state pending=""
  runs="$(gh api --paginate "repos/${repo}/actions/runs?head_sha=${sha}&per_page=100" --jq '.workflow_runs[] | [(.path | split("@")[0] | split("/") | last), .event, .status, (.conclusion // ""), .id] | @tsv')" \
    || die "wait-ci: \`gh api repos/${repo}/actions/runs?head_sha=${sha}\` failed (see its message above; the token needs actions: read)"
  for wf in ci.yml security.yml; do
    state="$(LC_ALL=C sort -t$'\t' -k5,5n <<<"${runs}" | awk -F'\t' -v w="${wf}" '$1 == w && ($2 == "workflow_dispatch" || $2 == "schedule") { c = ($3 == "completed" ? $4 : "pending") } END { print c }')"
    case "${state}" in
      success) ;;
      pending) pending+=" ${wf}" ;;
      "") die "${sha} has no full-tier run of ${wf} (a push to main runs only the job families its diff touches, so its ci-ok may have skipped the images, e2e and OLM jobs): run \`gh workflow run ci.yml --ref main\` and \`gh workflow run security.yml --ref main\` on this exact head, wait for green, then tag" ;;
      *) die "the full-tier run of ${wf} on ${sha} ended ${state}" ;;
    esac
  done
  if [[ -n "${pending}" ]]; then
    echo "waiting for the full-tier run of${pending} on ${sha}"
    return 1
  fi
  echo "ok: full-tier (dispatch or schedule) ci.yml and security.yml runs are green on ${sha}"
}

# ------------------------------------------------------------ preflight ---
# The checks a maintainer runs before `git push origin vX.Y.Z`: the same ones
# the release workflow makes (check, wait-ci) but answered at once, so a tag is
# never pushed to a commit the workflow would then refuse. Check runs must exist
# on the tagged SHA: a commit merged by the Actions token starts none (the
# automerge workflow dispatches ci.yml and security.yml for that reason); if
# there are none, dispatch both on the commit.
cmd_preflight() {
  local tag="${1:?usage: release.sh preflight TAG [SHA]}" sha="${2:-}"
  ALLOW_UNRELEASED=0 REQUIRE_DATED=1 cmd_check "${tag}"
  if [[ -z "${sha}" ]]; then
    if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
      sha="$(git rev-list -n 1 "refs/tags/${tag}")"
    else
      sha="$(git rev-parse HEAD)"
    fi
  fi
  # An existing tag decides what is released: an explicit SHA that is not the
  # commit the tag points at would be preflighted while another is shipped.
  if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
    local tagged given
    tagged="$(git rev-list -n 1 "refs/tags/${tag}")"
    given="$(git rev-parse -q --verify "${sha}^{commit}")" || die "${sha} is not a commit in this clone"
    [[ "${given}" == "${tagged}" ]] \
      || die "${tag} already exists and points at ${tagged}, not at ${sha}; preflight the tagged commit or delete the local tag"
  fi
  # Fail before any network call: a SHA that is not on main is refused by
  # `prepare` after the approval, so say it now.
  git rev-parse -q --verify refs/remotes/origin/main >/dev/null \
    || die "origin/main is not fetched; run \`git fetch origin main\` to check that ${sha} is on main"
  git merge-base --is-ancestor "${sha}" refs/remotes/origin/main \
    || die "${tag} would point at ${sha}, which is not an ancestor of origin/main; merge it to main (and fetch) first"
  CI_WAIT_ATTEMPTS=1 CI_WAIT_EMPTY_ATTEMPTS=1 CI_WAIT_SLEEP=0 cmd_wait_ci "${sha}"
  echo "ok: preflight ${tag} on ${sha}"
}

# ---------------------------------------------------------------- notes ---
cmd_notes() {
  local version="${1:?usage: release.sh notes VERSION}"
  local section
  # A release candidate falls back to the section of its final version.
  [[ -n "$(changelog_heading "${version}")" ]] || version="${version%%-*}"
  section="$(awk -v v="${version}" '
    index($0, "## [" v "]") == 1 { on = 1; next }
    on && index($0, "## [") == 1 { exit }
    on { print }
  ' CHANGELOG.md)"
  # Trim leading and trailing blank lines.
  section="$(printf '%s\n' "${section}" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}' | sed '/./,$!d')"
  [[ -n "${section}" ]] || die "CHANGELOG.md has no content for ${version}"
  printf '%s\n' "${section}"
}

# ----------------------------------------------------------------- pins ---
# The chart, the bundle and the catalog of a release name the images that
# release pushed by digest, so a tag moved later changes nothing installed from
# them. The digests exist only once the images are pushed, so the committed tree
# keeps tags (validate-packaging checks that) and the release rewrites a COPY.
# DIGESTS is digests.txt: one `host/owner/name@sha256:...` per line. The tag
# stays next to the digest, in a comment, for humans.

# digest_of NAME DIGESTS -> the full reference `host/owner/NAME@sha256:...`
digest_of() {
  local name=$1 file=$2 line
  line="$(awk -v n="/${name}@" 'index($0, n) { print; exit }' "${file}")"
  [[ -n "${line}" ]] || die "${file} has no digest for ${name}"
  echo "${line}"
}

cmd_pin_chart() {
  local dir="${1:?usage: release.sh pin-chart DIR DIGESTS VERSION}" digests="${2:?}" version="${3:?}"
  local values="${dir}/values.yaml" name op con agent tmp targets
  [[ -f "${values}" && -f "${digests}" ]] || die "pin-chart: ${values} or ${digests} is missing"
  name="$(chart_dir="${dir}" chart_field name)"
  con="$(digest_of pgtoolbox-console "${digests}")"
  # Each target is "values.path=replacement": the key holds "" in the committed
  # chart and gets the digest (or, for the agent's full reference, the
  # reference), the tag kept in a comment.
  case "${name}" in
    pgtoolbox)
      op="$(digest_of pgtoolbox-operator "${digests}")"
      agent="$(digest_of pgtoolbox-evidence-agent "${digests}")"
      targets="image.digest=${op#*@} # ${op%@*}:${version}
operator.console.image.digest=${con#*@} # ${con%@*}:${version}
operator.evidenceAgent.image=\"${agent}\" # ${agent%@*}:${version}" ;;
    pgtools)
      agent="$(digest_of pgtoolbox-evidence-agent "${digests}")"
      op="$(digest_of pgtoolbox-operator "${digests}")"
      targets="console.image.digest=${con#*@} # ${con%@*}:${version}
evidence.image.digest=${agent#*@} # ${agent%@*}:${version}"
      # pgAdmin without the operator copies the pgtoolbox binary into its Pod.
      targets+=$'\n'"pgadmin.tools.image.digest=${op#*@} # ${op%@*}:${version}" ;;
    *) die "pin-chart: ${dir} is neither pgtoolbox nor pgtools (${name})" ;;
  esac
  tmp="${values}.pinned"
  awk -v targets="${targets}" '
    BEGIN {
      n = split(targets, lines, "\n")
      for (i = 1; i <= n; i++) { k = lines[i]; sub(/=.*/, "", k); v = lines[i]; sub(/^[^=]*=/, "", v); want[k] = v }
    }
    # The dotted path of every key line, from its indentation (two spaces a level).
    /^ *[A-Za-z][A-Za-z0-9_]*:/ {
      match($0, /^ */); lvl = RLENGTH / 2
      key = $0; sub(/^ */, "", key); sub(/:.*/, "", key)
      keys[lvl] = key; for (i = lvl + 1; i < 32; i++) delete keys[i]
      path = keys[0]; for (i = 1; i <= lvl; i++) path = path "." keys[i]
      if ((path in want) && $0 ~ /: ""$/) {
        match($0, /^ */); print substr($0, 1, RLENGTH) key ": " want[path]; done[path] = 1; next
      }
    }
    { print }
    END { for (k in want) if (!(k in done)) { print "pin-chart: no empty " k " to pin" > "/dev/stderr"; bad = 1 } exit bad ? 3 : 0 }
  ' "${values}" > "${tmp}" || die "pin-chart: ${values} does not have the expected image keys"
  mv "${tmp}" "${values}"
}

# pin-refs: repo:TAG -> repo@digest for every image DIGESTS names, anywhere in
# FILE (CSV annotation, deployment, args, relatedImages; the catalog's bundle
# image). The committed tree carries the CORE version (0.1.0) while a release
# candidate pushes 0.1.0-rc.1, so TAG is the core of VERSION or VERSION itself,
# whichever the tree carries. Loud by design: a file in which nothing was
# pinned, or where a reference to an own repository is left on a tag, is an
# error, never a silent no-op (the rc once published bundles that named tags
# that were never pushed).
cmd_pin_refs() {
  local file="${1:?usage: release.sh pin-refs FILE DIGESTS VERSION}" digests="${2:?}" version="${3:?}" ref repo esc_repo grep_repo esc_ref tags core pinned=0 left
  [[ -f "${file}" && -f "${digests}" ]] || die "pin-refs: ${file} or ${digests} is missing"
  core="${version%%-*}"
  tags="${version//./\\.}"
  [[ "${core}" == "${version}" ]] || tags="${tags}|${core//./\\.}"
  while read -r ref; do
    [[ -n "${ref}" ]] || continue
    repo="${ref%@*}"
    esc_repo="$(printf '%s' "${repo}" | sed 's/[.[\*^$/]/\\&/g')"
    grep_repo="$(printf '%s' "${repo}" | sed 's/[.[\*^$]/\\&/g')"
    esc_ref="$(printf '%s' "${ref}" | sed 's/[&/\]/\\&/g')"
    # The tag must end at a delimiter: 1.0.0 must not match inside 1.0.0-rc.1.
    if grep -qE "${grep_repo}:(${tags})([^0-9A-Za-z._-]|\$)" "${file}"; then
      sed -i -E "s/${esc_repo}:(${tags})([^0-9A-Za-z._-]|\$)/${esc_ref}\\2/g" "${file}"
      pinned=$((pinned + 1))
    fi
    left="$(grep -nE "${grep_repo}:[0-9A-Za-z]" "${file}" || true)"
    [[ -z "${left}" ]] || die "pin-refs: ${file} still references ${repo} by a tag other than ${tags//\\/} (expected the tree to carry ${core}):"$'\n'"${left}"
  done < "${digests}"
  ((pinned > 0)) || die "pin-refs: ${file} references none of the images in ${digests} at ${tags//\\/}; nothing was pinned"
}

# verify-pins: render a pinned chart and require each own image at exactly the
# digest DIGESTS names, with no reference to the same repository left on a tag.
cmd_verify_pins() {
  local dir="${1:?usage: release.sh verify-pins DIR DIGESTS}" digests="${2:?}" rendered name want chart
  local -a names extra=()
  chart="$(chart_dir="${dir}" chart_field name)"
  case "${chart}" in
    pgtoolbox) names=(pgtoolbox-operator pgtoolbox-console pgtoolbox-evidence-agent) ;;
    # The evidence agents render only when listed: list one.
    pgtools) names=(pgtoolbox-console pgtoolbox-evidence-agent)
      extra=(--set 'evidence.agents[0].cluster=pins' --set 'evidence.agents[0].secrets[0]=pins') ;;
    *) die "verify-pins: ${dir} is neither pgtoolbox nor pgtools (${chart})" ;;
  esac
  rendered="$(helm template pins "${dir}" --namespace pins ${extra[@]+"${extra[@]}"})" \
    || die "verify-pins: helm template ${dir} failed"
  for name in "${names[@]}"; do
    want="$(digest_of "${name}" "${digests}")"
    grep -qF "${want}" <<<"${rendered}" || die "verify-pins: the rendered ${chart} chart does not reference ${want}"
    if grep -E "image: .*${want%@*}:[0-9A-Za-z]|--[a-z-]*image=${want%@*}:" <<<"${rendered}" | grep -q .; then
      die "verify-pins: the rendered ${chart} chart still references ${want%@*} by tag"
    fi
  done
  echo "ok: the ${chart} chart pins ${names[*]} by digest"
}

# -------------------------------------------------------------- commits ---
# The commits since the previous tag of the same line (vX.Y.Z for the product,
# pkg/evidence/vX.Y.Z for the module), grouped by conventional-commit type.
# The CHANGELOG section is the curated story; this is the complete list.
cmd_commits() {
  local tag="${1:?usage: release.sh commits TAG}" match previous range
  case "${tag}" in
    "${evidence_module_dir}"/v*) match="${evidence_module_dir}/v*" ;;
    *) match="v*" ;;
  esac
  # A final tag starts after the previous FINAL tag, so the notes of v0.1.0 list
  # everything since v0.0.x, release candidates included; a release candidate
  # starts after the previous tag of any kind.
  local -a exclude=()
  [[ "${tag}" == *-* ]] || exclude=(--exclude '*-*')
  previous="$(git describe --tags --abbrev=0 --match "${match}" ${exclude[@]+"${exclude[@]}"} "${tag}^" 2>/dev/null || true)"
  # The module's tag pattern must not match the product's, and vice versa.
  if [[ "${match}" == "v*" && "${previous}" == */* ]]; then previous=""; fi
  range="${previous:+${previous}..}${tag}"
  local -a groups=("feat:Features" "fix:Fixes" "perf:Performance" "refactor:Refactoring" "docs:Documentation" "build:Build" "ci:CI" "test:Tests" "chore:Chores")
  local entry type title lines any=0
  for entry in "${groups[@]}"; do
    type="${entry%%:*}" title="${entry#*:}"
    lines="$(git log --no-merges --format='%h %s' "${range}" | sed -n -E "s/^([0-9a-f]+) ${type}(\([^)]*\))?!?: (.*)\$/- \3 (\1)/p")"
    [[ -n "${lines}" ]] || continue
    printf '### %s\n\n%s\n\n' "${title}" "${lines}"
    any=1
  done
  if [[ "${any}" == 0 ]]; then
    echo "No conventional commits in ${range}."
  fi
}

# commits-link TAG: a link to the commit log, not the log itself. The CHANGELOG
# section is the release's story; a first release would otherwise list the whole
# history. With a previous final tag (a candidate: any previous tag of the line)
# the link is the compare view, else the history up to the tag.
cmd_commits_link() {
  local tag="${1:?usage: release.sh commits-link TAG}" previous
  local -a exclude=()
  [[ "${tag}" == *-* ]] || exclude=(--exclude '*-*')
  previous="$(git describe --tags --abbrev=0 --match "v*" ${exclude[@]+"${exclude[@]}"} "${tag}^" 2>/dev/null || true)"
  if [[ -n "${previous}" && "${previous}" != */* ]]; then
    echo "Full commit log: [${previous}...${tag}](https://github.com/${repo}/compare/${previous}...${tag})"
  else
    echo "Full commit log: [history up to ${tag}](https://github.com/${repo}/commits/${tag})"
  fi
}

# --------------------------------------------------------- manifests ---
# package-charts: pin a copy of each chart to the digests just pushed, check
# the rendering, package it. The tarballs land in OUT_DIR for `helm push`.
cmd_package_charts() {
  local digests="${1:?usage: release.sh package-charts DIGESTS VERSION}" version="${2:?}" dir name work
  mkdir -p "${out_dir}"
  work="$(mktemp -d)"
  trap 'rm -rf "${work}"' RETURN
  for dir in "${chart_dirs[@]}"; do
    name="$(basename "${dir}")"
    cp -r "${dir}" "${work}/${name}"
    cmd_pin_chart "${work}/${name}" "${digests}" "${version}"
    cmd_verify_pins "${work}/${name}" "${digests}"
    helm package "${work}/${name}" --version "${version}" --app-version "${version}" -d "${out_dir}" >/dev/null
    [[ -f "${out_dir}/${name}-${version}.tgz" ]] || die "helm package did not produce ${out_dir}/${name}-${version}.tgz"
    echo "packaged ${out_dir}/${name}-${version}.tgz"
  done
}

# install-manifests: the plain-manifest installs, rendered by kustomize, with
# every own image pinned by digest.
cmd_install_manifests() {
  local digests="${1:?usage: release.sh install-manifests DIGESTS VERSION}" version="${2:?}"
  command -v kubectl >/dev/null || die "kubectl is not on PATH"
  mkdir -p "${out_dir}"
  local pair kustomization file
  for pair in "config/default:install-operator.yaml" "config/console:install-console.yaml"; do
    kustomization="${pair%%:*}" file="${out_dir}/${pair#*:}"
    kubectl kustomize "${kustomization}" > "${file}"
    cmd_pin_refs "${file}" "${digests}" "${version}"
    grep -q "@sha256:" "${file}" || die "${file} references no image by digest: the kustomize tag does not match ${version}"
    echo "rendered ${file}"
  done
}

# ----------------------------------------------------------------- sums ---
# One checksum file over everything a release attaches except itself, so a
# download can be checked without a registry. The workflow signs it keyless
# (cosign sign-blob) after this writes it.
cmd_sums() {
  cd "${out_dir}"
  local -a names
  mapfile -t names < <(ls -1 pgtoolbox-*.tgz pgtools-*.tgz install-*.yaml sbom-*.spdx.json digests.txt chart-digests.txt 2>/dev/null | LC_ALL=C sort)
  [[ ${#names[@]} -gt 0 ]] || die "nothing to checksum in ${out_dir}"
  sha256sum "${names[@]}" > SHA256SUMS
  echo "SHA256SUMS lists ${#names[@]} files"
}

# ------------------------------------------------------------ check-public ---
# Before a release becomes visible: every digest the workflow pushed must be
# pullable with no credentials. A new GHCR package is private until its owner
# makes it public, and a release that points at private packages is a release
# nobody can install. FILES are digests.txt and chart-digests.txt style lists
# (`host/owner/name[/sub]@sha256:...`, one per line).
cmd_check_public() {
  [[ $# -gt 0 ]] || die "usage: release.sh check-public FILE..."
  command -v curl >/dev/null || die "curl is not on PATH"
  # A package flipped to public while the run waits turns it green: poll every
  # CHECK_PUBLIC_SLEEP seconds (60) up to CHECK_PUBLIC_ATTEMPTS times (20).
  local attempts="${CHECK_PUBLIC_ATTEMPTS:-20}" attempt pkg
  local -a private=() unavailable=()
  for attempt in $(seq 1 "${attempts}"); do
    check_public_once "$@" && { echo "ok: every digest is publicly pullable"; return 0; }
    if ((${#private[@]} == 0)); then
      # Only registry errors: nothing says a package is private. Wait and retry.
      echo "registry unavailable (HTTP 5xx or timeout) for: ${unavailable[*]}; this says nothing about visibility"
      ((attempt < attempts)) && sleep "${CHECK_PUBLIC_SLEEP:-60}"
      continue
    fi
    # One annotation per wait, naming the settings page of every private package:
    # a maintainer who only sees the run page needs no log reading to act.
    {
      printf '::notice title=Make these GHCR packages public::'
      for pkg in "${private[@]}"; do
        printf 'https://github.com/users/%s/packages/container/%s/settings%%0A' "${repo%%/*}" "${pkg//\//%2F}"
      done
      echo
    }
    if ((attempt < attempts)); then
      echo "waiting for visibility changes (${attempt}/${attempts}); set each package to public here:"
      for pkg in "${private[@]}"; do
        echo "  https://github.com/users/${repo%%/*}/packages/container/${pkg//\//%2F}/settings"
      done
      sleep "${CHECK_PUBLIC_SLEEP:-60}"
    fi
  done
  {
    if ((${#private[@]} > 0)); then
      echo "::error::these packages are private (or missing) for an anonymous pull; the release stays a draft:"
      printf '%s\n' "${private[@]}" | LC_ALL=C sort -u | sed 's/^/  - /'
      echo "Make each package public (github.com/${repo%%/*}?tab=packages, Package settings, Change visibility), then use 'Re-run failed jobs' on this workflow run."
    fi
    if ((${#unavailable[@]} > 0)); then
      echo "::error::the registry did not answer (HTTP 5xx or timeout) for these packages; this is an outage, not a visibility problem:"
      printf '%s\n' "${unavailable[@]}" | LC_ALL=C sort -u | sed 's/^/  - /'
      echo "Use 'Re-run failed jobs' on this workflow run once ${registry} is back."
    fi
  } >&2
  return 1
}

# check_public_once FILE...: one anonymous pull of every digest; fills the
# caller's `private` array with the packages that answered 401/403/404 and its
# `unavailable` array with those the registry failed to answer (5xx, timeout),
# returns 1 when either is not empty.
check_public_once() {
  local ref name digest path token status accept
  private=() unavailable=()
  accept="application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json"
  while read -r ref; do
    [[ -n "${ref}" ]] || continue
    name="${ref%@*}"
    digest="${ref#*@}"
    path="${name#*/}" # drop the registry host
    # The token endpoint is asked with no credentials at all.
    token="$(curl -fsS "https://${registry}/token?service=${registry}&scope=repository:${path}:pull" | jq -r '.token // empty' || true)"
    status=000
    if [[ -n "${token}" ]]; then
      status="$(curl -sS -o /dev/null -w '%{http_code}' -I -H "Authorization: Bearer ${token}" -H "Accept: ${accept}" \
        "https://${registry}/v2/${path}/manifests/${digest}" || true)"
    fi
    case "${status}" in
      200) echo "public: ${ref}" ;;
      401 | 403 | 404)
        echo "NOT PUBLIC (HTTP ${status}): ${ref}" >&2
        private+=("${path#*/}") # the package name: the path below the owner
        ;;
      *)
        # 5xx, a timeout (000) or anything unexpected: the registry, not the package.
        echo "REGISTRY ERROR (HTTP ${status}): ${ref}" >&2
        unavailable+=("${path#*/}")
        ;;
    esac
  done < <(cat "$@")
  ((${#private[@]} == 0 && ${#unavailable[@]} == 0))
}

# -------------------------------------------------------- scan-digests ---
# grype on every digest of FILE, on every platform in SCAN_PLATFORMS (default
# linux/amd64 linux/arm64). Exit 1: a fixable critical vulnerability. Exit 2:
# the scan itself failed (a registry or authentication error, a platform the
# image lacks), which says nothing about the image and must not read as a
# finding. Everything else is listed, not hidden.
cmd_scan_digests() {
  local file="${1:?usage: release.sh scan-digests FILE}"
  command -v grype >/dev/null || die "grype is not on PATH"
  command -v jq >/dev/null || die "jq is not on PATH"
  local platforms="${SCAN_PLATFORMS:-linux/amd64 linux/arm64}"
  local ref platform json errlog critical status=0 summary="${GITHUB_STEP_SUMMARY:-/dev/null}"
  json="$(mktemp)" errlog="$(mktemp)"
  echo "## Published image scan" >> "${summary}"
  echo >> "${summary}"
  while read -r ref; do
    [[ -n "${ref}" ]] || continue
    for platform in ${platforms}; do
      echo "== ${ref} (${platform})"
      if ! grype "registry:${ref}" --platform "${platform}" -o json -q > "${json}" 2> "${errlog}" || ! jq -e '.matches' "${json}" > /dev/null 2>&1; then
        cat "${errlog}" >&2
        echo "::error::scan error, not a finding: grype could not scan ${ref} (${platform}); check registry access and the package's visibility"
        echo "- ${ref} (${platform}): SCAN ERROR" >> "${summary}"
        rm -f "${json}" "${errlog}"
        return 2
      fi
      jq -r '.matches[] | [.vulnerability.severity, (.vulnerability.fix.state // "unknown"), .vulnerability.id, .artifact.name, .artifact.version] | @tsv' "${json}" \
        | LC_ALL=C sort | uniq -c | tail -n 40 || true
      critical="$(jq '[.matches[] | select(.vulnerability.severity == "Critical" and .vulnerability.fix.state == "fixed")] | length' "${json}")"
      if ((critical > 0)); then
        echo "::error::${ref} (${platform}) has ${critical} fixable critical vulnerabilities"
        echo "- ${ref} (${platform}): ${critical} fixable critical" >> "${summary}"
        status=1
      else
        echo "${ref} (${platform}): no fixable critical"
        echo "- ${ref} (${platform}): no fixable critical" >> "${summary}"
      fi
    done
  done < "${file}"
  rm -f "${json}" "${errlog}"
  return "${status}"
}

# ----------------------------------------------------- verify-published ---
# The release as an anonymous user sees it. No docker login and no registry
# token is used or read here: the pulls are what a stranger's pull is. The
# GitHub token (GH_TOKEN) only lets `gh attestation verify` call the API.
# Signatures are keyless (the release workflow's identity), as release.yml
# makes them, so the same identity pattern is checked.
# VERIFY_ATTEMPTS and VERIFY_SLEEP retry each check (the attestation and the
# Sigstore entry can lag the push by a minute).
retry() {
  local attempt
  for attempt in $(seq 1 "${VERIFY_ATTEMPTS:-5}"); do
    "$@" > /dev/null && return 0
    echo "  attempt ${attempt}/${VERIFY_ATTEMPTS:-5} failed: $*" >&2
    ((attempt == ${VERIFY_ATTEMPTS:-5})) || sleep "${VERIFY_SLEEP:-20}"
  done
  return 1
}

cmd_verify_published() {
  local tag="${1:?usage: release.sh verify-published vX.Y.Z}" version ref digest entry name out chart status=0
  [[ "${tag}" =~ ${semver_re} ]] || die "verify-published: not a release tag: ${tag}"
  version="${tag#v}"
  for tool in docker cosign gh helm; do command -v "${tool}" >/dev/null || die "verify-published: ${tool} is not on PATH"; done
  local identity="^https://github.com/${repo}/\\.github/workflows/release\\.yml@refs/tags/"
  local issuer="https://token.actions.githubusercontent.com" work
  work="$(mktemp -d)"
  for entry in "${images[@]}"; do
    IFS='|' read -r name ref _ <<<"${entry}"
    ref="${prefix}/${ref}"
    if ! digest="$(docker buildx imagetools inspect "${ref}:${version}" --format '{{json .Manifest.Digest}}' 2> "${work}/err" | tr -d '"')" || [[ ! "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
      cat "${work}/err" >&2
      echo "::error::${ref}:${version} cannot be pulled anonymously"
      status=1
      continue
    fi
    if retry cosign verify "${ref}@${digest}" --certificate-identity-regexp "${identity}" --certificate-oidc-issuer "${issuer}" \
      && retry gh attestation verify "oci://${ref}@${digest}" --repo "${repo}" --signer-workflow "${repo}/.github/workflows/release.yml"; then
      echo "ok: ${ref}:${version} (${digest})"
    else
      echo "::error::${ref}:${version} (${digest}) does not verify"
      status=1
    fi
  done
  for chart in "${chart_dirs[@]}"; do
    chart="${prefix}/charts/$(basename "${chart}")"
    if ! out="$(helm pull "oci://${chart}" --version "${version}" --destination "${work}" 2>&1)"; then
      echo "${out}" >&2
      echo "::error::${chart}:${version} cannot be pulled anonymously"
      status=1
      continue
    fi
    digest="$(sed -n 's/^Digest: *//p' <<<"${out}")"
    if [[ ! "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
      echo "::error::helm pull printed no digest for ${chart}:${version}"
      status=1
      continue
    fi
    if retry cosign verify "${chart}@${digest}" --certificate-identity-regexp "${identity}" --certificate-oidc-issuer "${issuer}" \
      && retry gh attestation verify "oci://${chart}@${digest}" --repo "${repo}" --signer-workflow "${repo}/.github/workflows/release.yml"; then
      echo "ok: ${chart}:${version} (${digest})"
    else
      echo "::error::${chart}:${version} (${digest}) does not verify"
      status=1
    fi
  done
  rm -rf "${work}"
  return "${status}"
}

# -------------------------------------------------------------- dry-run ---
# Everything a release builds, without a registry: the images are exported to
# OCI archives (multi-arch, so the arm64 build is exercised), the bundle and
# catalog are built from copies pinned to those digests, and the charts and
# manifests are pinned, verified and packaged. Nothing is pushed, signed or
# attested. DRY_RUN_FAKE_DIGESTS=1 skips the image builds.
cmd_dry_run() {
  # The chart's own version, so the tags in the kustomize bases and the bundle
  # are the ones the pinning rewrites, exactly as in a release of this commit.
  local version entry name repo_name context file target platforms ref archive digest
  for tool in jq helm; do command -v "${tool}" >/dev/null || die "${tool} is not on PATH"; done
  version="$(chart_field version)"
  # DRY_RUN_TAG=v0.1.0-rc.1 rehearses a release candidate against this tree.
  if [[ -n "${DRY_RUN_TAG:-}" ]]; then
    [[ "${DRY_RUN_TAG}" =~ ${semver_re} ]] || die "DRY_RUN_TAG must be semantic and start with v: ${DRY_RUN_TAG}"
    version="${DRY_RUN_TAG#v}"
  fi
  mkdir -p "${out_dir}"
  : > "${out_dir}/digests.txt"
  # Global, not local: the EXIT trap runs after this function has returned.
  work="$(mktemp -d)"
  builder=""
  trap 'rm -rf "${work}"; [[ -z "${builder}" ]] || docker buildx rm --force "${builder}" >/dev/null 2>&1 || true' EXIT
  if [[ "${DRY_RUN_FAKE_DIGESTS:-0}" != 1 ]]; then
    command -v docker >/dev/null || die "docker is not on PATH (DRY_RUN_FAKE_DIGESTS=1 skips the builds)"
    # The default docker driver cannot build two platforms; a docker-container
    # builder can, and the Go stages cross-compile, so no QEMU is needed.
    builder="ptb-dryrun-$$"
    docker buildx create --name "${builder}" --driver docker-container >/dev/null
    export BUILDX_BUILDER="${builder}"
  fi
  for entry in "${images[@]}"; do
    IFS='|' read -r name repo_name context file target platforms <<<"${entry}"
    ref="${prefix}/${repo_name}"
    # The bundle names the operator and the agent, the catalog names the
    # bundle: both are built from a copy that carries the digests so far (the
    # same loop as release.yml's publish job). It runs with placeholder digests
    # too, on the copy, so the pinning of a release candidate is rehearsed on
    # every run without an image build.
    if [[ "${name}" == bundle || "${name}" == catalog ]]; then
      rm -rf "${work}/pinned-${name}"
      cp -r "${context}" "${work}/pinned-${name}"
      while IFS= read -r f; do
        cmd_pin_refs "${f}" "${out_dir}/digests.txt" "${version}"
      done < <(grep -rlE "${prefix//./\.}/pg[a-z-]+" "${work}/pinned-${name}" || true)
      if grep -rnE "${prefix//./\.}/pgtoolbox-[a-z-]+:[0-9A-Za-z]" "${work}/pinned-${name}"; then
        die "dry-run: the pinned ${name} still references an own image by tag"
      fi
      echo "DRY-RUN pinned ${name}: no own image left on a tag"
      context="${work}/pinned-${name}"
    fi
    if [[ "${DRY_RUN_FAKE_DIGESTS:-0}" == 1 ]]; then
      digest="sha256:$(printf '%s' "${name}" | sha256sum | cut -d' ' -f1)"
      echo "DRY-RUN (placeholder) ${ref}@${digest}"
    else
      archive="${out_dir}/${name}.oci.tar"
      local -a args=(buildx build --platform "${platforms}" -f "${file}" --build-arg "VERSION=${version}"
        --build-arg "IMG=${prefix}/pgtoolbox-operator:${version}" --output "type=oci,dest=${archive},name=${ref}:${version}")
      [[ -z "${target}" ]] || args+=(--target "${target}")
      docker "${args[@]}" "${context}"
      digest="$(tar -xOf "${archive}" index.json | jq -r '.manifests[0].digest')"
      [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "no digest in ${archive}"
      echo "DRY-RUN ${ref}@${digest} (${platforms})"
    fi
    echo "${ref}@${digest}" >> "${out_dir}/digests.txt"
  done
  cmd_package_charts "${out_dir}/digests.txt" "${version}"
  if command -v kubectl >/dev/null; then
    cmd_install_manifests "${out_dir}/digests.txt" "${version}"
  else
    echo "kubectl is not on PATH: install manifests not rendered"
  fi
  cmd_sums
  echo "--- digests"
  cat "${out_dir}/digests.txt"
  echo "--- checksums"
  cat "${out_dir}/SHA256SUMS"
}

case "${1:-}" in
  pin-chart) shift; cmd_pin_chart "$@" ;;
  pin-refs) shift; cmd_pin_refs "$@" ;;
  verify-pins) shift; cmd_verify_pins "$@" ;;
  package-charts) shift; cmd_package_charts "$@" ;;
  install-manifests) shift; cmd_install_manifests "$@" ;;
  check) shift; cmd_check "$@" ;;
  check-module) shift; cmd_check_module "$@" ;;
  wait-ci) shift; cmd_wait_ci "$@" ;;
  retry) shift; retry "$@" ;;
  preflight) shift; cmd_preflight "$@" ;;
  notes) shift; cmd_notes "$@" ;;
  commits-link) shift; cmd_commits_link "$@" ;;
  commits) shift; cmd_commits "$@" ;;
  sums) shift; cmd_sums "$@" ;;
  check-public) shift; cmd_check_public "$@" ;;
  scan-digests) shift; cmd_scan_digests "$@" ;;
  verify-published) shift; cmd_verify_published "$@" ;;
  dry-run) shift; cmd_dry_run "$@" ;;
  *) sed -n '5,50p' "${BASH_SOURCE[0]}" >&2; exit 2 ;;
esac
