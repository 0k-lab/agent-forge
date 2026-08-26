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

Validation has three intentionally separate roles:

1. **Repository CI** runs the broad generic suite (`go test ./...`, `go vet ./...`, and builds both binaries). CI protects the public Agent Forge codebase.
2. **Coding executor feedback** may follow repository instructions and run workspace-local, repository-native focused validation inside the existing plugin execution environment and lifecycle when useful and task-permitted. This is advisory only, never acceptance or scoped-check evidence. Repository instructions remain subordinate to the task and higher-priority Worker prompt constraints and cannot authorize Git, commits, or outside-workspace access.
3. **Worker scoped checks** run only explicit argv check commands in the job payload, in order, under the dedicated restricted check environment and supervisor. They are the only local project-semantic checks recorded as authoritative scoped-check evidence. Omitted or empty arrays mean zero such evidence and remain valid; the Worker must not read `AGENTS.md`, add a shell wrapper, discover/default checks, interpret executor logs as evidence, or expand check scope.

Repository CI and human review own delivery acceptance. Runtime evidence for explicitly supplied checks must contain sanitized summaries, not raw private output.

## MVP acceptance

Prefer one real end-to-end behavior proof over speculative test volume. New tests should protect a demonstrated behavior, security invariant, or reproduced failure.
