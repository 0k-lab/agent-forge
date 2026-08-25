#!/usr/bin/env python3
import json
import pathlib
import sys

workspace = sys.argv[sys.argv.index("-C") + 1]
schema = pathlib.Path(sys.argv[sys.argv.index("--output-schema") + 1])
output = pathlib.Path(sys.argv[sys.argv.index("--output-last-message") + 1])
sys.stdin.read()
json.loads(schema.read_text())
answer_go = pathlib.Path(workspace, "answer.go")
if answer_go.exists():
    answer_go.write_text(answer_go.read_text().replace("return 0", "return 42"))
else:
    pathlib.Path(workspace, "answer.txt").write_text("conformance\n")
output.write_text(json.dumps({"commit_subject": "test: prove conformance"}, separators=(",", ":")))
