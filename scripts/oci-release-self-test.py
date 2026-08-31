#!/usr/bin/env python3
import importlib.util
import hashlib
import json
import pathlib
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


SCRIPTS = pathlib.Path(__file__).parent


def load(name):
    path = SCRIPTS / f"{name.replace('_', '-')}.py"
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


ghcr = load("ghcr_tag_state")
package = load("ghcr_package_public")
verify = load("verify_oci_gate_release")


class Registry:
    def __init__(self, responses):
        self.responses = list(responses)
        self.requests = []

        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                owner.requests.append((self.path, self.headers))
                status, headers, body = owner.responses.pop(0)
                self.send_response(status)
                for key, value in headers.items():
                    self.send_header(key, value.replace("{base}", owner.base))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.base = f"http://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_args):
        self.server.shutdown()
        self.thread.join()
        self.server.server_close()


DIGEST = "sha256:" + "a" * 64


def authenticated_manifest(response):
    challenge = 'Bearer realm="{base}/token",service="ghcr.io"'
    return [
        (401, {"WWW-Authenticate": challenge}, b""),
        (200, {}, json.dumps({"token": "bearer-secret"}).encode()),
        response,
    ]


def descriptor(architecture, digest_char, annotations=None):
    result = {
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "digest": "sha256:" + digest_char * 64,
        "size": 123,
        "platform": {"os": "linux" if architecture != "unknown" else "unknown", "architecture": architecture},
    }
    if annotations:
        result["annotations"] = annotations
    return result


def raw_index(manifests=None):
    amd64_digest = "sha256:" + "1" * 64
    arm64_digest = "sha256:" + "2" * 64
    value = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": manifests or [
            descriptor("amd64", "1"),
            descriptor("arm64", "2"),
            descriptor("unknown", "3", {
                "vnd.docker.reference.type": "attestation-manifest",
                "vnd.docker.reference.digest": amd64_digest,
            }),
            descriptor("unknown", "4", {
                "vnd.docker.reference.type": "attestation-manifest",
                "vnd.docker.reference.digest": arm64_digest,
            }),
        ],
    }
    return json.dumps(value, separators=(",", ":")).encode()


class TagStateTests(unittest.TestCase):
    def probe(self, responses):
        with Registry(responses) as registry:
            result = ghcr.probe(
                registry.base, "0k-lab/agent-forge-gate", "v1.2.3",
                "actor", "secret", allow_http=True,
            )
            return result, registry.requests

    def test_manifest_404_is_absent(self):
        result, _ = self.probe(authenticated_manifest((404, {}, b"")))
        self.assertEqual(result, ("absent", ""))

    def test_anonymous_404_is_not_absent(self):
        with self.assertRaises(ghcr.RegistryError):
            self.probe([(404, {}, b"")])

    def test_manifest_200_requires_and_returns_digest(self):
        result, _ = self.probe(authenticated_manifest((200, {"Docker-Content-Digest": DIGEST}, b"{}")))
        self.assertEqual(result, ("present", DIGEST))

    def test_bearer_challenge_uses_basic_only_for_token(self):
        responses = authenticated_manifest((200, {"Docker-Content-Digest": DIGEST}, b"{}"))
        result, requests = self.probe(responses)
        self.assertEqual(result, ("present", DIGEST))
        self.assertNotIn("Authorization", requests[0][1])
        self.assertTrue(requests[1][1]["Authorization"].startswith("Basic "))
        self.assertEqual(requests[2][1]["Authorization"], "Bearer bearer-secret")
        self.assertIn("scope=repository%3A0k-lab%2Fagent-forge-gate%3Apull", requests[1][0])

    def test_manifest_failures_are_not_absent(self):
        for status in (403, 429, 500, 502, 503):
            with self.subTest(status=status):
                with self.assertRaises(ghcr.RegistryError):
                    self.probe(authenticated_manifest((status, {}, b"failure")))

    def test_manifest_redirect_to_404_is_not_absent(self):
        with self.assertRaises(ghcr.RegistryError):
            self.probe(authenticated_manifest((302, {"Location": "{base}/elsewhere"}, b"")))

    def test_bad_auth_and_digest_responses_fail(self):
        cases = [
            [(401, {}, b"")],
            [(401, {"WWW-Authenticate": "Basic realm=x"}, b"")],
            authenticated_manifest((200, {}, b"{}")),
            authenticated_manifest((200, {"Docker-Content-Digest": "sha256:bad"}, b"{}")),
        ]
        for responses in cases:
            with self.subTest(responses=responses):
                with self.assertRaises(ghcr.RegistryError):
                    self.probe(responses)

    def test_token_endpoint_failure_is_closed(self):
        challenge = 'Bearer realm="{base}/token",service="ghcr.io",scope="repository:x:pull"'
        with self.assertRaises(ghcr.RegistryError):
            self.probe([
                (401, {"WWW-Authenticate": challenge}, b""),
                (500, {}, b"failure"),
            ])

    def test_token_realm_must_match_registry_origin(self):
        self.assertEqual(
            ghcr.validate_token_realm("https://ghcr.io", "https://ghcr.io/token", False),
            "https://ghcr.io/token",
        )
        for realm in (
            "https://example.invalid/token",
            "https://user@ghcr.io/token",
            "https://ghcr.io/token#fragment",
        ):
            with self.subTest(realm=realm), self.assertRaises(ghcr.RegistryError):
                ghcr.validate_token_realm("https://ghcr.io", realm, False)


