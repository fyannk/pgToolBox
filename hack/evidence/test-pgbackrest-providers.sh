#!/bin/sh
# The pgBackRest provider journeys: real pgBackRest 2.60.0 writes straight to
# MinIO (S3, path-style) and Azurite (Azure), both over TLS with a CA made
# for this run, under the non-root repository path /pgbr/orders, plus an
# encrypted S3 repository under /pgbr/orders-enc
# (hack/evidence/pgbackrest-generator/generate-providers.sh). The evidence
# reader then reads them back through the real provider clients
# (internal/evidence/provider/{s3,azure}, build tag integration) with the
# prefix "pgbr/orders": the repo path without its leading slash.
#
# With EVIDENCE_AGENT_PGBACKREST_RESULT_DIR set, each journey writes its
# normalized result there (s3.json, azure.json) and the plain S3 and Azure
# results must be identical: test-provider-parity.sh does that.
#
# Docker only; no sudo. The containers and the network carry
# evidence.run=$EVIDENCE_RUN_ID, so what a
# cancelled run leaves can be found. Every credential below is a throwaway test value; the
# Azure key is Azurite's published development key.
set -eu

minio_image='bitnamilegacy/minio@sha256:451fe6858cb770cc9d0e77ba811ce287420f781c7c1b806a386f6896471a349c'
azurite_image='mcr.microsoft.com/azure-storage/azurite@sha256:647c63a91102a9d8e8000aab803436e1fc85fbb285e7ce830a82ee5d6661cf37'
generator_image=${PGBACKREST_GENERATOR_IMAGE:-evidence-agent-pgbackrest-generator:2.60.0}
run_id=${EVIDENCE_RUN_ID:-local}
run_tag="evidence-$run_id-$$"
network="$run_tag-pgbr"
minio="$run_tag-pgbr-minio"
azurite="$run_tag-pgbr-azurite"
generator="$run_tag-pgbr-generator"
root_access='test-root-access'
root_secret='test-root-secret-canary'
bucket='pgbr-proof'
container='pgbr-proof'
azure_account='devstoreaccount1'
azure_key='Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=='
certs=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/pgbackrest-providers.XXXXXX")
results=${EVIDENCE_AGENT_PGBACKREST_RESULT_DIR:-}

cleanup() {
    docker rm -f "$minio" "$azurite" "$generator" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
    rm -rf "$certs"
}
trap cleanup EXIT INT TERM

# A CA and one server certificate for both emulators: the names the
# generator uses on the Docker network and the loopback address the Go
# clients use.
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=pgtoolbox journey CA' \
    -keyout "$certs/ca.key" -out "$certs/ca.crt" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj '/CN=pgtoolbox journey storage' \
    -keyout "$certs/private.key" -out "$certs/server.csr" 2>/dev/null
printf 'subjectAltName=DNS:minio,DNS:azurite,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' > "$certs/ext.cnf"
openssl x509 -req -in "$certs/server.csr" -CA "$certs/ca.crt" -CAkey "$certs/ca.key" -CAcreateserial \
    -days 1 -extfile "$certs/ext.cnf" -out "$certs/public.crt" 2>/dev/null
# mc runs as the calling user and writes its configuration beside the CA.
mkdir -p "$certs/minio/CAs" "$certs/mc/certs/CAs"
cp "$certs/public.crt" "$certs/private.key" "$certs/minio/"
cp "$certs/ca.crt" "$certs/mc/certs/CAs/ca.crt"
# The containers read them as whatever user they run as.
chmod -R a+rX "$certs"

docker build --quiet --tag "$generator_image" hack/evidence/pgbackrest-generator >/dev/null
docker network create --label "evidence.run=$run_id" "$network" >/dev/null

docker run --detach --name "$minio" --label "evidence.run=$run_id" --network "$network" --network-alias minio \
    --publish 127.0.0.1::9000 --user 0 --entrypoint minio \
    --env "MINIO_ROOT_USER=$root_access" --env "MINIO_ROOT_PASSWORD=$root_secret" \
    --volume "$certs/minio:/certs:ro" \
    "$minio_image" server /data --certs-dir /certs >/dev/null
docker run --detach --name "$azurite" --label "evidence.run=$run_id" --network "$network" --network-alias azurite \
    --publish 127.0.0.1::10000 --volume "$certs:/certs:ro" \
    "$azurite_image" azurite-blob --blobHost 0.0.0.0 --inMemoryPersistence --disableTelemetry --skipApiVersionCheck \
    --cert /certs/public.crt --key /certs/private.key >/dev/null

