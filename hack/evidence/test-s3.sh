#!/bin/sh
set -eu

# MinIO no longer publishes a community image anyone can pull. minio/minio and
# minio/mc were removed from Docker Hub on 2026-09-11, this journey was
# repointed at quay.io, and on 2026-09-24 those repositories stopped serving
# anonymous pulls too: both registries now answer an unauthenticated pull with
# "unauthorized". That is what turned this check red on main with no change to
# this repository, and a third repoint to another MinIO-operated copy would be
# the same bet a third time.
#
# bitnamilegacy/minio is Broadcom's frozen archive of the last Bitnami build.
# It is public, anonymously pullable, and carries both binaries —
# /opt/bitnami/minio/bin/minio and /opt/bitnami/minio-client/bin/mc — so one
# pin serves the server and the client. Frozen is the point rather than a
# compromise here: the archive receives no updates, so the digest cannot move
# and the repository it belongs to has no reason to revoke it. This is a test
# fixture that runs on a loopback port for the length of one journey; nothing
# shipped depends on it, and it is not a supply-chain input to the binary.
#
# Tag 2025.5.24 (MinIO RELEASE.2025-05-24), multi-architecture index digest.
minio_image='bitnamilegacy/minio@sha256:451fe6858cb770cc9d0e77ba811ce287420f781c7c1b806a386f6896471a349c'
# EVIDENCE_RUN_ID (CI run id) makes every container, volume and image tag this
# script creates unique per run, so concurrent runs on one Docker host cannot
# collide and a leftover can be found by name. Unset locally.
run_tag="evidence-${EVIDENCE_RUN_ID:-local}-$$"
container="$run_tag-minio"
root_access='test-root-access'
root_secret='test-root-secret-canary'
viewer_access='test-viewer-access'
viewer_secret='test-viewer-secret-canary'
barman_image=${BARMAN_GENERATOR_IMAGE:-evidence-agent-barman-generator:3.19.1}
postgres_image='postgres@sha256:fbcea1bd13b6a882cd6caa6b58db3ae5c102efe50ec625b3e2a5cbc50db5bfe4'
postgres_container="$run_tag-postgres"
fixture_dir=$(mktemp -d)

cleanup() {
    docker exec --user 0 "$postgres_container" chmod -R a+rwX /var/lib/postgresql/data >/dev/null 2>&1 || true
    docker rm -f "$container" >/dev/null 2>&1 || true
    docker rm -f "$postgres_container" >/dev/null 2>&1 || true
    rm -rf "$fixture_dir"
}
trap cleanup EXIT INT TERM

# --entrypoint and --user 0: the Bitnami image starts a wrapper script as uid
# 1001, which cannot create /data. Running the binary directly as root is what
# the official image this replaced did, so the server the journey talks to is
# the same server it talked to before.
docker run --detach --name "$container" --label "evidence.run=${EVIDENCE_RUN_ID:-local}" \
    --publish 127.0.0.1::9000 \
    --user 0 \
    --entrypoint minio \
    --env "MINIO_ROOT_USER=$root_access" \
    --env "MINIO_ROOT_PASSWORD=$root_secret" \
    "$minio_image" server /data >/dev/null

port=$(docker port "$container" 9000/tcp | sed -n 's/.*://p')
endpoint="http://127.0.0.1:$port"
attempt=0
until curl --fail --silent "$endpoint/minio/health/ready" >/dev/null; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 100 ]; then
        docker logs "$container" >&2
        exit 1
    fi
    sleep 0.1
done

root_host="http://$root_access:$root_secret@127.0.0.1:$port"

# mc ships in the same image, so the client needs no second pin. HOME is set
# because mc writes its configuration before it runs the requested command,
# and the image's HOME is the container root.
mc() {
    docker run --rm --network host --user 0 \
        --env "MC_HOST_test=$root_host" --env 'HOME=/tmp' \
        --entrypoint mc "$minio_image" "$@"
}

# Separate from mc() only for --interactive: without it the piped stdin the
# fixture mutations depend on never reaches the container.
mc_pipe() {
    docker run --rm --interactive --network host --user 0 \
        --env "MC_HOST_test=$root_host" --env 'HOME=/tmp' \
        --entrypoint mc "$minio_image" pipe "$@"
}

mc mb test/evidence-agent-proof >/dev/null
printf '%s' 'outside-root' | mc_pipe test/evidence-agent-proof/outside/ignored >/dev/null
docker build --quiet --file internal/evidence/provider/s3/testdata/barman-generator.Dockerfile --tag "$barman_image" . >/dev/null

