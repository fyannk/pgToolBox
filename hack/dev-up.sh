#!/bin/sh
# Copyright © contributors to the pgtoolbox project.
# SPDX-License-Identifier: Apache-2.0
#
# Stand up a browsable pgToolBox against a real kind + CloudNativePG
# cluster: the operator, one PgConsole with pgAdmin and the evidence
# sidecar, and three seeded users — one per authorization level — then
# expose the console's own proxy on a single local port.
#
#     http://localhost:3000     the real pgtoolbox-proxy login page
#
#       viewer    / viewer      sees the overviews and the metrics screens
#       operator  / operator    + every other read screen and the log tails
#       dba       / dba         + the four day-2 operations, the access-request
#                               review panel, and the pgAdmin link-out
#       stranger  / stranger    unknown to the console: the 403 page with the
#                               request-access form, which files a real
#                               PgToolBoxAccessRequest for the dba to review
#
# This is deliberately NOT a copy of pgConsole's dev-up.sh. That script has
# to fake the trusted proxy — it runs three tiny header-injecting proxies on
# three ports, because pgConsole has no authentication of its own and would
# otherwise be unreachable above the denial page. pgToolBox *is* that proxy.
# Faking it here would test nothing and hide the part this repository owns,
# so there is one port, the real login, and real sessions; the level ladder
# comes from the seeded PgToolBoxUser objects, and the unknown
# user exercises the access-request flow end to end rather than a mock of it.
#
# First run does the full setup (~6 min). While the kind cluster is still
# up, re-running relaunches the forward fast. RECREATE=true forces a clean
# rebuild.
#
# The dev cluster is deliberately not the minimum one: it archives to a
# throwaway MinIO through the barman-cloud plugin, because the console's
# evidence screens have nothing to read against a bare cluster.
# SKIP_BACKUP=true drops the object store and the plugin (and with them the
# evidence sidecar); SKIP_EVIDENCE=true keeps the store but leaves the
# sidecar out.
#
# AUTH_MODE adds a real identity provider beside the seeded local accounts:
#
#   AUTH_MODE=local,oidc \
#   OIDC_ISSUER_URL=https://idp.example \
#   OIDC_CLIENT_ID=pgtoolboxdev \
#   OIDC_CLIENT_SECRET=... \
#   OIDC_SUBJECTS="me@example=dba" ./hack/dev-up.sh
#
# The login page then shows the local form with an SSO button beside it.
# AUTH_MODE=oidc alone drops the local form. The secret is read from the
# environment into a Kubernetes Secret and is never written to a file here.
#
# Register this redirect URI with the provider first:
#
#     http://localhost:$PORT/auth/oidc/callback
#
# with $PORT being whatever this script forwards (3000 by default). The
# proxy builds the redirect URI from the origin the request arrived on, so
# the port is yours to pick — it just has to match what you registered.
#
# Environment overrides: CLUSTER, KIND_NODE_IMAGE, CNPG_MANIFEST,
# CERT_MANAGER_MANIFEST, BARMAN_MANIFEST, PGCONSOLE_IMAGE, PGADMIN_IMAGE,
# VIEWER_IMAGE, MANAGER_IMG, PROXY_IMG, PORT, SKIP_BUILD=true,
# SKIP_BACKUP=true, SKIP_EVIDENCE=true, RECREATE=true, NO_FORWARD=true.
#
# Ctrl-C stops the forward; the cluster stays up.
# Tear down with:  kind delete cluster --name "$CLUSTER"
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

CLUSTER="${CLUSTER:-pgtoolbox-dev}"
CONTEXT="kind-$CLUSTER"
NAMESPACE="${NAMESPACE:-payments}"
CONSOLE="${CONSOLE:-orders}"
PGCLUSTER="${PGCLUSTER:-pg-orders}"
PORT="${PORT:-3000}"
# pgAdmin keys accounts on the email address, so subjects are email-shaped.
SUBJECT_DOMAIN="${SUBJECT_DOMAIN:-pgtoolbox.dev}"
# The console's first administrator, declared on the PgConsole rather than
# applied as an object: the operator materializes it and puts it back if it
# is deleted, so the dev stack always has somebody who can approve the first
# access request.
BOOTSTRAP_SUBJECT="${BOOTSTRAP_SUBJECT:-dba@$SUBJECT_DOMAIN}"

