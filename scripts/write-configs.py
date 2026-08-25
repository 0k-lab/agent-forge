#!/usr/bin/env python3
import json
import os
import sys

gate_path, worker_path, listen, database, gate_url, worker_id, repository_id, repository_path, plugin_id, plugin_path, lease_ttl, retry_base, max_attempts, heartbeat = sys.argv[1:]
root = os.path.dirname(worker_path)
worktree_root, runtime_root = os.path.join(root, "worktrees"), os.path.join(root, "runtime")
repository_root = repository_path if repository_path != "-" else os.path.join(root, "repositories")
for path in (worktree_root, runtime_root, repository_root):
    os.makedirs(path, mode=0o700, exist_ok=True)
execution = {
    "plugin_id": plugin_id, "environment": ["PATH", "CODEX_HOME", "CODEX_BIN"],
    "plugin_timeout": "15m", "check_timeout": "10m", "git_timeout": "1m", "cleanup_timeout": "10s",
    "plugin_output_bytes": 1048576, "check_output_bytes": 2048, "git_output_bytes": 1048576,
}
gate = {
    "version": 1, "listen": listen, "database": database, "owner_token_env": "FORGE_OWNER_TOKEN",
    "recovery_interval": "50ms", "lease_poll_interval": "10ms", "default_pool": "default",
    "lifecycle": {"lease_ttl": lease_ttl, "retry_base": retry_base, "max_attempts": int(max_attempts)},
    "default_execution": execution,
    "workers": [{"id": worker_id, "pool": "default", "token_env": "FORGE_WORKER_TOKEN", "concurrency": 1}],
    "repositories": [] if repository_path == "-" else [{"id": repository_id, "default_branch": "main", "worker_pool": "default", "execution": execution}],
}
worker = {
    "version": 1, "gate_url": gate_url, "id": worker_id, "token_env": "FORGE_WORKER_TOKEN",
    "heartbeat_interval": heartbeat, "concurrency": 1, "repository_roots": [repository_root],
    "worktree_root": worktree_root, "runtime_root": runtime_root,
    "repositories": [] if repository_path == "-" else [{"id": repository_id, "path": repository_path}],
    "plugins": [{"id": plugin_id, "argv": [plugin_path]}],
    "environment_allowlist": ["PATH", "CODEX_HOME", "CODEX_BIN"],
    "check_environment_allowlist": ["PATH"],
    "ceilings": {"plugin_timeout": "15m", "check_timeout": "10m", "git_timeout": "1m", "cleanup_timeout": "10s", "plugin_output_bytes": 1048576, "check_output_bytes": 2048, "git_output_bytes": 1048576},
}
for path, value in ((gate_path, gate), (worker_path, worker)):
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(value, handle, separators=(",", ":"))
    os.chmod(path, 0o600)
