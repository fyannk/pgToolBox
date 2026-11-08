#!/bin/sh
# Container test of the evidence agent in RUNTIME_MODE=agent, the way the
# pgtools chart's team mode runs it: shared-token authentication over plain HTTP
# inside one namespace, credentials as read-only files, no network but the
# published port. Two profiles: an arbitrary non-root UID, and a UID in group 0
# (what OpenShift's restricted SCC assigns).
set -eu

image=${1:-evidence-agent:dev}
test_prefix="evidence-${EVIDENCE_RUN_ID:-local}-agent-$$"
work=$(mktemp -d)
containers=""

cleanup() {
  for container_name in $containers; do
    docker rm -f "$container_name" >/dev/null 2>&1 || true
  done
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

# Files the container reads as an arbitrary UID: world-readable, read-only mount.
mkdir -p "$work/token" "$work/credentials"
chmod 0755 "$work" "$work/token" "$work/credentials"
token=$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr '+/' '-_' | tr -d '=\n')
printf '%s' "$token" > "$work/token/agent-token"
printf '%s' synthetic-access-key > "$work/credentials/aws-access-key-id"
printf '%s' synthetic-secret-key > "$work/credentials/aws-secret-access-key"
chmod 0444 "$work/token/agent-token" "$work/credentials/aws-access-key-id" "$work/credentials/aws-secret-access-key"

status() { # url [curl args...]
  curl --silent --output /dev/null --write-out '%{http_code}' --max-time 5 "$@"
}

run_profile() {
  profile=$1
  uid=$2
  gid=$3
  name="$test_prefix-$profile"
  containers="$name $containers"

  docker run --detach \
    --label "evidence.run=${EVIDENCE_RUN_ID:-local}" \
    --name "$name" \
    --read-only \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m \
    --user "$uid:$gid" \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --publish 127.0.0.1::8443 \
    --mount "type=bind,source=$work/token,target=/var/run/secrets/agent-token,readonly" \
    --mount "type=bind,source=$work/credentials,target=/var/run/secrets/agent-store,readonly" \
    --env RUNTIME_MODE=agent \
    --env AGENT_AUTH=sharedtoken \
    --env AGENT_TOKEN_FILE=/var/run/secrets/agent-token/agent-token \
    --env AGENT_PLAIN_HTTP=true \
    --env AGENT_NAME=orders \
    --env AGENT_NAMESPACE=database-team \
    --env PRODUCER_EPOCH=1 \
    --env REPOSITORY_FORMAT=barman-cloud \
    --env PROVIDER=s3 \
    --env "DESTINATION_PATH=s3://synthetic-$profile/repository" \
    --env BARMAN_SERVER_NAMES=orders \
    --env CNPG_CLUSTER_NAMESPACE=database-team \
    --env "CNPG_CLUSTER_UID=00000000-0000-0000-0000-$profile" \
    --env CNPG_CLUSTER_NAME=orders \
    --env STORE_CREDENTIAL_MODE=static-files \
    --env AWS_ACCESS_KEY_ID_FILE=/var/run/secrets/agent-store/aws-access-key-id \
    --env AWS_SECRET_ACCESS_KEY_FILE=/var/run/secrets/agent-store/aws-secret-access-key \
    --env AWS_REGION=us-east-1 \
    --env STORE_REQUEST_TIMEOUT=1s \
    "$image" >/dev/null

  port=$(docker port "$name" 8443/tcp | sed -n 's/.*://p' | head -n 1)
  attempt=0
  while [ "$attempt" -lt 40 ]; do
    if [ "$(status "http://127.0.0.1:$port/healthz")" = "200" ]; then
      break
    fi
    attempt=$((attempt + 1))
    sleep 0.25
  done
  if [ "$attempt" -eq 40 ]; then
    docker logs "$name" >&2
    return 1
  fi

  test "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$name")" = "true"
  test "$(docker inspect --format '{{.Config.User}}' "$name")" = "$uid:$gid"
  test "$(docker inspect --format '{{.HostConfig.SecurityOpt}}' "$name")" = "[no-new-privileges]"
  test "$(docker inspect --format '{{.HostConfig.CapDrop}}' "$name")" = "[ALL]"
  test -n "$(docker inspect --format '{{index .HostConfig.Tmpfs "/tmp"}}' "$name")"
  test "$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/run/secrets/agent-token"}}{{.RW}}{{end}}{{end}}' "$name")" = "false"
  test "$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/run/secrets/agent-store"}}{{.RW}}{{end}}{{end}}' "$name")" = "false"

  # The wire contract: /healthz needs no token; the data routes do.
  api="http://127.0.0.1:$port/api/v1alpha1/snapshot"
  test "$(status "$api")" = "401"
  test "$(status --header 'Authorization: Bearer not-the-token' "$api")" = "401"
  authorized=$(status --header "Authorization: Bearer $token" "$api")
  case "$authorized" in
    401|403) echo "agent refused its own shared token: HTTP $authorized" >&2; docker logs "$name" >&2; return 1 ;;
  esac
  # Another route that is not part of the contract is not served.
  test "$(status --header "Authorization: Bearer $token" "http://127.0.0.1:$port/api/v1/snapshot")" = "404"
  # Nothing of the token or the keys reaches the log.
  if docker logs "$name" 2>&1 | grep -F -e "$token" -e synthetic-secret-key -e synthetic-access-key; then
    echo "agent log disclosed a secret" >&2
    return 1
  fi

  docker stop --signal TERM --time 10 "$name" >/dev/null
  test "$(docker inspect --format '{{.State.ExitCode}}' "$name")" = "0"

  echo "evidence agent container profile $profile passed"
}

run_profile ordinary 12345 12345
run_profile openshift 1000710000 0

image_id=$(docker image inspect --format '{{.Id}}' "$image")
echo "agent restricted-runtime container tests passed for $image_id"
