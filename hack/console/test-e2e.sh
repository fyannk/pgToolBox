#!/usr/bin/env bash
# End-to-end boot test of the console on kind: a real API server and the
# pinned CloudNativePG release, the console deployed with the shape
# charts/pgtools renders (a Role in the watched namespace, the
# revocation-list Role by name, the one ClusterRole for SubjectAccessReviews,
# a projected token, the chart's environment). It proves the binary boots and
# becomes ready against a real cluster, and that sign-in with a pasted token
# and the SubjectAccessReview filter behave: a user bound to read the Cluster
# sees it, a user with no binding sees nothing and gets 403; the embedded SPA
# answers / and its hashed assets pre-compressed. The full chart
# install, two consoles and the NetworkPolicy job are the chart e2e
# (hack/e2e-chart.sh).
#
# Artifacts land in artifacts/e2e/.
set -eu

# shellcheck source=hack/lib/net.sh
source hack/lib/net.sh

KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
CNPG_MANIFEST="${CNPG_MANIFEST:-$(cnpg_manifest_url "${CNPG_DEFAULT_VERSION}")}"
# Overridable so that two runs sharing one Docker daemon do not delete each
# other's cluster.
CLUSTER_NAME="${CLUSTER_NAME:-pgtoolbox-console-e2e}"
OUT="artifacts/e2e"
IMAGE="${IMAGE:-pgtoolbox-console:dev}"
NS=payments

# A job that runs on a machine with other clusters sets E2E_REQUIRE_KUBECONFIG=1: kind
# and kubectl then use a kubeconfig that belongs to the run, never the default
# ~/.kube/config that holds the owner's real contexts.
if [ "${E2E_REQUIRE_KUBECONFIG:-0}" = "1" ]; then
  case "${KUBECONFIG:-}" in
    "" | *:* | "$HOME/.kube/config")
      echo "[e2e] E2E_REQUIRE_KUBECONFIG=1 needs KUBECONFIG set to one private file, got '${KUBECONFIG:-}'" >&2
      exit 1
      ;;
  esac
fi

mkdir -p "$OUT"
work=$(mktemp -d)
log() { echo "[e2e] $*" | tee -a "$OUT/summary.log"; }

dump() {
  kubectl -n "$NS" get all,roles,rolebindings,configmaps > "$OUT/objects.txt" 2>&1 || true
  kubectl -n "$NS" logs deployment/pgtoolbox-console --tail=400 > "$OUT/console.log" 2>&1 || true
  kubectl -n "$NS" describe deployment/pgtoolbox-console > "$OUT/console-describe.txt" 2>&1 || true
  # The tail goes to the job log; the full files are uploaded as artifacts.
  tail -n 60 "$OUT/console.log" 2>/dev/null || true
}

cleanup() {
  status=$?
  [ "$status" = 0 ] || dump
  rm -rf "${work:?}"
  if [ "${KEEP_CLUSTER:-false}" != "true" ]; then
    kind delete cluster --name "$CLUSTER_NAME" > /dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

fail() {
  log "FAIL: $*"
  exit 1
}

: > "$OUT/summary.log"
log "tuple: $KIND_NODE_IMAGE + $CNPG_MANIFEST"

log "creating kind cluster"
kind delete cluster --name "$CLUSTER_NAME" > /dev/null 2>&1 || true
hack/kind-create.sh --name "$CLUSTER_NAME" --image "$KIND_NODE_IMAGE" --config hack/kind/ci.yaml --wait 120s > /dev/null

log "installing CloudNativePG"
apply_manifest "$CNPG_MANIFEST" --server-side > /dev/null
kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=300s > /dev/null

log "creating the target cluster"
kubectl create namespace "$NS" > /dev/null
cat <<YAML | kubectl apply -f - > /dev/null
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: orders
  namespace: $NS
spec:
  instances: 1
  storage:
    size: 1Gi
YAML

phase=""
i=0
while [ "$phase" != "Cluster in healthy state" ]; do
  i=$((i + 1))
  [ "$i" -le 120 ] || fail "cluster never became healthy (last phase: $phase)"
  sleep 5
  phase=$(kubectl -n "$NS" get cluster orders -o jsonpath='{.status.phase}' 2>/dev/null || true)
done
log "cluster healthy"

log "deploying the console"
kind load docker-image "$IMAGE" --name "$CLUSTER_NAME" > /dev/null
LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 48 > "$work/session-key"
kubectl -n "$NS" create secret generic pgtoolbox-console-session --from-file=session-key="$work/session-key" > /dev/null
cat <<YAML | kubectl apply -f - > /dev/null
apiVersion: v1
kind: ServiceAccount
metadata: {name: pgtoolbox-console, namespace: $NS}
automountServiceAccountToken: false
---
# The chart's Role in the watched namespace (charts/pgtools
# templates/console/rbac.yaml): read-only, no Secret verb.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: pgtoolbox-console, namespace: $NS}
rules:
  - apiGroups: [postgresql.cnpg.io]
    resources: [clusters, backups, scheduledbackups, poolers, databases, databaseroles, publications, subscriptions, failoverquorums, imagecatalogs]
    verbs: [get, list, watch]
  - apiGroups: [barmancloud.cnpg.io]
    resources: [objectstores]
    verbs: [get, list, watch]
  - apiGroups: [pgtoolbox.fyannk.dev]
    resources: [pgadmins, evidenceagents]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [pods, persistentvolumeclaims, services]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [events, resourcequotas]
    verbs: [list, watch]
  - apiGroups: [policy]
    resources: [poddisruptionbudgets]
    verbs: [get, list, watch]
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [get, list, watch]
  - apiGroups: [batch]
    resources: [jobs]
    verbs: [get, list, watch]
  - apiGroups: [apps]
    resources: [replicasets, deployments]
    verbs: [get]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: pgtoolbox-console, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: pgtoolbox-console}
