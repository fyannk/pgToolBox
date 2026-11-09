#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Sourced, not run: what test/e2e/pgbackrest/run.sh adds to the CloudNativePG
# base of hack/e2e-cnpg-stack.sh -- the two pgBackRest CNPG-I plugins at the
# pinned versions, each image by digest, and MinIO over TLS with a CA the
# test creates through cert-manager.
#
# The pins live here, under hack/, in one place a bump touches. A newer
# plugin is a pin bump: the manifest checksum, the images and the decode test of internal/backupresolve/pgbackrest/testdata move
# together.

# Dalibo's plugin (pgbackrest.dalibo.com, the Stanza kind). The manifest at the
# v0.0.3 tag names its controller image 0.0.2 (the release notes describe
# 0.0.3's stanza maintenance, which writes Stanza.status.recoveryWindow): the
# digest below is the 0.0.3 controller, set with kustomize's images transformer.
DALIBO_PLUGIN_VERSION="${DALIBO_PLUGIN_VERSION:-v0.0.3}"
DALIBO_PLUGIN_MANIFEST="${DALIBO_PLUGIN_MANIFEST:-https://raw.githubusercontent.com/dalibo/cnpg-plugin-pgbackrest/${DALIBO_PLUGIN_VERSION}/manifest.yaml}"
DALIBO_PLUGIN_MANIFEST_SHA256="${DALIBO_PLUGIN_MANIFEST_SHA256:-095aab2716f841e2940771e9c5095f5355fad433e423983f7d95f4324f51f8f3}"
DALIBO_CONTROLLER_IMAGE="${DALIBO_CONTROLLER_IMAGE:-docker.io/dalibo/cnpg-pgbackrest-controller@sha256:9563249502db7c1ae22c5e1979ab7ffd040559ea8c7544bfad25c5254c7ceb2e}" # 0.0.3
DALIBO_SIDECAR_IMAGE="${DALIBO_SIDECAR_IMAGE:-docker.io/dalibo/cnpg-pgbackrest-sidecar@sha256:3cb4ab8de262147f800be40e0bb082d33728060665282f1524d95c9772ee77c3}"       # 0.0.3

# Opera's plugin (pgbackrest.cnpg.opera.com, the Archive kind), from the
# release's manifest.yaml asset.
OPERA_PLUGIN_VERSION="${OPERA_PLUGIN_VERSION:-v0.9.0}"
OPERA_PLUGIN_MANIFEST="${OPERA_PLUGIN_MANIFEST:-https://github.com/operasoftware/cnpg-plugin-pgbackrest/releases/download/${OPERA_PLUGIN_VERSION}/manifest.yaml}"
OPERA_PLUGIN_MANIFEST_SHA256="${OPERA_PLUGIN_MANIFEST_SHA256:-fccbea8d8c060bfe763d6d4713106e297c4384c2746dbce25ed83c5893f0b5bf}"
OPERA_CONTROLLER_IMAGE="${OPERA_CONTROLLER_IMAGE:-docker.io/operasoftware/cnpg-plugin-pgbackrest@sha256:56281d30a8b7693dd5040e813529bd7fd7081c4c0795bf6099db410e674514a2}" # v0.9.0
OPERA_SIDECAR_IMAGE="${OPERA_SIDECAR_IMAGE:-docker.io/operasoftware/cnpg-plugin-pgbackrest-sidecar@sha256:be0df0cbb875fe737acf9e8a8669f9e8441a6a82b3d5f80b05758bd3383a9e72}" # v0.9.0

# fetch_plugin_manifest <url> <sha256> <file>: download and verify.
fetch_plugin_manifest() {
  fetch_verified "$1" "$3" "$2"
}

