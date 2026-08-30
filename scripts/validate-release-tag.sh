#!/bin/sh
set -eu
export LC_ALL=C

usage() {
  echo "usage: $0 <vMAJOR.MINOR.PATCH> <40-lowercase-hex-commit>" >&2
  exit 2
}

[ "$#" -eq 2 ] || usage
VERSION=$1
COMMIT=$2
printf '%s\n' "$VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || usage
printf '%s\n' "$COMMIT" | grep -Eq '^[0-9a-f]{40}$' || usage

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
HEAD=$(git -C "$ROOT" rev-parse --verify HEAD 2>/dev/null) || { echo "release source is not a Git checkout" >&2; exit 1; }
[ "$HEAD" = "$COMMIT" ] || { echo "release commit does not match source HEAD" >&2; exit 1; }
[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ] || { echo "release source is not clean" >&2; exit 1; }
TAG_REF="refs/tags/$VERSION"
TAG_TYPE=$(git -C "$ROOT" cat-file -t "$TAG_REF" 2>/dev/null) || { echo "release tag does not exist: $VERSION" >&2; exit 1; }
[ "$TAG_TYPE" = tag ] || { echo "release tag must be annotated: $VERSION" >&2; exit 1; }
TAG_COMMIT=$(git -C "$ROOT" rev-parse --verify "$TAG_REF^{commit}" 2>/dev/null) || { echo "release tag does not resolve to a commit" >&2; exit 1; }
[ "$TAG_COMMIT" = "$COMMIT" ] || { echo "release tag does not point to release commit" >&2; exit 1; }
TAG_NAME=$(git -C "$ROOT" for-each-ref --format='%(refname:strip=2)' "$TAG_REF")
[ "$TAG_NAME" = "$VERSION" ] || { echo "release tag identity mismatch" >&2; exit 1; }
MAIN_REF=refs/remotes/origin/main
git -C "$ROOT" rev-parse --verify "$MAIN_REF^{commit}" >/dev/null 2>&1 || { echo "origin/main is unavailable for release validation" >&2; exit 1; }
git -C "$ROOT" merge-base --is-ancestor "$COMMIT" "$MAIN_REF" || { echo "release commit is not part of origin/main" >&2; exit 1; }
echo "release tag: $VERSION $COMMIT"
