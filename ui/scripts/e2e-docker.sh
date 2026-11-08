#!/bin/sh
# Runs the Playwright suite in the image the CI `ui` job runs in
# (node:22-bookworm + Playwright's Chromium and its Debian dependencies), so
# screenshots compare against baselines rendered with the same fonts.
#
#   scripts/e2e-docker.sh                 compare
#   scripts/e2e-docker.sh --update        re-render the baselines (review the diff!)
#
# Only ui/ is mounted; it uses the host's node_modules (same platform) and
# never reinstalls. Everything under ui/ is handed back to the caller's uid. The browser cache is a named volume, so reruns are fast.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
args="--workers=2"
[ "${1:-}" = "--update" ] && args="$args --update-snapshots=all"
image=${UI_E2E_IMAGE:-node:22-bookworm}
uid=$(id -u)
gid=$(id -g)
docker run --rm --init \
  -v "$here:/ui" -w /ui \
  -v pgtoolbox-ui-playwright:/ms-playwright \
  -e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright -e CI=1 \
  -e npm_config_verify_deps_before_run=false -e pnpm_config_verify_deps_before_run=false \
  "$image" sh -ec "
    trap 'chown -R $uid:$gid /ui 2>/dev/null || true' EXIT
    corepack enable >/dev/null 2>&1 || npm i -g pnpm@12.16.1 >/dev/null
    pnpm exec playwright install --with-deps chromium >/dev/null
    pnpm exec playwright test $args
  "
