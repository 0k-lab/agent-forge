#!/bin/sh
set -eu
export LC_ALL=C
unset GOFIPS140 TAR_OPTIONS GZIP POSIXLY_CORRECT
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
VERSION=v0.0.0
COMMIT=${1:-}
case "$COMMIT" in
  [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
  *) echo "usage: $0 <40-lowercase-hex-commit>" >&2; exit 2 ;;
esac
SECOND=$(mktemp -d)
SECOND_OUT="$SECOND/out"
EXTRACT=$(mktemp -d)
DIRTY_MARKER=
cleanup() {
  [ -z "$DIRTY_MARKER" ] || rm -f "$DIRTY_MARKER"
  rm -rf "$SECOND" "$EXTRACT"
}
trap cleanup EXIT INT TERM

SOURCE=$ROOT
if [ "$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || true)" != "$COMMIT" ] || [ -n "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ]; then
  SOURCE="$SECOND/source"
  mkdir -p "$SOURCE"
  (cd "$ROOT" && git ls-files -co --exclude-standard -z | tar --null -T - -cf -) | (cd "$SOURCE" && tar -xf -)
  git -C "$SOURCE" init -q
  git -C "$SOURCE" config user.name 'Agent Forge Release Test'
  git -C "$SOURCE" config user.email 'release-test@example.invalid'
  git -C "$SOURCE" add -A
  GIT_AUTHOR_DATE='2000-01-01T00:00:00Z' GIT_COMMITTER_DATE='2000-01-01T00:00:00Z' git -C "$SOURCE" commit -qm 'test: release source snapshot'
  COMMIT=$(git -C "$SOURCE" rev-parse HEAD)
fi
BUILDER="$SOURCE/scripts/build-release.sh"
PUBLISHER="$SOURCE/scripts/rename-noreplace.py"
DIST="$SOURCE/dist"

PUBLISH_TEST="$SECOND/publish"
mkdir -p "$PUBLISH_TEST/source-absent"
printf 'new\n' >"$PUBLISH_TEST/source-absent/artifact"
python3 "$PUBLISHER" "$PUBLISH_TEST/source-absent" "$PUBLISH_TEST/destination-absent"
[ ! -e "$PUBLISH_TEST/source-absent" ]
[ "$(cat "$PUBLISH_TEST/destination-absent/artifact")" = new ]

mkdir "$PUBLISH_TEST/source-empty" "$PUBLISH_TEST/destination-empty"
printf 'new\n' >"$PUBLISH_TEST/source-empty/artifact"
if python3 "$PUBLISHER" "$PUBLISH_TEST/source-empty" "$PUBLISH_TEST/destination-empty" >/dev/null 2>&1; then
  echo "publisher replaced an empty destination directory" >&2
  exit 1
fi
[ -f "$PUBLISH_TEST/source-empty/artifact" ] && [ -d "$PUBLISH_TEST/destination-empty" ]

mkdir "$PUBLISH_TEST/source-nonempty" "$PUBLISH_TEST/destination-nonempty"
printf 'new\n' >"$PUBLISH_TEST/source-nonempty/artifact"
printf 'keep\n' >"$PUBLISH_TEST/destination-nonempty/operator-data"
if python3 "$PUBLISHER" "$PUBLISH_TEST/source-nonempty" "$PUBLISH_TEST/destination-nonempty" >/dev/null 2>&1; then
  echo "publisher replaced a non-empty destination directory" >&2
  exit 1
fi
[ -f "$PUBLISH_TEST/source-nonempty/artifact" ] && [ "$(cat "$PUBLISH_TEST/destination-nonempty/operator-data")" = keep ]

mkdir "$PUBLISH_TEST/source-file"
printf 'new\n' >"$PUBLISH_TEST/source-file/artifact"
printf 'keep\n' >"$PUBLISH_TEST/destination-file"
if python3 "$PUBLISHER" "$PUBLISH_TEST/source-file" "$PUBLISH_TEST/destination-file" >/dev/null 2>&1; then
  echo "publisher replaced a destination file" >&2
  exit 1
fi
[ -f "$PUBLISH_TEST/source-file/artifact" ] && [ "$(cat "$PUBLISH_TEST/destination-file")" = keep ]

mkdir "$PUBLISH_TEST/source-symlink"
printf 'new\n' >"$PUBLISH_TEST/source-symlink/artifact"
ln -s destination-nonempty "$PUBLISH_TEST/destination-symlink"
if python3 "$PUBLISHER" "$PUBLISH_TEST/source-symlink" "$PUBLISH_TEST/destination-symlink" >/dev/null 2>&1; then
  echo "publisher replaced a destination symlink" >&2
  exit 1
fi
[ -f "$PUBLISH_TEST/source-symlink/artifact" ] && [ -L "$PUBLISH_TEST/destination-symlink" ]
[ "$(cat "$PUBLISH_TEST/destination-nonempty/operator-data")" = keep ]

PROTECTED="$SECOND/protected"
mkdir "$PROTECTED"
printf 'keep\n' >"$PROTECTED/operator-data"
printf 'not-a-release\n' >"$PROTECTED/SHA256SUMS"
if "$BUILDER" "$VERSION" "$COMMIT" "$PROTECTED" >/dev/null 2>&1; then
  echo "builder replaced an existing external directory" >&2
  exit 1
fi
[ "$(cat "$PROTECTED/operator-data")" = keep ]
if "$BUILDER" 'v1.2' "$COMMIT" "$SECOND/invalid-version" >/dev/null 2>&1; then
  echo "builder accepted an invalid version" >&2
  exit 1
fi
MULTILINE_VERSION=$(printf 'v1.2.3\nbad')
if "$BUILDER" "$MULTILINE_VERSION" "$COMMIT" "$SECOND/multiline-version" >/dev/null 2>&1; then
  echo "builder accepted a multiline version" >&2
  exit 1
fi
for invalid_semver in v01.2.3 v1.02.3 v1.2.03 v1.2.3-alpha v1.2.3-rc.1 v1.2.3-nightly; do
  if "$BUILDER" "$invalid_semver" "$COMMIT" "$SECOND/invalid-semver" >/dev/null 2>&1; then
    echo "builder accepted invalid SemVer: $invalid_semver" >&2
    exit 1
  fi
done
OVERLONG_VERSION=v1.2.3-
while [ "${#OVERLONG_VERSION}" -le 128 ]; do OVERLONG_VERSION="${OVERLONG_VERSION}a"; done
if "$BUILDER" "$OVERLONG_VERSION" "$COMMIT" "$SECOND/overlong-version" >/dev/null 2>&1; then
  echo "builder accepted an overlong version" >&2
  exit 1
fi
if "$BUILDER" "$VERSION" "$COMMIT" "$SOURCE/internal/release-output" >/dev/null 2>&1; then
  echo "builder accepted a non-dist repository output" >&2
  exit 1
fi
if "$BUILDER" "$VERSION" deadbeef "$SECOND/invalid-commit" >/dev/null 2>&1; then
  echo "builder accepted an abbreviated commit" >&2
  exit 1
fi
case "$COMMIT" in
  0*) MISMATCH="1${COMMIT#?}" ;;
  *) MISMATCH="0${COMMIT#?}" ;;
