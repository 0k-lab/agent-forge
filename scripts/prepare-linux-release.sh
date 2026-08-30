#!/bin/sh
set -eu
export LC_ALL=C
unset POSIXLY_CORRECT

usage() {
  echo "usage: $0 <vMAJOR.MINOR.PATCH> <full-matrix-directory> <linux-release-directory>" >&2
  exit 2
}

[ "$#" -eq 3 ] || usage
VERSION=$1
INPUT_ARG=$2
OUTPUT_ARG=$3
printf '%s\n' "$VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || usage
[ -d "$INPUT_ARG" ] && [ ! -L "$INPUT_ARG" ] || usage
case "$OUTPUT_ARG" in ''|.|..|/) usage ;; esac

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
INPUT=$(CDPATH= cd -- "$INPUT_ARG" && pwd -P)
OUTPUT_PARENT=$(dirname -- "$OUTPUT_ARG")
OUTPUT_NAME=$(basename -- "$OUTPUT_ARG")
case "$OUTPUT_NAME" in ''|.|..) usage ;; esac
mkdir -p "$OUTPUT_PARENT"
OUTPUT_PARENT=$(CDPATH= cd -- "$OUTPUT_PARENT" && pwd -P)
OUTPUT="$OUTPUT_PARENT/$OUTPUT_NAME"
[ "$OUTPUT" != "$INPUT" ] || usage
if [ -e "$OUTPUT" ] || [ -L "$OUTPUT" ]; then
  echo "refusing to replace existing output: $OUTPUT" >&2
  exit 1
fi
for tool in sha256sum mktemp sed sort cp python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done

EXPECTED_ALL="agent-forge-cli_${VERSION}_linux_amd64.tar.gz
agent-forge-cli_${VERSION}_linux_arm64.tar.gz
agent-forge-gate_${VERSION}_linux_amd64.tar.gz
agent-forge-gate_${VERSION}_linux_arm64.tar.gz
agent-forge-worker_${VERSION}_darwin_amd64.tar.gz
agent-forge-worker_${VERSION}_darwin_arm64.tar.gz
agent-forge-worker_${VERSION}_linux_amd64.tar.gz
agent-forge-worker_${VERSION}_linux_arm64.tar.gz"
EXPECTED_FILES="SHA256SUMS
$EXPECTED_ALL"
ACTUAL_FILES=$(
  cd "$INPUT"
  for path in ./*; do
    [ -e "$path" ] || continue
    basename -- "$path"
  done | sort
)
[ "$ACTUAL_FILES" = "$EXPECTED_FILES" ] || { echo "unexpected full release input set" >&2; exit 1; }
for name in $EXPECTED_ALL SHA256SUMS; do
  [ -f "$INPUT/$name" ] && [ ! -L "$INPUT/$name" ] || { echo "invalid release input: $name" >&2; exit 1; }
done
MANIFEST_NAMES=$(sed -n 's/^[0-9a-f]\{64\}  //p' "$INPUT/SHA256SUMS")
[ "$MANIFEST_NAMES" = "$EXPECTED_ALL" ] || { echo "unexpected full release checksum manifest" >&2; exit 1; }
(cd "$INPUT" && sha256sum -c SHA256SUMS >/dev/null)

umask 022
DEST=$(mktemp -d "$OUTPUT_PARENT/.${OUTPUT_NAME}.tmp.XXXXXX")
cleanup() { rm -rf "$DEST"; }
trap cleanup EXIT INT TERM
for name in $EXPECTED_ALL; do
  case "$name" in
    *_linux_*) cp -- "$INPUT/$name" "$DEST/$name" ;;
  esac
done
(
  cd "$DEST"
  sha256sum ./*.tar.gz | sed 's#  \./#  #' | sort -k2 >SHA256SUMS
)
chmod 0644 "$DEST"/*
python3 "$ROOT/scripts/rename-noreplace.py" "$DEST" "$OUTPUT"
DEST=
echo "Linux release assets: $OUTPUT"
