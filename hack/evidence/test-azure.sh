#!/usr/bin/env bash
set -euo pipefail
image='mcr.microsoft.com/azure-storage/azurite@sha256:647c63a91102a9d8e8000aab803436e1fc85fbb285e7ce830a82ee5d6661cf37'
# EVIDENCE_RUN_ID: see test-s3.sh. The blob port is ephemeral on loopback so two runs
# on one Docker host never fight over 10000.
name="evidence-${EVIDENCE_RUN_ID:-local}-azure-$$"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
docker run -d --rm --name "$name" --label "evidence.run=${EVIDENCE_RUN_ID:-local}" -p 127.0.0.1::10000 "$image" azurite-blob --blobHost 0.0.0.0 --inMemoryPersistence --disableTelemetry --skipApiVersionCheck >/dev/null
port=$(docker port "$name" 10000/tcp | sed -n 's/.*://p' | head -n 1)
# Same shape as test-s3.sh: a fixture that never comes up fails here, with its
# logs, instead of letting the journey report a bare "connection refused".
attempt=0
until curl -s -o /dev/null "http://127.0.0.1:$port/devstoreaccount1?comp=list"; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 120 ]; then
        echo "Azurite did not answer on 127.0.0.1:$port within 60s" >&2
        docker logs "$name" >&2 || true
        exit 1
    fi
    sleep 0.5
done
if ! AZURITE_BLOB_ENDPOINT="http://127.0.0.1:$port/devstoreaccount1" go test -tags=integration ./internal/evidence/provider/azure -run '^TestAzuriteJourneyWithReadOnlySAS$' -count=1 -v; then
    # A fixture that dies mid-journey (OOM, crash) is otherwise indistinguishable
    # from a provider regression.
    echo '--- Azurite container state and logs ---' >&2
    docker inspect --format 'status={{.State.Status}} oom={{.State.OOMKilled}} exit={{.State.ExitCode}}' "$name" >&2 || true
    docker logs "$name" >&2 || true
    exit 1
fi
