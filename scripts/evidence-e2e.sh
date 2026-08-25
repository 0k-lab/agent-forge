#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
RUN=$(mktemp -d)
REPO="$RUN/repo"
GATE_PID=
WORKER_PID=
OWNER_TOKEN=synthetic-owner-evidence-e2e
WORKER_TOKEN=synthetic-worker-evidence-e2e
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
HTTP="http://127.0.0.1:$PORT"
WS="ws://127.0.0.1:$PORT"

curl() { command curl --connect-timeout 1 --max-time 2 "$@"; }

cleanup() {
	for pid in "$WORKER_PID" "$GATE_PID"; do
		[ -z "$pid" ] || kill "$pid" 2>/dev/null || true
	done
	for pid in "$WORKER_PID" "$GATE_PID"; do
		[ -z "$pid" ] || wait "$pid" 2>/dev/null || true
	done
	rm -rf "$RUN"
}
trap cleanup EXIT INT TERM

cd "$ROOT"
go build -o "$RUN/forge-gate" ./cmd/forge-gate
go build -o "$RUN/forge-worker" ./cmd/forge-worker
go build -o "$RUN/raw-worker" ./internal/gate/testdata/raw-worker

mkdir -p "$REPO"
git -C "$REPO" init -q -b main
git -C "$REPO" config user.name "Evidence E2E"
git -C "$REPO" config user.email "evidence@example.invalid"
cat >"$REPO/answer.txt" <<'EOF'
before
EOF
cat >"$REPO/check.sh" <<EOF
#!/bin/sh
printf '%s\n' 'Authorization: Bearer synthetic-e2e-secret'
printf '%s\n' 'PRIVATE_ENV=synthetic-env-secret'
printf '%s\n' 'https://synthetic-output-user:synthetic-output-pass@example.invalid/?token=synthetic-output-query'
printf '%s\n' 'CrEdEnTiAl: synthetic-output-colon'
printf '%s\n' 'workspace:/synthetic/private/output-file'
printf '%s\n' 'config:C:\synthetic\private\output-file'
printf '%s\n' '{"Authorization":"Bearer synthetic-output-json secret","safe":"kept"}'
printf '%s\n' '["Authorization","Bearer synthetic-output-json-array-secret"]'
printf '%s\n' 'file:///private/reviewer/evidence-output'
printf '%s\n' 'synthetic-output-unlabeled-secret'
printf '%s\n' 'check --token synthetic-output-cli secret --later tail; safe'
printf '%s\n' '[credential]: synthetic-output-bracket secret'
printf '%s\n' 'Access Token: synthetic-output-access-token; safe'
printf '%s\n' '[Client Secret] = synthetic-output-client-secret&safe=kept'
printf '%s\n' 'check --access token synthetic-output-cli-access-token; safe'
printf '%s\n' 'workspace: \\synthetic-output-server\private\file'
printf '%s\n' '$REPO/private-file'
pwd
python3 -c 'import sys; sys.stdout.write("https://synthetic-partial-user:" + "x" * 5000 + "@example.invalid")'
exit 7
EOF
chmod +x "$REPO/check.sh"
git -C "$REPO" add .
git -C "$REPO" commit -qm "test: synthetic evidence failure"
BASE=$(git -C "$REPO" rev-parse HEAD)
cat >"$RUN/plugin" <<'EOF'
#!/usr/bin/env python3
import json,pathlib,sys
initialize=json.loads(sys.stdin.readline())
plugin_id=initialize["id"]
print(json.dumps({"version":"v1","id":plugin_id,"type":"initialized","capabilities":["workspace_edit"]},separators=(",",":")),flush=True)
request=json.loads(sys.stdin.readline())
pathlib.Path(request["workspace"],"answer.txt").write_text("after\n")
print(json.dumps({"version":"v1","id":plugin_id,"type":"result"},separators=(",",":")),flush=True)
EOF
chmod +x "$RUN/plugin"

