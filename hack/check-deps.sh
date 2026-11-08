#!/bin/sh
# Forbidden-dependency scan. No package that compiles into the console, the
# operator or the shared pkg/ libraries may come from a cloud
# provider SDK, a SQL driver, or repository-parsing code; those belong to the
# evidence agent (cmd/evidence-agent), whose object-store credentials never
# enter the console. The scan inspects the actual package dependency closure,
# not the raw module graph, which carries pruned transitive entries of
# test-only tooling.
set -eu

forbidden='^(github\.com/aws/|github\.com/Azure/|cloud\.google\.com/|github\.com/googleapis/|github\.com/jackc/pgx|github\.com/lib/pq|github\.com/go-sql-driver/|github\.com/microsoft/go-mssqldb|github\.com/mattn/go-sqlite|github\.com/minio/)'

if go list -deps ./cmd/console ./cmd/operator ./pkg/... | grep -E "$forbidden"; then
  echo "forbidden package in the build dependency closure" >&2
  exit 1
fi


echo "build dependency closure clean"
