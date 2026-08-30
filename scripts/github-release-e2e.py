#!/usr/bin/env python3
import hashlib
import json
import os
import subprocess
import tempfile
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PUBLISHER = ROOT / "scripts" / "publish-github-release.py"
VERSION = "v0.1.0"
COMMIT = "1" * 40
ARCHIVES = [
    f"agent-forge-{role}_{VERSION}_linux_{arch}.tar.gz"
    for role in ("cli", "gate", "worker")
    for arch in ("amd64", "arm64")
]
SBOM = f"agent-forge_{VERSION}_linux.spdx.json"


class State:
    def __init__(self, existing=False, corrupt_digest=False, external_upload=False, move_tag=False, redirect_stage=None):
        self.existing = existing
        self.corrupt_digest = corrupt_digest
        self.release = None
        self.assets = []
        self.published = False
        self.port = 0
        self.external_upload = external_upload
        self.move_tag = move_tag
        self.ref_reads = 0
        self.redirect_stage = redirect_stage
        self.redirect_port = 0
        self.redirect_authorizations = []


def handler_for(state):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format: str, *args: object) -> None:
            pass

        def send_json(self, status, value):
            body = json.dumps(value).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def send_redirect(self, status=302):
            self.send_response(status)
            self.send_header("Location", f"http://127.0.0.1:{state.redirect_port}/sink")
            self.end_headers()

        def do_GET(self):
            path = urllib.parse.urlparse(self.path).path
            if path.endswith(f"/git/ref/tags/{VERSION}"):
                if state.redirect_stage == "api_get":
                    self.send_redirect()
                    return
                state.ref_reads += 1
                tag_sha = "3" * 40 if state.move_tag and state.ref_reads > 1 else "2" * 40
                self.send_json(200, {"object": {"type": "tag", "sha": tag_sha}})
            elif path.endswith("/git/tags/" + "2" * 40):
                self.send_json(200, {"object": {"type": "commit", "sha": COMMIT}})
            elif path.endswith("/git/tags/" + "3" * 40):
                self.send_json(200, {"object": {"type": "commit", "sha": "4" * 40}})
            elif path.endswith(f"/releases/tags/{VERSION}"):
                if state.existing or state.release is not None:
                    self.send_json(200, state.release or {"id": 9, "draft": False})
                else:
                    self.send_json(404, {"message": "Not Found"})
            elif path.endswith("/releases/1/assets"):
                self.send_json(200, state.assets)
            else:
                self.send_json(404, {"message": "Not Found"})

        def do_POST(self):
            parsed = urllib.parse.urlparse(self.path)
            body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            if parsed.path.endswith("/releases"):
                if state.redirect_stage == "release_post":
                    self.send_redirect()
                    return
                request = json.loads(body)
                if state.existing or state.release is not None:
                    self.send_json(422, {"message": "already_exists"})
                    return
                if not request.get("draft") or request.get("tag_name") != VERSION or request.get("target_commitish") != COMMIT:
                    self.send_json(400, {"message": "bad release request"})
                    return
                state.release = {
                    "id": 1,
                    "draft": True,
                    "tag_name": VERSION,
                    "target_commitish": COMMIT,
                    "upload_url": (
                        "https://example.invalid/uploads{?name,label}"
                        if state.external_upload
                        else f"http://127.0.0.1:{state.port}/uploads{{?name,label}}"
                    ),
                }
                self.send_json(201, state.release)
            elif parsed.path == "/uploads":
                if state.redirect_stage == "asset_upload":
                    self.send_redirect()
                    return
                name = urllib.parse.parse_qs(parsed.query).get("name", [""])[0]
                digest = hashlib.sha256(body).hexdigest()
                if state.corrupt_digest and not state.assets:
                    digest = "0" * 64
                asset = {"id": len(state.assets) + 1, "name": name, "size": len(body), "digest": "sha256:" + digest}
                state.assets.append(asset)
                self.send_json(201, asset)
            else:
                self.send_json(404, {"message": "Not Found"})

        def do_PATCH(self):
            path = urllib.parse.urlparse(self.path).path
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
            if state.redirect_stage == "publish_patch":
                self.send_redirect(307)
                return
            if path.endswith("/releases/1") and body == {"draft": False}:
                state.published = True
                state.release["draft"] = False
                self.send_json(200, state.release)
            else:
                self.send_json(400, {"message": "bad publish request"})

    return Handler


