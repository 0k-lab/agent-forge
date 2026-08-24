#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
RUN="$ROOT/.e2e-run"
rm -rf "$RUN"
mkdir -p "$RUN" "$ROOT/evidence"
GATE_PID=
WORKER_PID=
OWNER_TOKEN=fake-owner-token-reference-e2e
WORKER_TOKEN=fake-worker-token-reference-e2e
cleanup() {
  [ -z "$WORKER_PID" ] || kill "$WORKER_PID" 2>/dev/null || true
  [ -z "$GATE_PID" ] || kill "$GATE_PID" 2>/dev/null || true
  [ -z "$WORKER_PID" ] || wait "$WORKER_PID" 2>/dev/null || true
  [ -z "$GATE_PID" ] || wait "$GATE_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
FORGE_OWNER_TOKEN=$OWNER_TOKEN FORGE_WORKER_TOKEN=$WORKER_TOKEN "$ROOT/bin/forge-gate" -addr 127.0.0.1:18080 -db "$RUN/forge.db" -worker-id worker-1 >"$RUN/gate.log" 2>&1 &
GATE_PID=$!
i=0
until curl -sS -o /dev/null -H "Authorization: Bearer $OWNER_TOKEN" http://127.0.0.1:18080/v1/workers/worker-1 2>/dev/null; do
  i=$((i+1)); [ "$i" -lt 50 ] || { echo "gate failed to become ready"; exit 1; }; sleep 0.1
done
FORGE_WORKER_TOKEN=$WORKER_TOKEN "$ROOT/bin/forge-worker" -gate ws://127.0.0.1:18080 -id worker-1 -plugin "$ROOT/bin/forge-ref-plugin" >"$RUN/worker.log" 2>&1 &
WORKER_PID=$!
i=0
until curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" http://127.0.0.1:18080/v1/workers/worker-1 2>/dev/null | python3 -c 'import json,sys; assert json.load(sys.stdin)["connected"]' 2>/dev/null; do
  i=$((i+1)); [ "$i" -lt 50 ] || { echo "worker failed to connect"; exit 1; }; sleep 0.1
done
SUBMIT=$(curl -fsS -X POST http://127.0.0.1:18080/v1/jobs -H "Authorization: Bearer $OWNER_TOKEN" -H 'Content-Type: application/json' -d '{"input":"hello agent forge"}')
JOB_ID=$(printf '%s' "$SUBMIT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
i=0
while :; do
  JOB=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "http://127.0.0.1:18080/v1/jobs/$JOB_ID")
  STATUS=$(printf '%s' "$JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
  [ "$STATUS" = succeeded ] && break
  i=$((i+1)); [ "$i" -lt 100 ] || { echo "job did not finish: $JOB"; exit 1; }; sleep 0.1
done
EVENTS=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "http://127.0.0.1:18080/v1/jobs/$JOB_ID/events")
LIVE=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" http://127.0.0.1:18080/v1/workers/worker-1)
kill "$WORKER_PID"
wait "$WORKER_PID" 2>/dev/null || true
WORKER_PID=
i=0
while :; do
  DEAD=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" http://127.0.0.1:18080/v1/workers/worker-1)
  CONNECTED=$(printf '%s' "$DEAD" | python3 -c 'import json,sys; print(str(json.load(sys.stdin)["connected"]).lower())')
  [ "$CONNECTED" = false ] && break
  i=$((i+1)); [ "$i" -lt 50 ] || { echo "worker remained connected: $DEAD"; exit 1; }; sleep 0.1
done
{
  echo "E2E PASS"
  echo "submit=$SUBMIT"
  echo "terminal_job=$JOB"
  echo "events=$EVENTS"
  echo "worker_live=$LIVE"
  echo "worker_after_stop=$DEAD"
} | tee "$ROOT/evidence/e2e-output.txt"
