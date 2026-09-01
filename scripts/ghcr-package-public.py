#!/usr/bin/env python3
"""Require the existing Agent Forge Gate GHCR package to be public."""

import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


class PackageError(RuntimeError):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


OPENER = urllib.request.build_opener(NoRedirect)


def require_public(api_base, organization, package, token):
    url = (f"{api_base.rstrip('/')}/orgs/{urllib.parse.quote(organization, safe='')}"
           f"/packages/container/{urllib.parse.quote(package, safe='')}")
    request = urllib.request.Request(url, headers={
        "Accept": "application/vnd.github+json",
        "Authorization": f"Bearer {token}",
        "X-GitHub-Api-Version": "2022-11-28",
    })
    try:
        with OPENER.open(request, timeout=30) as response:
            status, body = response.status, response.read()
    except urllib.error.HTTPError as error:
        status, body = error.code, error.read()
    except (OSError, urllib.error.URLError) as error:
        raise PackageError(f"GitHub package metadata request failed: {error}") from error
    if status == 404:
        raise PackageError("GHCR package 0k-lab/agent-forge-gate does not exist; complete the README one-time public-package bootstrap before creating the stable tag")
    if status != 200:
        raise PackageError(f"GitHub package metadata request failed with HTTP {status}")
    try:
        metadata = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise PackageError("GitHub package metadata response is malformed") from error
    repository = metadata.get("repository") if isinstance(metadata, dict) else None
    if (not isinstance(metadata, dict)
            or metadata.get("name") != package
            or metadata.get("package_type") != "container"
            or metadata.get("visibility") != "public"
            or not isinstance(repository, dict)
            or repository.get("full_name") != f"{organization}/agent-forge"):
        raise PackageError("GHCR package 0k-lab/agent-forge-gate is not public; complete the README one-time public-package bootstrap before creating the stable tag")


def main():
    if len(sys.argv) != 1:
        raise PackageError("usage: ghcr-package-public.py")
    token = os.environ.get("GITHUB_TOKEN", "")
    if not token:
        raise PackageError("GITHUB_TOKEN is required")
    require_public("https://api.github.com", "0k-lab", "agent-forge-gate", token)


if __name__ == "__main__":
    try:
        main()
    except PackageError as error:
        print(f"GHCR package preflight: {error}", file=sys.stderr)
        sys.exit(1)
