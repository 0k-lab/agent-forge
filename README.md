# Agent Forge MVP

A minimal Go vertical slice: submit a job to **forge-gate**, persist it in SQLite, lease it to a **forge-worker** over the worker's outbound WebSocket, execute a versioned JSON **plugin subprocess**, and store/expose the immutable result and event history.

## Boundaries

- **Gate** owns repository registrations and authorization, public-source clone/fetch/reuse, prepared local repository paths, Worker pools and authenticated slots, submission-time lifecycle/execution policy resolution, leases, exact GitHub App publication, CI observation/merge, and authoritative SQLite state.
- **Worker** consumes Gate-prepared local repositories, edits, runs only instructed checks, commits locally, and returns the candidate SHA/evidence. External repository URLs and GitHub credentials never cross the Gate/Worker boundary; Worker never clones, pushes, or writes external APIs.
- **Plugin** uses the strict NDJSON [plugin protocol v1](docs/plugin-protocol-v1.md). The reference plugin implements `text`; `forge-codex-plugin` implements `workspace_edit`, invokes `CODEX_BIN` (default `codex`) with bounded output and a timeout, obtains the actual-diff commit subject through Codex structured output, and never reports business success or commits.
- This MVP deliberately has no control panel, universal Worker image, reviewer, mTLS, PostgreSQL, or plugin marketplace. Its only browser UI is a read-only debug viewer. GitHub delivery is optional.

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
go build -o bin/forge-github ./cmd/forge-github
```

## Release artifacts

Build the release matrix from an exact semantic version and full commit SHA on Linux with GNU tar and Python 3:

```sh
scripts/build-release.sh v0.1.0 "$(git rev-parse HEAD)" dist
sha256sum -c dist/SHA256SUMS
```

The builder requires a clean Git checkout whose exact `HEAD` equals the supplied commit, then compiles from a fresh `git archive` of that commit rather than from mutable or ignored working-tree files. It uses the pinned Go toolchain, disables CGO, automatic VCS metadata, and ambient Go experiments (`GOFIPS140=off`), trims source paths, removes the Go build ID, normalizes tar metadata and gzip headers, and embeds that verified version and commit in every binary. Rebuilding the same source with the same inputs produces byte-identical archives and `SHA256SUMS`. The destination must not already exist; the completed temporary directory is published with Linux `renameat2(RENAME_NOREPLACE)` on the same filesystem, so publication also fails if any file, directory, or symlink appears after preflight. The builder never recursively deletes or replaces an existing output.

The canonical matrix is:

- Gate and CLI archives for `linux/amd64` and `linux/arm64`.
- Worker archives for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`.
- Each Worker archive contains `forge-worker`, `forge-codex-plugin`, and `forge-ref-plugin`.

Every archive contains a `VERSION` file, and each runtime binary supports `--version`. CI runs `scripts/release-artifacts-e2e.sh` and uploads the verified full matrix as short-lived workflow artifacts.

## OCI Gate image

Gate is published separately as the publicly pullable multi-architecture image `ghcr.io/0k-lab/agent-forge-gate:vMAJOR.MINOR.PATCH`. The release workflow publishes only that exact version tag—never `latest`, major, or minor tags—and refuses any version tag that already exists. Failed stable tags are never resumed or rerun; correct the release and use the next patch version. This is workflow policy, not registry-enforced immutability. Pinning the resolved index digest remains the strongest production reference, for example `ghcr.io/0k-lab/agent-forge-gate@sha256:<index-digest>`.

### One-time GHCR bootstrap before the first stable OCI release

Use a GitHub token with `write:packages` and permission to publish for `0k-lab/agent-forge`. Keep it in `GHCR_TOKEN`, never in argv or the image. From a clean checkout of the exact reviewed `main` commit:

```sh
export GHCR_USER='<github-user>'
export GHCR_TOKEN='<write-packages-token>'
COMMIT=$(git rev-parse HEAD)
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io --username "$GHCR_USER" --password-stdin
docker buildx build --file Dockerfile.gate \
  --platform linux/amd64,linux/arm64 --pull --no-cache --sbom=true --provenance=mode=max \
  --build-arg VERSION=v0.0.0 --build-arg COMMIT="$COMMIT" \
  --tag "ghcr.io/0k-lab/agent-forge-gate:bootstrap-$COMMIT" --push .
docker logout ghcr.io
unset GHCR_TOKEN
```

