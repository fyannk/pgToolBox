#!/bin/sh
set -eu

result_dir=$(mktemp -d)
cleanup() {
    rm -rf "$result_dir"
}
trap cleanup EXIT INT TERM

go test ./internal/evidence/provider/providertest ./internal/evidence/provider/s3 ./internal/evidence/provider/azure ./internal/evidence/provider/gcs ./internal/evidence/provider/cursor ./internal/evidence/store/storetest

EVIDENCE_AGENT_NORMALIZED_RESULT="$result_dir/s3-normalized.json" ./hack/evidence/test-s3.sh
EVIDENCE_AGENT_NORMALIZED_RESULT="$result_dir/azure-normalized.json" ./hack/evidence/test-azure.sh
EVIDENCE_AGENT_NORMALIZED_RESULT="$result_dir/gcs-normalized.json" ./hack/evidence/test-gcs.sh

cmp "$result_dir/s3-normalized.json" "$result_dir/azure-normalized.json"
cmp "$result_dir/s3-normalized.json" "$result_dir/gcs-normalized.json"
normalized_sha256=$(sha256sum "$result_dir/s3-normalized.json" | awk '{print $1}')

printf 'provider parity passed: S3, Azure, and GCS normalized output sha256:%s\n' "$normalized_sha256"

# pgBackRest writes S3 and Azure itself (GCS is not a pgBackRest repository
# type this journey covers): the two plain repositories must read the same.
mkdir -p "$result_dir/pgbackrest"
EVIDENCE_AGENT_PGBACKREST_RESULT_DIR="$result_dir/pgbackrest" ./hack/evidence/test-pgbackrest-providers.sh
pgbackrest_sha256=$(sha256sum "$result_dir/pgbackrest/s3.json" | awk '{print $1}')
printf 'pgBackRest provider parity passed: S3 and Azure normalized output sha256:%s\n' "$pgbackrest_sha256"
