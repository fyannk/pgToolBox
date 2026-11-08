#!/bin/sh
# npm audit for one dependency tree, minus the advisories this repository
# has reviewed and accepted in hack/npm-audit-accepted.txt.
#
# `npm audit` has no per-advisory exception, so the choice it leaves is
# between failing forever on something unfixable and lowering the severity
# threshold until nothing fails. Both destroy the signal. This keeps the
# threshold where it is and names the exceptions instead: an advisory that
# is not listed fails the build, including a new one in a package that
# already has an accepted entry.
#
# Usage: check-npm-audit.sh <directory> [severity]
set -eu

dir=${1:?usage: check-npm-audit.sh <directory> [severity]}
level=${2:-high}
accepted=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/npm-audit-accepted.txt

# npm audit exits non-zero whenever it finds anything at or above the level,
# so a failing status is expected here; the report is what matters.
report=$(cd "$dir" && npm audit --audit-level="$level" --json 2>/dev/null) || true
if [ -z "$report" ]; then
    echo "npm audit produced no report for $dir" >&2
    exit 1
fi

printf '%s' "$report" | ACCEPTED="$accepted" DIR="$dir" LEVEL="$level" python3 -c '
import json
import os
import sys

RANK = ["info", "low", "moderate", "high", "critical"]

directory = os.environ["DIR"]
level = os.environ["LEVEL"]
floor = RANK.index(level)

accepted = set()
with open(os.environ["ACCEPTED"], encoding="utf-8") as handle:
    for line in handle:
        line = line.strip()
        if line and not line.startswith("#"):
            accepted.add(line.split()[0])

try:
    report = json.load(sys.stdin)
except json.JSONDecodeError:
    print("npm audit did not return JSON", file=sys.stderr)
    raise SystemExit(1)

# Advisory identity lives on the "via" entries. One package can be reached
# through several advisories, and only those at or above the floor count.
unaccepted = {}
used = set()
for package, node in report.get("vulnerabilities", {}).items():
    for via in node.get("via", []):
        if not isinstance(via, dict):
            continue
        if RANK.index(via.get("severity", "info")) < floor:
            continue
        identifier = (via.get("url") or "").rsplit("/", 1)[-1]
        if identifier in accepted:
            used.add(identifier)
        else:
            unaccepted.setdefault(identifier, (package, via.get("title", "")))

for identifier, (package, title) in sorted(unaccepted.items()):
    print(
        f"{directory}: unaccepted {level}+ advisory {identifier} in {package}: {title}",
        file=sys.stderr,
    )
if unaccepted:
    print(
        f"{directory}: {len(unaccepted)} advisory(ies) at or above {level} are not in "
        "hack/npm-audit-accepted.txt. Fix them, or add an entry stating why the "
        "exposure does not apply and what would remove it.",
        file=sys.stderr,
    )
    raise SystemExit(1)

summary = f"{directory}: no unaccepted advisories at or above {level}"
if used:
    summary += f"; {len(used)} accepted"
print(summary)

# Entries unmatched in this tree are not reported: one file covers both
# trees, so an entry idle here is usually carrying the other one, and saying
# so on every run trains people to ignore the output. An entry that has
# outlived its advisory is caught by the re-check note each one carries.
'
