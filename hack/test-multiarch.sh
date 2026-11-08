#!/bin/sh
# Multi-architecture test: the image builds for linux/amd64 and
# linux/arm64.
set -eu

image="${1:-pgtoolbox-console:multiarch}"

docker buildx build --platform linux/amd64,linux/arm64 --tag "$image" . > /dev/null
echo "multiarch test passed (linux/amd64, linux/arm64)"
