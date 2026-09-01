#!/usr/bin/env python3
"""Negative-fixture tests for the structural OCI credential lifecycle."""
from __future__ import annotations
import os, subprocess, tempfile
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
CONTRACT=ROOT/"scripts/oci-gate-contract.sh"
WORKFLOW=ROOT/".github/workflows/release.yml"
OLD="""      - name: Remove GHCR credentials
        run: |
          set -euo pipefail
          [ "$HOME" = "$RUNNER_TEMP/agent-forge-oci-auth" ]
          [ "$DOCKER_CONFIG" = "$HOME/.docker" ]
          docker logout ghcr.io
          rm -rf -- "$DOCKER_CONFIG"
          install -d -m 0700 "$RUNNER_TEMP/agent-forge-oci-anonymous/.docker"
          printf 'HOME=%s\\n' "$RUNNER_TEMP/agent-forge-oci-anonymous" >>"$GITHUB_ENV"
          printf 'DOCKER_CONFIG=%s\\n' "$RUNNER_TEMP/agent-forge-oci-anonymous/.docker" >>"$GITHUB_ENV"
"""
CLEAN="""      - name: Remove GHCR credentials
        if: always()
        run: |
          set -euo pipefail
          [ "${HOME:-}" = "$RUNNER_TEMP/agent-forge-oci-auth" ]
          [ "${DOCKER_CONFIG:-}" = "$HOME/.docker" ]
          docker logout ghcr.io || true
          rm -rf -- "$DOCKER_CONFIG"
"""
ANON="""      - name: Use isolated anonymous Docker config
        run: |
          set -euo pipefail
          install -d -m 0700 "$RUNNER_TEMP/agent-forge-oci-anonymous/.docker"
          printf 'HOME=%s\\n' "$RUNNER_TEMP/agent-forge-oci-anonymous" >>"$GITHUB_ENV"
          printf 'DOCKER_CONFIG=%s\\n' "$RUNNER_TEMP/agent-forge-oci-anonymous/.docker" >>"$GITHUB_ENV"
"""
def once(s,old,new):
    assert s.count(old)==1, repr(old)
    return s.replace(old,new,1)
def drop(s,*parts):
    lines=[x for x in s.splitlines(True) if all(p in x for p in parts)]
    assert len(lines)==1, parts
    return s.replace(lines[0],"",1)
def run(s):
    with tempfile.NamedTemporaryFile("w",suffix=".yml") as f:
        f.write(s); f.flush(); e=os.environ.copy(); e["OCI_GATE_RELEASE"]=f.name; e["OCI_GATE_SELF_TEST_ACTIVE"]="1"
        return subprocess.run([str(CONTRACT)],cwd=ROOT,env=e,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
def main():
    source=WORKFLOW.read_text(); intended=source if CLEAN+ANON in source else once(source,OLD,CLEAN+ANON)
    pos=run(intended); assert pos.returncode==0,pos.stdout
    print("accept intended release credential lifecycle: PASS")
    fixtures=[
      ("authenticated HOME",drop(intended,"printf 'HOME=","oci-auth")),
      ("authenticated DOCKER_CONFIG",drop(intended,"printf 'DOCKER_CONFIG=","oci-auth")),
      ("cleanup rm",drop(intended,"rm -rf --")),
      ("cleanup always",once(intended,"        if: always()\n","        if: success()\n")),
      ("anonymous HOME",drop(intended,"printf 'HOME=","oci-anonymous")),
      ("anonymous DOCKER_CONFIG",drop(intended,"printf 'DOCKER_CONFIG=","oci-anonymous")),
      ("cleanup/anonymous ordering",once(intended,CLEAN+ANON,ANON+CLEAN)),
      ("comments do not satisfy",once(intended,'          rm -rf -- "$DOCKER_CONFIG"\n','          # rm -rf -- "$DOCKER_CONFIG"\n')),
      ("duplicate YAML key",once(intended,"    runs-on: ubuntu-latest\n    permissions:\n      contents: read\n      packages: write\n","    runs-on: ubuntu-latest\n    runs-on: ubuntu-latest\n    permissions:\n      contents: read\n      packages: write\n")),]
    bad=[]
    for name,text in fixtures:
      r=run(text); print(f"reject {name}: {'PASS' if r.returncode else 'FAIL (accepted)'}")
      if not r.returncode: bad.append(name)
    assert not bad,"negative fixtures accepted: "+", ".join(bad)
    print("OCI gate structural contract self-test: PASS")
if __name__=="__main__": main()
