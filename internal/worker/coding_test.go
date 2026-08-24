package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-forge/internal/protocol"
)

func TestCodingTaskUsesExactBaseAndCreatesCandidate(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Forge Test")
	git(t, repo, "config", "user.email", "forge@example.invalid")
	write(t, filepath.Join(repo, "answer.txt"), "base\n")
	git(t, repo, "add", "answer.txt")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "answer.txt"), "later\n")
	git(t, repo, "commit", "-qam", "later")

	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\npython3 -c 'import json,sys,pathlib; r=json.load(sys.stdin); pathlib.Path(r[\"workspace\"], \"answer.txt\").write_text(\"candidate\\n\"); print(json.dumps({\"version\":\"v1\",\"result\":\"edited\"}))'\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}

	sha, err := executeCodingTask(context.Background(), plugin, protocol.CodingTask{
		Repository:  repo,
		BaseSHA:     base,
		Instruction: "write candidate to answer.txt",
		Tests:       [][]string{{"grep", "-qx", "candidate", "answer.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, repo, "rev-parse", sha+"^"); got != base {
		t.Fatalf("candidate parent = %s, want base %s", got, base)
	}
	if got := git(t, repo, "show", sha+":answer.txt"); got != "candidate" {
		t.Fatalf("candidate content = %q", got)
	}
	if got := git(t, repo, "status", "--short"); got != "" {
		t.Fatalf("source repository changed: %s", got)
	}
}

func TestScopedTestFailurePreventsCandidateCreation(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Forge Test")
	git(t, repo, "config", "user.email", "forge@example.invalid")
	write(t, filepath.Join(repo, "answer.txt"), "base\n")
	git(t, repo, "add", "answer.txt")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")

	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\npython3 -c 'import json,sys,pathlib; r=json.load(sys.stdin); pathlib.Path(r[\"workspace\"], \"answer.txt\").write_text(\"wrong\\n\"); print(json.dumps({\"version\":\"v1\",\"result\":\"edited\"}))'\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}

	sha, err := executeCodingTask(context.Background(), plugin, protocol.CodingTask{
		Repository: repo,
		BaseSHA:    base,
		Tests:      [][]string{{"grep", "-qx", "expected", "answer.txt"}},
	})
	if err == nil || sha != "" {
		t.Fatalf("failed test produced candidate %q, err=%v", sha, err)
	}
	if got := git(t, repo, "rev-list", "--all", "--count"); got != "1" {
		t.Fatalf("failed test created a commit: count=%s", got)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