subjects: [{kind: ServiceAccount, name: pgtoolbox-console, namespace: $NS}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: pgtoolbox-console-revocations, namespace: $NS}
data: {}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: pgtoolbox-console-revocations, namespace: $NS}
rules:
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [pgtoolbox-console-revocations]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: pgtoolbox-console-revocations, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: pgtoolbox-console-revocations}
subjects: [{kind: ServiceAccount, name: pgtoolbox-console, namespace: $NS}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: pgtoolbox-console, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app.kubernetes.io/name: pgtoolbox-console}}
  template:
    metadata: {labels: {app.kubernetes.io/name: pgtoolbox-console}}
    spec:
      serviceAccountName: pgtoolbox-console
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: console
          image: $IMAGE
          imagePullPolicy: Never
          env:
            - {name: POD_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}
            - {name: WATCH_NAMESPACES, value: $NS}
            - {name: LISTEN_ADDR, value: ":3000"}
            - {name: PROBE_LISTEN_ADDR, value: ":3001"}
            - {name: AUTH_PROVIDER, value: token}
            - {name: SESSION_KEY_FILE, value: /var/run/console/session/session-key}
            - {name: SESSION_IDLE_TIMEOUT, value: 8h}
            - {name: REVOCATION_CONFIGMAP, value: pgtoolbox-console-revocations}
            # The harness talks plain HTTP to a NodePort.
            - {name: COOKIE_SECURE, value: "false"}
          ports:
            - {name: http, containerPort: 3000}
            - {name: probes, containerPort: 3001}
          readinessProbe: {httpGet: {path: /readyz, port: probes}, periodSeconds: 5}
          livenessProbe: {httpGet: {path: /healthz, port: probes}, periodSeconds: 20}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
            - {name: kube-api-access, mountPath: /var/run/secrets/kubernetes.io/serviceaccount, readOnly: true}
            - {name: session, mountPath: /var/run/console/session, readOnly: true}
      volumes:
        - {name: tmp, emptyDir: {}}
        - name: kube-api-access
          projected:
            sources:
              - serviceAccountToken: {path: token, expirationSeconds: 3600}
              - configMap: {name: kube-root-ca.crt, items: [{key: ca.crt, path: ca.crt}]}
              - downwardAPI: {items: [{path: namespace, fieldRef: {fieldPath: metadata.namespace}}]}
        - name: session
          secret: {secretName: pgtoolbox-console-session}
---
apiVersion: v1
kind: Service
metadata: {name: pgtoolbox-console-e2e-access, namespace: $NS}
spec:
  type: NodePort
  selector: {app.kubernetes.io/name: pgtoolbox-console}
  ports:
    - {name: http, port: 3000, targetPort: http}
    - {name: probes, port: 3001, targetPort: probes}
