#!/bin/sh
set -eu

usage() {
  echo "usage: $0 <vMAJOR.MINOR.PATCH> <40-lowercase-hex-commit> <output-directory>" >&2
  exit 2
}

[ "$#" -eq 3 ] || usage
VERSION=$1
COMMIT=$2
OUTPUT_ARG=$3
case "$VERSION" in ''|*[!0-9A-Za-z.]*) usage ;; esac
[ "${#VERSION}" -le 128 ] || usage
case "$COMMIT" in ''|*[!0-9a-f]*) usage ;; esac
printf '%s\n' "$VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || usage
printf '%s\n' "$COMMIT" | grep -Eq '^[0-9a-f]{40}$' || usage
case "$OUTPUT_ARG" in ''|.|..|/) usage ;; esac

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
OUTPUT_PARENT=$(dirname -- "$OUTPUT_ARG")
OUTPUT_NAME=$(basename -- "$OUTPUT_ARG")
case "$OUTPUT_NAME" in ''|.|..) usage ;; esac
mkdir -p "$OUTPUT_PARENT"
OUTPUT_PARENT=$(CDPATH= cd -- "$OUTPUT_PARENT" && pwd -P)
OUTPUT="$OUTPUT_PARENT/$OUTPUT_NAME"
[ "$OUTPUT" != "$ROOT" ] || usage
case "$OUTPUT" in
  "$ROOT"/*) [ "$OUTPUT" = "$ROOT/dist" ] || { echo "repository-local output must be $ROOT/dist" >&2; exit 1; } ;;
esac
for tool in go git tar gzip sha256sum mktemp grep sed sort python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done
tar --version 2>/dev/null | grep -q 'GNU tar' || { echo "GNU tar is required" >&2; exit 1; }
HEAD=$(git -C "$ROOT" rev-parse --verify HEAD 2>/dev/null) || { echo "release source is not a Git checkout" >&2; exit 1; }
[ "$HEAD" = "$COMMIT" ] || { echo "release commit does not match source HEAD" >&2; exit 1; }
[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ] || { echo "release source is not clean" >&2; exit 1; }
if [ -e "$OUTPUT" ] || [ -L "$OUTPUT" ]; then
  echo "refusing to replace existing output: $OUTPUT" >&2
  exit 1
fi

umask 022
WORK=$(mktemp -d "${TMPDIR:-/tmp}/agent-forge-release.XXXXXX")
DEST=$(mktemp -d "$OUTPUT_PARENT/.${OUTPUT_NAME}.tmp.XXXXXX")
cleanup() {
  rm -rf "$WORK" "$DEST"
}
trap cleanup EXIT INT TERM
SOURCE="$WORK/source"
mkdir -p "$SOURCE"
git -C "$ROOT" archive --format=tar "$COMMIT" | tar -xf - -C "$SOURCE"

LDFLAGS="-s -w -buildid= -X=agent-forge/internal/buildinfo.Version=$VERSION -X=agent-forge/internal/buildinfo.Commit=$COMMIT"
export CGO_ENABLED=0
export SOURCE_DATE_EPOCH=0
export LC_ALL=C
unset TAR_OPTIONS GZIP POSIXLY_CORRECT
export GOFLAGS=-mod=readonly
export GOENV=off
export GOWORK=off
export GOEXPERIMENT=
export GOFIPS140=off
export GOTOOLCHAIN=go1.24.4
[ "$(go env GOVERSION)" = go1.24.4 ] || { echo "Go toolchain selection failed" >&2; exit 1; }

build_binary() {
  goos=$1 goarch=$2 package=$3 destination=$4
  (
    cd "$SOURCE"
    unset GOAMD64 GOARM64
    case "$goarch" in
      amd64) export GOAMD64=v1 ;;
      arm64) export GOARM64=v8.0 ;;
      *) exit 1 ;;
    esac
    GOOS=$goos GOARCH=$goarch go build -trimpath -buildvcs=false -ldflags="$LDFLAGS" -o "$destination" "./$package"
  )
  chmod 0755 "$destination"
}

write_version() {
  destination=$1
  printf 'version=%s\ncommit=%s\n' "$VERSION" "$COMMIT" >"$destination"
  chmod 0644 "$destination"
}

archive_stage() {
  stage=$1 archive=$2 root_name=$3
  tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner --format=ustar --mode='u+rwX,go+rX,go-w' -cf - -C "$stage" "$root_name" |
    gzip -n -9 >"$DEST/$archive"
}

build_bundle() {
  role=$1 goos=$2 goarch=$3
  archive="agent-forge-${role}_${VERSION}_${goos}_${goarch}.tar.gz"
  root_name=${archive%.tar.gz}
  stage="$WORK/stage-$role-$goos-$goarch"
  bundle="$stage/$root_name"
  mkdir -p "$bundle"
  write_version "$bundle/VERSION"
  case "$role" in
    gate)
      build_binary "$goos" "$goarch" cmd/forge-gate "$bundle/forge-gate"
      ;;
    cli)
      build_binary "$goos" "$goarch" cmd/forge "$bundle/forge"
      ;;
    worker)
      build_binary "$goos" "$goarch" cmd/forge-worker "$bundle/forge-worker"
      build_binary "$goos" "$goarch" cmd/forge-codex-plugin "$bundle/forge-codex-plugin"
      build_binary "$goos" "$goarch" cmd/forge-ref-plugin "$bundle/forge-ref-plugin"
      ;;
    *) exit 1 ;;
  esac
  archive_stage "$stage" "$archive" "$root_name"
}

build_bundle gate linux amd64
build_bundle gate linux arm64
build_bundle cli linux amd64
build_bundle cli linux arm64
build_bundle worker linux amd64
build_bundle worker linux arm64
build_bundle worker darwin amd64
build_bundle worker darwin arm64
(
  cd "$DEST"
  sha256sum ./*.tar.gz | sed 's#  \./#  #' | LC_ALL=C sort -k2 >SHA256SUMS
)

python3 "$SOURCE/scripts/rename-noreplace.py" "$DEST" "$OUTPUT"
DEST=
echo "release artifacts: $OUTPUT"