# Authentication. "local" needs no identity provider and seeds three users
# with passwords; "oidc" points the proxy at a real one.
#
# OIDC needs OIDC_ISSUER_URL, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET, and at
# least one OIDC_SUBJECTS entry — a space-separated list of "subject=level"
# — because a level is granted per identity and there is no group mapping.
# Without one, every sign-in lands on the 403 page with nothing to approve
# it. The secret is read from the environment and written straight into a
# Kubernetes Secret; it is never echoed and never written to a file here.
#
# The provider must accept http://localhost:$PORT/auth/oidc/callback.
AUTH_MODE="${AUTH_MODE:-local}"
OIDC_ISSUER_URL="${OIDC_ISSUER_URL:-}"
OIDC_CLIENT_ID="${OIDC_CLIENT_ID:-}"
OIDC_CLIENT_SECRET="${OIDC_CLIENT_SECRET:-}"
OIDC_SUBJECTS="${OIDC_SUBJECTS:-}"

case ",$AUTH_MODE," in
  *,local,*) AUTH_LOCAL=true ;;
  *) AUTH_LOCAL=false ;;
esac
case ",$AUTH_MODE," in
  *,oidc,*) AUTH_OIDC=true ;;
  *) AUTH_OIDC=false ;;
esac
if [ "$AUTH_LOCAL" = false ] && [ "$AUTH_OIDC" = false ]; then
  echo "AUTH_MODE must name local, oidc, or both (got: $AUTH_MODE)" >&2
  exit 1
fi

KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
CNPG_MANIFEST="${CNPG_MANIFEST:-https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.30/releases/cnpg-1.30.0.yaml}"
# The barman-cloud plugin is what gives the dev cluster a real object store,
# and it needs cert-manager for the mTLS between operator and plugin.
CERT_MANAGER_MANIFEST="${CERT_MANAGER_MANIFEST:-https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml}"
BARMAN_MANIFEST="${BARMAN_MANIFEST:-https://github.com/cloudnative-pg/plugin-barman-cloud/releases/latest/download/manifest.yaml}"

# The family images. Every application in the console pod is one of ours.
PGCONSOLE_IMAGE="${PGCONSOLE_IMAGE:-ghcr.io/fyannk/pgconsole:0.3.0}"
PGADMIN_IMAGE="${PGADMIN_IMAGE:-ghcr.io/fyannk/pgadmin:latest}"
VIEWER_IMAGE="${VIEWER_IMAGE:-ghcr.io/fyannk/pgobjectstoreviewer:0.1.1}"

# Built from this working tree.
MANAGER_IMG="${MANAGER_IMG:-pgtoolbox:dev}"
PROXY_IMG="${PROXY_IMG:-pgtoolbox-proxy:dev}"

log() { echo "[dev-up] $*"; }
need() { command -v "$1" > /dev/null 2>&1 || { echo "dev-up needs '$1' on PATH" >&2; exit 1; }; }
need kind
need kubectl
need docker
need go
need helm

kc() { kubectl --context "$CONTEXT" "$@"; }

# deps_installed reports whether the third-party stack is already in the
# cluster. Only that half is skippable on a re-run: everything this repo
# owns is rebuilt and re-applied every time, because skipping it is how a
# re-run silently serves the previous build.
deps_installed() {
  kind get clusters 2> /dev/null | grep -qx "$CLUSTER" || return 1
  kc -n cnpg-system get deployment/cnpg-controller-manager > /dev/null 2>&1
}

if [ "${RECREATE:-}" = "true" ]; then
  log "RECREATE=true — deleting cluster $CLUSTER"
  kind delete cluster --name "$CLUSTER" > /dev/null 2>&1 || true
fi

if ! deps_installed; then
  if ! kind get clusters 2> /dev/null | grep -qx "$CLUSTER"; then
    log "creating kind cluster $CLUSTER ($KIND_NODE_IMAGE)"
    # Generous: on a loaded machine the control plane can take well over two
    # minutes, and kind reports that as a hard creation failure.
    kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" --wait 300s
  fi

  log "installing CloudNativePG"
  kc apply --server-side -f "$CNPG_MANIFEST" > /dev/null
  kc -n cnpg-system wait --for=condition=Available deployment/cnpg-controller-manager --timeout=300s

  if [ "${SKIP_BACKUP:-}" != "true" ]; then
    log "installing cert-manager"
    kc apply --server-side -f "$CERT_MANAGER_MANIFEST" > /dev/null
    for deployment in cert-manager cert-manager-webhook cert-manager-cainjector; do
      kc -n cert-manager rollout status "deployment/$deployment" --timeout=300s > /dev/null
    done

    log "installing the barman-cloud plugin"
    kc apply --server-side -f "$BARMAN_MANIFEST" > /dev/null
    kc -n cnpg-system rollout status deployment/barman-cloud --timeout=300s > /dev/null

    log "deploying the object store the cluster archives into"
    kc apply --server-side -f test/e2e/testdata/minio.yaml > /dev/null
    kc -n minio rollout status deployment/minio --timeout=300s > /dev/null
  fi
