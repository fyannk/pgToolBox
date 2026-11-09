#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Sourced, not run: the one place the e2e scripts get anything over the network
# from. A transient DNS, TLS or 5xx error must not fail a 25-minute kind job, so
# every fetch is retried with exponential backoff; a static manifest at a pinned
# version is also checked against a sha256 pinned here, next to its version.
#
#   retry <attempts> <command...>      the command until it succeeds, backing off
#   pull_image <platform> <ref>        docker pull, retried
#   fetch_url <url> <file>             curl to a file, retried
#   fetch_verified <url> <file> <sha>  fetch_url, then compare the sha256
#   apply_manifest <url|file> [kubectl apply flags...]
#                                      fetch (retried, verified when the URL is a
#                                      pinned default), then kubectl apply -f
#
# Knobs: RETRY_BASE_DELAY (seconds, default 2, doubled per attempt),
# RETRY_MAX_DELAY (default 60), FETCH_ATTEMPTS (default 5), FETCH_MAX_TIME
# (seconds per curl, default 120).
#
# Pins. A caller that overrides a manifest (CNPG_MANIFEST from hack/matrix.sh,
# including the local file it renders for CNPG main) fetches a URL that is not a
# pinned default and so is not checksummed: only the defaults below are. Bumping
# a *_DEFAULT_VERSION means bumping the sha256 beside it.

# shellcheck disable=SC2034 # the variables are read by the scripts that source this file
[[ -z "${PTB_NET_SH:-}" ]] || return 0
PTB_NET_SH=1

CNPG_DEFAULT_VERSION=1.30.0
CNPG_DEFAULT_SHA256=f8bede43fe4ee0d478c2355b204a36876b2ae4faac60f2a9452280b293da3b88
CERT_MANAGER_DEFAULT_VERSION=v1.16.2
CERT_MANAGER_DEFAULT_SHA256=1d51cdecd442f1f5f89783e9e0169b95d372724da203cc75dd7a5c4e50a10ce6
CALICO_DEFAULT_VERSION=v3.32.2
CALICO_DEFAULT_SHA256=a8c828a06a87c629a282ebbc424895b77f3a030251993e41ea400a743675bb02

cnpg_manifest_url() { # <version>
  echo "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${1%.*}/releases/cnpg-${1}.yaml"
}
cert_manager_manifest_url() { # <vX.Y.Z>
  echo "https://github.com/cert-manager/cert-manager/releases/download/${1}/cert-manager.yaml"
}
calico_manifest_url() { # <vX.Y.Z>
  echo "https://raw.githubusercontent.com/projectcalico/calico/${1}/manifests/calico.yaml"
}

# pinned_sha256 <url>: the pinned checksum when the URL is a pinned default, else nothing.
pinned_sha256() {
  case "$1" in
    "$(cnpg_manifest_url "${CNPG_DEFAULT_VERSION}")") echo "${CNPG_DEFAULT_SHA256}" ;;
    "$(cert_manager_manifest_url "${CERT_MANAGER_DEFAULT_VERSION}")") echo "${CERT_MANAGER_DEFAULT_SHA256}" ;;
    "$(calico_manifest_url "${CALICO_DEFAULT_VERSION}")") echo "${CALICO_DEFAULT_SHA256}" ;;
    *) ;;
  esac
}

# retry <attempts> <command...>: runs the command until it succeeds or the
# attempts are spent; sleeps base, 2*base, 4*base ... (capped) between tries,
# logs each failure to stderr and returns the last exit code.
retry() {
  local attempts=$1 n delay rc=0
  shift
  delay="${RETRY_BASE_DELAY:-2}"
  for ((n = 1; n <= attempts; n++)); do
    if "$@"; then
      return 0
    else
      rc=$?
    fi
    if ((n == attempts)); then
      echo "[retry] attempt ${n}/${attempts} failed (exit ${rc}), giving up: $*" >&2
      return "${rc}"
    fi
    echo "[retry] attempt ${n}/${attempts} failed (exit ${rc}), retrying in ${delay}s: $*" >&2
    sleep "${delay}"
    delay="$(awk -v d="${delay}" -v m="${RETRY_MAX_DELAY:-60}" 'BEGIN { d *= 2; if (d > m) d = m; print d }')"
  done
}

# pull_image <platform> <ref>: docker pull, retried (registry rate limits and resets).
pull_image() {
  retry "${FETCH_ATTEMPTS:-5}" docker pull --quiet --platform "$1" "$2" >/dev/null
}

# fetch_url <url> <file>: a failed fetch leaves no partial file behind.
fetch_url() {
  local url=$1 file=$2
  retry "${FETCH_ATTEMPTS:-5}" curl --fail --silent --show-error --location \
    --connect-timeout 20 --max-time "${FETCH_MAX_TIME:-120}" -o "${file}" "${url}" ||
    {
      rm -f "${file}"
      return 1
    }
}

# check_sha256 <file> <sha256> <what>
check_sha256() {
  local got
  got="$(sha256sum "$1" | cut -d' ' -f1)"
  if [[ "${got}" != "$2" ]]; then
    echo "$3 is not the pinned content (sha256 ${got}, want $2)" >&2
    return 1
  fi
}

# fetch_verified <url> <file> <sha256>
fetch_verified() {
  fetch_url "$1" "$2" || return 1
  check_sha256 "$2" "$3" "$1" || {
    rm -f "$2"
    return 1
  }
}

# fetch_manifest <url|file> <file>: fetch_verified when the URL is a pinned
# default, fetch_url otherwise; a local file (the CNPG main manifest
# hack/matrix.sh renders) is copied as it is.
fetch_manifest() {
  local sha
  if [[ "$1" != http://* && "$1" != https://* ]]; then
    cp -- "$1" "$2"
    return
  fi
  sha="$(pinned_sha256 "$1")"
  if [[ -n "${sha}" ]]; then
    fetch_verified "$1" "$2" "${sha}"
  else
    fetch_url "$1" "$2"
  fi
}

# apply_manifest <url|file> [kubectl apply flags...]: fetch_manifest to a
# temporary file, then kubectl apply from it.
apply_manifest() {
  local src=$1 dir status=0
  shift
  dir="$(mktemp -d "${TMPDIR:-/tmp}/ptb-manifest.XXXXXX")"
  fetch_manifest "${src}" "${dir}/manifest.yaml" || status=$?
  ((status != 0)) || kubectl apply "$@" -f "${dir}/manifest.yaml" || status=$?
  rm -rf "${dir}"
  return "${status}"
}
