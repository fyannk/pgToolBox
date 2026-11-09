#!/bin/sh
# Runs the Playwright suite in the image ui/e2e.Dockerfile defines (Playwright's
# own image by digest, plus the pinned pnpm), locally and in CI alike, so
# screenshots compare against baselines rendered with the same fonts.
#
#   scripts/e2e-docker.sh                 compare
#   scripts/e2e-docker.sh --update        re-render the baselines (review the diff!)
#
# Only ui/ is mounted; it uses the host's node_modules (same platform) and
# never reinstalls. Everything under ui/ is handed back to the caller's uid.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
args="--workers=2"
[ "${1:-}" = "--update" ] && args="$args --update-snapshots=all"
image=${UI_E2E_IMAGE:-pgtoolbox-ui-e2e:local}
if [ -z "${UI_E2E_IMAGE:-}" ]; then
  docker build --quiet --platform linux/amd64 -f "$here/e2e.Dockerfile" -t "$image" "$here/scripts" >/dev/null
fi
uid=$(id -u)
gid=$(id -g)
docker run --rm --init --platform linux/amd64 \
  -v "$here:/ui" -w /ui \
  "$image" sh -ec "
    trap 'chown -R $uid:$gid /ui 2>/dev/null || true' EXIT
    pnpm exec playwright test $args
  "
