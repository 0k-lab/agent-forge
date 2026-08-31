#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DOCKERFILE=$ROOT/Dockerfile.gate
IGNORE=$ROOT/.dockerignore
CI=$ROOT/.github/workflows/ci.yml
RELEASE=$ROOT/.github/workflows/release.yml
README=$ROOT/README.md

fail() { echo "oci gate contract: $*" >&2; exit 1; }
has() { grep -Fq -- "$2" "$1" || fail "$1 lacks: $2"; }
lacks() { ! grep -Eqi -- "$2" "$1" || fail "$1 contains forbidden pattern: $2"; }

[[ -f $DOCKERFILE ]] || fail "Dockerfile.gate is missing"
[[ -f $IGNORE ]] || fail ".dockerignore is missing"
for helper in ghcr-tag-state.py ghcr-package-public.py verify-oci-gate-release.py oci-release-self-test.py; do
  [[ -f $ROOT/scripts/$helper ]] || fail "$helper is missing"
done

mapfile -t froms < <(awk 'toupper($1) == "FROM" {print $2}' "$DOCKERFILE")
[[ ${#froms[@]} -eq 2 ]] || fail "Dockerfile.gate must have exactly two stages"
[[ ${froms[0]} == 'golang:1.24.4-bookworm@sha256:10f549dc8489597aa7ed2b62008199bb96717f52a8e8434ea035d5b44368f8a6' ]] || fail "builder base is not exact"
[[ ${froms[1]} == 'debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171' ]] || fail "runtime base is not exact"
[[ $(awk 'toupper($1) == "USER" {user=$2} END {print user}' "$DOCKERFILE") == 65532:65532 ]] || fail "runtime user is not 65532:65532"

for text in \
  'CGO_ENABLED=0' 'GOFLAGS=-mod=readonly' 'GOENV=off' 'GOWORK=off' 'GOEXPERIMENT=' \
  'GOFIPS140=off' 'GOTOOLCHAIN=go1.24.4' '-trimpath' '-buildvcs=false' '-buildid=' \
  'internal/buildinfo.Version=${VERSION}' 'internal/buildinfo.Commit=${COMMIT}' \
  'rm -rf /var/cache/apt/*' \
  'USER 65532:65532' 'ENTRYPOINT ["/usr/local/bin/forge-gate"]' \
  'CMD ["-config", "/etc/agent-forge/gate.json"]' 'EXPOSE 18080'; do
  has "$DOCKERFILE" "$text"
done
has "$ROOT/scripts/oci-gate-e2e.sh" 'timeout --signal=TERM 10m'
has "$ROOT/scripts/oci-gate-e2e.sh" '--entrypoint /usr/bin/git'
has "$ROOT/scripts/oci-gate-e2e.sh" 'test -s /etc/ssl/certs/ca-certificates.crt'
has "$ROOT/scripts/oci-gate-e2e.sh" 'dst=/var/lib/agent-forge/repositories'
has "$README" 'dst=/var/lib/agent-forge/repositories'
has "$DOCKERFILE" "^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"
has "$DOCKERFILE" "^[0-9a-f]{40}$"
has "$DOCKERFILE" 'org.opencontainers.image.source="https://github.com/0k-lab/agent-forge"'
has "$DOCKERFILE" 'org.opencontainers.image.version="${VERSION}"'
has "$DOCKERFILE" 'org.opencontainers.image.revision="${COMMIT}"'
lacks "$DOCKERFILE" 'forge-worker|forge-codex-plugin|forge-ref-plugin|cmd/forge([^/-]|$)|docker[.]io|healthcheck'

for text in '.git' 'dist' 'release' '.env' '*secret*' '*.db' 'gate.json' 'state' 'worktrees' 'evidence'; do has "$IGNORE" "$text"; done

has "$CI" 'oci-gate:'
has "$CI" 'scripts/oci-gate-contract.sh'
has "$CI" 'scripts/oci-gate-e2e.sh v999.0.0 "$GITHUB_SHA"'
has "$CI" 'python3 scripts/oci-release-self-test.py'
has "$RELEASE" 'publish-oci-gate:'
has "$RELEASE" 'prepare-linux:'
has "$RELEASE" 'needs: prepare-linux'
has "$RELEASE" 'needs: [prepare-linux, publish-oci-gate]'
prepare_line=$(grep -n '^  prepare-linux:' "$RELEASE" | cut -d: -f1)
oci_line=$(grep -n '^  publish-oci-gate:' "$RELEASE" | cut -d: -f1)
linux_line=$(grep -n '^  publish-linux:' "$RELEASE" | cut -d: -f1)
[[ $prepare_line -lt $oci_line && $oci_line -lt $linux_line ]] || fail "release jobs are not ordered prepare-linux -> publish-oci-gate -> publish-linux"
prepare_body=$(sed -n "${prepare_line},$((oci_line - 1))p" "$RELEASE")
oci_body=$(sed -n "${oci_line},$((linux_line - 1))p" "$RELEASE")
linux_body=$(sed -n "${linux_line},\$p" "$RELEASE")
grep -Fq 'scripts/build-release.sh' <<<"$prepare_body" || fail "prepare-linux does not build release assets"
grep -Fq 'linux-installer-privileged-e2e.sh' <<<"$prepare_body" || fail "prepare-linux lacks privileged acceptance"
grep -Fq 'actions/upload-artifact@' <<<"$prepare_body" || fail "prepare-linux does not upload prepared assets"
! grep -Fq 'publish-github-release.py' <<<"$prepare_body" || fail "prepare-linux publishes a GitHub Release"
grep -Fq 'actions/download-artifact@' <<<"$linux_body" || fail "publish-linux does not download prepared assets"
grep -Fq 'publish-github-release.py' <<<"$linux_body" || fail "publish-linux does not publish the GitHub Release"
grep -Fq 'OCI tag already exists; stable tags are never resumed' <<<"$oci_body" || fail "existing OCI tags are not a hard next-patch failure"
for text in \
  'python3 scripts/ghcr-package-public.py' \
  'python3 scripts/ghcr-tag-state.py 0k-lab/agent-forge-gate' \
  'docker logout ghcr.io' \
  'docker buildx imagetools inspect --raw "$IMAGE:$VERSION"' \
  'python3 scripts/verify-oci-gate-release.py' \
  'for architecture in amd64 arm64' \
  'reference="$IMAGE@$digest"' \
  'docker pull --platform "linux/$architecture" "$reference"'; do
  grep -Fq "$text" <<<"$oci_body" || fail "publish-oci-gate lacks: $text"
done
[[ $(grep -Fc 'docker buildx imagetools inspect --raw "$IMAGE:$VERSION"' <<<"$oci_body") -eq 2 ]] || fail "exact OCI tag must be revalidated after both runtime checks"
package_line=$(grep -n 'python3 scripts/ghcr-package-public.py' <<<"$oci_body" | cut -d: -f1)
probe_line=$(grep -n 'python3 scripts/ghcr-tag-state.py' <<<"$oci_body" | cut -d: -f1)
push_line=$(grep -n 'docker/build-push-action@' <<<"$oci_body" | cut -d: -f1)
[[ $package_line -lt $push_line && $probe_line -lt $push_line ]] || fail "public-package and exact-tag preflights must precede OCI build/push"
has "$RELEASE" 'packages: write'
has "$RELEASE" 'id-token: write'
has "$RELEASE" 'attestations: write'
for action in \
  'docker/setup-qemu-action@c7c53464625b32c7a7e944ae62b3e17d2b600130' \
  'docker/setup-buildx-action@8d2750c68a42422c14e847fe6c8ac0403b4cbd6f' \
  'docker/login-action@c94ce9fb468520275223c153574b00df6fe4bcc9' \
  'docker/build-push-action@10e90e3645eae34f1e60eeb005ba3a3d33f178e8' \
  'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02' \
  'actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093' \
  'actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8'; do has "$RELEASE" "$action"; done
for text in \
  'platforms: linux/amd64,linux/arm64' 'pull: true' 'no-cache: true' 'sbom: true' 'provenance: mode=max' \
  'ghcr.io/0k-lab/agent-forge-gate:${{ steps.identity.outputs.version }}' \
  'subject-name: ghcr.io/0k-lab/agent-forge-gate' 'push-to-registry: true'; do has "$RELEASE" "$text"; done
mapfile -t image_tags < <(awk '/^[[:space:]]+tags:[[:space:]]+/{sub(/^[[:space:]]+tags:[[:space:]]+/, ""); print}' "$RELEASE")
[[ ${#image_tags[@]} -eq 1 && ${image_tags[0]} == 'ghcr.io/0k-lab/agent-forge-gate:${{ steps.identity.outputs.version }}' ]] || fail "release workflow has a non-exact publication tag"
lacks "$RELEASE" 'agent-forge-gate:(latest|v?[0-9]+([.]v?[0-9]+)?)([[:space:]]|$)'
has "$README" 'One-time GHCR bootstrap before the first stable OCI release'
has "$README" 'make the package public in GitHub Package settings'
has "$README" 'verify an anonymous pull'
has "$README" 'only then create the stable git tag'
has "$README" 'registry-enforced immutability'
has "$README" 'docker login ghcr.io'
has "$README" 'bootstrap-$COMMIT'
has "$README" 'docker buildx build'
has "$README" '--push'
has "$ROOT/scripts/verify-oci-gate-release.py" 'vnd.docker.reference.digest'

echo "oci gate contract: PASS"
