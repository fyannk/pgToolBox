#!/usr/bin/env bash
set -euo pipefail
image='fsouza/fake-gcs-server@sha256:666f86b873120818b10a5e68d99401422fcf8b00c1f27fe89599c35236f48b4c'
# EVIDENCE_RUN_ID: see test-s3.sh. Ephemeral loopback port instead of a fixed 4443.
name="evidence-${EVIDENCE_RUN_ID:-local}-gcs-$$"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
docker run -d --rm --name "$name" --label "evidence.run=${EVIDENCE_RUN_ID:-local}" -p 127.0.0.1::4443 "$image" -scheme http >/dev/null
port=$(docker port "$name" 4443/tcp | sed -n 's/.*://p' | head -n 1)
endpoint="http://127.0.0.1:$port"
for _ in $(seq 1 30); do curl -fsS "$endpoint/storage/v1/b" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS -X POST "$endpoint/storage/v1/b?project=proof" -H 'Content-Type: application/json' -d '{"name":"evidence-agent-proof"}' >/dev/null
FAKE_GCS_ENDPOINT="$endpoint" go test -tags=integration ./internal/evidence/provider/gcs -run '^TestFakeGCSJourney$' -count=1 -v
