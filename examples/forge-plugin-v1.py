#!/usr/bin/env python3
"""Minimal dependency-free text plugin implemented from docs/plugin-protocol-v1.md."""

import json
import re
import sys

LIMITS = {
    "frame_bytes": 1048576,
    "progress_frames": 128,
    "text_bytes": 65536,
    "progress_text_bytes": 1024,
    "commit_subject_bytes": 256,
}
CAPABILITIES = {"text", "workspace_edit", "progress", "cancel", "commit_subject"}
ID = re.compile(r"[0-9a-f]{32}").fullmatch


def read(fields):
    line = sys.stdin.buffer.readline(1048578)
    if not line.endswith(b"\n") or line.endswith(b"\r\n") or len(line) == 1 or len(line) - 1 > 1048576:
        raise ValueError("invalid frame")
    body = line[:-1]
    compact(body)
    value = json.loads(body.decode("utf-8"), object_pairs_hook=no_duplicates, parse_constant=invalid_constant)
    if type(value) is not dict or set(value) != set(fields):
        raise ValueError("invalid fields")
    return value


def compact(body):
    quoted = escaped = False
    for byte in body:
        if quoted:
            if escaped:
                escaped = False
            elif byte == 92:
                escaped = True
            elif byte == 34:
                quoted = False
        elif byte == 34:
            quoted = True
        elif byte in b" \t\r\n":
            raise ValueError("non-compact frame")


def no_duplicates(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate field")
        value[key] = item
    return value


def invalid_constant(_):
    raise ValueError("non-standard constant")


def write(value):
    sys.stdout.buffer.write((json.dumps(value, separators=(",", ":"), ensure_ascii=False) + "\n").encode("utf-8"))
    sys.stdout.buffer.flush()


initialize = read(("version", "id", "type", "capabilities", "limits"))
capabilities = initialize["capabilities"]
limits = initialize["limits"]
if (
    initialize["version"] != "v1"
    or not ID(initialize["id"])
    or initialize["type"] != "initialize"
    or type(capabilities) is not list
    or any(type(item) is not str or item not in CAPABILITIES for item in capabilities)
    or len(capabilities) != len(set(capabilities))
    or "text" not in capabilities
    or type(limits) is not dict
    or set(limits) != set(LIMITS)
    or any(type(limits[key]) is not int or limits[key] != value for key, value in LIMITS.items())
):
    raise ValueError("incompatible")
write({"version": "v1", "id": initialize["id"], "type": "initialized", "capabilities": ["text"]})
execute = read(("version", "id", "type", "operation", "input"))
if (
    execute["version"] != "v1"
    or execute["id"] != initialize["id"]
    or execute["type"] != "execute"
    or execute["operation"] != "text"
    or type(execute["input"]) is not str
    or len(execute["input"].encode("utf-8")) > 65536
):
    raise ValueError("invalid request")
output = execute["input"].upper()
if len(output.encode("utf-8")) > 65536:
    raise ValueError("oversized output")
write({"version": "v1", "id": initialize["id"], "type": "result", "output": output})
