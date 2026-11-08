#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The per-image gate: every image this repository ships
# is held to the same restricted profile, one image at a time, so that a new
# Dockerfile target cannot ship without it.
#
#   hack/image-profile.sh IMAGE [--platform linux/arm64]
#
# Static, from the image config (docker inspect) and its filesystem:
#   - a numeric non-root USER (not 0, not root, not empty);
#   - an exec-form ENTRYPOINT (no shell wrapper);
#   - the OCI labels title, source, version and licenses;
#   - a licence file under /licenses;
#   - no setuid or setgid file anywhere in the filesystem.
# Dynamic, restricted runtime (arbitrary UID, read-only root filesystem, no
# capabilities, no privilege escalation, no network):
#   - the same start under an arbitrary UID in group 0 (what OpenShift's
#     restricted SCC assigns), still read-only and without capabilities;
#   - the binary starts and answers --help (any exit status below 3: 0 for a
#     clean help, 2 for the flag package's usage error; 125-127 and 128+ mean
#     the image cannot run under the profile);
#   - the image carries no shell (docker run --entrypoint /bin/sh fails).
# The console and the evidence agent have behavioural tests on top
# (hack/console/test-container.sh, hack/evidence/test-container.sh); this is
# the floor under every image, the operator included.
set -euo pipefail

image="${1:?usage: $0 IMAGE [docker run args...]}"
shift || true

fail=0
bad() { printf 'FAIL %s: %s\n' "${image}" "$1" >&2; fail=1; }

cfg="$(docker inspect "${image}" --format '{{json .Config}}')"
user="$(jq -r '.User // ""' <<<"${cfg}")"
case "${user%%:*}" in
  "" | 0 | root) bad "runs as '${user:-<unset>}': set a numeric non-root USER" ;;
  *[!0-9]*) bad "USER '${user}' is a name: kubelet cannot verify runAsNonRoot for it, use the numeric id" ;;
esac
[[ "$(jq -r '(.Entrypoint // []) | length' <<<"${cfg}")" -gt 0 ]] || bad "no ENTRYPOINT in exec form"
case "$(jq -r '(.Entrypoint // [""])[0]' <<<"${cfg}")" in */sh | */bash | sh | bash) bad "the ENTRYPOINT is a shell wrapper" ;; esac
for label in title source version licenses; do
  [[ -n "$(jq -r --arg l "org.opencontainers.image.${label}" '.Labels[$l] // ""' <<<"${cfg}")" ]] \
    || bad "OCI label org.opencontainers.image.${label} is missing"
done

restricted=(--rm --user 12345:12345 --read-only --tmpfs /tmp --cap-drop ALL
  --security-opt no-new-privileges --network none "$@")
set +e
# 180 s, not 60: a timeout here says nothing about the image.
out="$(timeout 180 docker run "${restricted[@]}" "${image}" --help 2>&1)"
status=$?
set -e
if ((status > 2)); then
  bad "does not run under the restricted profile (exit ${status}): ${out:0:300}"
fi
# An arbitrary UID in group 0: the numeric USER is a default, not a
# requirement. --user later on the command line wins over the one in
# `restricted`.
set +e
out="$(timeout 180 docker run "${restricted[@]}" --user 1000650000:0 "${image}" --help 2>&1)"
status=$?
set -e
if ((status > 2)); then
  bad "does not run as an arbitrary UID in group 0 (exit ${status}): ${out:0:300}"
fi
if docker run "${restricted[@]}" --entrypoint /bin/sh "${image}" -c true >/dev/null 2>&1; then
  bad "carries a shell (/bin/sh runs): an image of this repository is distroless"
fi
# The licence travels with the binary (a distroless image has no ls: read the
# layers through a created, never started, container).
ctr="$(docker create "${image}")"
trap 'docker rm -f "${ctr}" >/dev/null 2>&1 || true' EXIT
# Listed into a variable first: `grep -q` closing the pipe early would fail the
# pipeline under pipefail (SIGPIPE) on an image with many licence files.
licenses="$(docker cp "${ctr}:/licenses" - 2>/dev/null | tar -t 2>/dev/null || true)"
if ! grep -q 'LICENSE$' <<<"${licenses}"; then
  bad "no LICENSE under /licenses"
fi
# A setuid or setgid bit in a layer is a privilege-escalation path the profile
# above would never exercise. tar prints the mode as a symbolic string.
setid="$(docker export "${ctr}" | tar -tv 2>/dev/null | awk 'substr($1, 1, 1) == "-" && (substr($1, 4, 1) ~ /[sS]/ || substr($1, 7, 1) ~ /[sS]/) { print $NF }' || true)"
if [[ -n "${setid}" ]]; then
  bad "setuid or setgid files: $(tr '\n' ' ' <<<"${setid}")"
fi
((fail == 0)) || exit 1
echo "image profile ok: ${image} (user ${user}, exec entrypoint, OCI labels, runs restricted and as an arbitrary UID in group 0, no shell, no setuid file)"
