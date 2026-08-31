#!/bin/sh
set -eu
export LC_ALL=C
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
unset CDPATH ENV BASH_ENV GLOBIGNORE POSIXLY_CORRECT TAR_OPTIONS GZIP GOFIPS140
umask 077

BASELINE_VERSION=v0.1.6
BASELINE_COMMIT=34fc31ffd8b19597bb987a382509139849a21a95
STAGE=argument-validation
TMP=
CLEANUP_NEEDED=0

finish() {
  rc=$?
  trap - EXIT
  set +e
  if [ "$CLEANUP_NEEDED" -eq 1 ] && [ -x /opt/agent-forge/bin/forge ]; then
    /opt/agent-forge/bin/forge uninstall >/dev/null 2>&1 || rc=1
  fi
  if [ "$rc" -ne 0 ]; then
    printf '::error title=Online installer acceptance::stage=%s exit=%s\n' "$STAGE" "$rc" >&2
    account=0 install=0 gate_link=0 worker_link=0
    getent passwd agent-forge >/dev/null 2>&1 && account=1
    [ ! -e /opt/agent-forge ] && [ ! -L /opt/agent-forge ] || install=1
    [ ! -e /etc/systemd/system/agent-forge-gate.service ] && [ ! -L /etc/systemd/system/agent-forge-gate.service ] || gate_link=1
    [ ! -e /etc/systemd/system/agent-forge-worker.service ] && [ ! -L /etc/systemd/system/agent-forge-worker.service ] || worker_link=1
    printf '::error title=Online installer state::account=%s install=%s gate_link=%s worker_link=%s\n' \
      "$account" "$install" "$gate_link" "$worker_link" >&2
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
CANDIDATE_VERSION=$1
CANDIDATE_COMMIT=$2
ASSET_DIR=$3
case "$0" in
  /*/linux-online-installer-privileged-e2e.sh) DIAGNOSTICS=${0%/*}/linux-installer-diagnostics.py ;;
  *) echo "online privileged E2E requires an absolute script path" >&2; exit 1 ;;
esac
printf '%s\n' "$CANDIDATE_VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || usage
printf '%s\n' "$CANDIDATE_COMMIT" | grep -Eq '^[0-9a-f]{40}$' || usage
case "$ASSET_DIR" in /*) ;; *) usage ;; esac

# This test mutates account, /opt, and systemd state and is disposable-runner only.
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
[ -f "$DIAGNOSTICS" ] && [ ! -L "$DIAGNOSTICS" ] || { echo "missing installer diagnostics helper" >&2; exit 1; }
for path in /usr/bin/chmod /usr/bin/getent /usr/bin/grep /usr/bin/id /usr/bin/mkfifo /usr/bin/mktemp /usr/bin/python3 /usr/bin/readlink /usr/bin/rm /usr/bin/sha256sum /usr/bin/stat /usr/bin/systemctl /usr/bin/tar /usr/bin/uname; do
  [ -x "$path" ] || { echo "invalid privileged tool: $path" >&2; exit 1; }
done
/usr/bin/python3 "$DIAGNOSTICS" self-test

if getent passwd agent-forge >/dev/null || getent group agent-forge >/dev/null || [ -e /opt/agent-forge ] || [ -L /opt/agent-forge ]; then
  echo "runner is not clean for Agent Forge installation" >&2
  exit 1
fi
for unit in agent-forge-gate.service agent-forge-worker.service; do
  for root in \
    /etc/systemd/system.control /run/systemd/system.control /run/systemd/transient \
    /run/systemd/generator.early /etc/systemd/system /etc/systemd/system.attached \
    /run/systemd/system /run/systemd/system.attached /run/systemd/generator \
    /usr/local/lib/systemd/system /usr/lib/systemd/system /lib/systemd/system \
    /run/systemd/generator.late; do
    for path in "$root/$unit" "$root/$unit.d" "$root/agent-forge-.service.d" "$root/agent-.service.d" "$root/service.d"; do
      [ ! -e "$path" ] && [ ! -L "$path" ] || { echo "runner has pre-existing Agent Forge unit configuration" >&2; exit 1; }
    done
    for path in "$root"/*.wants/"$unit" "$root"/*.requires/"$unit"; do
      [ ! -e "$path" ] && [ ! -L "$path" ] || { echo "runner has pre-existing Agent Forge unit enablement" >&2; exit 1; }
    done
  done
  [ "$(systemctl show --property=LoadState --value "$unit")" = not-found ] || { echo "runner has a loaded Agent Forge unit" >&2; exit 1; }
done

# GitHub's Ubuntu image intentionally makes /opt 0777. Normalize only after
# the clean-host checks, and only that documented root-owned fixture.
STAGE=runner-normalization
[ -d /opt ] && [ ! -L /opt ] || { echo "invalid /opt fixture" >&2; exit 1; }
case "$(stat -c '%a:%u:%g' /opt)" in
  755:0:0) ;;
  777:0:0) chmod 0755 /opt ;;
  *) echo "unexpected /opt fixture metadata" >&2; exit 1 ;;
esac
[ "$(stat -c '%a:%u:%g' /opt)" = 755:0:0 ] || { echo "failed to normalize /opt fixture" >&2; exit 1; }

ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported runner architecture" >&2; exit 1 ;;
esac
CLI_ARCHIVE="agent-forge-cli_${CANDIDATE_VERSION}_linux_${ARCH}.tar.gz"
STAGE=candidate-verification
[ -f "$ASSET_DIR/$CLI_ARCHIVE" ] && [ ! -L "$ASSET_DIR/$CLI_ARCHIVE" ] || { echo "missing candidate CLI archive" >&2; exit 1; }
(cd "$ASSET_DIR" && sha256sum --check --strict SHA256SUMS >/dev/null)
TMP=$(mktemp -d "$RUNNER_TEMP/agent-forge-online.XXXXXX")
tar --extract --gzip --file "$ASSET_DIR/$CLI_ARCHIVE" --directory "$TMP" --no-same-owner --no-same-permissions
CANDIDATE="$TMP/agent-forge-cli_${CANDIDATE_VERSION}_linux_${ARCH}/forge"
[ -x "$CANDIDATE" ] && [ ! -L "$CANDIDATE" ] || { echo "invalid candidate forge" >&2; exit 1; }
[ "$("$CANDIDATE" --version)" = "forge $CANDIDATE_VERSION $CANDIDATE_COMMIT" ] || { echo "candidate identity mismatch" >&2; exit 1; }

STAGE=online-install
install_log="$TMP/install.log"
install_pipe="$TMP/install.pipe"
mkfifo "$install_pipe"
/usr/bin/python3 "$DIAGNOSTICS" capture "$install_pipe" "$install_log" &
capture_pid=$!
CLEANUP_NEEDED=1
if "$CANDIDATE" install --version "$BASELINE_VERSION" >"$install_pipe" 2>&1; then install_rc=0; else install_rc=$?; fi
wait "$capture_pid"
rm -f "$install_pipe"
if [ "$install_rc" -ne 0 ]; then
  install_stage=$(/usr/bin/python3 "$DIAGNOSTICS" parse "$install_log")
  [ -z "$install_stage" ] || printf '::error title=Online installer stage::install_stage=%s\n' "$install_stage" >&2
  exit 1
fi
[ ! -s "$install_log" ] || { echo "online install emitted unexpected output" >&2; exit 1; }

STAGE=installed-contract
[ "$(/opt/agent-forge/bin/forge --version)" = "forge $BASELINE_VERSION $BASELINE_COMMIT" ] || { echo "installed identity mismatch" >&2; exit 1; }
/usr/bin/python3 - "$BASELINE_VERSION" "$BASELINE_COMMIT" "$ARCH" <<'PY'
import hashlib
import json
import os
import stat
import sys

root = "/opt/agent-forge"
with open(root + "/install-receipt.json", "rb") as handle:
    receipt = json.load(handle)
if (receipt.get("version"), receipt.get("commit"), receipt.get("architecture")) != tuple(sys.argv[1:4]):
    raise SystemExit("receipt identity mismatch")
expected = {
    "bin/forge": 0o755, "bin/forge-gate": 0o755, "bin/forge-worker": 0o755,
    "bin/forge-codex-plugin": 0o755, "bin/forge-ref-plugin": 0o755,
    "etc/gate.json": 0o440, "etc/worker.json": 0o440,
    "secrets/gate.env": 0o600, "secrets/worker.env": 0o600,
    "systemd/agent-forge-gate.service": 0o644,
    "systemd/agent-forge-worker.service": 0o644,
}
if set(receipt.get("files", {})) != set(expected) or receipt.get("modes") != {name: mode for name, mode in expected.items()}:
    raise SystemExit("receipt immutable object set mismatch")
for name, mode in expected.items():
    path = os.path.join(root, name)
    metadata = os.lstat(path)
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1 or stat.S_IMODE(metadata.st_mode) != mode or metadata.st_uid != 0:
        raise SystemExit("immutable object metadata mismatch")
    with open(path, "rb") as handle:
        digest = hashlib.file_digest(handle, "sha256").hexdigest()
    if receipt["files"][name] != digest:
        raise SystemExit("immutable object digest mismatch")
metadata = os.lstat(root + "/install-receipt.json")
if not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1 or stat.S_IMODE(metadata.st_mode) != 0o400 or metadata.st_uid != 0:
    raise SystemExit("receipt metadata mismatch")
PY
for unit in agent-forge-gate.service agent-forge-worker.service; do
  [ "$(systemctl is-active "$unit" 2>/dev/null || true)" != active ]
  ! systemctl is-enabled "$unit" >/dev/null 2>&1
done
[ "$(readlink /etc/systemd/system/agent-forge-gate.service)" = /opt/agent-forge/systemd/agent-forge-gate.service ]
[ "$(readlink /etc/systemd/system/agent-forge-worker.service)" = /opt/agent-forge/systemd/agent-forge-worker.service ]

STAGE=installed-uninstall
uninstall_log="$TMP/uninstall.log"
uninstall_pipe="$TMP/uninstall.pipe"
mkfifo "$uninstall_pipe"
/usr/bin/python3 "$DIAGNOSTICS" capture "$uninstall_pipe" "$uninstall_log" &
capture_pid=$!
if /opt/agent-forge/bin/forge uninstall >"$uninstall_pipe" 2>&1; then uninstall_rc=0; else uninstall_rc=$?; fi
wait "$capture_pid"
rm -f "$uninstall_pipe"
if [ "$uninstall_rc" -ne 0 ]; then
  uninstall_stage=$(/usr/bin/python3 "$DIAGNOSTICS" parse "$uninstall_log")
  [ -z "$uninstall_stage" ] || printf '::error title=Online uninstall stage::install_stage=%s\n' "$uninstall_stage" >&2
  exit 1
fi
[ ! -s "$uninstall_log" ] || { echo "uninstall emitted unexpected output" >&2; exit 1; }
CLEANUP_NEEDED=0
for path in \
  /opt/agent-forge/bin/forge /opt/agent-forge/bin/forge-gate /opt/agent-forge/bin/forge-worker \
  /opt/agent-forge/bin/forge-codex-plugin /opt/agent-forge/bin/forge-ref-plugin \
  /opt/agent-forge/systemd/agent-forge-gate.service /opt/agent-forge/systemd/agent-forge-worker.service \
  /opt/agent-forge/install-receipt.json /opt/.agent-forge.previous \
  /etc/systemd/system/agent-forge-gate.service /etc/systemd/system/agent-forge-worker.service; do
  [ ! -e "$path" ] && [ ! -L "$path" ] || { echo "uninstall retained release object" >&2; exit 1; }
done
for path in /opt/.agent-forge.uninstall-*; do
  [ ! -e "$path" ] && [ ! -L "$path" ] || { echo "uninstall retained quarantine" >&2; exit 1; }
done
for unit in agent-forge-gate.service agent-forge-worker.service; do
  [ "$(systemctl show --property=LoadState --value "$unit")" = not-found ]
done
getent passwd agent-forge >/dev/null
getent group agent-forge >/dev/null
for path in /opt/agent-forge/etc /opt/agent-forge/secrets /opt/agent-forge/var; do [ -d "$path" ] && [ ! -L "$path" ]; done

STAGE=complete
echo "online installer privileged e2e: PASS"
