#!/bin/sh
# The Go toolchain is stated twice by necessity (mise.toml for people and CI,
# go.mod/go.work for the go command). They must say the same thing, and the
# language floor must stay above the standard-library advisories recorded in
# go.mod.
set -eu

want=$(awk -F'"' '$2 == "go" { print $4; exit }' mise.toml)
[ -n "$want" ] || { echo "mise.toml pins no go" >&2; exit 1; }
status=0
for f in go.mod go.work pkg/evidence/go.mod; do
  tc=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "$f")
  if [ "$tc" != "$want" ]; then
    echo "$f: toolchain go$tc, mise.toml pins go $want" >&2
    status=1
  fi
done
for f in go.mod go.work pkg/evidence/go.mod; do
  floor=$(awk '$1 == "go" { print $2; exit }' "$f")
  case "$floor" in
    1.26.[6-9]|1.26.[1-9][0-9]|1.2[7-9]*|1.[3-9][0-9]*) ;;
    *) echo "$f: go $floor is below the 1.26.6 floor (stdlib advisories, see go.mod)" >&2; status=1 ;;
  esac
done
# The release workflow installs cosign through an action that takes a release
# number, which no tool manager can read from mise.toml: it must say the same.
cosign_want=$(awk -F'"' '$2 == "aqua:sigstore/cosign" { print $4; exit }' mise.toml)
for f in .github/workflows/*.yml; do
  grep -q 'cosign-release:' "$f" || continue
  have=$(sed -n "s/^ *cosign-release: *['\"]\{0,1\}v\{0,1\}\([0-9.]*\).*/\1/p" "$f" | sort -u)
  for v in $have; do
    if [ "$v" != "$cosign_want" ]; then
      echo "$f: cosign-release v$v, mise.toml pins cosign $cosign_want" >&2
      status=1
    fi
  done
done
[ "$status" = 0 ] && echo "toolchain consistent (go $want, cosign $cosign_want)"
exit "$status"
