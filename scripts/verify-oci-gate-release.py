#!/usr/bin/env python3
"""Verify the exact digest and descriptor contract of a Gate OCI index."""

import hashlib
import json
import pathlib
import re
import sys


DIGEST_RE = re.compile(r"sha256:[0-9a-f]{64}\Z")
INDEX_MEDIA_TYPES = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}
MANIFEST_MEDIA_TYPES = {
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
}


class VerificationError(RuntimeError):
    pass


def verify_index(raw, expected_digest):
    actual_digest = "sha256:" + hashlib.sha256(raw).hexdigest()
    if not DIGEST_RE.fullmatch(expected_digest) or actual_digest != expected_digest:
        raise VerificationError(f"raw index digest mismatch: expected {expected_digest}, got {actual_digest}")
    try:
        index = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise VerificationError("index is not valid JSON") from error
    if not isinstance(index, dict) or index.get("schemaVersion") != 2 or index.get("mediaType") not in INDEX_MEDIA_TYPES:
        raise VerificationError("index header is malformed")
    manifests = index.get("manifests")
    if not isinstance(manifests, list):
        raise VerificationError("index manifests are malformed")

    runtime = {}
    attestation_references = []
    for descriptor in manifests:
        if not isinstance(descriptor, dict) or descriptor.get("mediaType") not in MANIFEST_MEDIA_TYPES:
            raise VerificationError("index contains a malformed manifest descriptor")
        digest = descriptor.get("digest")
        size = descriptor.get("size")
        platform = descriptor.get("platform")
        if (not isinstance(digest, str) or not DIGEST_RE.fullmatch(digest)
                or not isinstance(size, int) or isinstance(size, bool) or size <= 0
                or not isinstance(platform, dict)):
            raise VerificationError("index contains a malformed manifest descriptor")
        os_name, architecture = platform.get("os"), platform.get("architecture")
        if set(platform) != {"os", "architecture"}:
            raise VerificationError("index contains a non-exact platform descriptor")
        if (os_name, architecture) == ("unknown", "unknown"):
            annotations = descriptor.get("annotations")
            if not isinstance(annotations, dict) or annotations.get("vnd.docker.reference.type") != "attestation-manifest":
                raise VerificationError("unknown/unknown descriptor is not a BuildKit attestation manifest")
            reference_digest = annotations.get("vnd.docker.reference.digest")
            if not isinstance(reference_digest, str) or not DIGEST_RE.fullmatch(reference_digest):
                raise VerificationError("BuildKit attestation has no valid runtime reference digest")
            attestation_references.append(reference_digest)
            continue
        if os_name != "linux" or architecture not in {"amd64", "arm64"}:
            raise VerificationError(f"unexpected runtime platform: {os_name}/{architecture}")
        if architecture in runtime:
            raise VerificationError(f"duplicate runtime platform: linux/{architecture}")
        runtime[architecture] = digest
    if set(runtime) != {"amd64", "arm64"}:
        raise VerificationError("runtime platforms must be exactly linux/amd64 and linux/arm64")
    if sorted(attestation_references) != sorted(runtime.values()):
        raise VerificationError("BuildKit attestations must bind exactly once to each runtime manifest")
    return runtime


def main():
    if len(sys.argv) != 3:
        raise VerificationError("usage: verify-oci-gate-release.py <raw-index.json> <expected-digest>")
    runtime = verify_index(pathlib.Path(sys.argv[1]).read_bytes(), sys.argv[2])
    for architecture in ("amd64", "arm64"):
        print(f"{architecture}_digest={runtime[architecture]}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, VerificationError) as error:
        print(f"OCI Gate verification: {error}", file=sys.stderr)
        sys.exit(1)
