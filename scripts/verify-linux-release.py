#!/usr/bin/env python3
"""Verify the exact publishable Agent Forge Linux release asset set."""

import sys
from pathlib import Path

from release_assets import ReleaseError, validate_assets


def main() -> int:
    if len(sys.argv) != 3:
        print(f"usage: {sys.argv[0]} <vMAJOR.MINOR.PATCH> <asset-directory>", file=sys.stderr)
        return 2
    try:
        digests = validate_assets(sys.argv[1], Path(sys.argv[2]))
    except (OSError, ReleaseError) as error:
        print(f"Linux release verification failed: {error}", file=sys.stderr)
        return 1
    print(f"verified Linux release assets: {len(digests)} files")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
