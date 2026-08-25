#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$ROOT"
RUN=$(mktemp -d)
GATE_PID=
WORKER_PID=
RAW_PID=
OWNER_TOKEN=synthetic-owner-recovery-e2e
WORKER_TOKEN=synthetic-worker-recovery-e2e
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
HTTP=http://127.0.0.1:$PORT
WS=ws://127.0.0.1:$PORT

cleanup() {
	for pid in "$WORKER_PID" "$RAW_PID" "$GATE_PID"; do
		[ -z "$pid" ] || kill "$pid" 2>/dev/null || true
	done
	for pid in "$WORKER_PID" "$RAW_PID" "$GATE_PID"; do
		[ -z "$pid" ] || wait "$pid" 2>/dev/null || true
	done
	rm -rf "$RUN"
}
trap cleanup EXIT INT TERM

go build -o "$RUN/forge-gate" ./cmd/forge-gate
go build -o "$RUN/forge-worker" ./cmd/forge-worker
go build -o "$RUN/forge-ref-plugin" ./cmd/forge-ref-plugin
go build -o "$RUN/raw-worker" ./internal/gate/testdata/raw-worker

start_gate() {
	FORGE_OWNER_TOKEN=$OWNER_TOKEN FORGE_WORKER_TOKEN=$WORKER_TOKEN "$RUN/forge-gate" \
		-addr "127.0.0.1:$PORT" -db "$RUN/forge.db" -worker-id worker-1 \
		-lease-ttl 300ms -retry-base 200ms -max-attempts 3 -recovery-interval 50ms >"$RUN/gate.log" 2>&1 &
	GATE_PID=$!
	i=0
	until curl -fsS -o /dev/null -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/debug/jobs" 2>/dev/null; do
		i=$((i+1)); [ "$i" -lt 100 ] || { python3 -c 'import itertools,sys; print("".join(itertools.islice(open(sys.argv[1], errors="replace"), 80)), end="")' "$RUN/gate.log"; exit 1; }
		sleep 0.02
	done
}

start_gate
SUBMIT=$(curl -fsS -X POST "$HTTP/v1/jobs" -H "Authorization: Bearer $OWNER_TOKEN" -H 'Content-Type: application/json' -d '{"input":"recover me"}')
JOB_ID=$(printf '%s' "$SUBMIT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
"$RUN/raw-worker" -gate "$WS" -token "$WORKER_TOKEN" -mode abandon >"$RUN/first.json" 2>"$RUN/raw.log" &
RAW_PID=$!
i=0
until [ -s "$RUN/first.json" ]; do
	i=$((i+1)); [ "$i" -lt 100 ] || { echo "first lease missing"; exit 1; }
	sleep 0.02
done
FIRST_ATTEMPT=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["attempt_id"])' "$RUN/first.json")

kill "$GATE_PID"
wait "$GATE_PID" 2>/dev/null || true
GATE_PID=
wait "$RAW_PID" 2>/dev/null || true
RAW_PID=
python3 - "$RUN/forge.db" "$FIRST_ATTEMPT" <<'PY'
import sqlite3,sys,time
db,attempt=sys.argv[1:]
while True:
    deadline=sqlite3.connect(db).execute("select deadline_at from attempts where id=?",(attempt,)).fetchone()[0]
    if time.time_ns() >= deadline: break
    time.sleep(.01)
PY

start_gate
FORGE_WORKER_TOKEN=$WORKER_TOKEN "$RUN/forge-worker" -gate "$WS" -id worker-1 \
	-heartbeat-interval 50ms -plugin "$RUN/forge-ref-plugin" >"$RUN/worker.log" 2>&1 &
WORKER_PID=$!
i=0
while :; do
	JOB=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/jobs/$JOB_ID")
	STATUS=$(printf '%s' "$JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
	[ "$STATUS" = succeeded ] && break
	i=$((i+1)); [ "$i" -lt 150 ] || { echo "retry did not succeed: $JOB"; exit 1; }
	sleep 0.02
done
kill "$WORKER_PID"
wait "$WORKER_PID" 2>/dev/null || true
WORKER_PID=
i=0
until curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/workers/worker-1" | python3 -c 'import json,sys; assert not json.load(sys.stdin)["connected"]' 2>/dev/null; do
	i=$((i+1)); [ "$i" -lt 100 ] || { echo "worker did not disconnect"; exit 1; }
	sleep 0.02
done
LATE=$("$RUN/raw-worker" -gate "$WS" -token "$WORKER_TOKEN" -mode late -job "$JOB_ID" -attempt "$FIRST_ATTEMPT")
EVENTS=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/jobs/$JOB_ID/events")
TIMELINE=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/debug/jobs/$JOB_ID")
printf '%s\n' "$EVENTS" >"$RUN/events.json"
printf '%s\n' "$TIMELINE" >"$RUN/timeline.json"
python3 - "$RUN/forge.db" "$JOB_ID" "$FIRST_ATTEMPT" "$RUN/events.json" "$RUN/timeline.json" <<'PY'
import json,sqlite3,sys
db,job,first,events_path,timeline_path=sys.argv[1:]
attempts=sqlite3.connect(db).execute("select id,ordinal,status,leased_at,completed_at from attempts where job_id=? order by ordinal",(job,)).fetchall()
assert len(attempts)==2 and attempts[0][0]==first and attempts[0][1:3]==(1,"expired") and attempts[1][1:3]==(2,"succeeded"), attempts
assert attempts[1][3] >= attempts[0][4] + 200_000_000, attempts
events=json.load(open(events_path))
kinds=[event["kind"] for event in events]
assert kinds.count("lease_expired")==1 and kinds.count("retry_scheduled")==1, kinds
timeline=json.load(open(timeline_path))
leased=[event for event in timeline["events"] if event["type"]=="leased"]
assert [event["attempt_ordinal"] for event in leased]==[1,2], leased
assert all("lease_expires_at" in event for event in leased), leased
PY
[ "$LATE" = late_rejected ]
echo "RECOVERY E2E PASS attempts=1->2 expiry=1 retry=1 late=rejected"
