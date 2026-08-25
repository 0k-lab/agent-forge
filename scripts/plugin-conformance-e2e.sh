#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

go build -o "$tmp/conformance" "$root/cmd/forge-plugin-conformance"
go build -o "$tmp/reference" "$root/cmd/forge-ref-plugin"
go build -o "$tmp/codex-plugin" "$root/cmd/forge-codex-plugin"

cat >"$tmp/fake-codex" <<'PY'
#!/usr/bin/env python3
import json,pathlib,sys
workspace=pathlib.Path(sys.argv[sys.argv.index("-C")+1])
output=pathlib.Path(sys.argv[sys.argv.index("--output-last-message")+1])
json.loads(pathlib.Path(sys.argv[sys.argv.index("--output-schema")+1]).read_text())
sys.stdin.read()
(workspace/"answer.txt").write_text("conformance\n")
output.write_text(json.dumps({"commit_subject":"test: prove conformance"},separators=(",",":")))
PY
chmod 700 "$tmp/fake-codex"

run() {
	"$@"
}

run "$tmp/conformance" "$tmp/reference"
run "$tmp/conformance" "$root/examples/forge-plugin-v1.py"
CODEX_BIN="$tmp/fake-codex" run "$tmp/conformance" -operation workspace_edit "$tmp/codex-plugin"