The pinned Dockerfile bases and source label create the package and link it to this repository. Confirm the repository link and make the package public in GitHub Package settings. Then use a fresh Docker config with no GHCR credentials to verify an anonymous pull of the bootstrap image:

```sh
DOCKER_CONFIG=$(mktemp -d)
export DOCKER_CONFIG
docker pull "ghcr.io/0k-lab/agent-forge-gate:bootstrap-$COMMIT"
rm -rf "$DOCKER_CONFIG"
unset DOCKER_CONFIG
```

Confirm the package metadata reports public visibility; only then create the stable git tag. After the first stable image is published and verified, the bootstrap package version may be removed through GitHub Package settings.

The stable-tag workflow only reads package metadata and fails before any image write when the package is absent or nonpublic. It does not and cannot change package visibility through GitHub's supported REST API. GHCR has no registry-enforced create-only or immutable tag operation: repository Actions concurrency is the single-writer control, the workflow checks absence through an authenticated registry request, and it verifies the exact index digest again after descriptor-bound runtime checks. Consumers that require cryptographic identity must pin the digest rather than trusting a mutable tag.

The image runs as UID/GID `65532:65532`. Mount Gate config read-only, mount `/var/lib/agent-forge/state` writable, and supply tokens through environment variables or mounted secret files referenced by the runtime environment. The image contains Gate and Git for supported public-source/delivery operation; it does not contain Worker, CLI, or plugin binaries, and no universal Worker image is published.

```sh
docker run --rm --read-only --cap-drop=ALL \
  --security-opt=no-new-privileges --tmpfs /tmp:rw,nosuid,nodev,noexec \
  -p 18080:18080 \
  --mount type=bind,src="$PWD/gate.json",dst=/etc/agent-forge/gate.json,readonly \
  --mount type=bind,src="$PWD/state",dst=/var/lib/agent-forge/state \
  --mount type=bind,src="$PWD/repositories",dst=/var/lib/agent-forge/repositories \
  -e FORGE_OWNER_TOKEN -e FORGE_WORKER_TOKEN \
  ghcr.io/0k-lab/agent-forge-gate:v0.1.0
```

Create the host state and repositories directories owned by `65532:65532`; config should listen on `0.0.0.0:18080`, place SQLite under the mounted state directory, and use the mounted repositories directory for public-source storage. Keep secret values out of the image and config.

A pushed annotated `vMAJOR.MINOR.PATCH` tag starts `.github/workflows/release.yml`; prerelease suffixes, lightweight tags, and commits outside `origin/main` are rejected. The tag pipeline selects the six Linux archives, writes a Linux-only `SHA256SUMS`, generates an SPDX JSON SBOM with pinned Syft, creates GitHub build-provenance and SBOM attestations through OIDC, and completes privileged disposable acceptance before any OCI push. It uploads those exact prepared assets as a short-lived workflow artifact, publishes and anonymously validates the Gate image, then downloads and revalidates the prepared assets before GitHub Release publication. Existing releases and assets are never replaced. If upload fails after draft creation, the partial draft is deliberately retained and the stable version is not rerun; an operator must inspect it, and any corrected release uses the next patch version. macOS is distributed through a Homebrew Formula and bottles rather than as GitHub Release assets; Cask and Apple notarization are not part of this CLI release path.

## Linux install and upgrade

Online installation resolves only the exact stable version given; `latest`, ranges, and prereleases are rejected:

```sh
sudo ./forge install --version v0.1.6
```

The resolver uses the fixed `0k-lab/agent-forge` GitHub HTTPS API origin. It requires a published, immutable, non-draft release and its exact annotated tag-to-commit metadata, downloads only `SHA256SUMS` and the three archives for the current Linux architecture, verifies their API SHA-256 digests and sizes, then applies every existing offline manifest, archive, VERSION, and binary-identity check. The verified target CLI receives the complete offline arguments and performs the installation. This trusts GitHub's CA/DNS and immutable release/API metadata; it does not claim cryptographic attestation verification.

Exact offline installation remains available. Extract the matching `forge` binary from the CLI archive, place `SHA256SUMS` and the Linux CLI, Gate, and Worker archives in one absolute directory, and obtain the SHA-256 digest of the raw `SHA256SUMS` bytes through a trusted channel independent of that directory. Then run:

