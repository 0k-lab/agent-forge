# Agent Forge plugin protocol v1

This protocol is authority separation and robust local IPC, not an OS sandbox. Plugins are trusted same-UID executables installed by the Worker operator. One fresh process handles one operation.

## Transport and session

Each frame is one compact UTF-8 JSON object followed by LF. Stdout contains frames only; stderr is private diagnostics. Duplicate or unknown fields, invalid UTF-8, blank lines, CRLF, malformed JSON, frames over 1 MiB, and a final frame without LF are invalid.

The Worker generates a lowercase 32-hex `id`. Every frame has exactly `version`, `id`, `type`, and the fields shown below. `version` is exactly `v1`; IDs must match.

1. Worker sends `initialize`, plugin sends `initialized`.
2. Worker sends one `execute`.
3. Plugin may send negotiated `progress` frames, then exactly one `result` or `failure`.
4. Worker closes stdin and requires clean stdout EOF and exit 0 within 250 ms. Any trailing byte/frame or nonzero exit is a protocol failure.

```json
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"initialize","capabilities":["text"],"limits":{"frame_bytes":1048576,"progress_frames":128,"text_bytes":65536,"progress_text_bytes":1024,"commit_subject_bytes":256}}
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"initialized","capabilities":["text"]}
```

Capabilities are closed: `text`, `workspace_edit`, `progress`, `cancel`, `commit_subject`. `initialized.capabilities` is a duplicate-free subset of the offer and must include the operation capability. Unknown or unoffered selections fail.

## Operations and terminals

Text execution and success:

```json
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"execute","operation":"text","input":"hello"}
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"result","output":"HELLO"}
```

`input` and `output` are at most 65,536 UTF-8 bytes.

Workspace execution and success:

```json
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"execute","operation":"workspace_edit","workspace":"/operator/path","instruction":"edit the files","timeout_ms":60000}
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"result","commit_subject":"feat: describe the edit"}
```

The workspace result has exactly the common fields and optional `commit_subject`. If present, `commit_subject` requires negotiation, is 1..256 UTF-8 bytes, has no leading/trailing Unicode whitespace, controls, CR/LF/NUL/U+0085/U+2028/U+2029, or logical second line. When absent, Worker uses `chore: apply coding task`. Worker passes it as one argv element.

Progress requires negotiation, is limited to 128 frames, and has monotonically consecutive sequence numbers starting at 1. `stage` is one of `started`, `working`, `finalizing`; `text` is at most 1,024 UTF-8 bytes:

```json
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"progress","sequence":1,"stage":"started","text":"editing"}
```

Failure has `category` equal to `invalid_request`, `incompatible`, `execution_failed`, or `cancelled`. It describes only the plugin operation; Worker owns task disposition:

```json
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"failure","category":"execution_failed"}
```

If local context expires after negotiated execution, Worker sends exactly one cancel and allows 250 ms to drain; otherwise it terminates and reaps the existing ordinary/setsid process tree. Cancellation is authoritative even if success races it:

```json
{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"cancel"}
```

Progress and stderr stay Worker-local. Plugins cannot lease, retry, complete attempts, create candidate refs, or declare authoritative checks/state.

## Conformance

Build `forge-plugin-conformance`, then pass the plugin executable after flags. It runs one valid session plus fresh-process malformed-request rejection scenarios and exits nonzero on any failure:

Invalid initialization must produce no stdout before a bounded nonzero exit. Invalid execution may produce exactly the deterministic `initialized` frame, then no other stdout, before a bounded nonzero exit. The suite rejects blank, malformed, invalid UTF-8, CRLF, non-compact, oversized, and unterminated frames; invalid IDs, fields, capabilities, limits, versions, operations, payloads, and frame order; and contradictory or duplicate execute fields.

```sh
go build -o /tmp/forge-plugin-conformance ./cmd/forge-plugin-conformance
/tmp/forge-plugin-conformance ./examples/forge-plugin-v1.py
/tmp/forge-plugin-conformance ./bin/forge-ref-plugin
CODEX_BIN=/path/to/fake-codex /tmp/forge-plugin-conformance -operation workspace_edit ./bin/forge-codex-plugin
```

`scripts/plugin-conformance-e2e.sh` builds and runs that suite against the shipped reference plugin, the dependency-free Python example, and the Codex workspace plugin with a local fake Codex binary.
