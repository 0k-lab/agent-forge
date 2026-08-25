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
install -d -m 0700 ./forge-state
FORGE_OWNER_TOKEN=fake-owner-token FORGE_WORKER_TOKEN=fake-worker-token \
  ./bin/forge-gate -addr 127.0.0.1:18080 -db ./forge-state/forge.db -worker-id worker-1 \
  -lease-ttl 30s -retry-base 1s -max-attempts 3 -recovery-interval 1s
```

Terminal 2:

```sh
FORGE_WORKER_TOKEN=fake-worker-token ./bin/forge-worker \
  -gate ws://127.0.0.1:18080 -id worker-1 \
  -repo-root /srv/forge/repos -plugin ./bin/forge-ref-plugin \
  -heartbeat-interval 10s
```

## SQLite storage security

File-backed storage provides cross-UID confidentiality when Gate's immediate database directory is owned by its effective UID and has no group/world permission bits. A dedicated unprivileged UID is recommended, but Gate does not require root and does not reject effective UID 0. Both of these forms select the same file:

```sh
install -d -m 0700 /srv/forge/state
./bin/forge-gate -db /srv/forge/state/forge.db # plus the required tokens/options
./bin/forge-gate -db 'file:///srv/forge/state/forge.db?mode=rwc&cache=private' # equivalent URI form
```

Gate requires trusted, non-symlink path ancestors and checks that the immediate parent is private and effective-UID owned. The database and any existing `-journal`, `-wal`, and `-shm` files must be regular, non-symlink, effective-UID-owned files with exact mode `0600`. A missing database is atomically pre-created and verified as `0600`; startup always performs schema writes/migrations. Existing insecure files fail closed without chmod, removal, or replacement. Repair one offline only after taking a backup and verifying its owner and contents.

`:memory:`, `file::memory:?cache=private`, `file::memory:?cache=shared`, named or empty `file:` memory DSNs with `mode=memory`, relative or absolute plain paths, and validated `file:` paths are supported. File DSNs allow only absent `mode` or `mode=rwc` and optional cache value `private` or `shared`; all other or duplicate parameters are rejected. On non-Unix systems memory databases work, but file-backed storage returns an unsupported-storage error.

The private owned parent prevents mutation by other UIDs, and atomic `O_EXCL` creation protects the absent final target. This preflight is not a custom-VFS, race-free defense against a malicious process running as the same UID; the runtime and same-UID processes must be trusted. These filesystem controls are permissions, not encryption.

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

## Attempt evidence

Coding Workers bind bounded evidence to the exact current `(job, attempt, authenticated Worker)` before deleting the ephemeral worktree. Gate ACKs each durable evidence batch without clearing the lease; Worker keeps heartbeating, performs idempotent cleanup, binds any cleanup-failure record in a second ACKed batch, then sends the terminal result. A disconnect defers best-effort cleanup but does not claim evidence durability. Older Workers that send only a result remain compatible.

Evidence uses closed preparation, plugin, workspace-validation, scoped-check, candidate-commit, and cleanup phase/reason values. Scoped checks include the authoritative task check index, exit status when valid, duration, and argument count/order represented only by one fixed `[REDACTED]` placeholder per task argument. Any non-empty Worker output becomes exactly `[REDACTED]`; empty output remains empty, while `output_redacted` and bounded-capture `output_truncated` report what happened. Store rejects every other non-empty output, so the API and canonical payload hash contain only the fixed marker and structured safe fields. A batch and an attempt allow at most 34 records, with 96 KiB per batch and 64 KiB total stored output per attempt. Empty batches, unknown fields, mixed-purpose messages, oversized payloads, conflicting replays, expired leases, terminal attempts, and superseded attempts fail closed.

After a successful candidate commit, all scoped-check records name that exact candidate SHA; failed pre-commit checks remain base-bound. Evidence survives Gate restart on the still-leased attempt, while expiry/retry fencing prevents a prior attempt from appending later. Only the owner API exposes it:

```sh
curl -sS -H 'Authorization: Bearer fake-owner-token' \
  "http://127.0.0.1:18080/v1/jobs/$JOB_ID/attempts/$ATTEMPT_ID/evidence"
```

The read-only debug viewer intentionally does not render evidence output.

## Read-only debug viewer

Open `http://127.0.0.1:18080/debug/`, enter the owner token, and load recent jobs, job timelines, and Worker connection state. The public embedded shell contains no state; the password value is cleared after being copied to JavaScript memory and every JSON request sends it only in the `Authorization: Bearer <token>` header.