start_gate() {
	python3 "$ROOT/scripts/write-configs.py" "$RUN/gate.json" "$RUN/worker.json" "127.0.0.1:$PORT" "$RUN/forge.db" "$WS" worker-1 synthetic "$REPO" evidence "$RUN/plugin" 5s 100ms 2 20ms
	FORGE_OWNER_TOKEN=$OWNER_TOKEN FORGE_WORKER_TOKEN=$WORKER_TOKEN "$RUN/forge-gate" \
		-config "$RUN/gate.json" >"$RUN/gate.log" 2>&1 &
	GATE_PID=$!
	i=0
	until curl -fsS -o /dev/null -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/debug/jobs" 2>/dev/null; do
		i=$((i+1)); [ "$i" -lt 200 ] || { echo "evidence E2E: Gate readiness failed"; exit 1; }
		sleep 0.02
	done
}

start_gate
FORGE_WORKER_TOKEN=$WORKER_TOKEN "$RUN/forge-worker" -config "$RUN/worker.json" >"$RUN/worker.log" 2>&1 &
WORKER_PID=$!
i=0
until curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/workers/worker-1" 2>/dev/null | python3 -c 'import json,sys; assert json.load(sys.stdin)["connected"]' 2>/dev/null; do
	i=$((i+1)); [ "$i" -lt 200 ] || { echo "evidence E2E: Worker readiness failed"; exit 1; }
	sleep 0.02
done