def make_assets(path):
    for name in ARCHIVES:
        (path / name).write_bytes((name + "\n").encode())
    checksums = []
    for name in ARCHIVES:
        digest = hashlib.sha256((path / name).read_bytes()).hexdigest()
        checksums.append(f"{digest}  {name}\n")
    (path / "SHA256SUMS").write_text("".join(checksums), encoding="ascii")
    (path / SBOM).write_text(
        json.dumps(
            {
                "spdxVersion": "SPDX-2.3",
                "name": "agent-forge-linux",
                "packages": [
                    {"name": "agent-forge"},
                    {"name": "stdlib"},
                    {"name": "github.com/coder/websocket"},
                    {"name": "modernc.org/sqlite"},
                ],
            }
        ),
        encoding="utf-8",
    )


class PublicationTests(unittest.TestCase):
    def run_case(self, state):
        class RedirectTarget(BaseHTTPRequestHandler):
            def log_message(self, format: str, *args: object) -> None:
                pass

            def capture(self):
                state.redirect_authorizations.append(self.headers.get("Authorization"))
                body = b"{}"
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            do_GET = capture
            do_POST = capture
            do_PATCH = capture

        redirect_server = ThreadingHTTPServer(("127.0.0.1", 0), RedirectTarget)
        state.redirect_port = redirect_server.server_port
        redirect_thread = threading.Thread(target=redirect_server.serve_forever, daemon=True)
        redirect_thread.start()
        server = ThreadingHTTPServer(("127.0.0.1", 0), handler_for(state))
        state.port = server.server_port
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                assets = Path(directory)
                make_assets(assets)
                env = os.environ.copy()
                env.update(
                    GITHUB_TOKEN="test-token",
                    GITHUB_REPOSITORY="0k-lab/agent-forge",
                    GITHUB_API_URL=f"http://127.0.0.1:{server.server_port}",
                    AGENT_FORGE_RELEASE_TEST_ALLOW_HTTP="1",
                )
                return subprocess.run(
                    ["python3", str(PUBLISHER), VERSION, COMMIT, str(assets)],
                    env=env,
                    text=True,
                    capture_output=True,
                    check=False,
                )
        finally:
            server.shutdown()
            server.server_close()
            thread.join()
            redirect_server.shutdown()
            redirect_server.server_close()
            redirect_thread.join()

    def test_uploads_exact_assets_then_publishes(self):
        state = State()
        result = self.run_case(state)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(state.published)
        self.assertEqual(sorted(asset["name"] for asset in state.assets), sorted(ARCHIVES + ["SHA256SUMS", SBOM]))

    def test_refuses_existing_release_without_uploading(self):
        state = State(existing=True)
        result = self.run_case(state)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(state.assets)
        self.assertFalse(state.published)

    def test_refuses_digest_mismatch_without_publishing(self):
        state = State(corrupt_digest=True)
        result = self.run_case(state)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(state.published)

    def test_refuses_external_upload_host(self):
        state = State(external_upload=True)
        result = self.run_case(state)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("upload URL host", result.stderr)
        self.assertFalse(state.assets)
        self.assertFalse(state.published)

    def test_refuses_tag_move_before_publication(self):
        state = State(move_tag=True)
        result = self.run_case(state)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(state.published)

    def assert_redirect_rejected_without_token(self, stage):
        state = State(redirect_stage=stage)
        result = self.run_case(state)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state.redirect_authorizations, [])
        self.assertFalse(state.published)

    def test_api_get_redirect_does_not_forward_token(self):
        self.assert_redirect_rejected_without_token("api_get")

    def test_release_post_redirect_does_not_forward_token(self):
        self.assert_redirect_rejected_without_token("release_post")

    def test_asset_upload_redirect_does_not_forward_token(self):
        self.assert_redirect_rejected_without_token("asset_upload")

    def test_publish_patch_redirect_does_not_forward_token(self):
        self.assert_redirect_rejected_without_token("publish_patch")


if __name__ == "__main__":
    unittest.main()
