#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Fails when a console environment variable is not documented.
#
# Every `Env... = "NAME"` constant of internal/console/config must appear on
# the documentation site, either by its full name (`NAME`) or as a suffix
# next to its family on the same line: `RESTORE_DRILL_CPU_REQUEST`,
# `_MEMORY_LIMIT` documents RESTORE_DRILL_MEMORY_LIMIT. A setting that ships
# without a line in the docs is a setting no reader can find.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
docs=docs/site/docs

documented() {
  local name=$1 rest prefix suffix
  grep -rqE "\`${name}(=[^\`]*)?\`" "${docs}" && return 0
  # Shorthand: a line naming `_<tail>` and another variable of the same family.
  rest=${name}
  while [[ "${rest}" == *_* ]]; do
    rest=${rest#*_}
    suffix="_${rest}"
    prefix=${name%"${suffix}"}
    grep -rhF "\`${suffix}\`" "${docs}" | grep -qF "\`${prefix}_" && return 0
  done
  return 1
}

status=0 count=0
while IFS= read -r name; do
  count=$((count + 1))
  if ! documented "${name}"; then
    echo "${name} (internal/console/config) is not documented under ${docs}" >&2
    status=1
  fi
done < <(grep -hoE '^\s*Env[A-Za-z0-9]+\s*=\s*"[A-Z][A-Z0-9_]+"' internal/console/config/*.go \
  | sed -E 's/.*"([A-Z0-9_]+)"/\1/' | sort -u)

[[ ${status} -ne 0 ]] || echo "${count} console environment variables are documented."
exit "${status}"