fi

if [ "${SKIP_BUILD:-}" != "true" ]; then
  log "building the operator and proxy images"
  make docker-build docker-build-proxy IMG="$MANAGER_IMG" PROXY_IMG="$PROXY_IMG" VERSION=dev
fi

log "loading images into the cluster"
kind load docker-image "$MANAGER_IMG" "$PROXY_IMG" --name "$CLUSTER"
# Always pull, never "pull only if absent". A tag is a moving pointer, so
# a local copy of it can be older than the tag now resolves to, and
# side-loading that silently runs a stale component with no sign of it.
# Pulling a tag that is already current costs one HEAD request.
for image in "$PGCONSOLE_IMAGE" "$PGADMIN_IMAGE" "$VIEWER_IMAGE"; do
  docker pull --quiet "$image"
  kind load docker-image "$image" --name "$CLUSTER"
done

# Helm installs CRDs on first install and never touches them again, so on
# a re-run the API server would still validate against the previous shape
# of the API this build was just compiled from.
log "applying the CRDs"
kc apply --server-side --force-conflicts -f deploy/helm/pgtoolbox/crds > /dev/null

log "installing the operator (Helm chart)"
helm --kube-context "$CONTEXT" upgrade --install pgtoolbox deploy/helm/pgtoolbox \
  --namespace pgtoolbox --create-namespace \
  --set image.repository="${MANAGER_IMG%%:*}" \
  --set image.tag="${MANAGER_IMG##*:}" \
  --set image.pullPolicy=Never \
  --set proxyImage="$PROXY_IMG" \
  --set defaultImages.pgConsole="$PGCONSOLE_IMAGE" \
  --set defaultImages.pgAdmin="$PGADMIN_IMAGE" \
  --set defaultImages.objectStoreViewer="$VIEWER_IMAGE" \
  --set replicaCount=1 \
  --wait --timeout 300s > /dev/null

# The image tag does not change between runs, so nothing in the Deployment
# differs and Helm leaves the old Pod running the previous build.
kc -n pgtoolbox rollout restart deployment/pgtoolbox > /dev/null
kc -n pgtoolbox rollout status deployment/pgtoolbox --timeout=300s > /dev/null

log "seeding namespace $NAMESPACE"
kc create namespace "$NAMESPACE" --dry-run=client -o yaml | kc apply --server-side --force-conflicts -f - > /dev/null

if [ "${SKIP_BACKUP:-}" != "true" ]; then
  log "declaring the object store"
  kc apply --server-side --force-conflicts -f - > /dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: store-credentials
  namespace: $NAMESPACE
stringData:
  ACCESS_KEY_ID: e2eaccesskey
  ACCESS_SECRET_KEY: e2esecretkey
---
apiVersion: barmancloud.cnpg.io/v1
kind: ObjectStore
metadata:
  name: backups
  namespace: $NAMESPACE
spec:
  configuration:
    destinationPath: s3://e2e-backups/$PGCLUSTER
    endpointURL: http://minio.minio.svc:9000
    s3Credentials:
      accessKeyId: { name: store-credentials, key: ACCESS_KEY_ID }
      secretAccessKey: { name: store-credentials, key: ACCESS_SECRET_KEY }
EOF
fi

log "declaring the CloudNativePG cluster"
if [ "${SKIP_BACKUP:-}" != "true" ]; then
  plugins="  plugins:
    - name: barman-cloud.cloudnative-pg.io
      enabled: true
      parameters:
        barmanObjectName: backups"
else
  plugins=""
fi
kc apply --server-side --force-conflicts -f - > /dev/null <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: $PGCLUSTER
  namespace: $NAMESPACE
spec:
  instances: 1
  storage:
    size: 512Mi
$plugins
EOF

# The evidence sidecar needs the store; without one there is nothing to
# read and the console reports the repository panel as unavailable.
evidence_enabled=true
if [ "${SKIP_BACKUP:-}" = "true" ] || [ "${SKIP_EVIDENCE:-}" = "true" ]; then
  evidence_enabled=false
fi

