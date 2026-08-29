#!/usr/bin/env python3
"""Atomically rename a path without replacing any existing destination."""

from __future__ import annotations

import ctypes
import os
import sys

AT_FDCWD = -100
RENAME_NOREPLACE = 1


def main() -> int:
    if len(sys.argv) != 3:
        print(f"usage: {sys.argv[0]} <source> <destination>", file=sys.stderr)
        return 2

    libc = ctypes.CDLL(None, use_errno=True)
    try:
        renameat2 = libc.renameat2
    except AttributeError:
        print("renameat2 is unavailable on this Linux host", file=sys.stderr)
        return 1

    renameat2.argtypes = [
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_uint,
    ]
    renameat2.restype = ctypes.c_int

    source = os.fsencode(sys.argv[1])
    destination = os.fsencode(sys.argv[2])
    if renameat2(AT_FDCWD, source, AT_FDCWD, destination, RENAME_NOREPLACE) != 0:
        error = ctypes.get_errno()
        print(f"atomic no-replace publication failed: {os.strerror(error)}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
