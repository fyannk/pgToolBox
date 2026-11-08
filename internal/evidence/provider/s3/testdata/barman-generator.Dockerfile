# Copyright 2026 The pgToolBox Authors.
# Licensed under the Apache License, Version 2.0.

# This image generates test evidence only and is never part of the application
# image. Barman is GPL-3.0 and remains an external fixture-generation tool.
FROM python:3.13-slim@sha256:ae14d4ce8771606477a81446e7ef429edd6762305fc50c84507c1041fa8285d1

RUN python -m pip install --disable-pip-version-check --no-cache-dir \
        boto3==1.43.56 \
        botocore==1.43.56 \
        jmespath==1.1.0 \
        psycopg2-binary==2.9.12 \
        python-dateutil==2.9.0.post0 \
        s3transfer==0.19.2 \
        six==1.17.0 \
        urllib3==2.7.0 \
    && python -m pip install --disable-pip-version-check --no-cache-dir --no-deps barman==3.19.1 \
    && barman-cloud-wal-archive --version
