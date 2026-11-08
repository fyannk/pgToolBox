#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Runs loadgen against the cluster cluster.sh up created: the environment it wrote
# (kubeconfig, console and MinIO endpoints) is loaded, never ~/.kube/config.
#
#   hack/loadtest/loadgen.sh populate --namespaces 2 --clusters 20 --instances 3
#   hack/loadtest/loadgen.sh <churn|http|scrape|s3fill> [flags]    (-h for a command's flags)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE="${HERE}/.state"
[[ -s "${STATE}/env" ]] || { echo "no ${STATE}/env: run hack/loadtest/cluster.sh up first" >&2; exit 1; }
# shellcheck disable=SC1091
. "${STATE}/env"
exec "${STATE}/loadgen" "$@"