auth_block=""
bootstrap_password=""
if [ "$AUTH_LOCAL" = true ]; then
  auth_block="      local: {}"
  bootstrap_password="        passwordSecretRef: { name: dba-password }"
  # Before the console: the operator resolves this Secret the moment the
  # bootstrapAdmin field names it.
  log "seeding the bootstrap administrator's password"
  kc apply --server-side --force-conflicts -f - > /dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: dba-password
  namespace: $NAMESPACE
stringData:
  password: "$(go run ./hack/devtools/bcrypt dba)"
EOF
fi
if [ "$AUTH_OIDC" = true ]; then
  for required in OIDC_ISSUER_URL OIDC_CLIENT_ID; do
    eval "value=\$$required"
    [ -n "$value" ] || { echo "AUTH_MODE with oidc needs $required" >&2; exit 1; }
  done
  # The secret is only stored when one is supplied. A re-run without it
  # keeps what is already in the cluster, so repeating the command from
  # shell history cannot overwrite a working credential with a blank or a
  # placeholder — and asks for the real one only when there is none.
  if [ -n "$OIDC_CLIENT_SECRET" ]; then
    log "storing the OIDC client secret"
    kc -n "$NAMESPACE" create secret generic pgconsole-oidc \
      --from-literal=clientSecret="$OIDC_CLIENT_SECRET" \
      --dry-run=client -o yaml | kc apply --server-side --force-conflicts -f - > /dev/null
  elif kc -n "$NAMESPACE" get secret pgconsole-oidc > /dev/null 2>&1; then
    log "keeping the OIDC client secret already in the cluster"
  else
    echo "AUTH_MODE with oidc needs OIDC_CLIENT_SECRET the first time" >&2
    exit 1
  fi
  auth_block="${auth_block:+$auth_block
}      oidc:
        issuerURL: $OIDC_ISSUER_URL
        clientID: $OIDC_CLIENT_ID
        clientSecretRef: { name: pgconsole-oidc }"
fi

log "declaring the console"
kc apply --server-side --force-conflicts -f - > /dev/null <<EOF
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgConsole
metadata:
  name: $CONSOLE
  namespace: $NAMESPACE
spec:
  cnpgClusterRef: { name: $PGCLUSTER }
  proxy:
    authentication:
      bootstrapAdmin:
        subject: $BOOTSTRAP_SUBJECT
$bootstrap_password
$auth_block
  pgAdmin:
    enabled: true
  evidence:
    enabled: $evidence_enabled
  console:
    allowOperations: true
    allowLogs: true
    allowAccessReview: true
    # No allowInsecureLinks, and so no pgAdmin link-out in the UI. A
    # clusterIP console has no external hostname, so the only absolute URL
    # the operator could build is the proxy's own loopback one — which is
    # not the address a port-forward puts the browser on. pgAdmin is still
    # reachable at /pgadmin; only the rendered link is missing.
  # kind defaults no fsGroup, and both the pgAdmin settings volume and the
  # evidence socket directory need one. OpenShift allocates it from the
  # namespace range instead, which is why this is not a default.
  podSecurityContext:
    fsGroup: 5050
EOF

