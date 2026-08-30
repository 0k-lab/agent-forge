#!/bin/sh
set -eu
export LC_ALL=C
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
unset CDPATH ENV BASH_ENV GLOBIGNORE POSIXLY_CORRECT TAR_OPTIONS GZIP GOFIPS140
umask 077

STAGE=argument-validation
TMP=
BIND_MOUNT_ACTIVE=0
finish() {
  rc=$?
  trap - EXIT
  set +e
  if [ "$BIND_MOUNT_ACTIVE" -eq 1 ]; then
    if /usr/bin/umount /opt/agent-forge/bin; then
      BIND_MOUNT_ACTIVE=0
    else
      rc=1
    fi
  fi
  if [ "$rc" -ne 0 ]; then
    printf '::error title=Privileged Linux installer E2E::stage=%s exit=%s\n' "$STAGE" "$rc" >&2
    account_present=0
    install_present=0
    gate_link_present=0
    worker_link_present=0
    if getent passwd agent-forge >/dev/null 2>&1; then account_present=1; fi
    if [ -e /opt/agent-forge ] || [ -L /opt/agent-forge ]; then install_present=1; fi
    if [ -e /etc/systemd/system/agent-forge-gate.service ] || [ -L /etc/systemd/system/agent-forge-gate.service ]; then gate_link_present=1; fi
    if [ -e /etc/systemd/system/agent-forge-worker.service ] || [ -L /etc/systemd/system/agent-forge-worker.service ]; then worker_link_present=1; fi
    printf '::error title=Privileged Linux installer state::account=%s install=%s gate_link=%s worker_link=%s\n' \
      "$account_present" "$install_present" "$gate_link_present" "$worker_link_present" >&2
    if command -v systemctl >/dev/null 2>&1; then
      for unit in agent-forge-gate.service agent-forge-worker.service; do
        state=$(systemctl show --no-pager \
          --property=LoadState \
          --property=ActiveState \
          --property=SubState \
          --property=Result \
          --property=ExecMainCode \
          --property=ExecMainStatus \
          "$unit" 2>/dev/null | /usr/bin/tr '\n' ' ' || true)
        printf '::error title=Privileged Linux unit state::unit=%s %s\n' "$unit" "$state" >&2
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
case "$0" in
  /*/linux-installer-privileged-e2e.sh) DIAGNOSTICS=${0%/*}/linux-installer-diagnostics.py ;;
  *) echo "privileged E2E requires an absolute script path" >&2; exit 1 ;;
esac
[ -f "$DIAGNOSTICS" ] && [ ! -L "$DIAGNOSTICS" ] || { echo "missing installer diagnostics helper" >&2; exit 1; }
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
OLD_VERSION=${AGENT_FORGE_UPGRADE_FROM_VERSION:-}
OLD_COMMIT=${AGENT_FORGE_UPGRADE_FROM_COMMIT:-}
OLD_ASSET_DIR=${AGENT_FORGE_UPGRADE_FROM_ASSET_DIR:-}
if [ -n "$OLD_VERSION$OLD_COMMIT$OLD_ASSET_DIR" ]; then
  printf '%s\n' "$OLD_VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || usage
  printf '%s\n' "$OLD_COMMIT" | grep -Eq '^[0-9a-f]{40}$' || usage
  case "$OLD_ASSET_DIR" in /*) ;; *) usage ;; esac
  [ -d "$OLD_ASSET_DIR" ] && [ ! -L "$OLD_ASSET_DIR" ] && [ -f "$OLD_ASSET_DIR/SHA256SUMS" ] && [ ! -L "$OLD_ASSET_DIR/SHA256SUMS" ] || {
    echo "invalid old Linux asset directory" >&2
    exit 1
  }
fi

for tool in chmod cmp getent grep id journalctl mkfifo mktemp mount readlink rm runuser sha256sum stat systemctl tar tr umount uname wc python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done
for path in /usr/bin/chmod /usr/bin/cmp /usr/bin/getent /usr/bin/grep /usr/bin/id /usr/bin/journalctl /usr/bin/mkfifo /usr/bin/mktemp /usr/bin/mount /usr/bin/readlink /usr/bin/rm /usr/sbin/runuser /usr/bin/sha256sum /usr/bin/stat /usr/bin/systemctl /usr/bin/tar /usr/bin/tr /usr/bin/umount /usr/bin/wc /usr/bin/python3; do
  [ -x "$path" ] || { echo "invalid privileged tool: $path" >&2; exit 1; }
done
/usr/bin/python3 "$DIAGNOSTICS" self-test

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

# GitHub's Ubuntu image builder intentionally runs `chmod -R 777 /opt` so
# hosted tools can be updated by the runner account. The production installer
# must continue to reject that unsafe privileged ancestor. After proving the
# runner is otherwise clean, normalize only this documented disposable-runner
# fixture before invoking the exact release binary.
STAGE=runner-normalization
[ -d /opt ] && [ ! -L /opt ] || { echo "invalid /opt fixture" >&2; exit 1; }
opt_meta=$(/usr/bin/stat -c '%a:%u:%g' /opt)
case "$opt_meta" in
  755:0:0) ;;
  777:0:0) /usr/bin/chmod 0755 /opt ;;
  *) echo "unexpected /opt fixture metadata" >&2; exit 1 ;;
esac
[ "$(/usr/bin/stat -c '%a:%u:%g' /opt)" = 755:0:0 ] || { echo "failed to normalize /opt fixture" >&2; exit 1; }

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

OLD_BOOTSTRAP=
OLD_ANCHOR=
if [ -n "$OLD_VERSION" ]; then
  OLD_CLI_ARCHIVE="agent-forge-cli_${OLD_VERSION}_linux_${ARCH}.tar.gz"
  [ -f "$OLD_ASSET_DIR/$OLD_CLI_ARCHIVE" ] && [ ! -L "$OLD_ASSET_DIR/$OLD_CLI_ARCHIVE" ] || { echo "missing old CLI archive" >&2; exit 1; }
  for role in cli gate worker; do
    old_archive="agent-forge-${role}_${OLD_VERSION}_linux_${ARCH}.tar.gz"
    /usr/bin/grep -F "  $old_archive" "$OLD_ASSET_DIR/SHA256SUMS" >"$TMP/old-check"
    [ "$(/usr/bin/wc -l <"$TMP/old-check")" -eq 1 ] || { echo "missing or duplicate old archive checksum" >&2; exit 1; }
    (cd "$OLD_ASSET_DIR" && /usr/bin/sha256sum --check --strict "$TMP/old-check" >/dev/null)
  done
  OLD_ANCHOR=$(sha256sum "$OLD_ASSET_DIR/SHA256SUMS")
  OLD_ANCHOR=${OLD_ANCHOR%% *}
  tar --extract --gzip --file "$OLD_ASSET_DIR/$OLD_CLI_ARCHIVE" --directory "$TMP" --no-same-owner --no-same-permissions
  OLD_BOOTSTRAP="$TMP/agent-forge-cli_${OLD_VERSION}_linux_${ARCH}/forge"
  [ -x "$OLD_BOOTSTRAP" ] && [ ! -L "$OLD_BOOTSTRAP" ] || { echo "invalid old bootstrap forge" >&2; exit 1; }
  [ "$("$OLD_BOOTSTRAP" --version)" = "forge $OLD_VERSION $OLD_COMMIT" ] || { echo "old bootstrap identity mismatch" >&2; exit 1; }
fi

install_release() {
  release_bootstrap=$1 release_version=$2 release_commit=$3 release_assets=$4 release_anchor=$5 release_mode=$6
  install_log="$TMP/install.log"
  install_pipe="$TMP/install.pipe"
  /usr/bin/rm -f "$install_log" "$install_pipe"
  /usr/bin/mkfifo "$install_pipe"
  /usr/bin/python3 "$DIAGNOSTICS" capture "$install_pipe" "$install_log" &
  capture_pid=$!
  if [ "$release_mode" = upgrade ]; then
    if "$release_bootstrap" install \
      --version "$release_version" \
      --commit "$release_commit" \
      --asset-dir "$release_assets" \
      --sha256sums-sha256 "$release_anchor" \
      --enable-now --upgrade >"$install_pipe" 2>&1; then
      install_rc=0
    else
      install_rc=$?
    fi
  elif "$release_bootstrap" install \
    --version "$release_version" \
    --commit "$release_commit" \
    --asset-dir "$release_assets" \
    --sha256sums-sha256 "$release_anchor" \
    --enable-now >"$install_pipe" 2>&1; then
    install_rc=0
  else
    install_rc=$?
  fi
  if ! wait "$capture_pid"; then
    return 1
  fi
  /usr/bin/rm -f "$install_pipe"
  if [ "$install_rc" -eq 0 ]; then
    return 0
  fi
  install_stage=$(/usr/bin/python3 "$DIAGNOSTICS" parse "$install_log")
  if [ -n "$install_stage" ]; then
    printf '::error title=Privileged Linux installer stage::install_stage=%s\n' "$install_stage" >&2
  fi
  return 1
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

snapshot_upgrade_preserved_state() {
  output=$1
  {
    for path in \
      /opt/agent-forge/etc/gate.json \
      /opt/agent-forge/etc/worker.json \
      /opt/agent-forge/secrets/gate.env \
      /opt/agent-forge/secrets/worker.env; do
      stat -c '%n:%i:%s:%a:%u:%g:%y' "$path"
      sha256sum "$path"
    done
    for path in \
      /opt/agent-forge/var \
      /opt/agent-forge/var/gate \
      /opt/agent-forge/var/gate/state \
      /opt/agent-forge/var/repositories \
      /opt/agent-forge/var/worker \
      /opt/agent-forge/var/worker/worktrees \
      /opt/agent-forge/var/worker/runtime; do
      stat -c '%n:%i:%a:%u:%g' "$path"
    done
    stat -c '%n:%i:%a:%u:%g' /opt/agent-forge/var/gate/state/forge.db
    /usr/sbin/runuser -u agent-forge -- /usr/bin/python3 - <<'PY'
import sqlite3

with sqlite3.connect("file:/opt/agent-forge/var/gate/state/forge.db?mode=ro", uri=True) as database:
    marker = database.execute(
        "SELECT value FROM privileged_e2e_marker WHERE name = ?", ("preserved",)
    ).fetchone()
if marker != ("agent-forge-privileged-e2e-v1",):
    raise SystemExit("SQLite preservation marker mismatch")
print("sqlite-marker:" + marker[0])
PY
    for path in \
      /opt/agent-forge/var/repositories/.privileged-e2e-marker \
      /opt/agent-forge/var/worker/worktrees/.privileged-e2e-marker \
      /opt/agent-forge/var/worker/runtime/.privileged-e2e-marker; do
      stat -c '%n:%i:%s:%a:%u:%g:%y' "$path"
      sha256sum "$path"
    done
  } >"$output"
}

STAGE=initial-install
if [ -n "$OLD_VERSION" ]; then
  install_release "$OLD_BOOTSTRAP" "$OLD_VERSION" "$OLD_COMMIT" "$OLD_ASSET_DIR" "$OLD_ANCHOR" clean
else
  install_release "$BOOTSTRAP" "$VERSION" "$COMMIT" "$ASSET_DIR" "$ANCHOR" clean
fi

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

if [ -n "$OLD_VERSION" ]; then
  STAGE=bind-mount-topology-rejection
  GATE_PID_BEFORE=$(systemctl show --property=MainPID --value agent-forge-gate.service)
  WORKER_PID_BEFORE=$(systemctl show --property=MainPID --value agent-forge-worker.service)
  GATE_INVOCATION_BEFORE=$(systemctl show --property=InvocationID --value agent-forge-gate.service)
  WORKER_INVOCATION_BEFORE=$(systemctl show --property=InvocationID --value agent-forge-worker.service)
  OLD_CLI_ID_BEFORE=$(/opt/agent-forge/bin/forge --version)
  [ -n "$GATE_INVOCATION_BEFORE" ] && [ -n "$WORKER_INVOCATION_BEFORE" ]
  [ "$OLD_CLI_ID_BEFORE" = "forge $OLD_VERSION $OLD_COMMIT" ]
  /usr/bin/mount --bind /opt/agent-forge/bin /opt/agent-forge/bin
  BIND_MOUNT_ACTIVE=1

  bind_log="$TMP/bind-mount-upgrade.log"
  bind_pipe="$TMP/bind-mount-upgrade.pipe"
  /usr/bin/mkfifo "$bind_pipe"
  /usr/bin/python3 "$DIAGNOSTICS" capture "$bind_pipe" "$bind_log" &
  capture_pid=$!
  if "$BOOTSTRAP" install \
    --version "$VERSION" \
    --commit "$COMMIT" \
    --asset-dir "$ASSET_DIR" \
    --sha256sums-sha256 "$ANCHOR" \
    --upgrade --enable-now >"$bind_pipe" 2>&1; then
    bind_upgrade_rc=0
  else
    bind_upgrade_rc=$?
  fi
  wait "$capture_pid"
  /usr/bin/rm -f "$bind_pipe"
  [ "$bind_upgrade_rc" -ne 0 ] || { echo "bind-mounted upgrade unexpectedly succeeded" >&2; exit 1; }
  [ "$(/usr/bin/python3 "$DIAGNOSTICS" parse "$bind_log")" = publication ] || {
    echo "bind-mounted upgrade did not fail during publication" >&2
    exit 1
  }
  /usr/bin/python3 - "$bind_log" <<'PY'
import sys

with open(sys.argv[1], "rb") as handle:
    output = handle.read(4097)
if len(output) > 4096:
    raise SystemExit("bind-mounted upgrade output exceeded bound")
tokens = []
for path in ("/opt/agent-forge/secrets/gate.env", "/opt/agent-forge/secrets/worker.env"):
    with open(path, "rb") as handle:
        tokens.extend(line.partition(b"=")[2].strip() for line in handle if b"=" in line)
if any(token and token in output for token in tokens):
    raise SystemExit("token appeared in bind-mounted upgrade output")
PY
  assert_active agent-forge-gate.service
  assert_active agent-forge-worker.service
  [ "$(systemctl show --property=MainPID --value agent-forge-gate.service)" = "$GATE_PID_BEFORE" ]
  [ "$(systemctl show --property=MainPID --value agent-forge-worker.service)" = "$WORKER_PID_BEFORE" ]
  [ "$(systemctl show --property=InvocationID --value agent-forge-gate.service)" = "$GATE_INVOCATION_BEFORE" ]
  [ "$(systemctl show --property=InvocationID --value agent-forge-worker.service)" = "$WORKER_INVOCATION_BEFORE" ]
  [ "$(/opt/agent-forge/bin/forge --version)" = "forge $OLD_VERSION $OLD_COMMIT" ]
  [ ! -e /opt/.agent-forge.previous ] && [ ! -L /opt/.agent-forge.previous ]
  /usr/bin/umount /opt/agent-forge/bin
  BIND_MOUNT_ACTIVE=0

  STAGE=seed-upgrade-preservation-markers
  /usr/sbin/runuser -u agent-forge -- /usr/bin/python3 - <<'PY'
import sqlite3
from pathlib import Path

with sqlite3.connect("/opt/agent-forge/var/gate/state/forge.db") as database:
    database.execute(
        "CREATE TABLE privileged_e2e_marker (name TEXT PRIMARY KEY, value TEXT NOT NULL)"
    )
    database.execute(
        "INSERT INTO privileged_e2e_marker VALUES (?, ?)",
        ("preserved", "agent-forge-privileged-e2e-v1"),
    )
for path in (
    "/opt/agent-forge/var/repositories/.privileged-e2e-marker",
    "/opt/agent-forge/var/worker/worktrees/.privileged-e2e-marker",
    "/opt/agent-forge/var/worker/runtime/.privileged-e2e-marker",
):
    Path(path).write_bytes(b"agent-forge-privileged-e2e-v1\n")
PY
  UPGRADE_BEFORE="$TMP/upgrade-before"
  UPGRADE_AFTER="$TMP/upgrade-after"
  snapshot_upgrade_preserved_state "$UPGRADE_BEFORE"
  STAGE=exact-upgrade
  install_release "$BOOTSTRAP" "$VERSION" "$COMMIT" "$ASSET_DIR" "$ANCHOR" upgrade
  STAGE=upgrade-installed-identity
  [ "$(/opt/agent-forge/bin/forge --version)" = "forge $VERSION $COMMIT" ]
  assert_active agent-forge-gate.service
  assert_active agent-forge-worker.service
  STAGE=upgrade-authenticated-readiness
  wait_worker
  if ! /opt/agent-forge/bin/forge doctor >"$TMP/doctor-output" 2>&1; then
    echo "installed candidate doctor failed" >&2
    exit 1
  fi
  cmp "$TMP/doctor-output" /dev/stdin <<'EOF' || { echo "installed candidate doctor output mismatch" >&2; exit 1; }
PASS receipt
PASS cli-identity
PASS trusted-paths
PASS immutable-paths
PASS mutable-paths
PASS gate-enabled
PASS gate-active
PASS worker-enabled
PASS worker-active
PASS gate-readiness
PASS worker-readiness
PASS store-schema
RESULT healthy
EOF
  STAGE=upgrade-token-exposure
  assert_tokens_not_exposed
  snapshot_upgrade_preserved_state "$UPGRADE_AFTER"
  cmp "$UPGRADE_BEFORE" "$UPGRADE_AFTER" || { echo "upgrade changed preserved state" >&2; exit 1; }

  STAGE=explicit-rollback
  /opt/agent-forge/bin/forge rollback >"$TMP/rollback-output" 2>&1
  [ ! -s "$TMP/rollback-output" ] || { echo "rollback emitted unexpected output" >&2; exit 1; }
  for component in forge forge-gate forge-worker forge-codex-plugin forge-ref-plugin; do
    [ "$("/opt/agent-forge/bin/$component" --version)" = "$component $OLD_VERSION $OLD_COMMIT" ] || {
      echo "rollback binary identity mismatch: $component" >&2
      exit 1
    }
  done
  /usr/bin/python3 - "$OLD_VERSION" "$OLD_COMMIT" "$VERSION" "$COMMIT" <<'PY'
import hashlib
import json
import os
import stat
import sys

with open("/opt/agent-forge/install-receipt.json", "rb") as handle:
    receipt = json.load(handle)
if receipt.get("version") != sys.argv[1] or receipt.get("commit") != sys.argv[2]:
    raise SystemExit("rollback receipt identity mismatch")

slot = "/opt/.agent-forge.previous"
with open(slot + "/install-receipt.json", "rb") as handle:
    candidate = json.load(handle)
if candidate.get("version") != sys.argv[3] or candidate.get("commit") != sys.argv[4]:
    raise SystemExit("rollback slot identity mismatch")

expected_entries = {
    slot: {"bin", "systemd", "install-receipt.json"},
    slot + "/bin": {"forge", "forge-gate", "forge-worker", "forge-codex-plugin", "forge-ref-plugin"},
    slot + "/systemd": {"agent-forge-gate.service", "agent-forge-worker.service"},
}
for path, expected in expected_entries.items():
    if set(os.listdir(path)) != expected:
        raise SystemExit("rollback slot entry set mismatch")

expected_modes = {
    "": 0o700,
    "bin": 0o755,
    "systemd": 0o755,
    "install-receipt.json": 0o400,
    "bin/forge": 0o755,
    "bin/forge-gate": 0o755,
    "bin/forge-worker": 0o755,
    "bin/forge-codex-plugin": 0o755,
    "bin/forge-ref-plugin": 0o755,
    "systemd/agent-forge-gate.service": 0o644,
    "systemd/agent-forge-worker.service": 0o644,
}
for name, mode in expected_modes.items():
    metadata = os.lstat(os.path.join(slot, name))
    want_type = stat.S_IFDIR if name in ("", "bin", "systemd") else stat.S_IFREG
    if stat.S_IFMT(metadata.st_mode) != want_type or stat.S_IMODE(metadata.st_mode) != mode or metadata.st_uid != 0 or metadata.st_gid != 0:
        raise SystemExit("rollback slot metadata mismatch")

release_files = set(expected_modes) - {"", "bin", "systemd", "install-receipt.json"}
for name in release_files:
    with open(os.path.join(slot, name), "rb") as handle:
        digest = hashlib.file_digest(handle, "sha256").hexdigest()
    if candidate.get("files", {}).get(name) != digest or candidate.get("modes", {}).get(name) != expected_modes[name]:
        raise SystemExit("rollback slot receipt binding mismatch")
for forbidden in ("etc", "secrets", "var"):
    if os.path.lexists(os.path.join(slot, forbidden)):
        raise SystemExit("rollback slot contains mutable state")
PY
  assert_active agent-forge-gate.service
  assert_active agent-forge-worker.service
  STAGE=rollback-authenticated-readiness
  wait_worker
  STAGE=rollback-token-exposure
  assert_tokens_not_exposed
  snapshot_upgrade_preserved_state "$UPGRADE_AFTER"
  cmp "$UPGRADE_BEFORE" "$UPGRADE_AFTER" || { echo "rollback changed preserved state" >&2; exit 1; }
fi

BEFORE="$TMP/before"
AFTER="$TMP/after"
snapshot_validation_only_state "$BEFORE"
STAGE=validation-only-rerun
if [ -n "$OLD_VERSION" ]; then
  install_release "$OLD_BOOTSTRAP" "$OLD_VERSION" "$OLD_COMMIT" "$OLD_ASSET_DIR" "$OLD_ANCHOR" validate
else
  install_release "$BOOTSTRAP" "$VERSION" "$COMMIT" "$ASSET_DIR" "$ANCHOR" validate
fi
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
if [ -n "$OLD_VERSION" ]; then
  [ "$(/opt/agent-forge/bin/forge --version)" = "forge $OLD_VERSION $OLD_COMMIT" ]
else
  [ "$(/opt/agent-forge/bin/forge --version)" = "forge $VERSION $COMMIT" ]
fi
STAGE=complete
echo "privileged Linux installer e2e: PASS"
