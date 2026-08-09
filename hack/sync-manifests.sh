#!/usr/bin/env bash
# Copyright © contributors to the pgtoolbox project.
# SPDX-License-Identifier: Apache-2.0
#
# Propagate the generated manifests to the packaging that ships them.
#
# controller-gen owns config/crd/bases and config/rbac/role.yaml. The Helm
# chart and the OLM bundle carry their own copies of both, and until this
# script existed those copies were hand-maintained: a rule added to a
# controller reached the kustomize overlay and silently missed the two
# install paths most users take. Everything below is a copy, never an edit —
# the generated files stay the single source.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

crd_dir="config/crd/bases"
role_file="config/rbac/role.yaml"
helm_crd_dir="deploy/helm/pgtoolbox/crds"
helm_clusterrole="deploy/helm/pgtoolbox/templates/clusterrole.yaml"
olm_manifest_dir="deploy/olm/bundle/manifests"
olm_csv="${olm_manifest_dir}/pgtoolbox.clusterserviceversion.yaml"

# 1. The CRDs, copied verbatim to both packaging trees.
for crd in "${crd_dir}"/*.yaml; do
  cp "${crd}" "${helm_crd_dir}/$(basename "${crd}")"
  cp "${crd}" "${olm_manifest_dir}/$(basename "${crd}")"
done

# 2. The Helm ClusterRole: the chart's own templated header, then the
#    generated rules. The header is the only hand-written part.
{
  cat <<'HEADER'
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  labels:
    {{- include "pgtoolbox.labels" . | nindent 4 }}
  name: {{ include "pgtoolbox.fullname" . }}-manager-role
HEADER
  sed -n '/^rules:/,$p' "${role_file}"
} > "${helm_clusterrole}"

# 3. The OLM CSV's clusterPermissions block: the same rules, re-indented to
#    where the CSV carries them. Everything before and after the block is
#    preserved byte for byte.
python3 - "$@" <<PY
import re, sys

csv_path = "${olm_csv}"
role_path = "${role_file}"

with open(role_path, encoding="utf-8") as handle:
    role = handle.read()

rules = role.split("\nrules:\n", 1)[1].rstrip("\n")
indented = "\n".join(
    ("        " + line) if line.strip() else line for line in rules.splitlines()
)

with open(csv_path, encoding="utf-8") as handle:
    csv = handle.read()

# The block runs from "clusterPermissions:" to the next key at the same
# indentation, so replacing it never disturbs its neighbours.
pattern = re.compile(
    r"(^      clusterPermissions:\n      - serviceAccountName: pgtoolbox\n        rules:\n)"
    r".*?"
    r"(?=^      [a-zA-Z])",
    re.MULTILINE | re.DOTALL,
)
replacement, count = pattern.subn(lambda m: m.group(1) + indented + "\n", csv)
if count != 1:
    sys.exit(f"expected exactly one clusterPermissions block in {csv_path}, found {count}")

with open(csv_path, "w", encoding="utf-8") as handle:
    handle.write(replacement)
PY

echo "synced CRDs and RBAC into deploy/helm and deploy/olm"
