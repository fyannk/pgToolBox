#!/bin/sh
# Copyright 2026 The pgToolBox Authors.
# Licensed under the Apache License, Version 2.0.
#
# verify-signing-key.sh KEYFILE FINGERPRINT
#
# Succeeds only when KEYFILE (an armored or binary OpenPGP key file) holds
# exactly one key, whose fingerprint is FINGERPRINT. The generator image
# imports the whole file as the PGDG archive keyring: checking only the
# first fingerprint would let a file carrying a second key, appended by
# whoever can tamper with the download, sign the packages too. One key
# means one fingerprint record in all, subkeys included (the PGDG key has
# none).
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 KEYFILE FINGERPRINT" >&2
  exit 2
fi
keyfile=$1
expected=$2

colons=$(gpg --batch --with-colons --show-keys "$keyfile") || {
  echo "verify-signing-key: $keyfile is not a key file" >&2
  exit 1
}
fingerprints=$(printf '%s\n' "$colons" | awk -F: '$1 == "fpr" { print $10 }')
count=$(printf '%s\n' "$fingerprints" | grep -c . || true)
if [ "$count" -ne 1 ]; then
  echo "verify-signing-key: $keyfile holds $count fingerprints, want exactly one" >&2
  exit 1
fi
if [ "$fingerprints" != "$expected" ]; then
  echo "verify-signing-key: $keyfile is key $fingerprints, want $expected" >&2
  exit 1
fi