The owner-authenticated JSON routes are `GET /v1/debug/jobs`, `GET /v1/debug/workers`, and `GET /v1/debug/jobs/{id}`. List and timeline routes accept `limit` (default 25, maximum 100) and an opaque `cursor`. Records contain only diagnostic identifiers, kind/status, timestamps, connection state, exact available SHAs, and event type/time.

These routes cannot submit, retry, cancel, approve, edit policy, manage repositories, recover leases, or mutate state. They intentionally omit task inputs and instructions, repository paths, plugin/test results, authorization values, and event details or errors.

Job results are accepted only for the currently bound attempt. Repeating the identical result is idempotent; a different result or wrong attempt is rejected. Worker `connected` state is set on authenticated WebSocket establishment and cleared when that connection ends.

## Leases, retries, and recovery

Gate owns lease and retry policy. Defaults are a 30-second lease TTL, 1-second exponential retry base, 3 attempts, and a 1-second recovery sweep interval. All values must be positive and bounded; the recovery interval cannot exceed the lease TTL. `scoped_test_failed` and `invalid_task` are terminal, while `execution_failed` is retryable. Gate rejects unknown codes and conflicting claimed dispositions without spending another attempt.

A job moves through `pending -> leased -> succeeded`, `leased -> failed` for terminal failures, or `leased -> retry_wait -> leased` for retryable failures and expired leases. Each lease creates a durable attempt with a new ID and ordinal. Backoff is based on that ordinal. When the maximum is reached, the job becomes `failed` with `max_attempts_exceeded`; it is not leased again.

Workers heartbeat the exact job/attempt/Worker tuple while executing. The Worker default is 10 seconds, one third of the default Gate TTL; configure `-heartbeat-interval` shorter than `-lease-ttl`. Gate closes or rejects a stale connection, and Worker cancellation is best-effort, so old execution may physically overlap a retry. SQLite attempt and lease fencing ensures that old work can never publish an authoritative result or candidate. Candidate refs already created for an attempt remain preserved.

Gate performs an expiry sweep before it starts serving, then repeats at `-recovery-interval`. Restarting Gate with the same SQLite database preserves attempt history and recovers expired leases. A sweep or Store failure stops Gate rather than disabling recovery. Run Gate and Worker under supervisors: Workers return after Gate disconnects and the supervisor should reconnect them; Gate should also be restarted after a fatal recovery error.

The sanitized timeline exposes attempt ordinals, lease expiry, retry scheduling, dispositions, bounded failure codes, and success without task content, result bodies, tokens, private repository paths, or cursor secrets.

Submit a coding task with an absolute local repository path, full base SHA, instruction, explicit argv arrays, and an optional paired commit author:

```json
{"repository":"/srv/forge/repos/synthetic-project","base_sha":"0123456789abcdef0123456789abcdef01234567","instruction":"Fix the failing focused test.","tests":[["go","test","./internal/example","-run","TestFocused"]],"commit_author_name":"kricha","commit_author_email":"4619899+kricha@users.noreply.github.com"}
```

`commit_author_name` and `commit_author_email` must be supplied together or both omitted. Names are limited to 256 bytes and emails to 254 bytes; leading or trailing Unicode whitespace, unsafe Git-header characters, and malformed addresses are rejected. A supplied identity becomes the exact Git Author, while Git Committer remains `Agent Forge <forge@example.invalid>`. If both fields are omitted, both identities use that Agent Forge fallback for compatibility.

The terminal job contains `candidate_sha`; its deterministic candidate ref keeps that commit reachable after worktree removal, reflog expiration, and garbage collection. With no configured repository roots, legacy jobs still run and coding jobs fail with a bounded reason.

Owner-token holders are authorized to request commands only inside configured repository roots. Production Workers should run as dedicated unprivileged accounts or containers. The plugin receives only `PATH`, `HOME`, `CODEX_HOME`, `CODEX_BIN`, and `TMPDIR` when set; scoped tests receive an isolated home, temp, and cache.

Run `scripts/e2e.sh` for the reference transport proof, `scripts/recovery-e2e.sh` for restart/expiry/retry/late-result recovery, `scripts/evidence-e2e.sh` for the self-contained bounded-evidence/privacy proof, `scripts/sqlite-permissions-e2e.sh` for the Gate SQLite permission/startup proof, and `CODEX_BIN=/path/to/codex scripts/coding-e2e.sh` for the real coding proof. The scripts use synthetic data; recovery artifacts stay in a temporary directory and all spawned processes are cleaned up.
