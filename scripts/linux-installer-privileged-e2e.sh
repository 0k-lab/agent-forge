#!/bin/sh
set -eu
export LC_ALL=C
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
unset CDPATH ENV BASH_ENV GLOBIGNORE POSIXLY_CORRECT TAR_OPTIONS GZIP GOFIPS140
umask 077

STAGE=argument-validation
TMP=
finish() {
  rc=$?
  trap - EXIT
  if [ "$rc" -ne 0 ]; then
    printf '::error title=Privileged Linux installer E2E::stage=%s exit=%s\n' "$STAGE" "$rc" >&2
    if command -v systemctl >/dev/null 2>&1; then
      for unit in agent-forge-gate.service agent-forge-worker.service; do
        systemctl show --no-pager \
          --property=LoadState \
          --property=ActiveState \
          --property=SubState \
          --property=Result \
          --property=ExecMainCode \
          --property=ExecMainStatus \
          "$unit" 2>/dev/null || true
      done
    fi
  fi
  [ -z "$TMP" ] || rm -rf "$TMP"
  exit "$rc"
}
trap finish EXIT

usage() {
  echo "usage: $0 <vMAJOR.MINOR.PATCH> <40-lowercase-hex-commit> <absolute-linux-asset-dir>" >&2
  exit 2
}

