#!/usr/bin/env python3
"""Fail-closed GHCR manifest existence probe."""

import base64
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request


ACCEPT = ", ".join((
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.docker.distribution.manifest.v2+json",
))
DIGEST_RE = re.compile(r"sha256:[0-9a-f]{64}\Z")


class RegistryError(RuntimeError):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


OPENER = urllib.request.build_opener(NoRedirect)


def request(url, headers):
    try:
        with OPENER.open(urllib.request.Request(url, headers=headers), timeout=30) as response:
            return response.status, response.headers, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.headers, error.read()
    except (OSError, urllib.error.URLError) as error:
        raise RegistryError(f"registry request failed: {error}") from error


def bearer_parameters(challenge):
    if not challenge or not challenge[:7].lower() == "bearer ":
        raise RegistryError("registry did not return a Bearer authentication challenge")
    parameters = {}
    for item in urllib.request.parse_http_list(challenge[7:]):
        key, separator, value = item.partition("=")
        if not separator:
            raise RegistryError("registry returned a malformed Bearer challenge")
        parameters[key.strip().lower()] = value.strip().strip('"')
    if not parameters.get("realm"):
        raise RegistryError("registry Bearer challenge has no token realm")
    return parameters


def validate_token_realm(registry, realm, allow_http=False):
    registry_url = urllib.parse.urlsplit(registry)
    realm_url = urllib.parse.urlsplit(realm)
    if not allow_http and realm_url.scheme != "https":
        raise RegistryError("registry token realm must use HTTPS")
    if (realm_url.scheme, realm_url.hostname, realm_url.port) != (
            registry_url.scheme, registry_url.hostname, registry_url.port):
        raise RegistryError("registry token realm must use the registry origin")
    if realm_url.username or realm_url.password or realm_url.query or realm_url.fragment:
        raise RegistryError("registry token realm is malformed")
    return realm


def probe(registry, repository, reference, actor, token, allow_http=False):
    if not allow_http and not registry.startswith("https://"):
        raise RegistryError("registry URL must use HTTPS")
    if not re.fullmatch(r"[a-z0-9]+(?:[._/-][a-z0-9]+)*", repository):
        raise RegistryError("registry repository is malformed")
    auth_url = f"{registry.rstrip('/')}/v2/"
    status, headers, _ = request(auth_url, {"Accept": ACCEPT})
    if status != 401:
        raise RegistryError(f"registry authentication challenge failed with HTTP {status}")
    parameters = bearer_parameters(headers.get("WWW-Authenticate"))
    realm = validate_token_realm(registry, parameters.pop("realm"), allow_http)
    parameters["scope"] = f"repository:{repository}:pull"
    token_url = realm + "?" + urllib.parse.urlencode(parameters)
    basic = base64.b64encode(f"{actor}:{token}".encode()).decode()
    token_status, _, body = request(token_url, {"Authorization": f"Basic {basic}"})
    if token_status != 200:
        raise RegistryError(f"registry token request failed with HTTP {token_status}")
    try:
        bearer = json.loads(body)["token"]
    except (KeyError, TypeError, ValueError, UnicodeDecodeError) as error:
        raise RegistryError("registry token response is malformed") from error
    if not isinstance(bearer, str) or not bearer:
        raise RegistryError("registry token response is malformed")
    manifest_url = f"{registry.rstrip('/')}/v2/{repository}/manifests/{urllib.parse.quote(reference, safe='')}"
    status, headers, _ = request(manifest_url, {"Accept": ACCEPT, "Authorization": f"Bearer {bearer}"})
    if status == 404:
        return "absent", ""
    if status != 200:
        raise RegistryError(f"registry manifest request failed with HTTP {status}")
    digest = headers.get("Docker-Content-Digest", "")
    if not DIGEST_RE.fullmatch(digest):
        raise RegistryError("registry manifest response has no valid Docker-Content-Digest")
    return "present", digest


def main():
    if len(sys.argv) != 3:
        raise RegistryError("usage: ghcr-tag-state.py <repository> <exact-tag>")
    actor = os.environ.get("GITHUB_ACTOR", "")
    token = os.environ.get("GITHUB_TOKEN", "")
    if not actor or not token:
        raise RegistryError("GITHUB_ACTOR and GITHUB_TOKEN are required")
    state, digest = probe("https://ghcr.io", sys.argv[1], sys.argv[2], actor, token)
    print(f"state={state}")
    if digest:
        print(f"digest={digest}")


if __name__ == "__main__":
    try:
        main()
    except RegistryError as error:
        print(f"ghcr tag probe: {error}", file=sys.stderr)
        sys.exit(1)
