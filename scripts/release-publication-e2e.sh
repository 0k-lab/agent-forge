#!/bin/sh
set -eu
export LC_ALL=C
unset POSIXLY_CORRECT

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
PREPARE="$ROOT/scripts/prepare-linux-release.sh"
VALIDATE_TAG="$ROOT/scripts/validate-release-tag.sh"
VERSION=v0.1.0
TMP=$(mktemp -d)
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

grep -Fq 'ref: ${{ github.ref }}' "$ROOT/.github/workflows/release.yml" || {
  echo "release checkout must preserve the triggering annotated tag ref" >&2
  exit 1
}

INPUT="$TMP/input"
OUTPUT="$TMP/linux"
mkdir -p "$INPUT"
for role in cli gate worker; do
  for arch in amd64 arm64; do
    printf '%s\n' "$role-linux-$arch" >"$INPUT/agent-forge-${role}_${VERSION}_linux_${arch}.tar.gz"
  done
done
for arch in amd64 arm64; do
  printf '%s\n' "worker-darwin-$arch" >"$INPUT/agent-forge-worker_${VERSION}_darwin_${arch}.tar.gz"
done
(
  cd "$INPUT"
  sha256sum ./*.tar.gz | sed 's#  \./#  #' | sort -k2 >SHA256SUMS
)

"$PREPARE" "$VERSION" "$INPUT" "$OUTPUT"
EXPECTED='agent-forge-cli_v0.1.0_linux_amd64.tar.gz
agent-forge-cli_v0.1.0_linux_arm64.tar.gz
agent-forge-gate_v0.1.0_linux_amd64.tar.gz
agent-forge-gate_v0.1.0_linux_arm64.tar.gz
agent-forge-worker_v0.1.0_linux_amd64.tar.gz
agent-forge-worker_v0.1.0_linux_arm64.tar.gz'
ACTUAL=$(sed 's/^.*  //' "$OUTPUT/SHA256SUMS")
[ "$ACTUAL" = "$EXPECTED" ] || { echo "unexpected Linux release matrix" >&2; exit 1; }
[ "$(find "$OUTPUT" -maxdepth 1 -type f | wc -l)" -eq 7 ]
(cd "$OUTPUT" && sha256sum -c SHA256SUMS)

printf 'keep\n' >"$OUTPUT/operator-data"
if "$PREPARE" "$VERSION" "$INPUT" "$OUTPUT" >/dev/null 2>&1; then
  echo "prepare replaced existing output" >&2
  exit 1
fi
[ "$(cat "$OUTPUT/operator-data")" = keep ]

if "$PREPARE" v0.1.0-alpha "$INPUT" "$TMP/prerelease" >/dev/null 2>&1; then
  echo "prepare accepted a prerelease version" >&2
  exit 1
fi
cp -a "$INPUT" "$TMP/missing"
rm "$TMP/missing/agent-forge-worker_${VERSION}_darwin_arm64.tar.gz"
if "$PREPARE" "$VERSION" "$TMP/missing" "$TMP/missing-out" >/dev/null 2>&1; then
  echo "prepare accepted a missing matrix artifact" >&2
  exit 1
fi
cp -a "$INPUT" "$TMP/extra"
printf 'extra\n' >"$TMP/extra/unexpected"
if "$PREPARE" "$VERSION" "$TMP/extra" "$TMP/extra-out" >/dev/null 2>&1; then
  echo "prepare accepted an unexpected input file" >&2
  exit 1
fi

TAG_REPO="$TMP/tag-repo"
mkdir -p "$TAG_REPO/scripts"
cp "$VALIDATE_TAG" "$TAG_REPO/scripts/validate-release-tag.sh"
printf 'source\n' >"$TAG_REPO/source"
git -C "$TAG_REPO" init -q
git -C "$TAG_REPO" config user.name 'Release Test'
git -C "$TAG_REPO" config user.email 'release@example.invalid'
git -C "$TAG_REPO" add -A
git -C "$TAG_REPO" commit -qm 'test source'
COMMIT=$(git -C "$TAG_REPO" rev-parse HEAD)
git -C "$TAG_REPO" update-ref refs/remotes/origin/main "$COMMIT"
git -C "$TAG_REPO" tag -a "$VERSION" -m "$VERSION"
"$TAG_REPO/scripts/validate-release-tag.sh" "$VERSION" "$COMMIT"
git -C "$TAG_REPO" tag v0.2.0
if "$TAG_REPO/scripts/validate-release-tag.sh" v0.2.0 "$COMMIT" >/dev/null 2>&1; then
  echo "validator accepted a lightweight tag" >&2
  exit 1
fi
case "$COMMIT" in
  0*) MISMATCH="1${COMMIT#?}" ;;
  *) MISMATCH="0${COMMIT#?}" ;;
esac
if "$TAG_REPO/scripts/validate-release-tag.sh" "$VERSION" "$MISMATCH" >/dev/null 2>&1; then
  echo "validator accepted a mismatched commit" >&2
  exit 1
fi
printf 'dirty\n' >"$TAG_REPO/dirty"
if "$TAG_REPO/scripts/validate-release-tag.sh" "$VERSION" "$COMMIT" >/dev/null 2>&1; then
  echo "validator accepted a dirty checkout" >&2
  exit 1
fi
rm "$TAG_REPO/dirty"
printf 'side\n' >"$TAG_REPO/side"
git -C "$TAG_REPO" add side
git -C "$TAG_REPO" commit -qm 'unmerged side commit'
UNMERGED=$(git -C "$TAG_REPO" rev-parse HEAD)
git -C "$TAG_REPO" tag -a v0.3.0 -m v0.3.0
if "$TAG_REPO/scripts/validate-release-tag.sh" v0.3.0 "$UNMERGED" >/dev/null 2>&1; then
  echo "validator accepted a release commit outside origin/main" >&2
  exit 1
fi

python3 "$ROOT/scripts/github-release-e2e.py"
echo "release publication e2e: PASS"
