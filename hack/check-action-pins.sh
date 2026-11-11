#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Fails when the "allowed actions" list in
# docs/site/docs/development/repository-settings.md differs from the
# third-party actions pinned in .github/workflows and .github/actions
# (actions/* and github/* are covered by the "allow actions created by GitHub"
# tick). The list names each action as `owner/repo[/path]@*`: GitHub matches an
# entry that names a SHA only against that SHA, so an exact list would refuse
# every Dependabot bump, and the SHA pins are enforced by "Require actions to
# be pinned to a full-length commit SHA" instead. An owner who sets
# allowed_actions to `selected` from a stale list breaks every workflow that
# uses the missing action.
#
#   hack/check-action-pins.sh [DOC]     DOC defaults to the page above
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
doc="${1:-docs/site/docs/development/repository-settings.md}"

actual="$(grep -rhoE 'uses: [^./][^ ]+@[0-9a-f]{40}' .github/workflows .github/actions \
  | sed -E 's/uses: //; s/@[0-9a-f]{40}$/@*/' | sort -u | grep -vE '^(actions|github)/' || true)"
listed="$(grep -oE '^ *- `[^`]+@\*`' "${doc}" | sed -E 's/^ *- `//; s/`$//' | sort -u || true)"

if [[ "${actual}" != "${listed}" ]]; then
  echo "${doc}: the allowed-actions list differs from the pins in .github/workflows and .github/actions (< doc, > actual):" >&2
  diff <(echo "${listed}") <(echo "${actual}") >&2 || true
  echo "regenerate it: grep -rhoE 'uses: [^./][^ ]+@[0-9a-f]{40}' .github/workflows .github/actions | sed -E 's/uses: //; s/@[0-9a-f]{40}\$/@*/' | sort -u | grep -vE '^(actions|github)/'" >&2
  exit 1
fi
echo "ok: $(wc -l <<<"${actual}") third-party actions match ${doc}"