minio_port=$(docker port "$minio" 9000/tcp | sed -n 's/.*://p' | head -n 1)
azurite_port=$(docker port "$azurite" 10000/tcp | sed -n 's/.*://p' | head -n 1)
wait_for() {
    attempt=0
    until curl --silent --output /dev/null --cacert "$certs/ca.crt" "$2"; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge 120 ]; then
            echo "$1 did not answer on $2 within 60s" >&2
            docker logs "$1" >&2 || true
            exit 1
        fi
        sleep 0.5
    done
}
wait_for "$minio" "https://127.0.0.1:$minio_port/minio/health/ready"
wait_for "$azurite" "https://127.0.0.1:$azurite_port/$azure_account?comp=list"

docker run --rm --network host --user "$(id -u):$(id -g)" \
    --env "MC_HOST_test=https://$root_access:$root_secret@127.0.0.1:$minio_port" --env 'HOME=/tmp' \
    --volume "$certs/mc:/tmp/.mc" \
    --entrypoint mc "$minio_image" mb "test/$bucket" >/dev/null

# The container: pgBackRest creates neither it nor the bucket.
SSL_CERT_FILE="$certs/ca.crt" \
EVIDENCE_AGENT_PGBACKREST_AZURE_ENDPOINT="https://127.0.0.1:$azurite_port" \
EVIDENCE_AGENT_PGBACKREST_AZURE_PHASE=setup \
    go test -tags=integration ./internal/evidence/provider/azure -run '^TestAzuritePGBackRestJourney$' -count=1 >/dev/null

docker run --rm --name "$generator" --label "evidence.run=$run_id" --network "$network" \
    --volume "$certs/ca.crt:/certs/ca.crt:ro" \
    --env S3_HOST=minio --env S3_PORT=9000 --env "S3_BUCKET=$bucket" \
    --env "S3_KEY=$root_access" --env "S3_KEY_SECRET=$root_secret" \
    --env AZURE_HOST=azurite --env AZURE_PORT=10000 --env "AZURE_CONTAINER=$container" \
    --env "AZURE_ACCOUNT=$azure_account" --env "AZURE_KEY=$azure_key" \
    "$generator_image" pgbackrest-provider-repos

# The object keys pgBackRest wrote, as the bucket holds them: repo1-path
# /pgbr/orders becomes the key prefix pgbr/orders/ (no leading slash).
keys=$(docker run --rm --network host --user "$(id -u):$(id -g)" \
    --env "MC_HOST_test=https://$root_access:$root_secret@127.0.0.1:$minio_port" --env 'HOME=/tmp' \
    --volume "$certs/mc:/tmp/.mc" \
    --entrypoint mc "$minio_image" ls --recursive "test/$bucket" | awk '{print $NF}')
for key in pgbr/orders/backup/demo/backup.info pgbr/orders/archive/demo/archive.info pgbr/orders-enc/backup/demo/backup.info; do
    printf '%s\n' "$keys" | grep -qx "$key" || { echo "pgBackRest did not write $key; the bucket holds:" >&2; printf '%s\n' "$keys" >&2; exit 1; }
done
printf 'pgBackRest S3 key mapping: repo1-path=/pgbr/orders -> %s objects under pgbr/orders/\n' \
    "$(printf '%s\n' "$keys" | grep -c '^pgbr/orders/')"

s3_result='' azure_result=''
if [ -n "$results" ]; then
    s3_result="$results/s3.json" azure_result="$results/azure.json"
fi
EVIDENCE_AGENT_PGBACKREST_S3_ENDPOINT="https://127.0.0.1:$minio_port" \
EVIDENCE_AGENT_PGBACKREST_S3_CA_FILE="$certs/ca.crt" \
EVIDENCE_AGENT_PGBACKREST_S3_BUCKET="$bucket" \
EVIDENCE_AGENT_PGBACKREST_S3_ACCESS_KEY="$root_access" \
EVIDENCE_AGENT_PGBACKREST_S3_SECRET_KEY="$root_secret" \
EVIDENCE_AGENT_NORMALIZED_RESULT="$s3_result" \
    go test -tags=integration ./internal/evidence/provider/s3 -run '^TestS3PGBackRestJourney$' -count=1 -v
# The Azure adapter has no CA option; Go's TLS stack takes SSL_CERT_FILE.
SSL_CERT_FILE="$certs/ca.crt" \
EVIDENCE_AGENT_PGBACKREST_AZURE_ENDPOINT="https://127.0.0.1:$azurite_port" \
EVIDENCE_AGENT_PGBACKREST_AZURE_PHASE=verify \
EVIDENCE_AGENT_NORMALIZED_RESULT="$azure_result" \
    go test -tags=integration ./internal/evidence/provider/azure -run '^TestAzuritePGBackRestJourney$' -count=1 -v

if [ -n "$results" ]; then
    cmp "$s3_result" "$azure_result"
fi
printf 'pgBackRest provider journeys passed: %s over S3 (plain, encrypted) and Azure, TLS, repo path /pgbr/orders\n' \
    "$(docker run --rm "$generator_image" pgbackrest version)"
