# Agent Forge MVP

A minimal Go vertical slice: submit a job to **forge-gate**, persist it in SQLite, lease it to a **forge-worker** over the worker's outbound WebSocket, execute a versioned JSON **plugin subprocess**, and store/expose the immutable result and event history.

## Boundaries

- **Gate** owns HTTP/WebSocket APIs, per-worker bearer-token authentication, leases, and authoritative SQLite state.
- **Worker** initiates the only worker connection (outbound WebSocket), executes one configured plugin executable, and returns the lease-bound result.
- **Plugin** is an isolated process contract: one `{"version":"v1","input":"..."}` JSON object on stdin and one bounded `{"version":"v1","result":"..."}` object on stdout.
- This MVP deliberately has no frontend, Docker, GitHub integration, reviewer, mTLS, PostgreSQL, or plugin marketplace.

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
```

## Run

Terminal 1:

```sh
./bin/forge-gate -addr 127.0.0.1:18080 -db ./forge.db \
  -worker-id worker-1 -worker-token dev-token
```

Terminal 2:

```sh
./bin/forge-worker -gate ws://127.0.0.1:18080 -id worker-1 \
  -token dev-token -plugin ./bin/forge-ref-plugin
```

Submit and inspect:

```sh
JOB_JSON=$(curl -sS -X POST http://127.0.0.1:18080/v1/jobs \
  -H 'Content-Type: application/json' -d '{"input":"hello agent forge"}')
echo "$JOB_JSON"
JOB_ID=$(printf '%s' "$JOB_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -sS "http://127.0.0.1:18080/v1/jobs/$JOB_ID"
curl -sS "http://127.0.0.1:18080/v1/jobs/$JOB_ID/events"
curl -sS "http://127.0.0.1:18080/v1/workers/worker-1"
```

Job results are accepted only for the currently bound attempt. Repeating the identical result is idempotent; a different result or wrong attempt is rejected. Worker `connected` state is set on authenticated WebSocket establishment and cleared when that connection ends.
