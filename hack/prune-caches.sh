#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Keep the repository's Actions cache well under GitHub's 10 GB limit (which
# otherwise evicts the least recently used entry at random moments, mid-build).
# Caches are written from main only (ci.yml, matrix.yml), so this prunes:
#
#   1. caches of refs/pull/N/merge whose pull request is not open any more;
#   2. per family (a key without its trailing content hash: gobuild-Linux-e2e,
#      codeql-trap-1-2.27.4-javascript) all but the newest KEEP (2) entries: a
#      go.sum change leaves the old hash's cache behind for good;
#   3. then the least recently used of the rest until the total is at most
#      BUDGET_MB (8192).
#
#   hack/prune-caches.sh [--dry-run]       list, select, delete through `gh`
#   hack/prune-caches.sh select CACHES.json OPEN_PRS.txt   print the ids to delete
#
# Environment: GH_TOKEN (actions: write), GITHUB_REPOSITORY, KEEP, BUDGET_MB.
set -euo pipefail

keep="${KEEP:-2}"
budget_mb="${BUDGET_MB:-8192}"

select_ids() {
  local caches="$1" open_prs="$2"
  jq -r --argjson keep "${keep}" --argjson budget "$((budget_mb * 1048576))" \
    --rawfile open "${open_prs}" '
    ($open | split("\n") | map(select(length > 0)) | map("refs/pull/" + . + "/merge")) as $live
    | map(. + {family: (.key | sub("-[0-9a-f]{16,}$"; ""))}) as $all
    # 1. closed pull requests
    | ($all | map(select(.ref | test("^refs/pull/")) | select(.ref as $r | $live | index($r) | not))) as $closed
    | ($all - $closed) as $rest
    # 2. the stale members of each family
    | ($rest | group_by(.family) | map(sort_by(.last_accessed_at) | reverse | .[$keep:]) | add // []) as $stale
    | ($rest - $stale) as $left
    # 3. the budget, least recently used first
    | ($left | sort_by(.last_accessed_at) | reverse
        | reduce .[] as $c ({sum: 0, over: []};
            if (.sum + $c.size_in_bytes) > $budget then .over += [$c] else .sum += $c.size_in_bytes end)
        | .over) as $over
    | ($closed + $stale + $over) | map(.id) | unique | .[]' "${caches}"
}

if [[ "${1:-}" == "select" ]]; then
  select_ids "${2:?usage: prune-caches.sh select CACHES.json OPEN_PRS.txt}" "${3:?}"
  exit 0
fi

dry=0
[[ "${1:-}" != "--dry-run" ]] || dry=1
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
gh api --paginate "repos/${repo}/actions/caches?per_page=100" --jq '.actions_caches[]' | jq -s '.' > "${work}/caches.json"
gh pr list --repo "${repo}" --state open --json number --jq '.[].number' > "${work}/open.txt"
total="$(jq '[.[].size_in_bytes] | add // 0 | . / 1048576 | floor' "${work}/caches.json")"
select_ids "${work}/caches.json" "${work}/open.txt" > "${work}/ids.txt"
echo "cache: ${total} MiB in $(jq length "${work}/caches.json") entries; $(wc -l < "${work}/ids.txt") to delete"
while read -r id; do
  [[ -n "${id}" ]] || continue
  if ((dry)); then
    echo "would delete cache ${id}"
  else
    gh api -X DELETE "repos/${repo}/actions/caches/${id}" --silent
  fi
done < "${work}/ids.txt"
