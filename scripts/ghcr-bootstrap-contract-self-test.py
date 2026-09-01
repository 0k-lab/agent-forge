#!/usr/bin/env python3
"""Negative-fixture tests for the structural GHCR bootstrap contract."""

from __future__ import annotations

import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PARSER = ROOT / "scripts" / "ghcr-bootstrap-yaml-contract.py"
WORKFLOW = ROOT / ".github" / "workflows" / "ghcr-bootstrap.yml"


def run_contract(text: str) -> subprocess.CompletedProcess[str]:
    with tempfile.NamedTemporaryFile("w", suffix=".yml", encoding="utf-8") as fixture:
        fixture.write(text)
        fixture.flush()
        return subprocess.run(
            [sys.executable, str(PARSER), fixture.name],
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
        )


def reject(name: str, text: str, expected_error: str) -> None:
    result = run_contract(text)
    if result.returncode == 0:
        raise AssertionError(f"negative fixture accepted: {name}\n{result.stdout}")
    if expected_error not in result.stdout:
        raise AssertionError(
            f"negative fixture failed for the wrong reason: {name}\n"
            f"expected {expected_error!r} in:\n{result.stdout}"
        )
    print(f"reject {name}: PASS")


def replace_once(text: str, old: str, new: str) -> str:
    if text.count(old) != 1:
        raise AssertionError(f"fixture source is not unique: {old!r}")
    return text.replace(old, new, 1)


def main() -> int:
    source = WORKFLOW.read_text(encoding="utf-8")
    positive = run_contract(source)
    if positive.returncode != 0:
        raise AssertionError(f"positive workflow rejected:\n{positive.stdout}")
    print("accept intended workflow: PASS")

    reject("push trigger", replace_once(source, "  workflow_dispatch:\n", "  workflow_dispatch:\n  push:\n"), "trigger is not exact")
    reject("permissions write-all", replace_once(source, "permissions:\n  contents: read\n", "permissions: write-all\n"), "top-level permissions is not exact")
    reject(
        "broad top-level permissions",
        replace_once(source, "permissions:\n  contents: read\n", "permissions:\n  contents: read\n  packages: write\n"),
        "top-level permissions is not exact",
    )
    commented = "\n".join(f"# {line}" for line in source.splitlines())
    reject(
        "legitimate workflow only in comments with empty jobs",
        f"name: Decoy\non:\n  workflow_dispatch:\npermissions:\n  contents: read\njobs: {{}}\n\n{commented}\n",
        "top-level keys is not exact",
    )
    cleanup = source[source.index("      - name: Remove GHCR credentials\n") :]
    before_cleanup = source[: source.index("      - name: Remove GHCR credentials\n")]
    verification_at = before_cleanup.index("      - name: Verify exact authenticated index\n")
    reject(
        "cleanup before verification",
        before_cleanup[:verification_at] + cleanup + before_cleanup[verification_at:],
        "ordered bootstrap steps is not exact",
    )
    reject("cleanup without always", replace_once(source, "        if: always()\n", "        if: success()\n"), "ordered bootstrap steps is not exact")
    exact_tag = "          tags: ghcr.io/0k-lab/agent-forge-gate:bootstrap-${{ steps.identity.outputs.commit }}\n"
    tag_prefix = "          tags: |\n            ghcr.io/0k-lab/agent-forge-gate:bootstrap-${{ steps.identity.outputs.commit }}\n"
    reject("stable alternate tag", replace_once(source, exact_tag, tag_prefix + "            ghcr.io/0k-lab/agent-forge-gate:stable\n"), "ordered bootstrap steps is not exact")
    reject("latest alternate tag", replace_once(source, exact_tag, tag_prefix + "            ghcr.io/0k-lab/agent-forge-gate:latest\n"), "ordered bootstrap steps is not exact")

    print("GHCR bootstrap structural contract self-test: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
