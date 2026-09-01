#!/usr/bin/env python3
"""Validate the GHCR bootstrap workflow as parsed YAML, never as text."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any, NoReturn

import yaml


class UniqueBaseLoader(yaml.BaseLoader):
    """YAML 1.2-like scalar handling plus rejection of duplicate mapping keys."""


def construct_unique_mapping(loader: UniqueBaseLoader, node: yaml.MappingNode, deep: bool = False) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in result:
            raise yaml.constructor.ConstructorError(
                "while constructing a mapping",
                node.start_mark,
                f"found duplicate key {key!r}",
                key_node.start_mark,
            )
        result[key] = loader.construct_object(value_node, deep=deep)
    return result


UniqueBaseLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,
    construct_unique_mapping,
)

CHECKOUT_SHA = "11bd71901bbe5b1630ceea73d27597364c9af683"
QEMU_SHA = "c7c53464625b32c7a7e944ae62b3e17d2b600130"
BUILDX_SHA = "8d2750c68a42422c14e847fe6c8ac0403b4cbd6f"
LOGIN_SHA = "c94ce9fb468520275223c153574b00df6fe4bcc9"
BUILD_PUSH_SHA = "10e90e3645eae34f1e60eeb005ba3a3d33f178e8"
ATTEST_SHA = "4d101475d8b20a2381f78447822ac1eab6504dd8"

IDENTITY_RUN = '''set -euo pipefail
test "$GITHUB_REPOSITORY" = 0k-lab/agent-forge
test "$GITHUB_REF" = refs/heads/main
commit="$(git rev-parse HEAD)"
test "$commit" = "$GITHUB_SHA"
git fetch --no-tags origin main
remote_main="$(git rev-parse FETCH_HEAD)"
test "$commit" = "$remote_main"
test -z "$(git status --porcelain --untracked-files=normal)"
printf 'commit=%s\\n' "$commit" >>"$GITHUB_OUTPUT"
printf 'tag=bootstrap-%s\\n' "$commit" >>"$GITHUB_OUTPUT"
'''

REFUSE_RUN = '''echo "The exact bootstrap tag already exists and will not be replaced." >&2
exit 1
'''

DOCKER_CONFIG_RUN = '''set -euo pipefail
install -d -m 0700 "$RUNNER_TEMP/agent-forge-ghcr-bootstrap-auth/.docker"
printf 'HOME=%s\\n' "$RUNNER_TEMP/agent-forge-ghcr-bootstrap-auth" >>"$GITHUB_ENV"
printf 'DOCKER_CONFIG=%s\\n' "$RUNNER_TEMP/agent-forge-ghcr-bootstrap-auth/.docker" >>"$GITHUB_ENV"
'''

VERIFY_RUN = '''set -euo pipefail
docker buildx imagetools inspect --raw "$IMAGE:$TAG" >"$RUNNER_TEMP/agent-forge-gate-bootstrap-index.json"
python3 scripts/verify-oci-gate-release.py \\
  "$RUNNER_TEMP/agent-forge-gate-bootstrap-index.json" "$EXPECTED_DIGEST"
printf 'Bootstrap image: `%s@%s`\\n' "$IMAGE" "$EXPECTED_DIGEST" >>"$GITHUB_STEP_SUMMARY"
printf 'Bootstrap tag: `%s:%s`\\n' "$IMAGE" "$TAG" >>"$GITHUB_STEP_SUMMARY"
printf '\\nMake the package public in GitHub Package settings, then verify an anonymous pull before creating a stable release tag.\\n' >>"$GITHUB_STEP_SUMMARY"
'''

CLEANUP_RUN = '''set -euo pipefail
if [ "${HOME:-}" = "$RUNNER_TEMP/agent-forge-ghcr-bootstrap-auth" ] &&
   [ "${DOCKER_CONFIG:-}" = "$HOME/.docker" ]; then
  docker logout ghcr.io || true
  rm -rf -- "$DOCKER_CONFIG"
fi
'''

EXPECTED_STEPS: list[dict[str, Any]] = [
    {
        "uses": f"actions/checkout@{CHECKOUT_SHA}",
        "with": {
            "ref": "${{ github.sha }}",
            "fetch-depth": "0",
            "persist-credentials": "false",
        },
    },
    {
        "name": "Validate exact main identity",
        "id": "identity",
        "shell": "bash",
        "run": IDENTITY_RUN,
    },
    {
        "name": "Probe exact bootstrap tag",
        "id": "tag",
        "env": {
            "GITHUB_ACTOR": "${{ github.actor }}",
            "GITHUB_TOKEN": "${{ github.token }}",
        },
        "run": 'python3 scripts/ghcr-tag-state.py 0k-lab/agent-forge-gate "${{ steps.identity.outputs.tag }}" >>"$GITHUB_OUTPUT"',
    },
    {
        "name": "Refuse an existing bootstrap tag",
        "if": "steps.tag.outputs.state != 'absent'",
        "run": REFUSE_RUN,
    },
    {
        "name": "Use isolated authenticated Docker config",
        "run": DOCKER_CONFIG_RUN,
    },
    {"uses": f"docker/setup-qemu-action@{QEMU_SHA}"},
    {"uses": f"docker/setup-buildx-action@{BUILDX_SHA}"},
    {
        "uses": f"docker/login-action@{LOGIN_SHA}",
        "with": {
            "registry": "ghcr.io",
            "username": "${{ github.actor }}",
            "password": "${{ github.token }}",
        },
    },
    {
        "name": "Build and push exact bootstrap image",
        "id": "build",
        "uses": f"docker/build-push-action@{BUILD_PUSH_SHA}",
        "with": {
            "context": ".",
            "file": "Dockerfile.gate",
            "platforms": "linux/amd64,linux/arm64",
            "push": "true",
            "pull": "true",
            "no-cache": "true",
            "sbom": "true",
            "provenance": "mode=max",
            "tags": "ghcr.io/0k-lab/agent-forge-gate:bootstrap-${{ steps.identity.outputs.commit }}",
            "build-args": "VERSION=v0.0.0\nCOMMIT=${{ steps.identity.outputs.commit }}\n",
        },
    },
    {
        "name": "Attest bootstrap image provenance",
        "uses": f"actions/attest-build-provenance@{ATTEST_SHA}",
        "with": {
            "subject-name": "ghcr.io/0k-lab/agent-forge-gate",
            "subject-digest": "${{ steps.build.outputs.digest }}",
            "push-to-registry": "true",
        },
    },
    {
        "name": "Verify exact authenticated index",
        "env": {
            "IMAGE": "ghcr.io/0k-lab/agent-forge-gate",
            "TAG": "bootstrap-${{ steps.identity.outputs.commit }}",
            "EXPECTED_DIGEST": "${{ steps.build.outputs.digest }}",
        },
        "run": VERIFY_RUN,
    },
    {
        "name": "Remove GHCR credentials",
        "if": "always()",
        "run": CLEANUP_RUN,
    },
]


def fail(message: str) -> NoReturn:
    raise ValueError(message)


def require_equal(actual: Any, expected: Any, label: str) -> None:
    if actual != expected:
        fail(f"{label} is not exact: expected {expected!r}, got {actual!r}")


def validate(document: Any) -> None:
    if not isinstance(document, dict):
        fail("workflow root must be a mapping")
    require_equal(set(document), {"name", "on", "permissions", "concurrency", "jobs"}, "top-level keys")
    require_equal(document["name"], "GHCR Bootstrap", "workflow name")
    require_equal(document["on"], {"workflow_dispatch": ""}, "trigger")
    require_equal(document["permissions"], {"contents": "read"}, "top-level permissions")
    require_equal(
        document["concurrency"],
        {"group": "ghcr-bootstrap", "cancel-in-progress": "false"},
        "concurrency",
    )
    jobs = document["jobs"]
    require_equal(set(jobs) if isinstance(jobs, dict) else None, {"bootstrap"}, "jobs")
    job = jobs["bootstrap"]
    if not isinstance(job, dict):
        fail("bootstrap job must be a mapping")
    require_equal(set(job), {"runs-on", "permissions", "steps"}, "bootstrap job keys")
    require_equal(job["runs-on"], "ubuntu-latest", "bootstrap runner")
    require_equal(
        job["permissions"],
        {
            "contents": "read",
            "packages": "write",
            "id-token": "write",
            "attestations": "write",
        },
        "bootstrap permissions",
    )
    require_equal(job["steps"], EXPECTED_STEPS, "ordered bootstrap steps")


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {Path(argv[0]).name} WORKFLOW", file=sys.stderr)
        return 2
    workflow = Path(argv[1])
    try:
        document = yaml.load(workflow.read_text(encoding="utf-8"), Loader=UniqueBaseLoader)
        validate(document)
    except (OSError, UnicodeError, yaml.YAMLError, ValueError) as error:
        print(f"GHCR bootstrap YAML contract: FAIL: {error}", file=sys.stderr)
        return 1
    print("GHCR bootstrap YAML contract: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
