#!/bin/sh
# Regenerates the pgBackRest fixtures with real pgBackRest 2.60.0 and
# PostgreSQL 17 (hack/evidence/pgbackrest-generator) and either compares them
# with the committed ones or, with --update, replaces the committed ones.
#
# Labels, timestamps, system ids, checksums, salts and WAL checksums change on
# every run, so the comparison is not byte for byte. It is the Go test
# TestRegeneratedFixturesMatchCommittedShape, which runs the reader over the
# regenerated tree and requires the run-independent shape of its output to
# equal the committed testdata/golden/shape.json. What the
# shape keeps and drops is documented in testdata/README.md.
#
# Docker only; no sudo. The container is labelled evidence.run=$EVIDENCE_RUN_ID and the
# repository volume is named evidence-$EVIDENCE_RUN_ID-pgbackrest-repo, so a
# leftover of a cancelled run can be found with `docker ps --filter label=evidence.run`.
set -eu

image=${PGBACKREST_GENERATOR_IMAGE:-evidence-agent-pgbackrest-generator:2.60.0}
run_id=${EVIDENCE_RUN_ID:-local}
volume="evidence-$run_id-pgbackrest-repo"
fixtures=internal/evidence/formats/pgbackrest/testdata/pgbackrest-2.60.0
generated=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/pgbackrest-fixtures.XXXXXX")
cleanup() {
    rm -rf "$generated"
    docker volume rm --force "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker build --quiet --tag "$image" hack/evidence/pgbackrest-generator >/dev/null
docker volume rm --force "$volume" >/dev/null 2>&1 || true
docker volume create --label "evidence.run=$run_id" "$volume" >/dev/null
docker run --rm --label "evidence.run=$run_id" --volume "$volume:/repo" "$image" pgbackrest-fixtures |
    tar -C "$generated" -xf -

if [ "${1:-}" = --update ]; then
    rm -rf "$fixtures"
    mkdir -p "$fixtures"
    cp -R "$generated"/. "$fixtures"/
    printf 'pgBackRest fixtures replaced from %s; run go test ./internal/evidence/formats/pgbackrest -update and review the goldens\n' \
        "$(cat "$fixtures/pgbackrest-version.txt")"
    exit 0
fi

PGBACKREST_REGENERATED_FIXTURES="$generated" go test ./internal/evidence/formats/pgbackrest \
    -run '^(TestRegeneratedFixturesMatchCommittedShape|TestRegeneratedRetentionFollowsExpire)$' -count=1
printf 'pgBackRest fixture check passed: regenerated %s output has the committed shape\n' \
    "$(cat "$generated/pgbackrest-version.txt")"
