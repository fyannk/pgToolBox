#!/bin/sh
# Regenerates every committed generated file of the SPA and fails when the
# result differs from what is committed: the orval client and MSW handlers
# (from the OpenAPI document), the TanStack route tree (from src/routes) and
# the MSW fixture module (from the Go goldens). Run from ui/.
set -eu
# Keep byte-identical files untouched (content and mtime): orval cleans and
# rewrites its whole output, and an unchanged file should not look changed
# to make, the editor or a watching dev server.
generated="src/api/generated src/routeTree.gen.ts mocks/fixtures"
snap=$(mktemp -d)
trap 'rm -rf "$snap"' EXIT
# shellcheck disable=SC2086 # word splitting of the path list is intended
tar cf - $generated | (cd "$snap" && tar xpf -)
pnpm exec orval >/dev/null
pnpm exec tsr generate >/dev/null
node scripts/sync-fixtures.mjs
pnpm exec biome format --write src/routeTree.gen.ts >/dev/null 2>&1 || true
(cd "$snap" && find . -type f) | while IFS= read -r f; do
  if cmp -s "$snap/$f" "$f"; then cp -p "$snap/$f" "$f"; fi
done
changed=$(git status --porcelain -- src/api/generated src/routeTree.gen.ts mocks/fixtures)
if [ -n "$changed" ]; then
  echo "generated files differ from what is committed; run 'pnpm generate' and commit:" >&2
  echo "$changed" >&2
  git --no-pager diff --stat -- src/api/generated src/routeTree.gen.ts mocks/fixtures >&2
  exit 1
fi
echo "generated files are current"
