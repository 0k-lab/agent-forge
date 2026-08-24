# Contributing

Agent Forge is developed in a public repository. Treat every commit, issue, workflow log, artifact, and test fixture as publicly visible.

## Privacy and credentials

Never commit:

- access tokens, passwords, private keys, certificates, cookies, or signed URLs;
- private hostnames, private IP addresses, internal filesystem paths, or infrastructure inventories;
- production prompts, transcripts, source snapshots, command output, or artifacts that may contain private data;
- real customer/repository payloads as fixtures.

Use synthetic fixtures and clearly fake example credentials. When reporting failures, keep public reason codes bounded and store private diagnostics outside the repository.

## Testing policy

Two test scopes are intentionally separate:

1. **Repository CI** runs the broad generic suite (`go test ./...`, `go vet ./...`, and builds both binaries). CI protects the public Agent Forge codebase.
2. **Task execution** runs only explicit argv test commands in the job payload. A Worker must not add a shell wrapper, invent tests, or expand test scope on its own.

A task may request broader tests only through an explicit, bounded repository policy. Runtime evidence must contain sanitized summaries, not raw private output.

## MVP acceptance

Prefer one real end-to-end behavior proof over speculative test volume. New tests should protect a demonstrated behavior, security invariant, or reproduced failure.
