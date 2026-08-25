#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
RUN=$(mktemp -d)
REPO="$RUN/repo"
GATE_PID=
WORKER_PID=
OWNER_TOKEN=fake-owner-token-coding-e2e
WORKER_TOKEN=fake-worker-token-coding-e2e
cleanup() {
  [ -z "$WORKER_PID" ] || kill "$WORKER_PID" 2>/dev/null || true
  [ -z "$GATE_PID" ] || kill "$GATE_PID" 2>/dev/null || true
  [ -z "$WORKER_PID" ] || wait "$WORKER_PID" 2>/dev/null || true
  [ -z "$GATE_PID" ] || wait "$GATE_PID" 2>/dev/null || true
  rm -rf "$RUN"
}
trap cleanup EXIT INT TERM

mkdir -p "$REPO" "$ROOT/evidence"
git -C "$REPO" init -q -b main
git -C "$REPO" config user.name "Forge E2E"
git -C "$REPO" config user.email "forge@example.invalid"
cat >"$REPO/go.mod" <<'EOF'
module synthetic-task

go 1.24
EOF
cat >"$REPO/answer.go" <<'EOF'
package synthetic

func Answer() int { return 0 }
EOF
cat >"$REPO/answer_test.go" <<'EOF'
package synthetic

import "testing"

func TestAnswer(t *testing.T) {
	if Answer() != 42 {
		t.Fatalf("Answer() = %d, want 42", Answer())
	}
}
EOF
git -C "$REPO" add .
git -C "$REPO" commit -qm "test: add failing synthetic task"
BASE=$(git -C "$REPO" rev-parse HEAD)

python3 "$ROOT/scripts/write-configs.py" "$RUN/gate.json" "$RUN/worker.json" 127.0.0.1:18081 "$RUN/forge.db" ws://127.0.0.1:18081 coding-worker synthetic "$REPO" codex "$ROOT/bin/forge-codex-plugin" 30s 1s 3 10s
FORGE_OWNER_TOKEN=$OWNER_TOKEN FORGE_WORKER_TOKEN=$WORKER_TOKEN "$ROOT/bin/forge-gate" -config "$RUN/gate.json" >"$RUN/gate.log" 2>&1 &
GATE_PID=$!
i=0
until curl -sS -o /dev/null -H "Authorization: Bearer $OWNER_TOKEN" http://127.0.0.1:18081/v1/workers/coding-worker 2>/dev/null; do
  i=$((i+1)); [ "$i" -lt 50 ] || { echo "gate failed to become ready"; exit 1; }; sleep 0.1
done
CODEX_BIN=${CODEX_BIN:-codex} FORGE_WORKER_TOKEN=$WORKER_TOKEN "$ROOT/bin/forge-worker" -config "$RUN/worker.json" >"$RUN/worker.log" 2>&1 &
WORKER_PID=$!
i=0
until curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" http://127.0.0.1:18081/v1/workers/coding-worker 2>/dev/null | python3 -c 'import json,sys; assert json.load(sys.stdin)["connected"]' 2>/dev/null; do
  i=$((i+1)); [ "$i" -lt 50 ] || { echo "worker failed to connect"; exit 1; }; sleep 0.1
done

SUBMIT=$(python3 - "$BASE" <<'PY' | curl -fsS -X POST http://127.0.0.1:18081/v1/jobs -H "Authorization: Bearer $OWNER_TOKEN" -H 'Content-Type: application/json' --data-binary @-
import json,sys
json.dump({"repository_id":"synthetic","base_sha":sys.argv[1],"instruction":"Change Answer in answer.go to return 42. Edit files only; do not commit or run tests.","tests":[["go","test","./..."]],"commit_author_name":"kricha","commit_author_email":"4619899+kricha@users.noreply.github.com"},sys.stdout)
PY
)
JOB_ID=$(printf '%s' "$SUBMIT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
i=0
while :; do
  JOB=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "http://127.0.0.1:18081/v1/jobs/$JOB_ID")
  STATUS=$(printf '%s' "$JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
  [ "$STATUS" = succeeded ] && break
  [ "$STATUS" != failed ] || { echo "coding job failed: $(printf '%s' "$JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("error"))')"; exit 1; }
  i=$((i+1)); [ "$i" -lt 900 ] || { echo "coding job timed out"; exit 1; }; sleep 1
done
CANDIDATE=$(printf '%s' "$JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["candidate_sha"])')
ATTEMPT_ID=$(printf '%s' "$JOB" | python3 -c 'import json,sys; print(json.load(sys.stdin)["attempt_id"])')
CANDIDATE_REF="refs/agent-forge/candidates/$JOB_ID/$ATTEMPT_ID"
EVENTS=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "http://127.0.0.1:18081/v1/jobs/$JOB_ID/events")
printf '%s' "$EVENTS" | python3 -c 'import json,sys; events=json.load(sys.stdin); candidate=sys.argv[1]; assert events[-1]["detail"] == "candidate_sha=" + candidate' "$CANDIDATE"
git -C "$REPO" cat-file -e "$CANDIDATE^{commit}"
[ "$(git -C "$REPO" rev-parse "$CANDIDATE_REF")" = "$CANDIDATE" ]
PARENT=$(git -C "$REPO" rev-parse "$CANDIDATE^")
[ "$PARENT" = "$BASE" ]
AUTHOR=$(git -C "$REPO" show -s --format='%an <%ae>' "$CANDIDATE")
COMMITTER=$(git -C "$REPO" show -s --format='%cn <%ce>' "$CANDIDATE")
[ "$AUTHOR" = "kricha <4619899+kricha@users.noreply.github.com>" ]
[ "$COMMITTER" = "Agent Forge <forge@example.invalid>" ]
VERIFY="$RUN/verify"
git -C "$REPO" worktree add -q --detach "$VERIFY" "$CANDIDATE"
(cd "$VERIFY" && go test ./...)
git -C "$REPO" worktree remove --force "$VERIFY"
[ "$(git -C "$REPO" worktree list --porcelain | grep -c '^worktree ')" -eq 1 ]
git -C "$REPO" reflog expire --expire=now --all
git -C "$REPO" gc --prune=now
[ "$(git -C "$REPO" rev-parse "$CANDIDATE_REF")" = "$CANDIDATE" ]
git -C "$REPO" cat-file -e "$CANDIDATE^{commit}"

TERMINAL=$(printf '%s' "$JOB" | python3 -c 'import json,sys; j=json.load(sys.stdin); print(json.dumps({k:j[k] for k in ("status","candidate_sha","created_at","updated_at")},separators=(",",":")))')
{
  echo "CODING E2E PASS"
  echo "terminal_job=$TERMINAL"
  echo "base_sha=$BASE"
  echo "candidate_sha=$CANDIDATE"
  echo "candidate_parent=$PARENT"
  echo "candidate_author=$AUTHOR"
  echo "candidate_committer=$COMMITTER"
  echo "candidate_ref=$CANDIDATE_REF"
  echo "candidate_object=verified"
  echo "event_candidate_sha=verified"
  echo "scoped_test=passed"
  echo "worktrees_cleaned=verified"
  echo "candidate_survives_gc=verified"
} | tee "$ROOT/evidence/coding-e2e-output.txt"
