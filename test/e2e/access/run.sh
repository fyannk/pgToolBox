#!/bin/sh
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# The access-request scenario on kind: the operator and the
# console built from the working tree, the PgAccessRequest CRD and the two
# console ClusterRoles from config/, people signed in with pasted
# ServiceAccount tokens. It proves on a real API server what envtest cannot:
# the console writes as the person (a viewer asks, cannot decide; an operator
# approves and the RoleBinding really grants), four eyes, the operator
# deleting the grant at expiry and on revocation, and the garbage collector
# deleting it with its request.
#
#   CLUSTER_NAME=ptb-e2e-access-local ./test/e2e/access/run.sh
#   KEEP_CLUSTER=true leaves the cluster up. Artifacts land in artifacts/e2e-access/.
#
# CloudNativePG itself is not installed: only its CRDs (vendored under
# test/crds), which the console requires to start; access requests name no
# running Cluster.
set -eu

repo_root="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$repo_root"

KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
CLUSTER_NAME="${CLUSTER_NAME:-ptb-e2e-access-local}"
MANAGER_IMG="${MANAGER_IMG:-pgtoolbox-operator:e2e-access}"
CONSOLE_IMG="${CONSOLE_IMG:-pgtoolbox-console:e2e-access}"
KIND_IMAGE_PLATFORM="${KIND_IMAGE_PLATFORM:-linux/amd64}"
OUT="artifacts/e2e-access"
NS=access-e2e

if [ "${E2E_REQUIRE_KUBECONFIG:-0}" = "1" ]; then
  case "${KUBECONFIG:-}" in
    "" | *:* | "$HOME/.kube/config")
      echo "[e2e] E2E_REQUIRE_KUBECONFIG=1 needs KUBECONFIG set to one private file, got '${KUBECONFIG:-}'" >&2
      exit 1
      ;;
  esac
fi
for tool in kind kubectl docker curl; do
  command -v "$tool" > /dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

mkdir -p "$OUT"
work=$(mktemp -d)
log() { echo "[e2e] $*" | tee -a "$OUT/summary.log"; }

dump() {
  kubectl -n "$NS" get pgaccessrequests -o yaml > "$OUT/pgaccessrequests.yaml" 2>&1 || true
  kubectl -n "$NS" get rolebindings -o yaml > "$OUT/rolebindings.yaml" 2>&1 || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp > "$OUT/events.txt" 2>&1 || true
  kubectl -n "$NS" logs deployment/pgtoolbox-console --tail=400 > "$OUT/console.log" 2>&1 || true
  kubectl -n pgtoolbox-access logs deployment/pgtoolbox-access --tail=400 > "$OUT/operator.log" 2>&1 || true
  tail -n 40 "$OUT/operator.log" "$OUT/console.log" 2>/dev/null || true
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

# One platform only: see kind_load in hack/e2e-chart.sh.
kind_load() {
  archive="$work/image.tar"
  docker save --platform "$KIND_IMAGE_PLATFORM" "$1" > "$archive"
  kind load image-archive "$archive" --name "$CLUSTER_NAME" > /dev/null
  rm -f "$archive"
}

: > "$OUT/summary.log"
log "building the operator and console images"
make docker-build OPERATOR_IMG="$MANAGER_IMG" VERSION=e2e > "$OUT/build-operator.log" 2>&1 || fail "operator image build (see $OUT/build-operator.log)"
make docker-build-console CONSOLE_IMG="$CONSOLE_IMG" VERSION=e2e > "$OUT/build-console.log" 2>&1 || fail "console image build (see $OUT/build-console.log)"

log "creating kind cluster $CLUSTER_NAME ($KIND_NODE_IMAGE)"
kind delete cluster --name "$CLUSTER_NAME" > /dev/null 2>&1 || true
hack/kind-create.sh --name "$CLUSTER_NAME" --image "$KIND_NODE_IMAGE" --config hack/kind/ci.yaml --wait 180s > /dev/null
kind_load "$MANAGER_IMG"
kind_load "$CONSOLE_IMG"

log "installing the CRDs, the console ClusterRoles and the operator"
kubectl apply --server-side -f test/crds/cnpg-1.30/ > /dev/null
kubectl apply --server-side -f config/crd/bases/ > /dev/null
kubectl apply --server-side -f config/rbac/role.yaml -f config/rbac/pgtoolbox_console_viewer_clusterrole.yaml \
  -f config/rbac/pgtoolbox_console_operator_clusterrole.yaml > /dev/null
kubectl create namespace pgtoolbox-access > /dev/null
kubectl create namespace "$NS" > /dev/null
sed -e "s|@MANAGER_IMG@|$MANAGER_IMG|g" -e "s|@NS@|$NS|g" test/e2e/access/testdata/operator.yaml | kubectl apply --server-side -f - > /dev/null

log "deploying the console"
LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 48 > "$work/session-key"
kubectl -n "$NS" create secret generic pgtoolbox-console-session --from-file=session-key="$work/session-key" > /dev/null
cat <<YAML | kubectl apply -f - > /dev/null
apiVersion: v1
kind: ServiceAccount
metadata: {name: pgtoolbox-console, namespace: $NS}
automountServiceAccountToken: false
---
# The chart's console Role (charts/pgtools templates/console/rbac.yaml) with
# the read of PgAccessRequests the access-request view needs: still read-only.
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
    resources: [pgadmins, evidenceagents, pgaccessrequests]
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
          image: $CONSOLE_IMG
          imagePullPolicy: Never
          env:
            - {name: POD_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}
            - {name: WATCH_NAMESPACES, value: $NS}
            - {name: LISTEN_ADDR, value: ":3000"}
            - {name: PROBE_LISTEN_ADDR, value: ":3001"}
            - {name: AUTH_PROVIDER, value: token}
            - {name: SESSION_KEY_FILE, value: /var/run/console/session/session-key}
            - {name: REVOCATION_CONFIGMAP, value: pgtoolbox-console-revocations}
            - {name: COOKIE_SECURE, value: "false"}
          ports:
            - {name: http, containerPort: 3000}
            - {name: probes, containerPort: 3001}
          readinessProbe: {httpGet: {path: /readyz, port: probes}, periodSeconds: 5}
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
---
# The people: victor views (and may ask), alice and bob operate (and decide),
# nobody holds nothing.
apiVersion: v1
kind: ServiceAccount
metadata: {name: victor, namespace: $NS}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: alice, namespace: $NS}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: nobody, namespace: $NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: viewer-victor, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: pgtoolbox-console-viewer}
subjects: [{kind: ServiceAccount, name: victor, namespace: $NS}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: operator-alice, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: pgtoolbox-console-operator}
subjects: [{kind: ServiceAccount, name: alice, namespace: $NS}]
YAML