```sh
sudo ./forge install \
  --version v0.1.3 \
  --commit <full-lowercase-40-hex-release-commit> \
  --asset-dir /absolute/path/to/linux-release-assets \
  --sha256sums-sha256 <trusted-64-lowercase-hex-digest>
```

The default installation is staged under `/opt/agent-forge` for the dedicated `agent-forge` system account but is not started. Add `--enable-now` to reload systemd, enable and start Gate, wait for `/readyz`, start Worker, and verify its authenticated connection. `--run-as-root` is an explicit alternative service identity; it is never inferred from the caller's effective UID.

The installer verifies the trust anchor and exact six-entry Linux manifest before destination mutation, copies and hashes selected archives into private staging, applies a strict tar allowlist, generates isolated owner/Worker tokens, and publishes without replacement. Re-running the exact version is validation-only and does not rotate tokens or repair drift. Supplying a strictly newer exact SemVer with both `--upgrade` and `--enable-now` performs a serialized transactional upgrade: configuration, secrets, SQLite, repositories, and Worker state remain in place while binaries, units, and the receipt are replaced. Gate and Worker readiness are re-proved; any handled publication or readiness failure restores the exact previous files and re-proves the previous services. After readiness, the immediately previous binaries, units, and receipt are retained in one hardened sibling slot. The adjacent root-owned installer lock rejects concurrent lifecycle operations. Downgrades, implicit version replacement, upgrade without readiness activation, and releases with a different Gate SQLite schema fail closed. Schema-changing migrations, crash recovery, and repair remain out of scope.

`store_schema_version` is the persisted-storage compatibility epoch and MUST be bumped for any incompatible schema or persisted semantic change; upgrades and rollbacks across epochs are rejected.

Run `sudo /opt/agent-forge/bin/forge rollback` to activate that one exact older slot without downloading assets or changing configuration, secrets, SQLite, account state, or systemd links. Rollback validates both releases first, restores and re-proves the current release on handled failure, and rejects a second rollback because its slot is newer.

Run `sudo /opt/agent-forge/bin/forge uninstall` only from the exact currently installed release. Uninstall preserves the prior enabled/active state on handled failure, conditionally stops and disables Worker before Gate, removes only the five release binaries, two unit files, receipt, direct systemd links, and optional previous-release slot, then reloads systemd. Configuration, secrets, SQLite, repositories, worktrees, runtime state, and the dedicated `agent-forge` account remain unchanged under `/opt/agent-forge`; there is no purge mode. Retained state is not automatically adoptable by a later install. Handled failures restore the exact prior service state and re-prove only services that were active; the transaction does not promise crash atomicity. After commit, immutable quarantine cleanup is best-effort: any remaining adjacent `.agent-forge.uninstall-*` residue fails closed and an operator must validate and remove it before another lifecycle operation or reinstall.

### Linux installation diagnostics

Run `sudo /opt/agent-forge/bin/forge doctor` to inspect the installation at `/opt/agent-forge`. Root is required because the authenticated readiness checks must read the mode-`0600` owner token; the token and all config, secret, HMAC, repository credential, and database contents are omitted from output.

The command reports deterministic `PASS <check-id>` or `FAIL <check-id>` lines followed by `RESULT healthy` or `RESULT unhealthy`. Checks cover the strict receipt and installed CLI identity; trusted ancestors, exact object types, links, digests, ownership, and modes; mutable config, secret, and SQLite path safety; Gate and Worker systemd enabled/active state; the owner-authenticated exact Gate release proof; the owner-authenticated connected Worker proof; and the current SQLite schema. Exit `0` means every check passed, exit `2` means the installation is unhealthy, and exit `1` is reserved for usage or configuration errors.

`forge doctor` is strictly read-only: it does not repair or rewrite files, migrate the database, manage services, acquire installer/runtime locks, or rotate credentials. Auth-provider health is deferred because provider secrets exist only in the running Gate environment, not in canonical on-disk state. Remote repository access is deferred because proving it would require a credentialed network or Git operation; neither is represented as a fake `PASS` in this increment.

