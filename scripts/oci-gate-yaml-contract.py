#!/usr/bin/env python3
"""Validate the release OCI credential lifecycle as parsed YAML."""
from __future__ import annotations
import sys
from pathlib import Path
from typing import Any
import yaml
class UniqueBaseLoader(yaml.BaseLoader): pass
def mapping(loader,node,deep=False):
    out={}
    for kn,vn in node.value:
        key=loader.construct_object(kn,deep=deep)
        if key in out:
            raise yaml.constructor.ConstructorError("while constructing a mapping",node.start_mark,f"found duplicate key {key!r}",kn.start_mark)
        out[key]=loader.construct_object(vn,deep=deep)
    return out
UniqueBaseLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,mapping)
AUTH="""set -euo pipefail
install -d -m 0700 "$RUNNER_TEMP/agent-forge-oci-auth/.docker"
printf 'HOME=%s\\n' "$RUNNER_TEMP/agent-forge-oci-auth" >>"$GITHUB_ENV"
printf 'DOCKER_CONFIG=%s\\n' "$RUNNER_TEMP/agent-forge-oci-auth/.docker" >>"$GITHUB_ENV"
"""
CLEAN="""set -euo pipefail
[ "${HOME:-}" = "$RUNNER_TEMP/agent-forge-oci-auth" ]
[ "${DOCKER_CONFIG:-}" = "$HOME/.docker" ]
docker logout ghcr.io || true
rm -rf -- "$DOCKER_CONFIG"
"""
ANON="""set -euo pipefail
install -d -m 0700 "$RUNNER_TEMP/agent-forge-oci-anonymous/.docker"
printf 'HOME=%s\\n' "$RUNNER_TEMP/agent-forge-oci-anonymous" >>"$GITHUB_ENV"
printf 'DOCKER_CONFIG=%s\\n' "$RUNNER_TEMP/agent-forge-oci-anonymous/.docker" >>"$GITHUB_ENV"
"""
def fail(msg): raise ValueError(msg)
def named(steps,name):
    found=[(i,s) for i,s in enumerate(steps) if isinstance(s,dict) and s.get("name")==name]
    if len(found)!=1: fail(f"expected exactly one {name!r} step")
    return found[0]
def used(steps,prefix):
    found=[(i,s) for i,s in enumerate(steps) if isinstance(s,dict) and str(s.get("uses","")).startswith(prefix)]
    if len(found)!=1: fail(f"expected exactly one {prefix!r} action")
    return found[0]
def validate(doc):
    if not isinstance(doc,dict): fail("workflow root must be a mapping")
    jobs=doc.get("jobs")
    if not isinstance(jobs,dict) or not isinstance(jobs.get("publish-oci-gate"),dict): fail("publish-oci-gate job is missing")
    steps=jobs["publish-oci-gate"].get("steps")
    if not isinstance(steps,list): fail("publish-oci-gate steps must be a sequence")
    ai,a=named(steps,"Use isolated authenticated Docker config")
    ci,c=named(steps,"Remove GHCR credentials")
    ni,n=named(steps,"Use isolated anonymous Docker config")
    vi,_=named(steps,"Anonymously verify exact index")
    li,_=used(steps,"docker/login-action@")
    bi,_=used(steps,"docker/build-push-action@")
    ti,_=used(steps,"actions/attest-build-provenance@")
    if a != {"name":"Use isolated authenticated Docker config","run":AUTH}: fail("authenticated Docker config step is not exact")
    if c != {"name":"Remove GHCR credentials","if":"always()","run":CLEAN}: fail("credential cleanup step is not exact")
    if n != {"name":"Use isolated anonymous Docker config","run":ANON}: fail("anonymous Docker config step is not exact")
    if not (ai < li < bi < ti < ci < ni < vi): fail("credential lifecycle steps are not ordered auth -> login -> build -> attest -> cleanup -> anonymous -> verify")
def main(argv):
    if len(argv)!=2: print(f"usage: {Path(argv[0]).name} WORKFLOW",file=sys.stderr); return 2
    try:
        doc=yaml.load(Path(argv[1]).read_text(),Loader=UniqueBaseLoader); validate(doc)
    except (OSError,UnicodeError,yaml.YAMLError,ValueError) as e:
        print(f"OCI gate YAML contract: FAIL: {e}",file=sys.stderr); return 1
    print("OCI gate YAML contract: PASS"); return 0
if __name__=="__main__": raise SystemExit(main(sys.argv))
