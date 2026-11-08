#!/bin/sh
# Copyright 2026 The pgToolBox Authors.
# Licensed under the Apache License, Version 2.0.
#
# Tests hack/evidence/pgbackrest-generator/verify-signing-key.sh with
# throwaway keys in a temporary GNUPGHOME: the
# pinned key alone passes; the pinned key followed by another, another key
# alone, and a file that is not a key are refused.
set -eu

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
verify="$repo_root/hack/evidence/pgbackrest-generator/verify-signing-key.sh"
command -v gpg >/dev/null || { echo "test-verify-signing-key: gpg is required" >&2; exit 1; }

work=$(mktemp -d)
trap 'gpgconf --homedir "$work/gnupg" --kill all >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 700 "$GNUPGHOME"

for name in pinned intruder; do
  gpg --batch --quiet --passphrase "" --quick-generate-key "$name <$name@example.invalid>" ed25519 sign never 2>/dev/null
  gpg --batch --armor --export "$name@example.invalid" > "$work/$name.asc" 2>/dev/null
done
fingerprint() { gpg --batch --with-colons --show-keys "$1" 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }'; }
pinned=$(fingerprint "$work/pinned.asc")
cat "$work/pinned.asc" "$work/intruder.asc" > "$work/both.asc"
printf 'not a key\n' > "$work/junk.asc"

status=0
expect() {
  want=$1 description=$2
  shift 2
  if "$verify" "$@" >/dev/null 2>&1; then got=pass; else got=fail; fi
  if [ "$got" = "$want" ]; then
    echo "ok: $description"
  else
    echo "FAIL: $description: got $got, want $want" >&2
    status=1
  fi
}
expect pass "the pinned key alone" "$work/pinned.asc" "$pinned"
expect fail "the pinned key followed by another" "$work/both.asc" "$pinned"
expect fail "another key alone" "$work/intruder.asc" "$pinned"
expect fail "a file that is not a key" "$work/junk.asc" "$pinned"
expect fail "no arguments"
exit "$status"
