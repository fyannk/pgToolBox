#!/bin/sh
# Vendor the CloudNativePG CRDs of each supported minor (1.30 and later) into
# test/crds/cnpg-<minor>/ for the envtest suites.
# The CRDs are read from a CloudNativePG git checkout at a release tag with
# `git show`, so the checkout is never switched. Usage:
#   CNPG_SRC=../cloudnative-pg hack/sync-crds.sh            # default tags
#   CNPG_SRC=../cloudnative-pg hack/sync-crds.sh 1.30=v1.30.1
set -eu

src=${CNPG_SRC:-../cloudnative-pg}
[ -d "$src/.git" ] || { echo "sync-crds: CNPG_SRC=$src is not a git checkout" >&2; exit 1; }

pairs=${*:-"1.30=v1.30.0"}
for pair in $pairs; do
  minor=${pair%%=*}
  tag=${pair#*=}
  out=test/crds/cnpg-$minor
  rm -rf "$out"
  mkdir -p "$out"
  for file in $(git -C "$src" ls-tree --name-only "$tag" config/crd/bases/); do
    case "$file" in *.yaml) ;; *) continue ;; esac
    git -C "$src" show "$tag:$file" > "$out/$(basename "$file")"
  done
  printf '%s\n' "$tag" > "$out/VERSION"
  echo "sync-crds: $out <- $tag ($(ls "$out"/*.yaml | wc -l) CRDs)"
done
