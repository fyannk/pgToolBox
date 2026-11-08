#!/bin/sh
# Restricted-runtime test of the console image: it starts under an
# arbitrary UID with a read-only root filesystem, dropped capabilities and no
# privilege escalation, with the environment charts/pgtools renders (token
# sign-in, a mounted session key, the probe listener apart). The API server it
# is pointed at does not answer, so readiness must stay honest (503, constant
# body) while the probe and sign-in endpoints serve; SIGTERM drains gracefully.
set -eu

image="${1:-pgtoolbox-console:dev}"
name="pgtoolbox-console-test-$$"
dir=$(mktemp -d)

cleanup() {
  docker rm -f "$name" > /dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT INT TERM

# A session key as the chart generates it (48 random characters), and a
# kubeconfig naming an address nothing listens on.
LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 48 > "$dir/session-key"
cat > "$dir/kubeconfig" <<'YAML'
apiVersion: v1
kind: Config
clusters: [{name: none, cluster: {server: "https://127.0.0.1:1", insecure-skip-tls-verify: true}}]
users: [{name: none, user: {token: none}}]
contexts: [{name: none, context: {cluster: none, user: none}}]
current-context: none
YAML
# Readable by the arbitrary UID the container runs as.
chmod 0755 "$dir"
chmod 0644 "$dir/session-key" "$dir/kubeconfig"

docker run -d --name "$name" \
  --user 12345:12345 \
  --read-only \
  --tmpfs /tmp \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  -v "$dir:/config:ro" \
  -e KUBECONFIG=/config/kubeconfig \
  -e POD_NAMESPACE=team-a \
  -e WATCH_NAMESPACES=team-a \
  -e LISTEN_ADDR=:3000 \
  -e PROBE_LISTEN_ADDR=:3001 \
  -e AUTH_PROVIDER=token \
  -e SESSION_KEY_FILE=/config/session-key \
  -e SESSION_IDLE_TIMEOUT=8h \
  -e REVOCATION_CONFIGMAP=pgtoolbox-console-revocations \
  -p 127.0.0.1:0:3000 \
  -p 127.0.0.1:0:3001 \
  "$image" > /dev/null

port=$(docker port "$name" 3000/tcp | head -n1 | sed 's/.*://')
probe=$(docker port "$name" 3001/tcp | head -n1 | sed 's/.*://')

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${probe}/healthz" > /dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

health=$(curl -fsS "http://127.0.0.1:${probe}/healthz") || { docker logs "$name" >&2; exit 1; }
[ "$health" = "ok" ] || { echo "healthz body unexpected" >&2; exit 1; }

ready_status=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${probe}/readyz")
[ "$ready_status" = "503" ] || { echo "readyz must report 503 without an API server, got $ready_status" >&2; exit 1; }

ready_body=$(curl -s "http://127.0.0.1:${probe}/readyz")
[ "$ready_body" = "not ready" ] || { echo "readyz body must be constant" >&2; exit 1; }

login=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/auth/login")
[ "$login" = "200" ] || { echo "the sign-in page must render without an API server, got $login" >&2; exit 1; }

api=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/api/v1/clusters")
[ "$api" = "401" ] || { echo "the API must refuse a request without a session, got $api" >&2; exit 1; }

docker stop -t 30 "$name" > /dev/null
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$name")
[ "$exit_code" = "0" ] || { docker logs "$name" >&2; echo "SIGTERM shutdown was not graceful (exit $exit_code)" >&2; exit 1; }

echo "container test passed (arbitrary uid, read-only root, chart environment, honest readiness, graceful SIGTERM)"
