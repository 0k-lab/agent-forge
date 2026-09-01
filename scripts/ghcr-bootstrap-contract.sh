#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORKFLOW=$ROOT/.github/workflows/ghcr-bootstrap.yml
README=$ROOT/README.md
YAML_CONTRACT=$ROOT/scripts/ghcr-bootstrap-yaml-contract.py
SELF_TEST=$ROOT/scripts/ghcr-bootstrap-contract-self-test.py

fail() { echo "GHCR bootstrap contract: $*" >&2; exit 1; }
has() { grep -Fq -- "$2" "$1" || fail "$1 lacks: $2"; }
lacks() { ! grep -Eqi -- "$2" "$1" || fail "$1 contains forbidden pattern: $2"; }

[[ -f $WORKFLOW ]] || fail "ghcr-bootstrap.yml is missing"
[[ -x $YAML_CONTRACT ]] || fail "ghcr-bootstrap-yaml-contract.py is missing or not executable"
[[ -x $SELF_TEST ]] || fail "ghcr-bootstrap-contract-self-test.py is missing or not executable"
python3 "$SELF_TEST"
python3 "$YAML_CONTRACT" "$WORKFLOW"
has "$README" 'GHCR Bootstrap'
has "$README" 'uses the repository `GITHUB_TOKEN`; no personal package token is required'
has "$README" 'make the package public in GitHub Package settings'
has "$README" 'verify an anonymous pull'
lacks "$README" 'GHCR_TOKEN|write:packages'

echo "GHCR bootstrap contract: PASS"