# pgAdmin identifies accounts by email address, so a subject that is not
# one cannot be provisioned there. Real identities from an OIDC provider
# are email addresses; the seeded ones are too, so the pgAdmin half of the
# stack works rather than half-failing.
if [ "$AUTH_OIDC" = true ]; then
  # No passwords: the identity provider holds those. A user here only
  # says what level an already-authenticated identity gets, and a level
  # has to be granted per identity because no group mapping exists.
  [ -n "$OIDC_SUBJECTS" ] || log "warning: OIDC_SUBJECTS is empty, so every sign-in will land on the 403 page"
  for entry in $OIDC_SUBJECTS; do
    subject=${entry%%=*}
    level=${entry##*=}
    name=$(printf '%s' "$subject" | tr -c 'a-z0-9' '-' | sed 's/^-*//;s/-*$//' | cut -c1-40)
    log "granting $level to $subject"
    kc apply --server-side --force-conflicts -f - > /dev/null <<EOF
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxUser
metadata:
  name: $name
  namespace: $NAMESPACE
spec:
  pgConsoleRef: { name: $CONSOLE }
  subject: $subject
  level: $level
EOF
  done
fi

if [ "$AUTH_LOCAL" = true ]; then
  log "seeding one user per level"
  # No dba here: it is the console's bootstrapAdmin, materialized by the
  # operator. Seeding one would only be a duplicate subject the operator
  # drops. Its password Secret is still created below.
  for entry in "viewer:view" "operator:poweruser"; do
    name=${entry%%:*}
    level=${entry##*:}
    subject="$name@$SUBJECT_DOMAIN"
    # Local authentication compares against a bcrypt hash; the operator
    # copies it into the proxy configuration and hashes nothing itself.
    hash=$(go run ./hack/devtools/bcrypt "$name")
    kc apply --server-side --force-conflicts -f - > /dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: $name-password
  namespace: $NAMESPACE
stringData:
  password: "$hash"
---
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxUser
metadata:
  name: $name
  namespace: $NAMESPACE
spec:
  pgConsoleRef: { name: $CONSOLE }
  subject: $subject
  level: $level
  localPasswordSecretRef: { name: $name-password }
EOF
  done
fi

log "waiting for the console to become ready (pulls nothing; images are side-loaded)"
kc -n "$NAMESPACE" wait --for=condition=Available "deploy/$CONSOLE-pgconsole" --timeout=600s > /dev/null
# Same reason as the operator: the console Pod holds the previous proxy and
# console images behind an unchanged tag.
kc -n "$NAMESPACE" rollout restart "deploy/$CONSOLE-pgconsole" > /dev/null
kc -n "$NAMESPACE" rollout status "deploy/$CONSOLE-pgconsole" --timeout=600s > /dev/null

log "console conditions:"
kc -n "$NAMESPACE" get pgconsole "$CONSOLE" \
  -o jsonpath='{range .status.conditions[*]}  {.type}={.status} ({.reason}){"\n"}{end}' || true

if [ "${NO_FORWARD:-}" = "true" ]; then
  log "NO_FORWARD=true — not port-forwarding; the cluster stays up"
  exit 0
fi

PIDS=""
cleanup() {
  log "stopping the forward (the kind cluster stays up)"
  # shellcheck disable=SC2086
  [ -n "$PIDS" ] && kill $PIDS 2> /dev/null || true
}
trap cleanup EXIT INT TERM

log "port-forwarding the console proxy to 127.0.0.1:$PORT"
kc -n "$NAMESPACE" port-forward "svc/$CONSOLE-pgconsole" "$PORT:80" > /dev/null 2>&1 &
PIDS="$PIDS $!"

cat <<EOF

  pgToolBox dev is up. One port, the real pgtoolbox-proxy:

    http://localhost:$PORT

EOF

if [ "$AUTH_LOCAL" = true ]; then
cat <<EOF
  Sign in as any of these — the level is asserted by the proxy from the
  PgToolBoxUser's role, exactly as in production:

    viewer@$SUBJECT_DOMAIN   / viewer     the overviews and the metrics screens
    operator@$SUBJECT_DOMAIN / operator   + every other read screen, the log tails
    dba@$SUBJECT_DOMAIN      / dba        + the four day-2 operations, the
                                          access-request review panel, and
                                          pgAdmin (browse to /pgadmin)

  The subjects are email addresses because pgAdmin keys its accounts on
  one; a subject that is not an address cannot be provisioned there.

EOF
fi

if [ "$AUTH_OIDC" = true ]; then
cat <<EOF
  $OIDC_ISSUER_URL is enabled too. With local accounts alongside, the
  login page shows the form with an SSO button beside it; sign-in through
  the provider lands back on http://localhost:$PORT/auth/oidc/callback,
  which is the URI to register there.

  These identities have a level; anyone else reaches the 403 page:

    ${OIDC_SUBJECTS:-(none — set OIDC_SUBJECTS)}

EOF
fi

cat <<EOF
  pgAdmin: browse to http://localhost:$PORT/pgadmin directly. The console
  renders an in-UI link only when the PgConsole has an exposure hostname —
  a clusterIP console reached by port-forward has no absolute URL the
  operator could put there.

  Sign in as an identity with no PgToolBoxUser to see the proxy reject an
  unknown one — the 403 page's form files a real
  PgToolBoxAccessRequest, which 'dba' can then approve in the review panel.
  Approving it makes the operator materialize the PgToolBoxUser, and the
  next reconcile lets that identity in.

  Inspect the objects with:
    kubectl --context $CONTEXT -n $NAMESPACE get pgconsole,pguser,pgreq

  Ctrl-C stops the forward; the kind cluster stays up, so re-running this
  script rebuilds and redeploys only what this repo owns.
  Full teardown:  kind delete cluster --name $CLUSTER

EOF

log "serving — Ctrl-C to stop"
wait
