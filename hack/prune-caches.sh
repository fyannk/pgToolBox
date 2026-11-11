#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Keep the repository's Actions cache well under GitHub's 10 GB limit (which
# otherwise evicts the least recently used entry at random moments, mid-build).
# Caches are written from main only (ci.yml, matrix.yml), so this prunes:
#
#   1. caches of refs/pull/N/merge whose pull request is not open any more;
#   2. per family all but the newest KEEP (1) entries. A family is one logical
#      cache: the key without its trailing date (gobuild-Linux-e2e-<hash>-
#      2026-11-11), content hash and -pN suffix, per ref. Real keys:
#        gobuild-Linux-e2e-<64hex>-2026-11-11          -> gobuild-Linux-e2e
#        gomod-Linux-<64hex>, node-Linux-<64hex>        -> gomod-Linux, node-Linux
#        codeql-trap-1-2.27.4-javascript-<sha>          -> codeql-trap-1-2.27.4-javascript
#        mise-v1-...-2026.11.4-<job hash>-p692-<hash>   -> one family per job hash
#      mise keys end in the hash of mise.toml; every mise entry of a ref whose
#      hash differs from the one on the newest mise entry of that ref is dead
#      (a mise.toml change retires all of them), and a ref keeps at most
#      MISE_KEEP (16) mise entries, newest first, so a changed job set cannot
#      pile them up either;
#        buildkit-blob-1-sha256:...                     -> its own family (a blob is content)
#      The newest (by creation) member of a family is the one jobs restore, so
#      it is never taken by step 2, a go.sum change or a new day leaves the old
#      copies behind and those go;
#   3. only if the newest members alone still exceed BUDGET_MB (8192): the
#      rebuildable compile caches (date-suffixed gobuild-*) first, largest
#      first, then the rest least recently used first. Toolchain and module
#      caches (mise, node, gomod) are the last to go, so a job's start-of-job
#      restores are not what the budget eats.
#
#   hack/prune-caches.sh [--dry-run]       list, select, delete through `gh`
#   hack/prune-caches.sh select CACHES.json OPEN_PRS.txt   print the ids to delete
#
# A failed DELETE (404: GitHub already evicted it) is reported and skipped; any
# other failure is reported, the loop goes on and the exit status is 1.
# The cache usage before and after goes to $GITHUB_STEP_SUMMARY when it is set.
# Environment: GH_TOKEN (actions: write), GITHUB_REPOSITORY, KEEP, MISE_KEEP, BUDGET_MB.
set -euo pipefail

keep="${KEEP:-1}"
budget_mb="${BUDGET_MB:-8192}"
mise_keep="${MISE_KEEP:-16}"

select_ids() {
  local caches="$1" open_prs="$2"
  jq -r --argjson keep "${keep}" --argjson misekeep "${mise_keep}" --argjson budget "$((budget_mb * 1048576))" \
    --rawfile open "${open_prs}" '
    def family: .key | sub("-[0-9]{4}-[0-9]{2}-[0-9]{2}$"; "") | sub("(-p[0-9]+)?-[0-9a-f]{16,}$"; "");
    ($open | split("\n") | map(select(length > 0)) | map("refs/pull/" + . + "/merge")) as $live
    | map(. + {fam: (.ref + " " + family), dated: (.key | test("-[0-9]{4}-[0-9]{2}-[0-9]{2}$"))}) as $all
    # 1. closed pull requests
    | ($all | map(select(.ref | test("^refs/pull/")) | select(.ref as $r | $live | index($r) | not))) as $closed
    | ($all - $closed) as $rest
    # 2. everything but the newest members of each family
    # 2a. mise entries from an older mise.toml than the newest of their ref, and
    #     beyond the per-ref cap
    | ($rest | map(select(.key | startswith("mise-")))
       | group_by(.ref)
       | map(sort_by(.created_at) | reverse
             | (.[0].key | capture("-(?<t>[0-9a-f]{16,})$").t) as $cur
             | (map(select((.key | capture("-(?<t>[0-9a-f]{16,})$").t) != $cur))) as $old
             | ((. - $old) | .[$misekeep:]) as $extra
             | $old + $extra)
       | add // []) as $miseDead
    | ($rest | group_by(.fam) | map(sort_by(.created_at) | reverse | .[$keep:]) | add // []) as $stale0
    | ($stale0 + $miseDead | unique_by(.id)) as $stale
    | ($rest - $stale) as $left
    # 3. over budget: rebuildable build caches (largest first), then LRU
    | ($left | sort_by([(if .dated then 0 else 1 end), (if .dated then -.size_in_bytes else 0 end), .last_accessed_at]) as $order
       | (($left | map(.size_in_bytes) | add // 0) - $budget) as $excess
       | if $excess <= 0 then [] else
           ($order | reduce .[] as $c ({freed: 0, out: []};
              if .freed < $excess then .out += [$c] | .freed += $c.size_in_bytes else . end) | .out)
         end) as $over
    | ($closed + $stale + $over) | map(.id) | unique | .[]' "${caches}"
}

if [[ "${1:-}" == "select" ]]; then
  select_ids "${2:?usage: prune-caches.sh select CACHES.json OPEN_PRS.txt}" "${3:?}"
  exit 0
fi

size_of() { jq --argjson id "$1" '[.[] | select(.id == $id) | .size_in_bytes] | add // 0' "${work}/caches.json"; }

dry=0
[[ "${1:-}" != "--dry-run" ]] || dry=1
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
gh api --paginate "repos/${repo}/actions/caches?per_page=100" --jq '.actions_caches[]' | jq -s '.' > "${work}/caches.json"
gh pr list --repo "${repo}" --state open --limit 1000 --json number --jq '.[].number' > "${work}/open.txt"
total="$(jq '[.[].size_in_bytes] | add // 0 | . / 1048576 | floor' "${work}/caches.json")"
select_ids "${work}/caches.json" "${work}/open.txt" > "${work}/ids.txt"
echo "cache: ${total} MiB in $(jq length "${work}/caches.json") entries; $(wc -l < "${work}/ids.txt") to delete"
failed=0
gone=0 # MiB-rounded bytes that are no longer there
gone_bytes=0
while read -r id; do
  [[ -n "${id}" ]] || continue
  if ((dry)); then
    echo "would delete cache ${id}"
  elif err="$(gh api -X DELETE "repos/${repo}/actions/caches/${id}" --silent 2>&1)"; then
    echo "deleted cache ${id}"
    gone_bytes=$((gone_bytes + $(size_of "${id}")))
  elif grep -q 'HTTP 404' <<<"${err}"; then
    echo "cache ${id} is already gone (404)"
    gone_bytes=$((gone_bytes + $(size_of "${id}")))
  else
    echo "::warning::could not delete cache ${id}: ${err}" >&2
    failed=$((failed + 1))
  fi
done < "${work}/ids.txt"
gone=$((gone_bytes / 1048576))
after=$((total - gone))
echo "cache after: ${after} MiB (freed ${gone} MiB)"
if ((dry == 0)) && [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "### Actions cache"
    echo
    echo "| | MiB | entries |"
    echo "|---|---|---|"
    echo "| before | ${total} | $(jq length "${work}/caches.json") |"
    echo "| after | ${after} | $(($(jq length "${work}/caches.json") - $(wc -l < "${work}/ids.txt") + failed)) |"
    echo
    echo "Budget ${budget_mb} MiB; freed ${gone} MiB; ${failed} failed deletion(s)."
  } >> "${GITHUB_STEP_SUMMARY}"
fi
((failed == 0)) || { echo "${failed} deletion(s) failed" >&2; exit 1; }
