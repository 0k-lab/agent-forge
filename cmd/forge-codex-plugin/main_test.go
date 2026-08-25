package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-forge/internal/pluginprotocol"
)

func TestCodexPluginV1WithFakeBinary(t *testing.T) {
	workspace := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
	head := git("rev-parse", "HEAD")
	fake := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(fake, []byte(`#!/usr/bin/env python3
import json,os,pathlib,stat,sys
workspace=sys.argv[sys.argv.index("-C")+1]
schema=pathlib.Path(sys.argv[sys.argv.index("--output-schema")+1])
output=pathlib.Path(sys.argv[sys.argv.index("--output-last-message")+1])
assert schema.parent == output.parent and schema.parent.parent == pathlib.Path(os.environ["TMPDIR"])
assert stat.S_IMODE(schema.parent.stat().st_mode) == 0o700
assert stat.S_IMODE(schema.stat().st_mode) == 0o600
spec=json.loads(schema.read_text())
assert spec["required"] == ["commit_subject"] and spec["additionalProperties"] is False
prompt=sys.stdin.read()
assert "actual resulting diff" in prompt
pathlib.Path(workspace,"answer.txt").write_text("edited\n")
output.write_text(json.dumps({"commit_subject":"fix: use executor result"},separators=(",",":")))
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BIN", fake)
	privateTmp := t.TempDir()
	t.Setenv("TMPDIR", privateTmp)
	id := strings.Repeat("a", 32)
	input := `{"version":"v1","id":"` + id + `","type":"initialize","capabilities":["workspace_edit","commit_subject"],"limits":{"frame_bytes":1048576,"progress_frames":128,"text_bytes":65536,"progress_text_bytes":1024,"commit_subject_bytes":256}}` + "\n" +
		`{"version":"v1","id":"` + id + `","type":"execute","operation":"workspace_edit","workspace":` + quote(workspace) + `,"instruction":"edit","timeout_ms":1000}` + "\n"
	var output bytes.Buffer
	if err := serve(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	want := `{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["workspace_edit","commit_subject"]}` + "\n" +
		`{"version":"v1","id":"` + id + `","type":"result","commit_subject":"fix: use executor result"}` + "\n"
	if output.String() != want || git("rev-parse", "HEAD") != head {
		t.Fatalf("output=%q head=%s", output.String(), git("rev-parse", "HEAD"))
	}
	entries, err := os.ReadDir(privateTmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private temporary files not cleaned: %v, %v", entries, err)
	}
}

func TestCodexPluginRejectsMalformedFinalOutputWithoutLeaks(t *testing.T) {
	privateTmp := t.TempDir()
	fake := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(fake, []byte(`#!/usr/bin/env python3
import os,pathlib,sys
workspace=pathlib.Path(sys.argv[sys.argv.index("-C")+1])
output=pathlib.Path(sys.argv[sys.argv.index("--output-last-message")+1])
sys.stdin.read()
(workspace/"answer.txt").write_text("edited\n")
body=os.environ.get("FINAL_HEX","")
if body:
    output.write_bytes(bytes.fromhex(body))
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BIN", fake)
	t.Setenv("TMPDIR", privateTmp)
	for name, body := range map[string][]byte{
		"missing":       nil,
		"malformed":     []byte(`{"commit_subject":`),
		"duplicate":     []byte(`{"commit_subject":"fix: one","commit_subject":"fix: two"}`),
		"unknown":       []byte(`{"commit_subject":"fix: one","private":"secret"}`),
		"trailing":      []byte(`{"commit_subject":"fix: one"}{}`),
		"invalid UTF-8": {'{', '"', 'c', 'o', 'm', 'm', 'i', 't', '_', 's', 'u', 'b', 'j', 'e', 'c', 't', '"', ':', '"', 0xff, '"', '}'},
		"oversized":     []byte(`{"commit_subject":"` + strings.Repeat("x", 400) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				if output, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
			}
			git("init", "-q")
			if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
			t.Setenv("FINAL_HEX", hex.EncodeToString(body))
			if _, err := executeCodex(context.Background(), pluginprotocol.Request{Workspace: workspace, Instruction: "private instruction", TimeoutMS: 1000}); err == nil || err.Error() != "codex_failed" || strings.Contains(err.Error(), workspace) || strings.Contains(err.Error(), "private") {
				t.Fatalf("error = %v", err)
			}
			entries, err := filepath.Glob(filepath.Join(privateTmp, "forge-codex-*"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("Codex temporary files not cleaned: %v, %v", entries, err)
			}
		})
	}
}

func quote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