---
# The users: one may read the Cluster (pgtoolbox-console-viewer's core), one may not;
# a scraper may get /metrics.
apiVersion: v1
kind: ServiceAccount
metadata: {name: dba, namespace: $NS}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: stranger, namespace: $NS}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: scraper, namespace: $NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: e2e-viewer, namespace: $NS}
rules:
  - apiGroups: [postgresql.cnpg.io]
    resources: [clusters]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: e2e-viewer, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: e2e-viewer}
subjects: [{kind: ServiceAccount, name: dba, namespace: $NS}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: $NS-pgtoolbox-console-metrics-reader}
rules:
  - nonResourceURLs: [/metrics]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: $NS-pgtoolbox-console-metrics-reader}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: $NS-pgtoolbox-console-metrics-reader}
subjects: [{kind: ServiceAccount, name: scraper, namespace: $NS}]
YAML

# Ready means the informer cache synced and the revocation list was read.
kubectl -n "$NS" rollout status deployment/pgtoolbox-console --timeout=180s > /dev/null || fail "the console never became ready"
log "console ready"

node_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CLUSTER_NAME}-control-plane")
http_port=$(kubectl -n "$NS" get svc pgtoolbox-console-e2e-access -o jsonpath='{.spec.ports[?(@.name=="http")].nodePort}')
probe_port=$(kubectl -n "$NS" get svc pgtoolbox-console-e2e-access -o jsonpath='{.spec.ports[?(@.name=="probes")].nodePort}')
base="http://${node_ip}:${http_port}"
probe="http://${node_ip}:${probe_port}"

# expect <want-status> <description> curl-args...: one request, its status
# checked, its body kept in $work/body.
expect() {
  want=$1
  what=$2
  shift 2
  # A connection-level error (no HTTP status) is retried for a while: the
  # NodePort is reachable only once kube-proxy has programmed the Service, a
  # few seconds after the Deployment reports Ready. An HTTP status is final.
  got="curl-error"
  for _ in $(seq 1 15); do
    if got=$(curl -s -o "$work/body" -w '%{http_code}' --max-time 20 "$@"); then break; fi
    got="curl-error"
    sleep 2
  done
  if [ "$got" != "$want" ]; then
    cat "$work/body" >> "$OUT/summary.log" 2>/dev/null || true
    fail "$what: HTTP $got, want $want"
  fi
  log "ok: $what ($got)"
}

contains() {
  grep -q -- "$1" "$work/body" || fail "$2: the body lacks $1"
}

lacks() {
  if grep -q -- "$1" "$work/body"; then fail "$2: the body contains $1"; fi
}

# sign_in <user>: a pasted token, the break-glass path, into a fresh jar.
sign_in() {
  token=$(kubectl -n "$NS" create token "$1" --duration=1h)
  : > "$work/$1.jar"
  expect 302 "sign-in as $1" -c "$work/$1.jar" -H "Origin: $base" \
    --data-urlencode "token=$token" --data-urlencode "rd=/" "$base/auth/token"
}

expect 200 "readiness" "$probe/readyz"
expect 401 "API without a session" "$base/api/v1/clusters"
contains loginURL "API without a session"
expect 200 "sign-in page" "$base/auth/login"

sign_in dba
# The SPA is embedded: / and any client route answer
# index.html with the build's module script, never the "not embedded" page;
# its hashed assets are public, immutable and served pre-compressed.
expect 200 "SPA index for dba" -b "$work/dba.jar" "$base/"
contains '<div id="root">' "SPA index for dba"
lacks 'SPA not embedded' "SPA index for dba"
script=$(sed -n 's|.*<script type="module" crossorigin src="\(/assets/[^"]*\.js\)".*|\1|p' "$work/body" | head -n 1)
[ -n "$script" ] || fail "SPA index for dba: no hashed module script in index.html"
expect 200 "SPA client route for dba" -b "$work/dba.jar" "$base/c/$NS/orders/instances"
contains "$script" "SPA client route for dba"
curl -s -o /dev/null -D "$work/asset.headers" -H 'Accept-Encoding: br' --max-time 20 "$base$script" ||
  fail "SPA asset $script: request failed"
