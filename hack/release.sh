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
#   release.sh check [TAG]      semver, chart versions, CHANGELOG section and, when
#                               the tag exists, that it is annotated (REQUIRE_TAG=1
#                               insists it exists; REQUIRE_MAIN=1 insists its commit
#                               is on origin/main)
#   release.sh check-module TAG nested module tag (pkg/evidence/vX.Y.Z): module
#                               path, semver, annotated, zero requirements
#   release.sh wait-ci SHA      wait for the ci-ok and security-gate checks of a
#                               commit to succeed (gh api; needs GH_TOKEN)
#   release.sh notes VERSION    the CHANGELOG section of a version, as notes
#   release.sh commits TAG     the conventional commits since the previous tag of the
#                               same line, grouped by type, as Markdown
#   release.sh pin-chart DIR DIGESTS VERSION
#                               rewrite a copy of a chart (pgtoolbox or pgtools) so
#                               its own images are referenced by digest (tag kept
#                               in a comment)
#   release.sh pin-refs FILE DIGESTS VERSION
#                               rewrite repo:VERSION to repo@digest in a bundle,
#                               catalog or manifest file
#   release.sh verify-pins DIR DIGESTS
#                               render a pinned chart and fail on any own image not
#                               at the digest DIGESTS names
#   release.sh package-charts DIGESTS VERSION
#                               pin, verify and package both charts into OUT_DIR
#   release.sh install-manifests DIGESTS VERSION
#                               render config/default and config/console into
#                               OUT_DIR with their images pinned by digest
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
#   DRY_RUN_FAKE_DIGESTS=1   dry-run: skip the image builds and use placeholder
#                            digests, to exercise the pinning and packaging only
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

  local chart_version app_version dir
  for dir in "${chart_dirs[@]}"; do
    chart_version="$(chart_dir="${dir}" chart_field version)"
    app_version="$(chart_dir="${dir}" chart_field appVersion)"
    if [[ "${chart_version}" != "${version}" || "${app_version}" != "${version}" ]]; then
      die "${dir}/Chart.yaml says version=${chart_version} appVersion=${app_version}; the tag says ${version}"
    fi
  done

  if ! awk -v v="${version}" 'index($0, "## [" v "]") == 1 { found = 1 } END { exit !found }' CHANGELOG.md; then
    die "CHANGELOG.md has no '## [${version}]' section"
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
# the newest run of each counts.
cmd_wait_ci() {
  local sha="${1:?usage: release.sh wait-ci SHA}"
  command -v gh >/dev/null || die "gh is not on PATH"
  local attempt runs name conclusion missing failed
  for attempt in $(seq 1 "${CI_WAIT_ATTEMPTS:-120}"); do
    runs="$(gh api --paginate "repos/${repo}/commits/${sha}/check-runs?per_page=100" --jq '.check_runs[] | [.name, .status, (.conclusion // "")] | @tsv')"
    missing="" failed=""
    for name in ci-ok security-gate; do
      conclusion="$(awk -F'\t' -v n="${name}" '$1 == n { c = ($2 == "completed" ? $3 : "pending") } END { print c }' <<<"${runs}")"
      case "${conclusion}" in
        success) ;;
        "" | pending) missing+=" ${name}" ;;
        *) failed+=" ${name}=${conclusion}" ;;
      esac
    done
    [[ -z "${failed}" ]] || die "CI failed on ${sha}:${failed}"
    if [[ -z "${missing}" ]]; then
      echo "ci-ok and security-gate are green on ${sha}"
      return 0
    fi
    echo "waiting for${missing} on ${sha} (${attempt}/${CI_WAIT_ATTEMPTS:-120}); dispatch ci.yml and security.yml on the commit if no run exists"
    sleep "${CI_WAIT_SLEEP:-30}"
  done
  die "CI did not finish on ${sha} in time"
}

# ---------------------------------------------------------------- notes ---
cmd_notes() {
  local version="${1:?usage: release.sh notes VERSION}"
  local section
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

# pin-refs: repo:VERSION -> repo@digest for every image DIGESTS names, anywhere
# in FILE (CSV annotation, deployment, args, relatedImages; the catalog's
# bundle image).
cmd_pin_refs() {
  local file="${1:?usage: release.sh pin-refs FILE DIGESTS VERSION}" digests="${2:?}" version="${3:?}" ref repo esc_repo esc_v esc_ref
  [[ -f "${file}" && -f "${digests}" ]] || die "pin-refs: ${file} or ${digests} is missing"
  esc_v="${version//./\\.}"
  while read -r ref; do
    [[ -n "${ref}" ]] || continue
    repo="${ref%@*}"
    esc_repo="$(printf '%s' "${repo}" | sed 's/[.[\*^$/]/\\&/g')"
    esc_ref="$(printf '%s' "${ref}" | sed 's/[&/\]/\\&/g')"
    # The tag must end at a delimiter: 1.0.0 must not match inside 1.0.0-rc.1.
    sed -i -E "s/${esc_repo}:${esc_v}([^0-9A-Za-z._-]|\$)/${esc_ref}\\1/g" "${file}"
  done < "${digests}"
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
  previous="$(git describe --tags --abbrev=0 --match "${match}" "${tag}^" 2>/dev/null || true)"
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
  mapfile -t names < <(ls -1 pgtoolbox-*.tgz pgtools-*.tgz install-*.yaml sbom-*.spdx.json digests.txt 2>/dev/null | LC_ALL=C sort)
  [[ ${#names[@]} -gt 0 ]] || die "nothing to checksum in ${out_dir}"
  sha256sum "${names[@]}" > SHA256SUMS
  echo "SHA256SUMS lists ${#names[@]} files"
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
    if [[ "${DRY_RUN_FAKE_DIGESTS:-0}" == 1 ]]; then
      digest="sha256:$(printf '%s' "${name}" | sha256sum | cut -d' ' -f1)"
      echo "DRY-RUN (placeholder) ${ref}@${digest}"
    else
      if [[ "${name}" == bundle || "${name}" == catalog ]]; then
        # The bundle names the operator and the agent, the catalog names the
        # bundle: both are built from a copy that carries the digests so far.
        rm -rf "${work}/pinned-${name}"
        cp -r "${context}" "${work}/pinned-${name}"
        while IFS= read -r f; do
          cmd_pin_refs "${f}" "${out_dir}/digests.txt" "${version}"
        done < <(grep -rlE "${prefix//./\.}/pg[a-z-]+" "${work}/pinned-${name}" || true)
        context="${work}/pinned-${name}"
      fi
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
  notes) shift; cmd_notes "$@" ;;
  commits) shift; cmd_commits "$@" ;;
  sums) shift; cmd_sums "$@" ;;
  dry-run) shift; cmd_dry_run "$@" ;;
  *) sed -n '5,50p' "${BASH_SOURCE[0]}" >&2; exit 2 ;;
esac
