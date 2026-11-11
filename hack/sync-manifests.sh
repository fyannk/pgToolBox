#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
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
helm_crd_dir="charts/pgtoolbox/operator-crds"
helm_clusterrole="charts/pgtoolbox/templates/operator/clusterrole.yaml"
olm_manifest_dir="deploy/olm/bundle/manifests"
olm_csv="${olm_manifest_dir}/pgtoolbox.clusterserviceversion.yaml"

# 1. The CRDs, copied verbatim to both packaging trees. The chart keeps them in
#    operator-crds/, not crds/: Helm installs crds/ unconditionally, and a
#    console-only release (operator.enabled: false, the default) must
#    not put the operator's CRDs into a cluster. templates/operator/crds.yaml
#    renders them when the operator half is enabled.
for crd in "${crd_dir}"/*.yaml; do
  cp "${crd}" "${helm_crd_dir}/$(basename "${crd}")"
  cp "${crd}" "${olm_manifest_dir}/$(basename "${crd}")"
done

# 2. The Helm ClusterRole: the chart's own templated header, then the
#    generated rules. The header (and the operator.enabled guard around the
#    file) is the only hand-written part.
{
  echo '{{- if .Values.operator.enabled }}'
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
  printf '\n{{- end }}\n'
} > "${helm_clusterrole}"

# 2b. The two user ClusterRoles and the metrics reader: hand-written once in
#     config/rbac, copied into the bundle as plain manifests (OLM applies
#     ClusterRoles shipped beside the CSV) and into the chart as templates. The
#     user roles have fixed names, on purpose: people are bound to them by name.
rules_of() { sed -n '/^rules:/,$p' "$1"; }
helm_user_roles="charts/pgtoolbox/templates/operator/user-clusterroles.yaml"
helm_metrics_reader="charts/pgtoolbox/templates/operator/metrics-reader.yaml"
{
  echo '{{- if and .Values.operator.enabled .Values.operator.userRoles.create }}'
  for role in viewer operator approver; do
    cat <<HEADER
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: pgtoolbox-console-${role}
  labels:
    {{- include "pgtoolbox.labels" . | nindent 4 }}
HEADER
    rules_of "config/rbac/pgtoolbox_console_${role}_clusterrole.yaml"
  done
  echo '{{- if .Values.operator.userRoles.aggregateToAdmin }}'
  cat <<'HEADER'
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: pgtoolbox-aggregate-to-admin
  labels:
    {{- include "pgtoolbox.labels" . | nindent 4 }}
    rbac.authorization.k8s.io/aggregate-to-admin: "true"
HEADER
  rules_of config/rbac/pgtoolbox_aggregate_to_admin_clusterrole.yaml
  echo '{{- end }}'
  echo '{{- end }}'
} > "${helm_user_roles}"
{
  echo '{{- if and .Values.operator.enabled .Values.operator.metrics.readerClusterRole }}'
  cat <<'HEADER'
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "pgtoolbox.fullname" . }}-metrics-reader
  labels:
    {{- include "pgtoolbox.labels" . | nindent 4 }}
HEADER
  rules_of config/rbac/metrics_reader_clusterrole.yaml
  cat <<'FOOTER'
{{- with .Values.operator.metrics.readerSubjects }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "pgtoolbox.fullname" $ }}-metrics-reader
  labels:
    {{- include "pgtoolbox.labels" $ | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ include "pgtoolbox.fullname" $ }}-metrics-reader
subjects:
  {{- range . }}
  - kind: ServiceAccount
    name: {{ .name }}
    namespace: {{ .namespace }}
  {{- end }}
{{- end }}
{{- end }}
FOOTER
} > "${helm_metrics_reader}"
# 2c. The console's own ClusterRoles, which the operator binds (never holds) for
#     each PgToolBoxConsole: fixed names, the operator's `bind` rule names them.
# 2d. The restore-drill delete guard (config/admission): copied as is, rendered
#     only where the API server serves admissionregistration.k8s.io/v1
#     policies (1.30+). The OLM bundle cannot carry a ValidatingAdmissionPolicy.
helm_drill_guard="charts/pgtoolbox/templates/operator/restoredrill-guard.yaml"
{
  echo '{{- $guard := .Values.operator.restoreDrillGuard.enabled }}'
  echo '{{- if and .Values.operator.enabled (or (eq (toString $guard) "true") (and (eq (toString $guard) "auto") (.Capabilities.APIVersions.Has "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"))) }}'
  echo '# Generated from config/admission/restore_drill_guard.yaml by hack/sync-manifests.sh; do not edit.'
  grep -v '^#' config/admission/restore_drill_guard.yaml
  echo '{{- end }}'
} > "${helm_drill_guard}"
# 2e. The operator chart's admission policy (image and workload-identity
#     allow-lists, Secret references, PgAdmin credentials) as a kustomize
#     component, config/components/admission: the chart is the source (the
#     allow-lists are values), rendered with its defaults; the release-specific
#     labels are dropped so the copy does not change with the version. OLM
#     cannot carry it.
admission_component="config/components/admission/admission_policy.yaml"
{
  echo '# Generated from charts/pgtoolbox/templates/operator/admissionpolicy.yaml (default'
  echo '# values) by hack/sync-manifests.sh; do not edit. To change the allow-lists, patch'
  echo '# spec.variables in your overlay, or render the chart with your values.'
  helm template pgtoolbox charts/pgtoolbox --namespace pgtoolbox --set operator.enabled=true \
      --show-only templates/operator/admissionpolicy.yaml \
    | sed -e '/^# Source: /d' \
          -e '/^    app.kubernetes.io\/instance: /d' \
          -e '/^    app.kubernetes.io\/version: /d' \
          -e '/^    app.kubernetes.io\/managed-by: /d' \
          -e '/^    helm.sh\/chart: /d' \
    | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
} > "${admission_component}"
helm_console_roles="charts/pgtoolbox/templates/operator/console-clusterroles.yaml"
{
  echo '{{- if .Values.operator.enabled }}'
  for role in pgtoolbox-console pgtoolbox-console-pod-logs pgtoolbox-console-restore-drills; do
    cat <<HEADER
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${role}
  labels:
    {{- include "pgtoolbox.labels" . | nindent 4 }}
HEADER
    rules_of "config/rbac/${role//-/_}_clusterrole.yaml"
  done
  echo '{{- end }}'
} > "${helm_console_roles}"
for role in pgtoolbox-console pgtoolbox-console-pod-logs pgtoolbox-console-restore-drills; do
  cp "config/rbac/${role//-/_}_clusterrole.yaml" "${olm_manifest_dir}/${role}_rbac.authorization.k8s.io_v1_clusterrole.yaml"
done
cp config/rbac/pgtoolbox_console_viewer_clusterrole.yaml "${olm_manifest_dir}/pgtoolbox-console-viewer_rbac.authorization.k8s.io_v1_clusterrole.yaml"
cp config/rbac/pgtoolbox_console_operator_clusterrole.yaml "${olm_manifest_dir}/pgtoolbox-console-operator_rbac.authorization.k8s.io_v1_clusterrole.yaml"
cp config/rbac/pgtoolbox_console_approver_clusterrole.yaml "${olm_manifest_dir}/pgtoolbox-console-approver_rbac.authorization.k8s.io_v1_clusterrole.yaml"
cp config/rbac/pgtoolbox_aggregate_to_admin_clusterrole.yaml "${olm_manifest_dir}/pgtoolbox-aggregate-to-admin_rbac.authorization.k8s.io_v1_clusterrole.yaml"
# The PodDisruptionBudget, the one non-RBAC object the bundle ships beside the CSV.
sed '/^  namespace: system$/d' config/manager/poddisruptionbudget.yaml > "${olm_manifest_dir}/pgtoolbox_policy_v1_poddisruptionbudget.yaml"
cp config/rbac/metrics_reader_clusterrole.yaml "${olm_manifest_dir}/pgtoolbox-metrics-reader_rbac.authorization.k8s.io_v1_clusterrole.yaml"

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

echo "synced CRDs and RBAC into charts/ and deploy/olm"