CI and release publication run an additional privileged installer acceptance test only on a clean disposable GitHub-hosted runner. It exercises the real dedicated account, `/opt/agent-forge`, systemd enable/start, authenticated Worker connection, validation-only same-version rerun, and service restart before a release can be published. CI also runs online install/uninstall acceptance on a clean disposable GitHub-hosted VM without enabling services. GitHub's Ubuntu image provisioning intentionally makes `/opt` mode `0777`; after all clean-host guards, the acceptance fixture permits only root-owned mode `0755` or that exact documented `0777` state and normalizes the latter to `0755`. The production installer remains fail-closed on writable privileged ancestors.

## Run

Gate and Worker accept only `-config`. Config files name secret environment variables; secret values never enter config, SQLite, leases, evidence, or logs.

Gate config (`gate.json`):

```json
{"version":1,"listen":"127.0.0.1:18080","database":"/srv/forge/state/forge.db","owner_token_env":"FORGE_OWNER_TOKEN","recovery_interval":"1s","lease_poll_interval":"100ms","default_pool":"coding","lifecycle":{"lease_ttl":"30s","retry_base":"1s","max_attempts":3},"default_execution":{"plugin_id":"reference","environment":["PATH"],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576},"workers":[{"id":"worker-1","pool":"coding","token_env":"FORGE_WORKER_TOKEN","concurrency":2}],"repositories":[{"id":"agent-forge","default_branch":"main","worker_pool":"coding","execution":{"plugin_id":"codex","environment":["PATH","CODEX_HOME","CODEX_BIN"],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576}}]}
```

Worker config (`worker.json`):

```json
{"version":1,"gate_url":"ws://127.0.0.1:18080","id":"worker-1","token_env":"FORGE_WORKER_TOKEN","heartbeat_interval":"10s","concurrency":2,"repository_roots":["/srv/forge/repos"],"worktree_root":"/srv/forge/worktrees","runtime_root":"/srv/forge/runtime","repositories":[{"id":"agent-forge","path":"/srv/forge/repos/agent-forge"}],"plugins":[{"id":"codex","argv":["/srv/forge/bin/forge-codex-plugin"]},{"id":"reference","argv":["/srv/forge/bin/forge-ref-plugin"]}],"environment_allowlist":["PATH","CODEX_HOME","CODEX_BIN"],"check_environment_allowlist":["PATH"],"ceilings":{"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576}}
```

`environment_allowlist` is the ceiling for policy-requested plugin variables. Scoped checks inherit only its explicit `check_environment_allowlist` subset; absent or empty means none, apart from Worker-created `HOME`, `TMPDIR`, and `XDG_CACHE_HOME`.

Public GitHub HTTPS sources are opt-in Gate repository registrations. Gate config owns the canonical URL, repository root, Git executable, default branch, and execution policy:

```json
{"public_repository_root":"/srv/forge/public-repositories","git_executable":"/usr/bin/git","repositories":[{"id":"agent-forge","repository_url":"https://github.com/0k-lab/agent-forge.git","default_branch":"main","worker_pool":"coding","execution":{"plugin_id":"codex","environment":["PATH","CODEX_HOME","CODEX_BIN"],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576}}]}
```

Submit `{"repository_id":"agent-forge","base_sha":"<40 lowercase hex>","instruction":"..."}`. Config accepts only the exact public HTTPS clone-URL shape: no credentials, port, query, fragment, escaping, missing `.git`, trailing slash, dot segment, host alias, or case folding.

Before creating a pending job, Gate maps the configured URL to a SHA-256-named bare repository under `public_repository_root`, serializes provisioning, atomically installs a temporary clone, and fetches only the configured default branch without tags, prompts, credentials, global/system Git config, redirects, or hooks. Reuse validates the bare repository and exact origin; the requested base must exist and be an ancestor of the fetched branch. Preparation failures return bounded structured Gate errors without repository paths. Worker receives only the prepared local path and expected base, then creates the candidate commit and cleans its worktree.

```sh
FORGE_OWNER_TOKEN=fake-owner-token FORGE_WORKER_TOKEN=fake-worker-token ./bin/forge-gate -config gate.json
FORGE_WORKER_TOKEN=fake-worker-token ./bin/forge-worker -config worker.json
```

## Optional GitHub delivery

Add Gate-owned automatic delivery beside the public repository settings. The App ID value is read only from the named environment variable; the private key remains at the named protected path. Delivery retries do not rerun Worker:

