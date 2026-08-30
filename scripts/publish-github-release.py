#!/usr/bin/env python3
"""Publish an exact Agent Forge Linux release without replacing remote state."""

from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

from release_assets import COMMIT_RE, VERSION_RE, ReleaseError, validate_assets


class RejectRedirects(urllib.request.HTTPRedirectHandler):
    """Fail closed instead of forwarding bearer credentials through redirects."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class GitHubAPI:
    def __init__(self, base_url: str, token: str) -> None:
        self.base_url = base_url.rstrip("/")
        parsed = urllib.parse.urlparse(self.base_url)
        allow_http = os.environ.get("AGENT_FORGE_RELEASE_TEST_ALLOW_HTTP") == "1"
        if parsed.scheme != "https" and not (allow_http and parsed.scheme == "http" and parsed.hostname in {"127.0.0.1", "localhost"}):
            raise ReleaseError("GitHub API URL must use HTTPS")
        if not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise ReleaseError("GitHub API URL is malformed")
        self.api_host = parsed.hostname
        self.api_port = parsed.port
        self.headers = {
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {token}",
            "User-Agent": "agent-forge-release-publisher",
            "X-GitHub-Api-Version": "2022-11-28",
        }
        self.opener = urllib.request.build_opener(RejectRedirects())

    def request_json(self, method: str, url: str, value: Any | None = None) -> Any:
        data = None if value is None else json.dumps(value, separators=(",", ":")).encode("utf-8")
        headers = dict(self.headers)
        if data is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=60) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            detail = error.read(4096).decode("utf-8", "replace")
            raise ReleaseError(f"GitHub API {method} failed with HTTP {error.code}: {detail}") from error
        except urllib.error.URLError as error:
            raise ReleaseError(f"GitHub API {method} request failed: {error.reason}") from error

    def get_optional(self, url: str) -> Any | None:
        request = urllib.request.Request(url, headers=self.headers, method="GET")
        try:
            with self.opener.open(request, timeout=60) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return None
            detail = error.read(4096).decode("utf-8", "replace")
            raise ReleaseError(f"GitHub API GET failed with HTTP {error.code}: {detail}") from error
        except urllib.error.URLError as error:
            raise ReleaseError(f"GitHub API GET request failed: {error.reason}") from error

    def upload(self, upload_url: str, name: str, path: Path) -> Any:
        base = upload_url.split("{", 1)[0]
        parsed = urllib.parse.urlparse(base)
        allow_http = os.environ.get("AGENT_FORGE_RELEASE_TEST_ALLOW_HTTP") == "1"
        if parsed.scheme != "https" and not (allow_http and parsed.scheme == "http" and parsed.hostname in {"127.0.0.1", "localhost"}):
            raise ReleaseError("GitHub upload URL must use HTTPS")
        allowed_host = parsed.hostname == self.api_host and parsed.port == self.api_port
        if self.api_host == "api.github.com":
            allowed_host = parsed.hostname == "uploads.github.com" and parsed.port in {None, 443}
        if not allowed_host or parsed.username or parsed.password or parsed.fragment:
            raise ReleaseError("GitHub upload URL host does not match the API boundary")
        separator = "&" if parsed.query else "?"
        url = base + separator + urllib.parse.urlencode({"name": name})
        headers = dict(self.headers)
        headers["Content-Type"] = "application/octet-stream"
        request = urllib.request.Request(url, data=path.read_bytes(), headers=headers, method="POST")
        try:
            with self.opener.open(request, timeout=300) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            detail = error.read(4096).decode("utf-8", "replace")
            raise ReleaseError(f"GitHub asset upload failed with HTTP {error.code}: {detail}") from error
        except urllib.error.URLError as error:
            raise ReleaseError(f"GitHub asset upload failed: {error.reason}") from error


def resolve_tag_commit(api: GitHubAPI, repository: str, version: str) -> str:
    encoded = urllib.parse.quote(version, safe="")
    ref = api.request_json("GET", f"{api.base_url}/repos/{repository}/git/ref/tags/{encoded}")
    obj = ref.get("object") if isinstance(ref, dict) else None
    if not isinstance(obj, dict) or obj.get("type") != "tag" or not COMMIT_RE.fullmatch(str(obj.get("sha", ""))):
        raise ReleaseError("release ref is not an annotated tag")
    for _ in range(8):
        tag = api.request_json("GET", f"{api.base_url}/repos/{repository}/git/tags/{obj['sha']}")
        obj = tag.get("object") if isinstance(tag, dict) else None
        if not isinstance(obj, dict) or not COMMIT_RE.fullmatch(str(obj.get("sha", ""))):
            raise ReleaseError("annotated release tag has an invalid target")
        if obj.get("type") == "commit":
            return str(obj["sha"])
        if obj.get("type") != "tag":
            raise ReleaseError("annotated release tag does not target a commit")
    raise ReleaseError("annotated release tag chain is too deep")


def publish(version: str, commit: str, directory: Path) -> None:
    token = os.environ.get("GITHUB_TOKEN", "")
    repository = os.environ.get("GITHUB_REPOSITORY", "")
    api_url = os.environ.get("GITHUB_API_URL", "https://api.github.com")
    if not token or "\n" in token or "\r" in token:
        raise ReleaseError("GITHUB_TOKEN is missing or malformed")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ReleaseError("GITHUB_REPOSITORY is missing or malformed")
    if not VERSION_RE.fullmatch(version) or not COMMIT_RE.fullmatch(commit):
        raise ReleaseError("release version or commit is malformed")
    digests = validate_assets(version, directory)
    api = GitHubAPI(api_url, token)
    if resolve_tag_commit(api, repository, version) != commit:
        raise ReleaseError("remote annotated tag does not point to the release commit")

    encoded = urllib.parse.quote(version, safe="")
    release_by_tag = f"{api.base_url}/repos/{repository}/releases/tags/{encoded}"
    if api.get_optional(release_by_tag) is not None:
        raise ReleaseError("refusing to replace an existing GitHub Release")

    release = api.request_json(
        "POST",
        f"{api.base_url}/repos/{repository}/releases",
        {
            "tag_name": version,
            "target_commitish": commit,
            "name": f"Agent Forge {version}",
            "body": f"Agent Forge {version}. Verify Linux assets with SHA256SUMS and GitHub attestations.",
            "draft": True,
            "prerelease": False,
            "generate_release_notes": False,
        },
    )
    if not isinstance(release, dict) or release.get("draft") is not True or not isinstance(release.get("id"), int) or not isinstance(release.get("upload_url"), str):
        raise ReleaseError("GitHub returned an invalid draft release")
    release_id = release["id"]

    for name in sorted(digests):
        result = api.upload(release["upload_url"], name, directory / name)
        if not isinstance(result, dict) or result.get("name") != name or result.get("size") != (directory / name).stat().st_size:
            raise ReleaseError(f"GitHub returned invalid upload metadata: {name}")
        if result.get("digest") != f"sha256:{digests[name]}":
            raise ReleaseError(f"GitHub returned a mismatched upload digest: {name}")

    assets = api.request_json("GET", f"{api.base_url}/repos/{repository}/releases/{release_id}/assets?per_page=100")
    if not isinstance(assets, list) or len(assets) != len(digests):
        raise ReleaseError("draft release has an unexpected remote asset count")
    remote: dict[str, Any] = {}
    for asset in assets:
        if not isinstance(asset, dict) or not isinstance(asset.get("name"), str) or asset["name"] in remote:
            raise ReleaseError("draft release has malformed or duplicate remote assets")
        remote[asset["name"]] = asset
    if set(remote) != set(digests):
        raise ReleaseError("draft release has an unexpected remote asset set")
    for name, digest in digests.items():
        asset = remote[name]
        if asset.get("size") != (directory / name).stat().st_size or asset.get("digest") != f"sha256:{digest}":
            raise ReleaseError(f"draft release failed remote verification: {name}")

    if resolve_tag_commit(api, repository, version) != commit:
        raise ReleaseError("remote annotated tag moved before publication")
    published = api.request_json("PATCH", f"{api.base_url}/repos/{repository}/releases/{release_id}", {"draft": False})
    if not isinstance(published, dict) or published.get("draft") is not False or published.get("id") != release_id:
        raise ReleaseError("GitHub did not confirm release publication")
    print(f"published GitHub Release: {version} {commit}")


def main() -> int:
    if len(sys.argv) != 4:
        print(f"usage: {sys.argv[0]} <vMAJOR.MINOR.PATCH> <40-lowercase-hex-commit> <asset-directory>", file=sys.stderr)
        return 2
    try:
        publish(sys.argv[1], sys.argv[2], Path(sys.argv[3]))
    except (OSError, ReleaseError) as error:
        print(f"release publication failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
