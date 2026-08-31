#!/usr/bin/env bash
set -Eeuo pipefail

on_error() {
  local status=$? line=$1 command=$2
  printf '::error file=scripts/oci-gate-e2e.sh,line=%s::command failed (exit %s): %s\n' \
    "$line" "$status" "$command" >&2
  return "$status"
}
trap 'on_error "$LINENO" "$BASH_COMMAND"' ERR

if [[ ${OCI_GATE_E2E_BOUNDED:-} != 1 ]]; then
  exec timeout --signal=TERM 10m env OCI_GATE_E2E_BOUNDED=1 "$0" "$@"
fi

usage() { echo "usage: $0 <vMAJOR.MINOR.PATCH> <40-lowercase-hex-commit>" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
VERSION=$1
COMMIT=$2
[[ $VERSION =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || usage
[[ $COMMIT =~ ^[0-9a-f]{40}$ ]] || usage

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
RUN=$(mktemp -d)
SUFFIX=${RUN##*/}
IMAGE=agent-forge-gate-e2e:$SUFFIX
CONTAINER=agent-forge-gate-e2e-$SUFFIX
cleanup() {
  local status=$?
  trap - ERR
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker run --rm --user 0:0 --entrypoint /bin/chmod -v "$RUN:/cleanup" "$IMAGE" \
    -R a+rwx /cleanup/state /cleanup/repositories >/dev/null 2>&1 || true
  docker image rm -f "$IMAGE" >/dev/null 2>&1 || true
  if ! rm -rf -- "$RUN"; then
    printf '::error file=scripts/oci-gate-e2e.sh::cleanup could not remove its temporary directory\n' >&2
    status=1
  fi
  return "$status"
}
trap cleanup EXIT

docker build --pull --no-cache --load --file "$ROOT/Dockerfile.gate" \
  --build-arg "VERSION=$VERSION" --build-arg "COMMIT=$COMMIT" --tag "$IMAGE" "$ROOT"

[[ $(docker run --rm "$IMAGE" --version) == "forge-gate $VERSION $COMMIT" ]]
[[ $(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' "$IMAGE") == https://github.com/0k-lab/agent-forge ]]
[[ $(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$IMAGE") == "$VERSION" ]]
[[ $(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$IMAGE") == "$COMMIT" ]]
case $(uname -m) in x86_64) EXPECTED_ARCH=amd64;; aarch64|arm64) EXPECTED_ARCH=arm64;; *) echo "unsupported host architecture" >&2; exit 1;; esac
[[ $(docker image inspect --format '{{.Architecture}}' "$IMAGE") == "$EXPECTED_ARCH" ]]
[[ $(docker image inspect --format '{{.Config.User}}' "$IMAGE") == 65532:65532 ]]
[[ $(docker image inspect --format '{{json .Config.Entrypoint}}' "$IMAGE") == '["/usr/local/bin/forge-gate"]' ]]
[[ $(docker image inspect --format '{{json .Config.Cmd}}' "$IMAGE") == '["-config","/etc/agent-forge/gate.json"]' ]]
[[ $(docker run --rm --entrypoint /usr/bin/git "$IMAGE" --version) == git\ version\ * ]]
docker run --rm --entrypoint /bin/sh "$IMAGE" -eu -c 'test -s /etc/ssl/certs/ca-certificates.crt'
docker run --rm --entrypoint /bin/sh "$IMAGE" -eu -c \
  'test -x /usr/local/bin/forge-gate; ! find / -xdev -type f \( -name "forge-worker" -o -name "forge-*-plugin" \) -print -quit 2>/dev/null | grep -q .'

mkdir "$RUN/state" "$RUN/repositories"
docker run --rm --user 0:0 --entrypoint /bin/sh -v "$RUN:/host" "$IMAGE" -eu -c \
  'chown 65532:65532 /host/state /host/repositories && chmod 0700 /host/state /host/repositories'
docker run --rm --read-only \
  --mount "type=bind,src=$RUN/repositories,dst=/var/lib/agent-forge/repositories" \
  --entrypoint /bin/sh "$IMAGE" -eu -c 'test -w /var/lib/agent-forge/repositories'
cat >"$RUN/gate.json" <<'JSON'
{"version":1,"listen":"0.0.0.0:18080","database":"/var/lib/agent-forge/state/forge.db","owner_token_env":"FORGE_OWNER_TOKEN","recovery_interval":"1s","lease_poll_interval":"100ms","default_pool":"coding","lifecycle":{"lease_ttl":"30s","retry_base":"1s","max_attempts":3},"default_execution":{"plugin_id":"reference","environment":[],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576},"workers":[{"id":"worker-1","pool":"coding","token_env":"FORGE_WORKER_TOKEN","concurrency":1}],"repositories":[]}
JSON
chmod 0444 "$RUN/gate.json"

start_and_require_ready() {
  docker run --detach --name "$CONTAINER" --read-only --cap-drop=ALL \
    --security-opt=no-new-privileges --tmpfs /tmp:rw,nosuid,nodev,noexec \
    --mount "type=bind,src=$RUN/gate.json,dst=/etc/agent-forge/gate.json,readonly" \
    --mount "type=bind,src=$RUN/state,dst=/var/lib/agent-forge/state" \
    --mount "type=bind,src=$RUN/repositories,dst=/var/lib/agent-forge/repositories" \
    --env FORGE_OWNER_TOKEN=oci-e2e-owner-token --env FORGE_WORKER_TOKEN=oci-e2e-worker-token \
    --publish 127.0.0.1:0:18080 "$IMAGE" >/dev/null
  local port response
  port=$(docker port "$CONTAINER" 18080/tcp | sed -n '1s/.*://p')
  for _ in {1..60}; do
    response=$(curl --fail --silent --show-error --max-time 2 "http://127.0.0.1:$port/readyz" 2>/dev/null || true)
    [[ $response == '{"status":"ready"}' ]] && return
    docker container inspect --format '{{.State.Running}}' "$CONTAINER" 2>/dev/null | grep -qx true || break
    sleep 0.5
  done
  docker logs "$CONTAINER" >&2 || true
  return 1
}

start_and_require_ready
docker rm -f "$CONTAINER" >/dev/null
docker run --rm --entrypoint /usr/bin/test -v "$RUN/state:/state" "$IMAGE" -s /state/forge.db
start_and_require_ready
docker rm -f "$CONTAINER" >/dev/null
docker run --rm --entrypoint /usr/bin/test -v "$RUN/state:/state" "$IMAGE" -s /state/forge.db

echo "oci gate e2e: PASS"