```json
{"delivery":{"api_base":"https://api.github.com","github_app_id_env":"FORGE_GITHUB_APP_ID","github_app_private_key_path":"/srv/forge/secrets/github-app.pem","max_attempts":3,"retry_base":"5s","poll_interval":"10s","no_runs_grace":"2m","timeout":"30m"}}
```

With this configured, a coding candidate enters `delivering`; `forge submit --wait` continues through exact branch/PR reconciliation, canonical GitHub Actions polling, and exact-head merge. Status, result, and events expose only the delivery phase, deterministic branch, PR URL, CI state, merge SHA, and allowlisted failure code. Public Actions runs are observed without an installation token, so the GitHub App does not require Actions permission. Without delivery configuration, candidate-only behavior is unchanged.

`forge-github` publishes one already-reviewed Worker candidate without reading Gate SQLite or Worker credentials. It must run as the same trusted local UID that owns the non-group/world-writable repository root. Production config accepts only `https://api.github.com`. The config and App private key must be owned regular `0600` files in owned directories that are not group/world writable. `git_executable` must be the canonical absolute path of an owned executable regular file whose file and parent are not group/world writable.

```json
{"version":1,"api_base":"https://api.github.com","owner":"octo-org","repository":"project","local_repository":"/srv/forge/repos/project","git_executable":"/srv/forge/bin/git","github_app_id_env":"FORGE_GITHUB_APP_ID","github_app_private_key_path":"/srv/forge/secrets/github-app.pem"}
```

Pass the strict publication JSON on stdin so values and credentials do not enter argv. `candidate_ref` is the exact deterministic Worker ref, and `expected_parent_sha` must equal the current local `refs/heads/<base_branch>`:

```sh
printf '%s\n' '{"version":1,"candidate_sha":"2222222222222222222222222222222222222222","expected_parent_sha":"1111111111111111111111111111111111111111","expected_tree_sha":"3333333333333333333333333333333333333333","candidate_ref":"refs/agent-forge/candidates/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","base_branch":"main","new_branch":"forge/job-123","pr_title":"Reviewed candidate","pr_body":"Ready for review."}' |
  FORGE_GITHUB_APP_ID=12345 ./bin/forge-github -config github.json
```

The CLI fails closed on a dirty worktree, local ownership or Git safety failure, base drift, candidate/ref/tree/parent mismatch, unsafe or conflicting branches, installation/repository/permission failures, and bounded API failures. It derives only `contents:write` and `pull_requests:write`, adding `workflows:write` when the exact base-to-candidate diff changes `.github/workflows` or a descendant. It copies the exact candidate and base history into a fresh private bare repository, reverifies their identities there, and performs the credentialed push only from that clean repository with isolated Git configuration. It then creates or reconciles one open PR. Repeating the same request returns the same branch and PR. It never merges or waits for CI.

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

The coding attempt contains `candidate_sha`; its deterministic candidate ref keeps that commit reachable after worktree removal, reflog expiration, and garbage collection. Without automatic delivery it is also the terminal job result; with delivery configured the job becomes terminal only after merge or a typed delivery failure. With no configured repository roots, legacy jobs still run and coding jobs fail with a bounded reason.

Before worktree creation, Worker requires the exact base commit to exist and be an ancestor of `refs/heads/<default_branch>`. Static registrations never fetch; Gate prepares configured public sources as described above. Unknown identities, default-branch mismatch, canonical path drift, or Gate limits above local ceilings fail closed with path/argv/secret-free errors. Supplied scoped argv remains task data; no central verification profile or global command allowlist is introduced. Production Workers should run as dedicated unprivileged accounts or containers.

Run `scripts/e2e.sh` for the reference transport proof, `scripts/public-source-e2e.sh` for the Gate clone/preparation through Worker candidate/cleanup proof, `scripts/plugin-conformance-e2e.sh` for deterministic reference/Python/fake-Codex protocol conformance, `scripts/recovery-e2e.sh` for restart/expiry/retry/late-result recovery, `scripts/evidence-e2e.sh` for the self-contained bounded-evidence/privacy proof, `scripts/sqlite-permissions-e2e.sh` for the Gate SQLite permission/startup proof, and `CODEX_BIN=/path/to/codex scripts/coding-e2e.sh` for the real coding proof. The scripts use synthetic data; recovery artifacts stay in a temporary directory and all spawned processes are cleaned up.
