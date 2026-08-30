#!/usr/bin/env python3
"""Bound and parse secret-free Agent Forge installer diagnostics."""

from __future__ import annotations

import io
import json
import os
import stat
import sys
import tempfile
from pathlib import Path
from typing import BinaryIO

MAX_DIAGNOSTIC_BYTES = 4096
ALLOWED_STAGES = frozenset(
    {
        "validate",
        "assets",
        "archives",
        "existing",
        "host",
        "account",
        "staging",
        "publication",
        "activation",
    }
)


def capture(source: BinaryIO, destination: str) -> None:
    captured = bytearray()
    while True:
        chunk = source.read(65536)
        if not chunk:
            break
        remaining = MAX_DIAGNOSTIC_BYTES + 1 - len(captured)
        if remaining > 0:
            captured.extend(chunk[:remaining])
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = os.open(destination, flags, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(captured)


def parse(path: str) -> str:
    try:
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
            return ""
        with open(path, "rb") as handle:
            body = handle.read(MAX_DIAGNOSTIC_BYTES + 1)
        if len(body) > MAX_DIAGNOSTIC_BYTES:
            return ""
        payload = json.loads(body)
        if not isinstance(payload, dict):
            return ""
        stage = payload.get("install_stage", "")
        if not isinstance(stage, str) or stage not in ALLOWED_STAGES:
            return ""
        return stage
    except (OSError, UnicodeError, ValueError, json.JSONDecodeError):
        return ""


def self_test() -> None:
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        valid = root / "valid.json"
        valid.write_text('{"failure_code":"invalid_input","install_stage":"account"}\n')
        assert parse(str(valid)) == "account"

        fixtures = {
            "malformed": b"{",
            "trailing": b'{"install_stage":"account"} trailing',
            "non-object": b'["account"]',
            "non-string": b'{"install_stage":["account"]}',
            "unknown": b'{"install_stage":"unknown"}',
            "injection": b'{"install_stage":"account\\n::error::injected"}',
            "secret-raw": b'owner-token-must-not-be-emitted',
            "oversized": b'{"install_stage":"account","padding":"' + b"x" * 4096 + b'"}',
        }
        for name, body in fixtures.items():
            path = root / f"{name}.json"
            path.write_bytes(body)
            assert parse(str(path)) == "", name

        captured = root / "captured.log"
        capture(io.BytesIO(b"x" * (1024 * 1024 + 123)), str(captured))
        assert captured.stat().st_size == MAX_DIAGNOSTIC_BYTES + 1
        assert parse(str(captured)) == ""


def main(argv: list[str]) -> int:
    if argv == ["self-test"]:
        self_test()
        return 0
    if len(argv) == 3 and argv[0] == "capture":
        with open(argv[1], "rb", buffering=0) as source:
            capture(source, argv[2])
        return 0
    if len(argv) == 2 and argv[0] == "parse":
        print(parse(argv[1]))
        return 0
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