esac
if "$BUILDER" "$VERSION" "$MISMATCH" "$SECOND/mismatched-commit" >/dev/null 2>&1; then
  echo "builder accepted a commit that does not match source HEAD" >&2
  exit 1
fi
DIRTY_MARKER="$SOURCE/.release-dirty-test"
printf 'dirty\n' >"$DIRTY_MARKER"
if "$BUILDER" "$VERSION" "$COMMIT" "$SECOND/dirty-source" >/dev/null 2>&1; then
  echo "builder accepted a dirty source tree" >&2
  exit 1
fi
rm -f "$DIRTY_MARKER"
[ ! -e "$SECOND/invalid-version" ] && [ ! -e "$SECOND/multiline-version" ] && [ ! -e "$SECOND/invalid-semver" ] && [ ! -e "$SECOND/overlong-version" ] && [ ! -e "$SOURCE/internal/release-output" ] && [ ! -e "$SECOND/invalid-commit" ] && [ ! -e "$SECOND/mismatched-commit" ] && [ ! -e "$SECOND/dirty-source" ]

"$BUILDER" "$VERSION" "$COMMIT" "$DIST"
MANIFEST_BEFORE=$(sha256sum "$DIST/SHA256SUMS")
if "$BUILDER" "$VERSION" "$COMMIT" "$DIST" >/dev/null 2>&1; then
  echo "builder replaced an existing canonical output" >&2
  exit 1
fi
[ "$(sha256sum "$DIST/SHA256SUMS")" = "$MANIFEST_BEFORE" ]
GOFIPS140=latest TAR_OPTIONS=--exclude=VERSION GZIP=-1 POSIXLY_CORRECT=1 "$BUILDER" "$VERSION" "$COMMIT" "$SECOND_OUT"

