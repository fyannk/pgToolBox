#!/bin/sh
set -eu

image=${1:-evidence-agent:dev}
container=""
filesystem_archive=$(mktemp)

cleanup() {
  if [ -n "$container" ]; then
    docker rm -f "$container" >/dev/null 2>&1 || true
  fi
  rm -f "$filesystem_archive"
}
trap cleanup EXIT INT TERM

container=$(docker run --detach \
  --label "evidence.run=${EVIDENCE_RUN_ID:-local}" \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m \
  --user 12345:12345 \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --publish 127.0.0.1::3000 \
  --env REPOSITORY_FORMAT=barman-cloud \
  --env PROVIDER=s3 \
  --env DESTINATION_PATH=s3://synthetic-test/repository \
  --env AWS_REGION=us-east-1 \
  "$image")

port=$(docker port "$container" 3000/tcp | sed -n 's/.*://p')
attempt=0
while [ "$attempt" -lt 20 ]; do
  if curl --fail --silent "http://127.0.0.1:$port/healthz" | grep -qx live; then
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.25
done
if [ "$attempt" -eq 20 ]; then
  docker logs "$container" >&2
  exit 1
fi

ready_status=$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:$port/readyz")
test "$ready_status" = "503"
test "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$container")" = "true"
test "$(docker inspect --format '{{.Config.User}}' "$container")" = "12345:12345"
test "$(docker inspect --format '{{.HostConfig.SecurityOpt}}' "$container")" = "[no-new-privileges]"
test "$(docker inspect --format '{{.HostConfig.CapDrop}}' "$container")" = "[ALL]"
test -n "$(docker inspect --format '{{index .HostConfig.Tmpfs "/tmp"}}' "$container")"

docker export "$container" > "$filesystem_archive"
if tar -tf "$filesystem_archive" | grep -E '(^|/)(src|go)/.+|(^|/)(apk|apt|apt-get|dpkg|rpm|yum|dnf)$|(^|/)bin/(ba)?sh$'; then
  echo "runtime image contains source, a package manager, or a shell" >&2
  exit 1
fi
tar -tf "$filesystem_archive" | grep -qx 'evidence-agent'
tar -tf "$filesystem_archive" | grep -qx 'licenses/evidence-agent/LICENSE'

docker stop --signal TERM --time 10 "$container" >/dev/null
exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$container")
test "$exit_code" = "0"
image_id=$(docker image inspect --format '{{.Id}}' "$image")

echo "restricted container test passed for $image_id: arbitrary UID, read-only root, /tmp tmpfs, no-new-privileges, dropped capabilities, graceful SIGTERM"
