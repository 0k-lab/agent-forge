#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
RUN=$(mktemp -d)
PIDS=()
trap 'for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done; rm -rf -- "$RUN"' EXIT
chmod 700 "$RUN"

go build -o "$RUN/forge-gate" "$ROOT/cmd/forge-gate"

start_gate() {
	local db=$1 log=$2
	python3 "$ROOT/scripts/write-configs.py" "$log.gate.json" "$log.worker.json" 127.0.0.1:0 "$db" ws://127.0.0.1:1 worker-1 unused - reference /bin/true 30s 1s 3 10s
	FORGE_OWNER_TOKEN=synthetic-owner FORGE_WORKER_TOKEN=synthetic-worker \
		"$RUN/forge-gate" -config "$log.gate.json" >"$log" 2>&1 &
	PID=$!
	PIDS+=("$PID")
	for _ in {1..100}; do
		grep -q '"event":"listening"' "$log" && return
		kill -0 "$PID" 2>/dev/null || break
		sleep 0.05
	done
	return 1
}

stop_gate() {
	kill "$PID"
	wait "$PID"
	PIDS=("${PIDS[@]/$PID}")
}

expect_failure() {
	local db=$1 log=$2
	python3 "$ROOT/scripts/write-configs.py" "$log.gate.json" "$log.worker.json" 127.0.0.1:0 "$db" ws://127.0.0.1:1 worker-1 unused - reference /bin/true 30s 1s 3 10s
	if FORGE_OWNER_TOKEN=synthetic-owner FORGE_WORKER_TOKEN=synthetic-worker \
		"$RUN/forge-gate" -config "$log.gate.json" >"$log" 2>&1; then
		return 1
	fi
	! grep -q '"event":"listening"' "$log"
}

private=$RUN/private
mkdir -m 700 "$private"
: >"$RUN/create.log"
chmod 600 "$RUN/create.log"
saved_umask=$(umask)
umask 0777
start_gate "$private/created.db" "$RUN/create.log"
umask "$saved_umask"
stop_gate
[[ $(stat -c %a "$private/created.db") == 600 ]]

public=$RUN/synthetic-public-parent
mkdir -m 755 "$public"
public_db=$public/missing.db
public_mode=$(stat -c %a "$public")
expect_failure "$public_db" "$RUN/public.log"
[[ ! -e $public_db ]]
[[ $(stat -c %a "$public") == "$public_mode" ]]
! grep -Fq "$(basename "$public")" "$RUN/public.log"
! grep -Fq "$public" "$RUN/public.log"

insecure=$private/synthetic-private-path.db
: >"$insecure"
chmod 644 "$insecure"
before=$(stat -c '%i:%a' "$insecure")
expect_failure "$insecure" "$RUN/insecure.log"
[[ $(stat -c '%i:%a' "$insecure") == "$before" ]]
! grep -Eq '"event":"listening"|synthetic-private-path' "$RUN/insecure.log"

malformed="file:$private/synthetic-credential%zz.db"
expect_failure "$malformed" "$RUN/malformed.log"
! grep -Eq '"event":"listening"|synthetic-credential' "$RUN/malformed.log"

start_gate :memory: "$RUN/memory.log"
stop_gate

echo "sqlite permissions e2e: PASS"