SUBMIT=$(python3 - "$BASE" <<'PY' | curl -fsS -X POST "$HTTP/v1/jobs" -H "Authorization: Bearer $OWNER_TOKEN" -H 'Content-Type: application/json' --data-binary @-
import json,sys
json.dump({"repository_id":"synthetic","base_sha":sys.argv[1],"instruction":"make the synthetic edit","tests":[["sh","./check.sh","--endpoint=https://synthetic-argv-user:synthetic-argv-pass@example.invalid/?authorization=synthetic-argv-query","credential:synthetic-argv-colon","workspace:/synthetic/private/argv-file",r"config:C:\synthetic\private\argv-file",'{"token":"synthetic-argv-json secret","safe":"kept"}',"--credential synthetic-argv-cli secret --later tail; safe","[api_key]: synthetic-argv-bracket secret","Access Token: synthetic-argv-access-token","--client secret synthetic-argv-client-secret; safe",r"workspace: \\synthetic-argv-server\private\file",'["Authorization","Bearer synthetic-argv-json-array-secret"]',"file:///private/reviewer/evidence-argv","synthetic-argv-unlabeled-secret"]]},sys.stdout)
PY
)
JOB_ID=$(printf '%s' "$SUBMIT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
i=0
while :; do
	JOB=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/jobs/$JOB_ID")
	STATUS=$(printf '%s' "$JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
	[ "$STATUS" = failed ] && break
	[ "$STATUS" != succeeded ] || { echo "evidence E2E: failing check unexpectedly succeeded"; exit 1; }
	i=$((i+1)); [ "$i" -lt 250 ] || { echo "evidence E2E: terminal wait failed"; exit 1; }
	sleep 0.02
done
ATTEMPT_ID=$(printf '%s' "$JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["attempt_id"])')
printf '%s' "$JOB" | python3 -c 'import json,sys; j=json.load(sys.stdin); assert j["status"]=="failed" and j["error"]=="scoped_test_failed" and j.get("candidate_sha","")==""'
[ "$(git -C "$REPO" worktree list --porcelain | grep -c '^worktree ')" -eq 1 ]

kill "$WORKER_PID"
wait "$WORKER_PID" 2>/dev/null || true
WORKER_PID=
MISSING_PLUGIN="$RUN/missing-plugin"
[ ! -e "$MISSING_PLUGIN" ]
python3 "$ROOT/scripts/write-configs.py" "$RUN/gate.json" "$RUN/worker.json" "127.0.0.1:$PORT" "$RUN/forge.db" "$WS" worker-1 synthetic "$REPO" evidence "$MISSING_PLUGIN" 5s 100ms 2 20ms
if FORGE_WORKER_TOKEN=$WORKER_TOKEN "$RUN/forge-worker" -config "$RUN/worker.json" >"$RUN/missing-plugin-startup.log" 2>&1; then
	echo "evidence E2E: missing plugin passed startup validation"
	exit 1
fi
! grep -F "$MISSING_PLUGIN" "$RUN/missing-plugin-startup.log"

printf '#!/bin/sh\nexit 1\n' >"$MISSING_PLUGIN"
chmod 700 "$MISSING_PLUGIN"
python3 "$ROOT/scripts/write-configs.py" "$RUN/gate.json" "$RUN/worker.json" "127.0.0.1:$PORT" "$RUN/forge.db" "$WS" worker-1 synthetic "$REPO" evidence "$MISSING_PLUGIN" 5s 100ms 2 20ms
FORGE_WORKER_TOKEN=$WORKER_TOKEN "$RUN/forge-worker" -config "$RUN/worker.json" >"$RUN/missing-plugin-worker.log" 2>&1 &
WORKER_PID=$!
i=0
until curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/workers/worker-1" 2>/dev/null | python3 -c 'import json,sys; assert json.load(sys.stdin)["connected"]' 2>/dev/null; do
	i=$((i+1)); [ "$i" -lt 200 ] || { echo "evidence E2E: plugin worker readiness failed"; exit 1; }
	sleep 0.02
done
rm -f "$MISSING_PLUGIN"

PLUGIN_SUBMIT=$(python3 - "$BASE" <<'PY' | curl -fsS -X POST "$HTTP/v1/jobs" -H "Authorization: Bearer $OWNER_TOKEN" -H 'Content-Type: application/json' --data-binary @-
import json,sys
json.dump({"repository_id":"synthetic","base_sha":sys.argv[1],"instruction":"prove synthetic plugin start failure","tests":[["sh","./check.sh"]]},sys.stdout)
PY
)
PLUGIN_JOB_ID=$(printf '%s' "$PLUGIN_SUBMIT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
i=0
while :; do
	PLUGIN_JOB=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/jobs/$PLUGIN_JOB_ID")
	PLUGIN_STATUS=$(printf '%s' "$PLUGIN_JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
	[ "$PLUGIN_STATUS" = failed ] && break
	[ "$PLUGIN_STATUS" != succeeded ] || { echo "evidence E2E: missing plugin unexpectedly succeeded"; exit 1; }
	i=$((i+1)); [ "$i" -lt 250 ] || { echo "evidence E2E: plugin terminal wait failed"; exit 1; }
	sleep 0.02
done
PLUGIN_ATTEMPT_ID=$(printf '%s' "$PLUGIN_JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["attempt_id"])')
printf '%s' "$PLUGIN_JOB" | python3 -c 'import json,sys; j=json.load(sys.stdin); assert j["status"]=="failed" and j["error"]=="max_attempts_exceeded" and j.get("candidate_sha","")==""'
[ "$(git -C "$REPO" worktree list --porcelain | grep -c '^worktree ')" -eq 1 ]

kill "$WORKER_PID"
wait "$WORKER_PID" 2>/dev/null || true
WORKER_PID=
kill "$GATE_PID"
wait "$GATE_PID" 2>/dev/null || true
GATE_PID=
start_gate

[ "$(curl -sS -o /dev/null -w '%{http_code}' "$HTTP/v1/jobs/$JOB_ID/attempts/$ATTEMPT_ID/evidence")" = 401 ]
[ "$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $WORKER_TOKEN" "$HTTP/v1/jobs/$JOB_ID/attempts/$ATTEMPT_ID/evidence")" = 401 ]
curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/jobs/$JOB_ID/attempts/$ATTEMPT_ID/evidence" >"$RUN/evidence.json"
curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/debug/jobs/$JOB_ID" >"$RUN/timeline.json"
curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/jobs/$PLUGIN_JOB_ID/attempts/$PLUGIN_ATTEMPT_ID/evidence" >"$RUN/plugin-evidence.json"
curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$HTTP/v1/debug/jobs/$PLUGIN_JOB_ID" >"$RUN/plugin-timeline.json"
python3 - "$RUN/evidence.json" "$RUN/timeline.json" "$REPO" "$BASE" "$RUN" "$ROOT" "$OWNER_TOKEN" "$WORKER_TOKEN" <<'PY'
import json,sys
evidence_path,timeline_path,repository,base,run,root,owner_token,worker_token=sys.argv[1:]
records=json.load(open(evidence_path))
assert len(records)==1, records
r=records[0]
assert r["phase"]=="scoped_check" and r["reason"]=="scoped_check_failed", r
assert r["check_index"]==0 and r["exit_code"]==7 and 0 <= r["duration_ms"] <= 600000, r
assert r["base_sha"]==base and r.get("candidate_sha","")=="", r
assert r["argv_redacted"] and len(r["argv"])==15 and set(r["argv"])=={"[REDACTED]"}, r["argv"]
assert r["output"]=="[REDACTED]" and r["output_redacted"] and r["output_truncated"], r
for private in ("synthetic-e2e-secret","synthetic-env-secret","synthetic-output-user","synthetic-output-pass","synthetic-output-query","synthetic-output-colon","/synthetic/private/output-file",r"C:\synthetic\private\output-file","synthetic-output-json secret","synthetic-output-json-array-secret","file:///private/reviewer/evidence-output","synthetic-output-unlabeled-secret","synthetic-output-cli secret","later tail","synthetic-output-bracket secret","synthetic-output-access-token","synthetic-output-client-secret","synthetic-output-cli-access-token","synthetic-partial-user",r"\\synthetic-output-server\private\file","synthetic-argv-user","synthetic-argv-pass","synthetic-argv-query","synthetic-argv-colon","/synthetic/private/argv-file",r"C:\synthetic\private\argv-file","synthetic-argv-json secret","synthetic-argv-json-array-secret","file:///private/reviewer/evidence-argv","synthetic-argv-unlabeled-secret","synthetic-argv-cli secret","synthetic-argv-bracket secret","synthetic-argv-access-token","synthetic-argv-client-secret",r"\\synthetic-argv-server\private\file",repository,run,root,owner_token,worker_token,"forge-worktree-","forge-runtime-"):
    assert private not in json.dumps(records), private
timeline=open(timeline_path).read()
for private in ("synthetic-e2e-secret","synthetic-env-secret","synthetic-output-json-array-secret","file:///private/reviewer/evidence-output","synthetic-output-unlabeled-secret","synthetic-argv-json-array-secret","file:///private/reviewer/evidence-argv","synthetic-argv-unlabeled-secret",repository,run,root,owner_token,worker_token,"forge-worktree-","forge-runtime-"):
    assert private not in timeline, private
PY
python3 - "$RUN/plugin-evidence.json" "$RUN/plugin-timeline.json" "$BASE" "$PLUGIN_ATTEMPT_ID" "$RUN" "$ROOT" "$OWNER_TOKEN" "$WORKER_TOKEN" <<'PY'
import json,sys
evidence_path,timeline_path,base,attempt,run,root,owner_token,worker_token=sys.argv[1:]
records=json.load(open(evidence_path))
assert len(records)==1, records
r=records[0]
assert r["phase"]=="plugin" and r["reason"]=="plugin_start_failed", r
assert r["base_sha"]==base and 0 <= r["duration_ms"] <= 900000, r
for field in ("candidate_sha","check_index","exit_code","output","output_redacted","output_truncated","argv","argv_redacted"):
    assert field not in r, (field,r)
timeline=json.load(open(timeline_path))
assert timeline["job"]["status"]=="failed" and timeline["job"].get("candidate_sha","")=="", timeline["job"]
final=timeline["events"][-1]
assert final["type"]=="failed" and final["attempt_id"]==attempt==timeline["job"]["attempt_id"], final
assert final["attempt_ordinal"]==2 and final["disposition"]=="terminal" and final["failure_code"]=="max_attempts_exceeded", final
rendered=json.dumps([records,timeline])
for private in (run,root,owner_token,worker_token,"forge-worktree-","forge-runtime-","synthetic-e2e-secret","synthetic-env-secret"):
    assert private not in rendered, private
PY
LATE=$("$RUN/raw-worker" -gate "$WS" -token "$WORKER_TOKEN" -mode late-evidence -job "$JOB_ID" -attempt "$ATTEMPT_ID" -base "$BASE")
[ "$LATE" = late_evidence_rejected ]

echo "EVIDENCE E2E PASS typed=scoped_check_failed,plugin_start_failed terminal=max_attempts_exceeded bounded=yes redacted=yes restart=yes cleanup=yes late=rejected"