# render_pgbackrest_plugins <dir>: writes <dir>/dalibo.yaml and <dir>/opera.yaml.
# The two plugins collide when applied as shipped -- both ship a Service
# `pgbackrest`, a Role `leader-election-role` with its RoleBinding and a
# ClusterRoleBinding `metrics-auth-rolebinding` in cnpg-system, bound to
# different ServiceAccounts -- so Opera's are renamed (kustomize patches; the
# Service name is also the server certificate's name).
render_pgbackrest_plugins() {
  local dir=$1 b64
  mkdir -p "${dir}/dalibo" "${dir}/opera"
  fetch_plugin_manifest "${DALIBO_PLUGIN_MANIFEST}" "${DALIBO_PLUGIN_MANIFEST_SHA256}" "${dir}/dalibo/manifest.yaml"
  fetch_plugin_manifest "${OPERA_PLUGIN_MANIFEST}" "${OPERA_PLUGIN_MANIFEST_SHA256}" "${dir}/opera/manifest.yaml"

  b64="$(printf '%s' "${DALIBO_SIDECAR_IMAGE}" | base64 -w0)"
  cat >"${dir}/dalibo/kustomization.yaml" <<YAML
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [manifest.yaml]
images:
  - name: registry.hub.docker.com/dalibo/cnpg-pgbackrest-controller
    newName: ${DALIBO_CONTROLLER_IMAGE%@*}
    digest: ${DALIBO_CONTROLLER_IMAGE#*@}
patches:
  - target: {kind: Secret, name: "plugin-pgbackrest-.*"}
    patch: |-
      - {op: replace, path: /data/SIDECAR_IMAGE, value: ${b64}}
YAML
  kubectl kustomize "${dir}/dalibo" >"${dir}/dalibo.yaml"

  b64="$(printf '%s' "${OPERA_SIDECAR_IMAGE}" | base64 -w0)"
  cat >"${dir}/opera/kustomization.yaml" <<YAML
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [manifest.yaml]
images:
  - name: operasoftware/cnpg-plugin-pgbackrest
    newName: ${OPERA_CONTROLLER_IMAGE%@*}
    digest: ${OPERA_CONTROLLER_IMAGE#*@}
patches:
  - target: {kind: Secret, name: "plugin-pgbackrest-.*"}
    patch: |-
      - {op: replace, path: /data/SIDECAR_IMAGE, value: ${b64}}
  - target: {kind: Service, name: pgbackrest}
    patch: |-
      - {op: replace, path: /metadata/name, value: pgbackrest-opera}
  - target: {kind: Certificate, name: pgbackrest-server}
    patch: |-
      - {op: replace, path: /spec/commonName, value: pgbackrest-opera}
      - {op: replace, path: /spec/dnsNames/0, value: pgbackrest-opera}
  - target: {kind: Role, name: leader-election-role}
    patch: |-
      - {op: replace, path: /metadata/name, value: opera-leader-election-role}
  - target: {kind: RoleBinding, name: leader-election-rolebinding}
    patch: |-
      - {op: replace, path: /metadata/name, value: opera-leader-election-rolebinding}
      - {op: replace, path: /roleRef/name, value: opera-leader-election-role}
  - target: {kind: ClusterRoleBinding, name: metrics-auth-rolebinding}
    patch: |-
      - {op: replace, path: /metadata/name, value: opera-metrics-auth-rolebinding}
YAML
  kubectl kustomize "${dir}/opera" >"${dir}/opera.yaml"
}

# install_pgbackrest_plugins <dir>: apply both rendered manifests (cert-manager's
# webhook can answer a little after its Deployment is Available: retry) and wait
# for the two controllers.
install_pgbackrest_plugins() {
  local dir=$1 i
  render_pgbackrest_plugins "${dir}"
  log "installing the pgBackRest plugins (Dalibo ${DALIBO_PLUGIN_VERSION}, Opera ${OPERA_PLUGIN_VERSION})"
  for i in $(seq 1 12); do
    kubectl apply --server-side --force-conflicts -f "${dir}/dalibo.yaml" -f "${dir}/opera.yaml" && break
    if ((i == 12)); then
      log "the pgBackRest plugin manifests did not apply: cert-manager's webhook never answered"
      return 1
    fi
    sleep 10
  done
  kubectl -n cnpg-system rollout status deployment/pgbackrest-controller --timeout=300s
  kubectl -n cnpg-system rollout status deployment/pgbackrest --timeout=300s
}