kubectl -n pgtoolbox-access rollout status deployment/pgtoolbox-access --timeout=300s > /dev/null || fail "the operator never became ready"
kubectl -n "$NS" rollout status deployment/pgtoolbox-console --timeout=300s > /dev/null || fail "the console never became ready"
log "operator and console ready"

node_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CLUSTER_NAME}-control-plane")
http_port=$(kubectl -n "$NS" get svc pgtoolbox-console-e2e-access -o jsonpath='{.spec.ports[?(@.name=="http")].nodePort}')
base="http://${node_ip}:${http_port}"

# expect <want-status> <description> curl-args...: one request, its status
# checked, its body kept in $work/body.
expect() {
  want=$1
  what=$2
  shift 2
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

sign_in() {
  token=$(kubectl -n "$NS" create token "$1" --duration=1h)
  : > "$work/$1.jar"
  expect 302 "sign-in as $1" -c "$work/$1.jar" -H "Origin: $base" \
    --data-urlencode "token=$token" --data-urlencode "rd=/" "$base/auth/token"
}

# api <want> <what> <user> <method> <path> [json]: one API call as <user>.
api() {
  want=$1 what=$2 user=$3 method=$4 path=$5
  if [ "$#" -ge 6 ]; then
    expect "$want" "$what" -b "$work/$user.jar" -X "$method" -H "Origin: $base" -H "X-PgToolBox-Request: 1" \
      -H "Content-Type: application/json" --data "$6" "$base$path"
  else
    expect "$want" "$what" -b "$work/$user.jar" -X "$method" "$base$path"
  fi
}

# name_of: the "name" of the request in the last body.
name_of() { sed -n 's/^{"namespace":"[^"]*","name":"\([^"]*\)".*/\1/p' "$work/body"; }

# wait_for <what> <timeout-seconds> <command...>: poll until the command succeeds.
wait_for() {
  what=$1 limit=$2
  shift 2
  i=0
  until "$@" > /dev/null 2>&1; do
    i=$((i + 2))
    [ "$i" -le "$limit" ] || fail "$what: not after ${limit}s"
    sleep 2
  done
  log "ok: $what"
}

phase_is() { [ "$(kubectl -n "$NS" get pgaccessrequest "$1" -o jsonpath='{.status.phase}')" = "$2" ]; }
synced_is() {
  [ "$(kubectl -n "$NS" get pgaccessrequest "$1" -o jsonpath='{.status.conditions[?(@.type=="RoleBindingSynced")].reason}')" = "$2" ]
}
no_binding() { ! kubectl -n "$NS" get rolebinding "pgaccess-$1" > /dev/null 2>&1; }
can_patch() { [ "$(kubectl auth can-i patch clusters.postgresql.cnpg.io -n "$NS" --as="system:serviceaccount:$NS:$1")" = "yes" ]; }
cannot_patch() { ! can_patch "$1"; }

base_path="/api/v1/namespaces/$NS/access-requests"
expires=$(date -u -d '+100 seconds' '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -v+100S '+%Y-%m-%dT%H:%M:%SZ')

sign_in victor
sign_in alice
sign_in nobody

api 200 "list as victor" victor GET /api/v1/access-requests
contains '"canRequest":true' "list as victor"
contains '"canReview":false' "list as victor"
api 403 "a request by nobody" nobody POST "$base_path" '{"capability":"viewer","justification":"let me in"}'
api 422 "a request for system:authenticated" victor POST "$base_path" \
  '{"capability":"viewer","justification":"x","subject":{"kind":"Group","name":"system:authenticated"}}'

api 201 "victor asks for operator until $expires" victor POST "$base_path" \
  "{\"capability\":\"operator\",\"justification\":\"INC-1: restart a standby\",\"expiresAt\":\"$expires\"}"
contains "\"name\":\"victor\"" "the subject defaults to the caller"
first=$(name_of)
[ -n "$first" ] || fail "no request name in the create answer"
wait_for "the operator marks $first Pending" 60 phase_is "$first" Pending
cannot_patch victor || fail "victor may patch Clusters before any approval"

api 403 "victor approves (no create rolebindings)" victor POST "$base_path/$first/approve" '{}'
api 200 "alice lists" alice GET "/api/v1/access-requests?namespace=$NS"
contains "\"name\":\"$first\"" "alice lists"
api 200 "alice approves $first" alice POST "$base_path/$first/approve" '{"note":"for INC-1"}'
contains "\"decidedBy\":\"system:serviceaccount:$NS:alice\"" "the approver is recorded"
contains '"phase":"Approved"' "approve answer"
wait_for "RoleBinding pgaccess-$first grants victor" 30 can_patch victor
wait_for "the operator reports $first granted" 60 synced_is "$first" AccessGranted

api 201 "alice asks for herself" alice POST "$base_path" '{"capability":"viewer","justification":"mine"}'
own=$(name_of)
api 403 "alice approves her own request" alice POST "$base_path/$own/approve" '{}'
api 422 "alice denies without a note" alice POST "$base_path/$own/deny" '{}'
api 200 "alice withdraws her own request" alice POST "$base_path/$own/deny" '{"note":"withdrawn"}'

log "waiting for $first to expire at $expires"
wait_for "the operator expires $first" 240 phase_is "$first" Expired
wait_for "the grant of $first is gone" 30 no_binding "$first"
cannot_patch victor || fail "victor may still patch Clusters after expiry"
kubectl -n "$NS" get events --field-selector "involvedObject.name=$first" -o jsonpath='{.items[*].reason}' | grep -q RoleBindingDeleted ||
  fail "no RoleBindingDeleted event on $first"
log "ok: RoleBindingDeleted event on $first"

api 201 "victor asks for viewer, no expiry" victor POST "$base_path" '{"capability":"viewer","justification":"dashboards"}'
second=$(name_of)
api 200 "alice approves $second" alice POST "$base_path/$second/approve" '{}'
wait_for "the operator reports $second granted" 60 synced_is "$second" AccessGranted
api 409 "alice approves $second twice" alice POST "$base_path/$second/approve" '{}'
api 403 "victor revokes (no update status)" victor POST "$base_path/$second/revoke" '{}'
api 200 "alice revokes $second" alice POST "$base_path/$second/revoke" '{"note":"incident closed"}'
wait_for "the operator deletes the grant of $second" 60 no_binding "$second"
wait_for "the operator reports $second revoked" 60 synced_is "$second" AccessRevoked

api 201 "victor asks again" victor POST "$base_path" '{"capability":"viewer","justification":"again"}'
third=$(name_of)
api 200 "alice approves $third" alice POST "$base_path/$third/approve" '{}'
kubectl -n "$NS" get rolebinding "pgaccess-$third" > /dev/null || fail "no grant for $third"
kubectl -n "$NS" delete pgaccessrequest "$third" --wait=true > /dev/null
wait_for "the garbage collector deletes the grant of a deleted request" 60 no_binding "$third"

api 200 "victor's own view" victor GET /api/v1/access-requests
contains '"mine":true' "victor's own view"

log "access-request e2e passed"
