# Copyright 2026 The pgToolBox Authors.
# Licensed under the Apache License, Version 2.0.

# This image generates test evidence only and is never part of an application
# image: real pgBackRest 2.60.0 and PostgreSQL 17 write a posix repository that
# generate.sh reduces to the small metadata files the reader parses
# (internal/evidence/formats/pgbackrest/testdata). pgBackRest is MIT-licensed
# and PostgreSQL is under the PostgreSQL licence; neither ships with pgToolBox.
#
# Every input is pinned: the base by digest, the PGDG signing key by
# fingerprint, the packages by exact version. PGDG keeps only the newest few
# builds in its main suite; when a pinned build moves to apt-archive.postgresql.org
# the build fails, and the pin is bumped (and the fixtures regenerated) on purpose.
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc

ARG PGBACKREST_PACKAGE_VERSION=2.60.0-1.pgdg12+1
ARG POSTGRESQL_PACKAGE_VERSION=17.11-1.pgdg12+2
ARG PGDG_KEY_FINGERPRINT=B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8

SHELL ["/bin/sh", "-eu", "-c"]

# The whole key file becomes the archive keyring, so it must hold the one
# pinned key and nothing else (verify-signing-key.sh).
COPY verify-signing-key.sh /usr/local/bin/verify-signing-key
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /tmp/pgdg.asc \
    && verify-signing-key /tmp/pgdg.asc "$PGDG_KEY_FINGERPRINT" \
    && gpg --batch --dearmor < /tmp/pgdg.asc > /usr/share/keyrings/pgdg.gpg \
    && rm /tmp/pgdg.asc \
    && echo "deb [signed-by=/usr/share/keyrings/pgdg.gpg] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list \
    && mkdir -p /etc/postgresql-common/createcluster.d \
    && echo "create_main_cluster = false" > /etc/postgresql-common/createcluster.d/no-main.conf \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        "postgresql-17=${POSTGRESQL_PACKAGE_VERSION}" \
        "postgresql-client-17=${POSTGRESQL_PACKAGE_VERSION}" \
        "pgbackrest=${PGBACKREST_PACKAGE_VERSION}" \
    && apt-get purge -y --auto-remove curl gnupg \
    && rm -rf /var/lib/apt/lists/* \
    && pgbackrest version | grep -qx 'pgBackRest 2.60.0' \
    && /usr/lib/postgresql/17/bin/postgres --version

COPY generate.sh /usr/local/bin/pgbackrest-fixtures
COPY generate-providers.sh /usr/local/bin/pgbackrest-provider-repos
COPY generate-many.sh /usr/local/bin/pgbackrest-many-backups
