#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Fails when the "allowed actions" list in
# docs/site/docs/development/repository-settings.md differs from the
# third-party `uses: owner/repo@<sha>` pins of .github/workflows and
# .github/actions (actions/* and github/* are covered by the "allow actions
# created by GitHub" tick). An owner who sets allowed_actions to `selected`
# from a stale list breaks every workflow that uses the missing action.
#
#   hack/check-action-pins.sh [DOC]     DOC defaults to the page above
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
doc="${1:-docs/site/docs/development/repository-settings.md}"

actual="$(grep -rhoE 'uses: [^./][^ ]+@[0-9a-f]{40}' .github/workflows .github/actions \
  | sed 's/uses: //' | sort -u | grep -vE '^(actions|github)/' || true)"
listed="$(grep -oE '^ *- `[^`]+@[0-9a-f]{40}`' "${doc}" | sed -E 's/^ *- `//; s/`$//' | sort -u || true)"

if [[ "${actual}" != "${listed}" ]]; then
  echo "${doc}: the allowed-actions list differs from the pins in .github/workflows and .github/actions (< doc, > actual):" >&2
  diff <(echo "${listed}") <(echo "${actual}") >&2 || true
  echo "regenerate it: grep -rhoE 'uses: [^./][^ ]+@[0-9a-f]{40}' .github/workflows .github/actions | sed 's/uses: //' | sort -u | grep -vE '^(actions|github)/'" >&2
  exit 1
fi
echo "ok: $(wc -l <<<"${actual}") third-party action pins match ${doc}"