EXPECTED='agent-forge-cli_v0.0.0_linux_amd64.tar.gz
agent-forge-cli_v0.0.0_linux_arm64.tar.gz
agent-forge-gate_v0.0.0_linux_amd64.tar.gz
agent-forge-gate_v0.0.0_linux_arm64.tar.gz
agent-forge-worker_v0.0.0_darwin_amd64.tar.gz
agent-forge-worker_v0.0.0_darwin_arm64.tar.gz
agent-forge-worker_v0.0.0_linux_amd64.tar.gz
agent-forge-worker_v0.0.0_linux_arm64.tar.gz'
ACTUAL=$(sed 's/^.*  //' "$DIST/SHA256SUMS")
[ "$ACTUAL" = "$EXPECTED" ] || { echo "unexpected release matrix" >&2; printf '%s\n' "$ACTUAL" >&2; exit 1; }
(
  cd "$DIST"
  sha256sum -c SHA256SUMS
)
cmp "$DIST/SHA256SUMS" "$SECOND_OUT/SHA256SUMS"
for archive in $EXPECTED; do
  cmp "$DIST/$archive" "$SECOND_OUT/$archive"
done

check_archive() {
  role=$1 os=$2 arch=$3
  archive="agent-forge-${role}_${VERSION}_${os}_${arch}.tar.gz"
  root=${archive%.tar.gz}
  case "$role" in
    gate) binaries='forge-gate' ;;
    cli) binaries='forge' ;;
    worker) binaries='forge-codex-plugin forge-ref-plugin forge-worker' ;;
    *) exit 1 ;;
  esac
  want="$root/
$root/VERSION"
  for binary in $binaries; do
    want="$want
$root/$binary"
  done
  got=$(tar -tzf "$DIST/$archive")
  [ "$got" = "$want" ] || { echo "unexpected contents: $archive" >&2; printf '%s\n' "$got" >&2; exit 1; }
  rm -rf "$EXTRACT/$root"
  tar -xzf "$DIST/$archive" -C "$EXTRACT"
  [ "$(cat "$EXTRACT/$root/VERSION")" = "version=$VERSION
commit=$COMMIT" ] || { echo "bad VERSION: $archive" >&2; exit 1; }
  [ "$(stat -c '%a' "$EXTRACT/$root/VERSION")" = 644 ] || { echo "bad VERSION mode: $archive" >&2; exit 1; }
  for binary in $binaries; do
    path="$EXTRACT/$root/$binary"
    [ -x "$path" ] || { echo "not executable: $archive/$binary" >&2; exit 1; }
    [ "$(stat -c '%a' "$path")" = 755 ] || { echo "bad binary mode: $archive/$binary" >&2; exit 1; }
    metadata=$(go version -m "$path")
    printf '%s\n' "$metadata" | grep -F ': go1.24.4' >/dev/null
    printf '%s\n' "$metadata" | grep -F "GOOS=$os" >/dev/null
    printf '%s\n' "$metadata" | grep -F "GOARCH=$arch" >/dev/null
    case "$arch" in
      amd64) printf '%s\n' "$metadata" | grep -F 'GOAMD64=v1' >/dev/null ;;
      arm64) printf '%s\n' "$metadata" | grep -F 'GOARM64=v8.0' >/dev/null ;;
    esac
    if printf '%s\n' "$metadata" | grep -F 'GOFIPS140=' >/dev/null; then
      echo "ambient GOFIPS140 leaked into $archive/$binary" >&2
      exit 1
    fi
    printf '%s\n' "$metadata" | grep -F -- '-trimpath=true' >/dev/null
    grep -aF "$VERSION" "$path" >/dev/null
    grep -aF "$COMMIT" "$path" >/dev/null
  done
}

check_archive gate linux amd64
check_archive gate linux arm64
check_archive cli linux amd64
check_archive cli linux arm64
check_archive worker linux amd64
check_archive worker linux arm64
check_archive worker darwin amd64
check_archive worker darwin arm64

for pair in 'gate forge-gate' 'cli forge' 'worker forge-worker' 'worker forge-codex-plugin' 'worker forge-ref-plugin'; do
  role=${pair%% *}
  binary=${pair#* }
  archive="agent-forge-${role}_${VERSION}_linux_amd64.tar.gz"
  root=${archive%.tar.gz}
  output=$("$EXTRACT/$root/$binary" --version)
  [ "$output" = "$binary $VERSION $COMMIT" ] || { echo "bad version output: $output" >&2; exit 1; }
done

echo "release artifacts e2e: PASS"