tr -d '\r' < "$work/asset.headers" > "$work/asset.h"
grep -q '^HTTP/[0-9.]* 200' "$work/asset.h" || fail "SPA asset $script: $(head -n 1 "$work/asset.h")"
grep -qi '^content-type: text/javascript' "$work/asset.h" || fail "SPA asset $script: wrong Content-Type"
grep -qi '^content-encoding: br$' "$work/asset.h" || fail "SPA asset $script: not served from its .br sibling"
grep -qi '^cache-control: public, max-age=31536000, immutable$' "$work/asset.h" || fail "SPA asset $script: not immutable"
log "ok: SPA asset $script (200, text/javascript, br, immutable, no session)"
expect 404 "missing SPA asset never falls back to index.html" "$base/assets/index-missing.js"
expect 200 "session of dba" -b "$work/dba.jar" "$base/api/v1/session"
contains "system:serviceaccount:$NS:dba" "session of dba"
expect 200 "cluster list of dba" -b "$work/dba.jar" "$base/api/v1/clusters"
contains '"name":"orders"' "cluster list of dba"
expect 200 "overview of orders for dba" -b "$work/dba.jar" "$base/api/v1/namespaces/$NS/clusters/orders"
contains '"verdict"' "overview of orders for dba"
contains '"capabilities":{"view":true' "overview of orders for dba"
expect 200 "instances of orders for dba" -b "$work/dba.jar" "$base/api/v1/namespaces/$NS/clusters/orders/instances"
expect 403 "unsafe API call without the CSRF header" -b "$work/dba.jar" -X POST -H "Origin: $base" "$base/api/v1/session"
# The parity screens answer for a viewer.
expect 200 "vocabulary" -b "$work/dba.jar" "$base/api/v1/vocabulary"
contains '"healthOrder"' "vocabulary"
for view in replication backups storage objects history metrics topology operations; do
  expect 200 "$view of orders for dba" -b "$work/dba.jar" "$base/api/v1/namespaces/$NS/clusters/orders/$view"
done
contains '"allowed":false' "operations of orders for a viewer"
expect 200 "admin page" -b "$work/dba.jar" "$base/api/v1/admin"
# Writes go to the API server as the caller: a viewer's request is
# refused there (token passthrough), a mistyped confirmation never leaves.
expect 422 "operation with a mistyped confirmation" -b "$work/dba.jar" -X POST -H "Origin: $base" -H "X-PgToolBox-Request: 1" \
  -H "Content-Type: application/json" --data '{"kind":"reload","confirmation":"order"}' "$base/api/v1/namespaces/$NS/clusters/orders/operations"
expect 403 "reload by a viewer (refused by the API server as the viewer)" -b "$work/dba.jar" -X POST -H "Origin: $base" -H "X-PgToolBox-Request: 1" \
  -H "Content-Type: application/json" --data '{"kind":"reload","confirmation":"orders"}' "$base/api/v1/namespaces/$NS/clusters/orders/operations"

sign_in stranger
expect 200 "cluster list of stranger" -b "$work/stranger.jar" "$base/api/v1/clusters"
lacks '"name":"orders"' "cluster list of stranger"
expect 403 "overview of orders for stranger" -b "$work/stranger.jar" "$base/api/v1/namespaces/$NS/clusters/orders"
expect 403 "absent cluster for stranger (no existence oracle)" -b "$work/stranger.jar" "$base/api/v1/namespaces/$NS/clusters/ghost"
expect 403 "backups of orders for stranger" -b "$work/stranger.jar" "$base/api/v1/namespaces/$NS/clusters/orders/backups"
expect 403 "operation by stranger" -b "$work/stranger.jar" -X POST -H "Origin: $base" -H "X-PgToolBox-Request: 1" \
  -H "Content-Type: application/json" --data '{"kind":"backup","confirmation":"orders"}' "$base/api/v1/namespaces/$NS/clusters/orders/operations"

expect 401 "/metrics without a token" "$probe/metrics"
expect 403 "/metrics as dba" -H "Authorization: Bearer $(kubectl -n "$NS" create token dba --duration=10m)" "$probe/metrics"
expect 200 "/metrics as the scraper" -H "Authorization: Bearer $(kubectl -n "$NS" create token scraper --duration=10m)" "$probe/metrics"
contains pgtoolbox_console_sar_reviews_total "/metrics as the scraper"
contains pgtoolbox_console_cached_objects "/metrics as the scraper"

expect 200 "sign-out" -b "$work/dba.jar" -c "$work/dba.jar" -X POST -H "Origin: $base" "$base/logout"
expect 401 "API after sign-out" -b "$work/dba.jar" "$base/api/v1/clusters"

log "console e2e passed"