docker run --detach --name "$postgres_container" --label "evidence.run=${EVIDENCE_RUN_ID:-local}" \
    --publish 127.0.0.1::5432 \
    --env 'POSTGRES_PASSWORD=test-postgres-password' \
    --volume "$fixture_dir/pgdata:/var/lib/postgresql/data" \
    "$postgres_image" >/dev/null
postgres_port=$(docker port "$postgres_container" 5432/tcp | sed -n 's/.*://p')
attempt=0
until docker exec "$postgres_container" pg_isready --username postgres >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 100 ]; then
        docker logs "$postgres_container" >&2
        exit 1
    fi
    sleep 0.1
done

docker run --rm --network host \
    --volume "$fixture_dir/pgdata:/var/lib/postgresql/data:ro" \
    --env "AWS_ACCESS_KEY_ID=$root_access" \
    --env "AWS_SECRET_ACCESS_KEY=$root_secret" \
    --env 'AWS_DEFAULT_REGION=us-east-1' \
    --env 'PGPASSWORD=test-postgres-password' \
    "$barman_image" barman-cloud-backup \
        --endpoint-url "$endpoint" --addressing-style path --gzip \
        --host 127.0.0.1 --port "$postgres_port" --user postgres \
        s3://evidence-agent-proof/repository alpha >/dev/null

completed_id=$(mc ls test/evidence-agent-proof/repository/alpha/base/ | awk '{print $NF}' | tr -d '/')
if [ -z "$completed_id" ]; then
    echo 'Barman did not generate a completed backup' >&2
    exit 1
fi

for mutation in started failed malformed; do
    mc cp --recursive \
        "test/evidence-agent-proof/repository/alpha/base/$completed_id/" \
        "test/evidence-agent-proof/repository/alpha/base/$mutation/" >/dev/null
done
mc cat \
    "test/evidence-agent-proof/repository/alpha/base/$completed_id/backup.info" \
    | sed 's/^status=DONE$/status=STARTED/' \
    | mc_pipe test/evidence-agent-proof/repository/alpha/base/started/backup.info >/dev/null
mc cat \
    "test/evidence-agent-proof/repository/alpha/base/$completed_id/backup.info" \
    | sed 's/^status=DONE$/status=FAILED/' \
    | mc_pipe test/evidence-agent-proof/repository/alpha/base/failed/backup.info >/dev/null
printf '%s\n' 'malformed Barman metadata' \
    | mc_pipe test/evidence-agent-proof/repository/alpha/base/malformed/backup.info >/dev/null
mc cp \
    "test/evidence-agent-proof/repository/alpha/base/$completed_id/backup.info" \
    test/evidence-agent-proof/repository/alpha/base/missing-artifact/backup.info >/dev/null
mc cp \
    "test/evidence-agent-proof/repository/alpha/base/$completed_id/data.tar.gz" \
    test/evidence-agent-proof/repository/alpha/base/missing-info/data.tar.gz >/dev/null

docker run --rm --network host \
    --env "AWS_ACCESS_KEY_ID=$root_access" \
    --env "AWS_SECRET_ACCESS_KEY=$root_secret" \
    --env 'AWS_DEFAULT_REGION=us-east-1' \
    --env "TEST_ENDPOINT=$endpoint" \
    "$barman_image" sh -c 'truncate -s 16777216 /tmp/000000010000000000000001 && barman-cloud-wal-archive --endpoint-url "$TEST_ENDPOINT" --addressing-style path s3://evidence-agent-proof/repository alpha /tmp/000000010000000000000001' >/dev/null
printf 'Barman fixture generator: %s\n' "$(docker run --rm "$barman_image" barman-cloud-backup --version)"
printf 'PostgreSQL fixture image: %s\n' "$postgres_image"
printf 'S3 fixture image: %s\n' "$minio_image"
mc admin user add test "$viewer_access" "$viewer_secret" >/dev/null
policy_file="$PWD/internal/evidence/provider/s3/testdata/minio-readonly-policy.json"
docker run --rm --network host --user 0 \
    --env "MC_HOST_test=$root_host" --env 'HOME=/tmp' \
    --volume "$policy_file:/policy.json:ro" \
    --entrypoint mc "$minio_image" admin policy create test evidence-agent-readonly /policy.json >/dev/null
mc admin policy attach test evidence-agent-readonly --user "$viewer_access" >/dev/null

EVIDENCE_AGENT_S3_INTEGRATION_ENDPOINT="$endpoint" \
EVIDENCE_AGENT_S3_INTEGRATION_ACCESS_KEY="$viewer_access" \
EVIDENCE_AGENT_S3_INTEGRATION_SECRET_KEY="$viewer_secret" \
go test -tags=integration ./internal/evidence/provider/s3 -run '^TestS3MinIOJourney$' -count=1 -v
