# Agent Forge MVP

A minimal Go vertical slice: submit a job to **forge-gate**, persist it in SQLite, lease it to a **forge-worker** over the worker's outbound WebSocket, execute a versioned JSON **plugin subprocess**, and store/expose the immutable result and event history.

## Boundaries

- **Gate** owns repository IDs/default branches, Worker pools and authenticated slots, submission-time lifecycle/execution policy resolution, leases, and authoritative SQLite state.
- **Worker** owns repository-ID-to-local-path and plugin-ID-to-local-argv mappings, canonical repository/worktree/runtime roots, inherited-environment allowlisting, and local timeout/output ceilings. Local paths and plugin argv never cross the Worker/Gate boundary.
- **Plugin** uses the strict NDJSON [plugin protocol v1](docs/plugin-protocol-v1.md). The reference plugin implements `text`; `forge-codex-plugin` implements `workspace_edit`, invokes `CODEX_BIN` (default `codex`) with bounded output and a timeout, obtains the actual-diff commit subject through Codex structured output, and never reports business success or commits.
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
go build -o bin/forge ./cmd/forge
```

## Run

Both commands accept only `-config`. Config files name secret environment variables; secret values never enter config, SQLite, leases, evidence, or logs.

Gate config (`gate.json`):

```json
{"version":1,"listen":"127.0.0.1:18080","database":"/srv/forge/state/forge.db","owner_token_env":"FORGE_OWNER_TOKEN","recovery_interval":"1s","lease_poll_interval":"100ms","default_pool":"coding","lifecycle":{"lease_ttl":"30s","retry_base":"1s","max_attempts":3},"default_execution":{"plugin_id":"reference","environment":["PATH"],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576},"workers":[{"id":"worker-1","pool":"coding","token_env":"FORGE_WORKER_TOKEN","concurrency":2}],"repositories":[{"id":"agent-forge","default_branch":"main","worker_pool":"coding","execution":{"plugin_id":"codex","environment":["PATH","CODEX_HOME","CODEX_BIN"],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576}}]}
```

Worker config (`worker.json`):

```json
{"version":1,"gate_url":"ws://127.0.0.1:18080","id":"worker-1","token_env":"FORGE_WORKER_TOKEN","heartbeat_interval":"10s","concurrency":2,"repository_roots":["/srv/forge/repos"],"worktree_root":"/srv/forge/worktrees","runtime_root":"/srv/forge/runtime","repositories":[{"id":"agent-forge","path":"/srv/forge/repos/agent-forge"}],"plugins":[{"id":"codex","argv":["/srv/forge/bin/forge-codex-plugin"]},{"id":"reference","argv":["/srv/forge/bin/forge-ref-plugin"]}],"environment_allowlist":["PATH","CODEX_HOME","CODEX_BIN"],"check_environment_allowlist":["PATH"],"ceilings":{"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576}}
```

`environment_allowlist` is the ceiling for policy-requested plugin variables. Scoped checks inherit only its explicit `check_environment_allowlist` subset; absent or empty means none, apart from Worker-created `HOME`, `TMPDIR`, and `XDG_CACHE_HOME`.

```sh
FORGE_OWNER_TOKEN=fake-owner-token FORGE_WORKER_TOKEN=fake-worker-token ./bin/forge-gate -config gate.json
FORGE_WORKER_TOKEN=fake-worker-token ./bin/forge-worker -config worker.json
```

Configs are strict versioned JSON: unknown/duplicate fields, trailing data, unsafe or duplicate IDs/references, missing/colliding token values, noncanonical paths, and contradictory bounds fail startup with bounded value-free errors. `concurrency: 2` opens two independent outbound lanes (`worker-1` and `worker-1#1` server-side); attempts are never multiplexed on one WebSocket.

## SQLite storage security

File-backed storage provides cross-UID confidentiality when Gate's immediate database directory is owned by its effective UID and has no group/world permission bits. A dedicated unprivileged UID is recommended, but Gate does not require root and does not reject effective UID 0. Both of these forms select the same file:

```sh
install -d -m 0700 /srv/forge/state
# Set the Gate config's database field to either:
/srv/forge/state/forge.db
file:///srv/forge/state/forge.db?mode=rwc&cache=private
```

Gate requires trusted, non-symlink path ancestors and checks that the immediate parent is private and effective-UID owned. The database and any existing `-journal`, `-wal`, and `-shm` files must be regular, non-symlink, effective-UID-owned files with exact mode `0600`. A missing database is atomically pre-created and verified as `0600`; startup always performs schema writes/migrations. Existing insecure files fail closed without chmod, removal, or replacement. Repair one offline only after taking a backup and verifying its owner and contents.

`:memory:`, `file::memory:?cache=private`, `file::memory:?cache=shared`, named or empty `file:` memory DSNs with `mode=memory`, relative or absolute plain paths, and validated `file:` paths are supported. File DSNs allow only absent `mode` or `mode=rwc` and optional cache value `private` or `shared`; all other or duplicate parameters are rejected. On non-Unix systems memory databases work, but file-backed storage returns an unsupported-storage error.

If only the immediate database parent is missing, Gate creates it as exact mode `0700`, verifies that it is a non-symlink directory owned by the effective UID, and then creates the database. Gate never creates multiple missing directory levels. An existing parent must already be private and effective-UID owned; Gate never chmods, removes, or replaces an existing insecure or wrong-owner object. The private owned parent prevents mutation by other UIDs, and atomic `O_EXCL` creation protects the absent final target. This preflight is not a custom-VFS, race-free defense against a malicious process running as the same UID; the runtime and same-UID processes must be trusted. These filesystem controls are permissions, not encryption.

Set the CLI connection values once, then submit and inspect without putting the owner token in argv:

```sh
export FORGE_GATE_URL=http://127.0.0.1:18080
export FORGE_OWNER_TOKEN=fake-owner-token
JOB_JSON=$(printf '%s\n' '{"input":"hello agent forge"}' | ./bin/forge submit -file -)
echo "$JOB_JSON"
JOB_ID=$(printf '%s' "$JOB_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
./bin/forge status "$JOB_ID"
./bin/forge events "$JOB_ID"
./bin/forge wait -timeout 10m -poll 1s "$JOB_ID"
./bin/forge result "$JOB_ID"
```

`forge` has exactly five subcommands: `submit`, `wait`, `status`, `events`, and `result`. Each accepts `-url` (default `FORGE_GATE_URL`) and `-token-env` (default `FORGE_OWNER_TOKEN`); the token value is read only from that environment variable. `submit -file task.json -wait` submits one strict JSON object and prints the terminal aggregate. `wait` and `result` print the same bounded aggregate containing the authoritative job, ordered attempts, ordered events, and evidence nested under each attempt.

## Attempt evidence

Coding Workers bind bounded evidence to the exact current `(job, attempt, authenticated Worker)` before deleting the ephemeral worktree. Gate ACKs each durable evidence batch without clearing the lease; Worker keeps heartbeating, performs idempotent cleanup, binds any cleanup-failure record in a second ACKed batch, then sends the terminal result. A disconnect defers best-effort cleanup but does not claim evidence durability. Older Workers that send only a result remain compatible.

Evidence uses closed preparation, plugin, workspace-validation, scoped-check, candidate-commit, and cleanup phase/reason values. Scoped checks include the authoritative task check index, exit status when valid, duration, and argument count/order represented only by one fixed `[REDACTED]` placeholder per task argument. Any non-empty Worker output becomes exactly `[REDACTED]`; empty output remains empty, while `output_redacted` and bounded-capture `output_truncated` report what happened. Store rejects every other non-empty output, so the API and canonical payload hash contain only the fixed marker and structured safe fields. A batch and an attempt allow at most 34 records, with 96 KiB per batch and 64 KiB total stored output per attempt. Empty batches, unknown fields, mixed-purpose messages, oversized payloads, conflicting replays, expired leases, terminal attempts, and superseded attempts fail closed.

After a successful candidate commit, all scoped-check records name that exact candidate SHA; failed pre-commit checks remain base-bound. Evidence survives Gate restart on the still-leased attempt, while expiry/retry fencing prevents a prior attempt from appending later. Only the owner API exposes it. The supported CLI nests evidence under each attempt, so callers do not construct attempt URLs:

```sh
./bin/forge result "$JOB_ID"
```

The read-only debug viewer intentionally does not render evidence output.

## Read-only debug viewer

Open `http://127.0.0.1:18080/debug/`, enter the owner token, and load recent jobs, job timelines, and Worker connection state. The public embedded shell contains no state; the password value is cleared after being copied to JavaScript memory and every JSON request sends it only in the `Authorization: Bearer <token>` header.

The owner-authenticated JSON routes are `GET /v1/debug/jobs`, `GET /v1/debug/workers`, and `GET /v1/debug/jobs/{id}`. List and timeline routes accept `limit` (default 25, maximum 100) and an opaque `cursor`. Records contain only diagnostic identifiers, kind/status, timestamps, connection state, exact available SHAs, and event type/time.

These routes cannot submit, retry, cancel, approve, edit policy, manage repositories, recover leases, or mutate state. They intentionally omit task inputs and instructions, repository paths, plugin/test results, authorization values, and event details or errors.

Job results are accepted only for the currently bound attempt. Repeating the identical result is idempotent; a different result or wrong attempt is rejected. Worker `connected` state is set on authenticated WebSocket establishment and cleared when that connection ends.

## Leases, retries, and recovery

Gate resolves lifecycle and portable execution policy when accepting a job. The canonical policy is persisted on the job and copied byte-for-byte to every attempt. `exponential-v1` schedules `min(retry_base * 2^(attempt_ordinal-1), 24h)` with saturation; `retry_at` is written once in the terminating transaction. Startup config changes never rewrite queued, leased, or retry-waiting jobs. Active legacy rows without resolved policy block configured startup; terminal legacy history remains readable. `scoped_test_failed` and `invalid_task` are terminal, while `execution_failed` is retryable.

A job moves through `pending -> leased -> succeeded`, `leased -> failed` for terminal failures, or `leased -> retry_wait -> leased` for retryable failures and expired leases. Each lease creates a durable attempt with a new ID and ordinal. Backoff is based on that ordinal. When the maximum is reached, the job becomes `failed` with `max_attempts_exceeded`; it is not leased again.

Workers heartbeat the exact job/attempt/effective-slot/live-generation tuple while executing. Heartbeats are valid strictly before the deadline and extend it by the persisted attempt TTL without shortening it. Same-slot takeover fences the old random generation; compare-and-clear disconnect handling cannot mark its replacement offline. Gate closes or rejects a stale connection, and Worker cancellation is best-effort, so old execution may physically overlap a retry. SQLite fencing prevents old work from publishing evidence, failure, success, or a candidate.

Gate performs an expiry sweep before it starts serving, then repeats at the configured recovery interval. The interval is operational scan cadence only; persisted attempt policy controls deadlines/retries. Restarting Gate preserves attempt history and marks stale slot sessions disconnected. A sweep or Store failure stops Gate rather than disabling recovery.

The sanitized timeline exposes attempt ordinals, lease expiry, retry scheduling, dispositions, bounded failure codes, and success without task content, result bodies, tokens, private repository paths, or cursor secrets.

Submit a coding task with a stable repository ID, full base SHA, instruction, optional scoped-check argv arrays, and an optional paired commit author:

```json
{"repository_id":"agent-forge","base_sha":"0123456789abcdef0123456789abcdef01234567","instruction":"Fix the failing focused test.","tests":[["go","test","./internal/example","-run","TestFocused"]],"commit_author_name":"kricha","commit_author_email":"4619899+kricha@users.noreply.github.com"}
```

The Codex plugin may follow non-conflicting repository instructions and run workspace-local, repository-native focused validation inside its existing execution environment and lifecycle. That output and any resulting claim are advisory executor feedback, never Worker acceptance evidence. Repository instructions cannot override the task or higher-priority Worker prompt constraints, authorize Git or commits, or allow access outside the workspace.

Only `tests` argv explicitly supplied by the task run as authoritative Worker scoped checks, in their dedicated restricted check environment and supervisor. The Worker never reads `AGENTS.md`, discovers or defaults checks, or treats executor logs as check evidence. `tests` may be omitted or empty; the Worker then still creates and validates the structural candidate commit/ref but emits zero scoped-check evidence. Repository CI and human review own delivery acceptance.

`commit_author_name` and `commit_author_email` must be supplied together or both omitted. Names are limited to 256 bytes and emails to 254 bytes; leading or trailing Unicode whitespace, unsafe Git-header characters, and malformed addresses are rejected. A supplied identity becomes the exact Git Author, while Git Committer remains `Agent Forge <forge@example.invalid>`. If both fields are omitted, both identities use that Agent Forge fallback for compatibility.

The terminal job contains `candidate_sha`; its deterministic candidate ref keeps that commit reachable after worktree removal, reflog expiration, and garbage collection. With no configured repository roots, legacy jobs still run and coding jobs fail with a bounded reason.

Before worktree creation, Worker requires the exact base commit to exist and be an ancestor of its configured local `refs/heads/<default_branch>`. It never fetches. Unknown IDs, default-branch mismatch, symlink/canonical drift, root escape, or Gate limits above local ceilings fail closed with path/argv/secret-free errors. Supplied scoped argv remains task data; no central verification profile or global command allowlist is introduced. Production Workers should run as dedicated unprivileged accounts or containers.

Run `scripts/e2e.sh` for the reference transport proof, `scripts/plugin-conformance-e2e.sh` for deterministic reference/Python/fake-Codex protocol conformance, `scripts/recovery-e2e.sh` for restart/expiry/retry/late-result recovery, `scripts/evidence-e2e.sh` for the self-contained bounded-evidence/privacy proof, `scripts/sqlite-permissions-e2e.sh` for the Gate SQLite permission/startup proof, and `CODEX_BIN=/path/to/codex scripts/coding-e2e.sh` for the real coding proof. The scripts use synthetic data; recovery artifacts stay in a temporary directory and all spawned processes are cleaned up.
