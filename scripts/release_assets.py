"""Canonical Agent Forge Linux release asset validation."""

from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path

VERSION_RE = re.compile(r"^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")


class ReleaseError(RuntimeError):
    pass


def expected_archives(version: str) -> list[str]:
    return [
        f"agent-forge-{role}_{version}_linux_{arch}.tar.gz"
        for role in ("cli", "gate", "worker")
        for arch in ("amd64", "arm64")
    ]


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def validate_assets(version: str, directory: Path) -> dict[str, str]:
    if not VERSION_RE.fullmatch(version):
        raise ReleaseError("release version is malformed")
    archives = expected_archives(version)
    sbom_name = f"agent-forge_{version}_linux.spdx.json"
    expected = set(archives + ["SHA256SUMS", sbom_name])
    if not directory.is_dir() or directory.is_symlink():
        raise ReleaseError("release asset directory is not a regular directory")
    actual = {path.name for path in directory.iterdir()}
    if actual != expected:
        raise ReleaseError("release asset set does not match the Linux publication contract")
    for name in expected:
        path = directory / name
        if not path.is_file() or path.is_symlink():
            raise ReleaseError(f"release asset is not a regular file: {name}")

    manifest: dict[str, str] = {}
    lines = (directory / "SHA256SUMS").read_text(encoding="ascii").splitlines()
    if len(lines) != len(archives):
        raise ReleaseError("checksum manifest has an unexpected entry count")
    for line in lines:
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9._-]+)", line)
        if not match:
            raise ReleaseError("checksum manifest has malformed content")
        digest, name = match.groups()
        if name in manifest or name not in archives:
            raise ReleaseError("checksum manifest has an unexpected or duplicate asset")
        manifest[name] = digest
    if list(manifest) != sorted(archives):
        raise ReleaseError("checksum manifest is not sorted by asset name")
    for name in archives:
        if manifest.get(name) != sha256_file(directory / name):
            raise ReleaseError(f"checksum mismatch: {name}")

    try:
        sbom = json.loads((directory / sbom_name).read_text(encoding="utf-8"))
    except (UnicodeError, json.JSONDecodeError) as error:
        raise ReleaseError("SBOM is not valid UTF-8 JSON") from error
    if not isinstance(sbom, dict) or sbom.get("spdxVersion") not in {"SPDX-2.2", "SPDX-2.3"}:
        raise ReleaseError("SBOM is not SPDX 2.2 or SPDX 2.3 JSON")
    if sbom.get("name") != "agent-forge-linux" or not isinstance(sbom.get("packages"), list):
        raise ReleaseError("SBOM does not describe the canonical Linux release")
    package_names = {package.get("name") for package in sbom["packages"] if isinstance(package, dict)}
    required_packages = {"agent-forge", "stdlib", "github.com/coder/websocket", "modernc.org/sqlite"}
    if not required_packages.issubset(package_names):
        raise ReleaseError("SBOM is missing required release packages")

    return {name: sha256_file(directory / name) for name in sorted(expected)}
