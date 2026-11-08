#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Lint the workflows: actionlint for syntax and shell, zizmor for security
# (unpinned actions, template injection, excessive permissions, credential
# persistence). Both come from mise.toml, so a new rule never turns an
# unrelated pull request red; without them on PATH the script says how to get
# them rather than downloading anything.
#
#   ./hack/lint-workflows.sh
#
# zizmor's online audits (impostor commits, known-vulnerable actions, stale
# pins) ask the GitHub API and need GH_TOKEN; without it zizmor runs offline.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

for tool in actionlint zizmor; do
  command -v "${tool}" >/dev/null || { echo "${tool} is not on PATH: run 'mise install' (versions in mise.toml)" >&2; exit 2; }
done

actionlint
if [[ -n "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ]]; then
  zizmor --format plain --no-progress .github
else
  zizmor --format plain --no-progress --offline .github
fi