[ "$#" -eq 3 ] || usage
VERSION=$1
COMMIT=$2
ASSET_DIR=$3
printf '%s\n' "$VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || usage
printf '%s\n' "$COMMIT" | grep -Eq '^[0-9a-f]{40}$' || usage
case "$ASSET_DIR" in /*) ;; *) usage ;; esac

# This test intentionally mutates account, /opt, and systemd state. It must never
# run on a persistent or self-hosted machine.
STAGE=runner-preflight
[ "${AGENT_FORGE_PRIVILEGED_E2E:-}" = 1 ] || { echo "privileged E2E opt-in is required" >&2; exit 1; }
[ "${CI:-}" = true ] && [ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] || {
  echo "privileged E2E requires a disposable GitHub-hosted runner" >&2
  exit 1
}
case "${RUNNER_TEMP:-}" in /*) [ -d "$RUNNER_TEMP" ] && [ ! -L "$RUNNER_TEMP" ] || exit 1 ;; *) exit 1 ;; esac
[ "$(id -u)" -eq 0 ] || { echo "privileged E2E must run as root" >&2; exit 1; }
[ -d /run/systemd/system ] || { echo "systemd is not the service manager" >&2; exit 1; }
[ -d "$ASSET_DIR" ] && [ ! -L "$ASSET_DIR" ] && [ -f "$ASSET_DIR/SHA256SUMS" ] && [ ! -L "$ASSET_DIR/SHA256SUMS" ] || {
  echo "invalid Linux asset directory" >&2
  exit 1
}

for tool in cmp getent grep id journalctl mktemp readlink rm sha256sum stat systemctl tar uname python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done
for path in /usr/bin/cmp /usr/bin/getent /usr/bin/grep /usr/bin/id /usr/bin/journalctl /usr/bin/mktemp /usr/bin/readlink /usr/bin/rm /usr/bin/sha256sum /usr/bin/stat /usr/bin/systemctl /usr/bin/tar /usr/bin/python3; do
  [ -x "$path" ] || { echo "invalid privileged tool: $path" >&2; exit 1; }
done

# Require a truly clean host boundary before the first mutation.
if getent passwd agent-forge >/dev/null || getent group agent-forge >/dev/null || [ -e /opt/agent-forge ] || [ -L /opt/agent-forge ]; then
  echo "runner is not clean for Agent Forge installation" >&2
  exit 1
fi
for unit in agent-forge-gate.service agent-forge-worker.service; do
  for root in \
    /etc/systemd/system.control \
    /run/systemd/system.control \
    /run/systemd/transient \
    /run/systemd/generator.early \
    /etc/systemd/system \
    /etc/systemd/system.attached \
    /run/systemd/system \
    /run/systemd/system.attached \
    /run/systemd/generator \
    /usr/local/lib/systemd/system \
    /usr/lib/systemd/system \
    /lib/systemd/system \
    /run/systemd/generator.late; do
    for path in \
      "$root/$unit" \
      "$root/$unit.d" \
      "$root/agent-forge-.service.d" \
      "$root/agent-.service.d" \
      "$root/service.d"; do
      [ ! -e "$path" ] && [ ! -L "$path" ] || {
        echo "runner has pre-existing Agent Forge unit configuration" >&2
        exit 1
      }
    done
    for path in "$root"/*.wants/"$unit" "$root"/*.requires/"$unit"; do
      [ ! -e "$path" ] && [ ! -L "$path" ] || {
        echo "runner has pre-existing Agent Forge unit enablement" >&2
        exit 1
      }
    done
  done
  [ "$(systemctl show --property=LoadState --value "$unit")" = not-found ] || {
    echo "runner has a loaded or transient Agent Forge unit" >&2
    exit 1
  }
done

ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported runner architecture: $ARCH" >&2; exit 1 ;;
esac
CLI_ARCHIVE="agent-forge-cli_${VERSION}_linux_${ARCH}.tar.gz"
STAGE=asset-verification
[ -f "$ASSET_DIR/$CLI_ARCHIVE" ] && [ ! -L "$ASSET_DIR/$CLI_ARCHIVE" ] || { echo "missing CLI archive" >&2; exit 1; }
(
  cd "$ASSET_DIR"
  sha256sum -c SHA256SUMS >/dev/null
)
ANCHOR=$(sha256sum "$ASSET_DIR/SHA256SUMS")
ANCHOR=${ANCHOR%% *}
TMP=$(mktemp -d "$RUNNER_TEMP/agent-forge-privileged.XXXXXX")

tar --extract --gzip --file "$ASSET_DIR/$CLI_ARCHIVE" --directory "$TMP" --no-same-owner --no-same-permissions
BOOTSTRAP="$TMP/agent-forge-cli_${VERSION}_linux_${ARCH}/forge"
[ -x "$BOOTSTRAP" ] && [ ! -L "$BOOTSTRAP" ] || { echo "invalid bootstrap forge" >&2; exit 1; }
[ "$("$BOOTSTRAP" --version)" = "forge $VERSION $COMMIT" ] || { echo "bootstrap identity mismatch" >&2; exit 1; }

install_exact() {
  "$BOOTSTRAP" install \
    --version "$VERSION" \
    --commit "$COMMIT" \
    --asset-dir "$ASSET_DIR" \
    --sha256sums-sha256 "$ANCHOR" \
    --enable-now
}

assert_meta() {
  path=$1 mode=$2 uid=$3 gid=$4
  [ -e "$path" ] && [ ! -L "$path" ] || { echo "missing real path: $path" >&2; exit 1; }
  got=$(stat -c '%a:%u:%g' "$path")
  [ "$got" = "$mode:$uid:$gid" ] || { echo "metadata mismatch: $path $got" >&2; exit 1; }
}

assert_dir_meta() {
  [ -d "$1" ] && [ ! -L "$1" ] || { echo "not a real directory: $1" >&2; exit 1; }
  assert_meta "$@"
}

assert_file_meta() {
  [ -f "$1" ] && [ ! -L "$1" ] || { echo "not a real file: $1" >&2; exit 1; }
  assert_meta "$@"
}

assert_active() {
  unit=$1
  [ "$(systemctl is-enabled "$unit")" = enabled ]
  [ "$(systemctl is-active "$unit")" = active ]
  [ "$(systemctl show --property=User --value "$unit")" = agent-forge ]
  [ "$(systemctl show --property=Group --value "$unit")" = agent-forge ]
  pid=$(systemctl show --property=MainPID --value "$unit")
  case "$pid" in ''|0|*[!0-9]*) echo "invalid MainPID: $unit" >&2; exit 1 ;; esac
  [ "$(stat -c '%u' "/proc/$pid")" = "$SERVICE_UID" ] || { echo "service UID mismatch: $unit" >&2; exit 1; }
}

wait_worker() {
  OWNER_TOKEN=
  while IFS='=' read -r key value; do
    if [ "$key" = FORGE_OWNER_TOKEN ]; then OWNER_TOKEN=$value; fi
  done </opt/agent-forge/secrets/gate.env
  [ -n "$OWNER_TOKEN" ] || { echo "owner token missing" >&2; exit 1; }
  FORGE_OWNER_TOKEN=$OWNER_TOKEN python3 - <<'PY'
import json
import os
import time
import urllib.error
import urllib.request

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None

opener = urllib.request.build_opener(NoRedirect())
deadline = time.monotonic() + 30
while True:
    request = urllib.request.Request(
        "http://127.0.0.1:18080/v1/workers/worker-1",
        headers={"Authorization": "Bearer " + os.environ["FORGE_OWNER_TOKEN"]},
        method="GET",
    )
    try:
        with opener.open(request, timeout=2) as response:
            body = response.read(4097)
            if len(body) <= 4096 and response.status == 200:
                text = body.decode("utf-8")
                value, end = json.JSONDecoder().raw_decode(text)
                if not text[end:].strip() and isinstance(value, dict) and value.get("id") == "worker-1" and value.get("connected") is True:
                    break
    except (OSError, UnicodeError, ValueError, urllib.error.URLError):
        pass
    if time.monotonic() >= deadline:
        raise SystemExit("authenticated Worker readiness timeout")
    time.sleep(0.1)
PY
  unset OWNER_TOKEN FORGE_OWNER_TOKEN
}

assert_tokens_not_exposed() {
  OWNER_TOKEN=
  WORKER_TOKEN=
  while IFS='=' read -r key value; do
    case "$key" in
      FORGE_OWNER_TOKEN) OWNER_TOKEN=$value ;;
      FORGE_WORKER_TOKEN) WORKER_TOKEN=$value ;;
    esac
  done </opt/agent-forge/secrets/gate.env
  [ -n "$OWNER_TOKEN" ] && [ -n "$WORKER_TOKEN" ] && [ "$OWNER_TOKEN" != "$WORKER_TOKEN" ] || {
    echo "installed token contract mismatch" >&2
    exit 1
  }
  GATE_PID=$(systemctl show --property=MainPID --value agent-forge-gate.service)
  WORKER_PID=$(systemctl show --property=MainPID --value agent-forge-worker.service)
  FORGE_OWNER_TOKEN=$OWNER_TOKEN FORGE_WORKER_TOKEN=$WORKER_TOKEN GATE_PID=$GATE_PID WORKER_PID=$WORKER_PID python3 - <<'PY'
import os
import subprocess

tokens = [os.environ["FORGE_OWNER_TOKEN"].encode(), os.environ["FORGE_WORKER_TOKEN"].encode()]
for name in ("GATE_PID", "WORKER_PID"):
    pid = os.environ[name]
    if not pid.isdigit() or pid == "0":
        raise SystemExit("invalid service PID for token exposure check")
    with open("/proc/" + pid + "/cmdline", "rb") as handle:
        body = handle.read(1_048_577)
    if len(body) > 1_048_576:
        raise SystemExit("service command line exceeded bound")
    if any(token in body for token in tokens):
        raise SystemExit("service token appeared in process command line")

result = subprocess.run(
    [
        "/usr/bin/journalctl",
        "--no-pager",
        "--output=cat",
        "--lines=500",
        "--unit=agent-forge-gate.service",
        "--unit=agent-forge-worker.service",
    ],
    env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "LC_ALL": "C"},
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    check=False,
)
if result.returncode != 0:
    raise SystemExit("bounded service journal read failed")
if len(result.stdout) > 8_388_608 or len(result.stderr) > 65_536:
    raise SystemExit("bounded service journal output exceeded limit")
if any(token in result.stdout or token in result.stderr for token in tokens):
    raise SystemExit("service token appeared in journal output")
PY
  unset OWNER_TOKEN WORKER_TOKEN FORGE_OWNER_TOKEN FORGE_WORKER_TOKEN GATE_PID WORKER_PID
}

snapshot_validation_only_state() {
  output=$1
  {
    for path in \
      /opt/agent-forge/install-receipt.json \
      /opt/agent-forge/bin/forge \
      /opt/agent-forge/bin/forge-gate \
      /opt/agent-forge/bin/forge-worker \
      /opt/agent-forge/bin/forge-codex-plugin \
      /opt/agent-forge/bin/forge-ref-plugin \
      /opt/agent-forge/etc/gate.json \
      /opt/agent-forge/etc/worker.json \
      /opt/agent-forge/secrets/gate.env \
      /opt/agent-forge/secrets/worker.env \
      /opt/agent-forge/systemd/agent-forge-gate.service \
      /opt/agent-forge/systemd/agent-forge-worker.service; do
      stat -c '%n:%i:%s:%a:%u:%g:%y' "$path"
      sha256sum "$path"
    done
    getent passwd agent-forge
    getent group agent-forge
    readlink /etc/systemd/system/agent-forge-gate.service
    readlink /etc/systemd/system/agent-forge-worker.service
    systemctl show --property=GeneratorsStartTimestampMonotonic --property=FinishTimestampMonotonic --property=Reloading --no-pager
    for unit in agent-forge-gate.service agent-forge-worker.service; do
      systemctl show --property=MainPID --property=InvocationID --property=ExecMainStartTimestampMonotonic --property=ActiveState --property=UnitFileState --no-pager "$unit"
    done
  } >"$output"
}

STAGE=initial-install
install_exact

STAGE=account-and-filesystem-contract
PASSWD=$(getent passwd agent-forge)
old_ifs=$IFS
IFS=:
set -- $PASSWD
IFS=$old_ifs
[ "$#" -eq 7 ] && [ "$1" = agent-forge ] && [ "$6" = /opt/agent-forge ] && [ "$7" = /usr/sbin/nologin ] || {
  echo "service account passwd contract mismatch" >&2
  exit 1
}
SERVICE_UID=$3
SERVICE_GID=$4
[ "$SERVICE_UID" -gt 0 ] && [ "$SERVICE_GID" -gt 0 ]
GROUP=$(getent group agent-forge)
[ "$GROUP" = "agent-forge:x:$SERVICE_GID:" ] || {
  echo "service account group contract mismatch" >&2
  exit 1
}
[ "$(id -u agent-forge)" = "$SERVICE_UID" ]
[ "$(id -g agent-forge)" = "$SERVICE_GID" ]
[ "$(id -G agent-forge)" = "$SERVICE_GID" ]

assert_dir_meta /opt/agent-forge 755 0 0
assert_dir_meta /opt/agent-forge/bin 755 0 0
assert_dir_meta /opt/agent-forge/etc 750 0 "$SERVICE_GID"
assert_dir_meta /opt/agent-forge/secrets 700 0 0
assert_dir_meta /opt/agent-forge/systemd 755 0 0
assert_dir_meta /opt/agent-forge/var 755 0 0
assert_dir_meta /opt/agent-forge/var/gate 700 "$SERVICE_UID" "$SERVICE_GID"
assert_dir_meta /opt/agent-forge/var/gate/state 700 "$SERVICE_UID" "$SERVICE_GID"
assert_dir_meta /opt/agent-forge/var/repositories 700 "$SERVICE_UID" "$SERVICE_GID"
assert_dir_meta /opt/agent-forge/var/worker 700 "$SERVICE_UID" "$SERVICE_GID"
assert_dir_meta /opt/agent-forge/var/worker/worktrees 700 "$SERVICE_UID" "$SERVICE_GID"
assert_dir_meta /opt/agent-forge/var/worker/runtime 700 "$SERVICE_UID" "$SERVICE_GID"
for file in forge forge-gate forge-worker forge-codex-plugin forge-ref-plugin; do
  assert_file_meta "/opt/agent-forge/bin/$file" 755 0 0
done
assert_file_meta /opt/agent-forge/install-receipt.json 400 0 0
assert_file_meta /opt/agent-forge/etc/gate.json 440 0 "$SERVICE_GID"
assert_file_meta /opt/agent-forge/etc/worker.json 440 0 "$SERVICE_GID"
assert_file_meta /opt/agent-forge/secrets/gate.env 600 0 0
assert_file_meta /opt/agent-forge/secrets/worker.env 600 0 0
assert_file_meta /opt/agent-forge/systemd/agent-forge-gate.service 644 0 0
assert_file_meta /opt/agent-forge/systemd/agent-forge-worker.service 644 0 0
[ "$(readlink /etc/systemd/system/agent-forge-gate.service)" = /opt/agent-forge/systemd/agent-forge-gate.service ]
[ "$(readlink /etc/systemd/system/agent-forge-worker.service)" = /opt/agent-forge/systemd/agent-forge-worker.service ]
assert_active agent-forge-gate.service
assert_active agent-forge-worker.service
STAGE=initial-authenticated-readiness
wait_worker
STAGE=initial-token-exposure
assert_tokens_not_exposed

BEFORE="$TMP/before"
AFTER="$TMP/after"
snapshot_validation_only_state "$BEFORE"
STAGE=validation-only-rerun
install_exact
snapshot_validation_only_state "$AFTER"
cmp "$BEFORE" "$AFTER" || { echo "same-version rerun mutated installation or service state" >&2; exit 1; }

STAGE=service-restart
systemctl restart agent-forge-gate.service
systemctl restart agent-forge-worker.service
assert_active agent-forge-gate.service
assert_active agent-forge-worker.service
STAGE=restart-authenticated-readiness
wait_worker
STAGE=restart-token-exposure
assert_tokens_not_exposed

STAGE=final-installed-identity
[ "$(/opt/agent-forge/bin/forge --version)" = "forge $VERSION $COMMIT" ]
STAGE=complete
echo "privileged Linux installer e2e: PASS"