class IndexTests(unittest.TestCase):
    def test_valid_index_returns_exact_runtime_digests(self):
        raw = raw_index()
        expected = "sha256:" + hashlib.sha256(raw).hexdigest()
        self.assertEqual(verify.verify_index(raw, expected), {
            "amd64": "sha256:" + "1" * 64,
            "arm64": "sha256:" + "2" * 64,
        })

    def test_raw_digest_must_match(self):
        with self.assertRaises(verify.VerificationError):
            verify.verify_index(raw_index(), DIGEST)

    def test_runtime_platforms_are_exact_and_unique(self):
        variant = descriptor("arm64", "2")
        variant["platform"]["variant"] = "v8"
        bad_manifests = [
            [descriptor("amd64", "1"), descriptor("s390x", "2")],
            [descriptor("amd64", "1"), descriptor("amd64", "2"), descriptor("arm64", "3")],
            [descriptor("amd64", "1")],
            [descriptor("amd64", "1"), variant],
        ]
        for manifests in bad_manifests:
            with self.subTest(manifests=manifests):
                raw = raw_index(manifests)
                digest = "sha256:" + hashlib.sha256(raw).hexdigest()
                with self.assertRaises(verify.VerificationError):
                    verify.verify_index(raw, digest)

    def test_only_marked_buildkit_attestations_are_allowed(self):
        cases = [
            [descriptor("amd64", "1"), descriptor("arm64", "2")],
            [descriptor("amd64", "1"), descriptor("arm64", "2"), descriptor("unknown", "3")],
            [descriptor("amd64", "1"), descriptor("arm64", "2"),
             descriptor("unknown", "3", {"vnd.docker.reference.type": "other"})],
            [descriptor("amd64", "1"), descriptor("arm64", "2"),
             descriptor("unknown", "3", {
                 "vnd.docker.reference.type": "attestation-manifest",
                 "vnd.docker.reference.digest": "sha256:" + "9" * 64,
             })],
        ]
        for manifests in cases:
            raw = raw_index(manifests)
            digest = "sha256:" + hashlib.sha256(raw).hexdigest()
            with self.assertRaises(verify.VerificationError):
                verify.verify_index(raw, digest)

    def test_malformed_index_and_descriptors_fail(self):
        cases = [b"not json", b"[]", raw_index([{}]), raw_index([descriptor("amd64", "x")])]
        for raw in cases:
            digest = "sha256:" + hashlib.sha256(raw).hexdigest()
            with self.subTest(raw=raw):
                with self.assertRaises(verify.VerificationError):
                    verify.verify_index(raw, digest)


class PackageVisibilityTests(unittest.TestCase):
    def check(self, response):
        with Registry([response]) as server:
            package.require_public(server.base, "0k-lab", "agent-forge-gate", "secret")
            return server.requests

    def test_exact_public_package_is_accepted(self):
        body = json.dumps({
            "name": "agent-forge-gate",
            "package_type": "container",
            "visibility": "public",
            "repository": {"full_name": "0k-lab/agent-forge"},
        }).encode()
        requests = self.check((200, {}, body))
        self.assertEqual(requests[0][0], "/orgs/0k-lab/packages/container/agent-forge-gate")
        self.assertEqual(requests[0][1]["Authorization"], "Bearer secret")

    def test_absent_or_nonpublic_package_fails_closed(self):
        for response in [
            (404, {}, b'{"message":"Not Found"}'),
            (200, {}, b'{"visibility":"private"}'),
            (200, {}, b'{}'),
            (200, {}, b'{"name":"other","package_type":"container","visibility":"public","repository":{"full_name":"0k-lab/agent-forge"}}'),
            (200, {}, b'{"name":"agent-forge-gate","package_type":"npm","visibility":"public","repository":{"full_name":"0k-lab/agent-forge"}}'),
            (200, {}, b'{"name":"agent-forge-gate","package_type":"container","visibility":"public","repository":{"full_name":"0k-lab/other"}}'),
            (403, {}, b'forbidden'),
            (500, {}, b'failure'),
        ]:
            with self.subTest(response=response), self.assertRaises(package.PackageError):
                self.check(response)

if __name__ == "__main__":
    unittest.main(verbosity=2)
