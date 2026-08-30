#!/bin/sh
set -eu
export LC_ALL=C

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
VERSION=${1:-}
COMMIT=${2:-}
ASSET_DIR=${3:-}

case "$VERSION" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "usage: $0 <vMAJOR.MINOR.PATCH> <40-lowercase-hex-commit> <absolute-asset-dir>" >&2; exit 2 ;;
esac
case "$COMMIT" in
  [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
  *) echo "usage: $0 <vMAJOR.MINOR.PATCH> <40-lowercase-hex-commit> <absolute-asset-dir>" >&2; exit 2 ;;
esac
case "$ASSET_DIR" in
  /*) ;;
  *) echo "usage: $0 <vMAJOR.MINOR.PATCH> <40-lowercase-hex-commit> <absolute-asset-dir>" >&2; exit 2 ;;
esac
[ -f "$ASSET_DIR/SHA256SUMS" ] || { echo "missing SHA256SUMS" >&2; exit 1; }

RELEASE_DIR=$ASSET_DIR
TMP=
cleanup() { [ -z "$TMP" ] || rm -rf "$TMP"; }
trap cleanup EXIT INT TERM
if sed -n 's/^[0-9a-f]\{64\}  //p' "$ASSET_DIR/SHA256SUMS" | grep -q '_darwin_'; then
  TMP=$(mktemp -d)
  "$ROOT/scripts/prepare-linux-release.sh" "$VERSION" "$ASSET_DIR" "$TMP/linux"
  RELEASE_DIR=$TMP/linux
fi

ANCHOR=$(sha256sum "$RELEASE_DIR/SHA256SUMS")
ANCHOR=${ANCHOR%% *}
AGENT_FORGE_INSTALL_E2E_VERSION=$VERSION \
AGENT_FORGE_INSTALL_E2E_COMMIT=$COMMIT \
AGENT_FORGE_INSTALL_E2E_ASSET_DIR=$RELEASE_DIR \
AGENT_FORGE_INSTALL_E2E_SHA256SUMS_SHA256=$ANCHOR \
go test -count=1 -run '^TestReleaseInstallE2E$' ./internal/linuxinstall

echo "linux installer e2e: PASS"
