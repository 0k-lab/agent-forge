# Agent Forge MVP

A minimal Go vertical slice: submit a job to **forge-gate**, persist it in SQLite, lease it to a **forge-worker** over the worker's outbound WebSocket, execute a versioned JSON **plugin subprocess**, and store/expose the immutable result and event history.

## Boundaries

- **Gate** owns HTTP/WebSocket APIs, a distinct owner bearer token for every HTTP endpoint, per-worker bearer-token authentication, leases, and authoritative SQLite state.
- **Worker** initiates the only worker connection (outbound WebSocket), accepts coding jobs only inside configured repository roots, creates a detached worktree at the approved base SHA, executes one configured plugin, runs only the supplied argv test commands in a sanitized environment, and retains the verified candidate under `refs/agent-forge/candidates/<job_id>/<attempt_id>`.
- **Plugin** is an edit-only subprocess contract. The reference plugin accepts legacy input; `forge-codex-plugin` accepts a workspace and instruction, invokes `CODEX_BIN` (default `codex`) with bounded output and a timeout, and never reports business success or commits.
- This MVP deliberately has no control panel, Docker, GitHub integration, reviewer, mTLS, PostgreSQL, or plugin marketplace. Its only browser UI is a read-only debug viewer.

## Build and test

Requires Go 1.24+.

```sh
go mod download
gofmt -w .
go test ./...
go vet ./...
mkdir -p bin
go build -o bin/forge-gate ./cmd/forge-gate
go build -o bin/forge-worker ./cmd/forge-worker
go build -o bin/forge-ref-plugin ./cmd/forge-ref-plugin
go build -o bin/forge-codex-plugin ./cmd/forge-codex-plugin
```

## Run

Terminal 1:

```sh
FORGE_OWNER_TOKEN=fake-owner-token FORGE_WORKER_TOKEN=fake-worker-token \
  ./bin/forge-gate -addr 127.0.0.1:18080 -db ./forge.db -worker-id worker-1
```

Terminal 2:

```sh
FORGE_WORKER_TOKEN=fake-worker-token ./bin/forge-worker \
  -gate ws://127.0.0.1:18080 -id worker-1 \
  -repo-root /srv/forge/repos -plugin ./bin/forge-ref-plugin
```

Submit and inspect:

```sh
JOB_JSON=$(curl -sS -X POST http://127.0.0.1:18080/v1/jobs \
  -H 'Authorization: Bearer fake-owner-token' \
  -H 'Content-Type: application/json' -d '{"input":"hello agent forge"}')
echo "$JOB_JSON"
JOB_ID=$(printf '%s' "$JOB_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -sS -H 'Authorization: Bearer fake-owner-token' "http://127.0.0.1:18080/v1/jobs/$JOB_ID"
curl -sS -H 'Authorization: Bearer fake-owner-token' "http://127.0.0.1:18080/v1/jobs/$JOB_ID/events"
curl -sS -H 'Authorization: Bearer fake-owner-token' "http://127.0.0.1:18080/v1/workers/worker-1"
```

## Read-only debug viewer

Open `http://127.0.0.1:18080/debug/`, enter the owner token, and load recent jobs, job timelines, and Worker connection state. The public embedded shell contains no state; the password value is cleared after being copied to JavaScript memory and every JSON request sends it only in the `Authorization: Bearer ...` header.

The owner-authenticated JSON routes are `GET /v1/debug/jobs`, `GET /v1/debug/workers`, and `GET /v1/debug/jobs/{id}`. List and timeline routes accept `limit` (default 25, maximum 100) and an opaque `cursor`. Records contain only diagnostic identifiers, kind/status, timestamps, connection state, exact available SHAs, and event type/time.

These routes cannot submit, retry, cancel, approve, edit policy, manage repositories, recover leases, or mutate state. They intentionally omit task inputs and instructions, repository paths, plugin/test results, authorization values, and event details or errors.

Job results are accepted only for the currently bound attempt. Repeating the identical result is idempotent; a different result or wrong attempt is rejected. Worker `connected` state is set on authenticated WebSocket establishment and cleared when that connection ends.

Submit a coding task with an absolute local repository path, full base SHA, instruction, explicit argv arrays, and an optional paired commit author:

```json
{"repository":"/srv/forge/repos/synthetic-project","base_sha":"0123456789abcdef0123456789abcdef01234567","instruction":"Fix the failing focused test.","tests":[["go","test","./internal/example","-run","TestFocused"]],"commit_author_name":"kricha","commit_author_email":"4619899+kricha@users.noreply.github.com"}
```

`commit_author_name` and `commit_author_email` must be supplied together or both omitted. Names are limited to 256 bytes and emails to 254 bytes; leading or trailing Unicode whitespace, unsafe Git-header characters, and malformed addresses are rejected. A supplied identity becomes the exact Git Author, while Git Committer remains `Agent Forge <forge@example.invalid>`. If both fields are omitted, both identities use that Agent Forge fallback for compatibility.

The terminal job contains `candidate_sha`; its deterministic candidate ref keeps that commit reachable after worktree removal, reflog expiration, and garbage collection. With no configured repository roots, legacy jobs still run and coding jobs fail with a bounded reason.

Owner-token holders are authorized to request commands only inside configured repository roots. Production Workers should run as dedicated unprivileged accounts or containers. The plugin receives only `PATH`, `HOME`, `CODEX_HOME`, `CODEX_BIN`, and `TMPDIR` when set; scoped tests receive an isolated home, temp, and cache.

Run `scripts/e2e.sh` for the reference transport proof and `CODEX_BIN=/path/to/codex scripts/coding-e2e.sh` for the real coding proof. Both scripts use only synthetic data; the coding script deletes its temporary repository and worktrees.
