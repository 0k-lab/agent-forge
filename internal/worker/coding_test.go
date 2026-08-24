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
	write(t, plugin, "#!/bin/sh\npython3 -c 'import json,sys,pathlib; r=json.load(sys.stdin); pathlib.Path(r[\"workspace\"], \"answer.txt\").write_text(\"candidate\\n\"); print(json.dumps({\"version\":\"v1\",\"result\":\"Mallory <mallory@example.com>\"}))'\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}

	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, "11111111111111111111111111111111", "22222222222222222222222222222222", protocol.CodingTask{
		Repository:        repo,
		BaseSHA:           base,
		Instruction:       "write candidate to answer.txt",
		Tests:             [][]string{{"grep", "-qx", "candidate", "answer.txt"}},
		CommitAuthorName:  "kricha",
		CommitAuthorEmail: "4619899+kricha@users.noreply.github.com",
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
	if got := git(t, repo, "show", "-s", "--format=%an <%ae>%n%cn <%ce>", sha); got != "kricha <4619899+kricha@users.noreply.github.com>\nAgent Forge <forge@example.invalid>" {
		t.Fatalf("candidate identity = %q", got)
	}
	if got := git(t, repo, "status", "--short"); got != "" {
		t.Fatalf("source repository changed: %s", got)
	}
	ref := "refs/agent-forge/candidates/11111111111111111111111111111111/22222222222222222222222222222222"
	if got := git(t, repo, "rev-parse", ref); got != sha {
		t.Fatalf("candidate ref = %s, want %s", got, sha)
	}
	git(t, repo, "reflog", "expire", "--expire=now", "--all")
	git(t, repo, "gc", "--prune=now")
	if got := git(t, repo, "rev-parse", ref); got != sha {
		t.Fatalf("candidate after GC = %s, want %s", got, sha)
	}
}

func TestCodingTaskWithoutCommitAuthorUsesAgentForgeFallback(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccc", protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, repo, "show", "-s", "--format=%an <%ae>%n%cn <%ce>", sha); got != "Agent Forge <forge@example.invalid>\nAgent Forge <forge@example.invalid>" {
		t.Fatalf("fallback identity = %q", got)
	}
}

func TestCodingTaskRejectsInvalidCommitAuthorBeforePluginOrCandidate(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	marker := filepath.Join(t.TempDir(), "plugin-invoked")
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\n: > \""+marker+"\"\nprintf '%s\\n' '{\"version\":\"v1\",\"result\":\"edited\"}'\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	jobID := "dddddddddddddddddddddddddddddddd"
	attemptID := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, jobID, attemptID, protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}, CommitAuthorName: "kricha",
	})
	if err == nil || sha != "" || err.Error() != "invalid commit author" {
		t.Fatalf("invalid author result: sha=%q err=%v", sha, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid author invoked plugin: %v", err)
	}
	if got := git(t, repo, "rev-list", "--all", "--count"); got != "1" {
		t.Fatalf("invalid author created a commit: count=%s", got)
	}
	ref := "refs/agent-forge/candidates/" + jobID + "/" + attemptID
	if err := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", ref).Run(); err == nil {
		t.Fatal("invalid author created a candidate ref")
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

	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, "33333333333333333333333333333333", "44444444444444444444444444444444", protocol.CodingTask{
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

func TestRepositoryCapabilityAllowsChildAndRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	sibling := root + "-sibling"
	outside := t.TempDir()
	for _, dir := range []string{child, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	roots, err := canonicalRepositoryRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := allowedRepository(child, roots); err != nil || got == "" {
		t.Fatalf("allowed child rejected: got=%q err=%v", got, err)
	}
	for _, repository := range []string{sibling, link} {
		if got, err := allowedRepository(repository, roots); err == nil || got != "" {
			t.Fatalf("escape accepted: got=%q err=%v", got, err)
		}
	}
}

func TestCodingTaskFailsWithoutRepositoryRoots(t *testing.T) {
	sha, err := executeCodingTask(context.Background(), "unused", nil, "55555555555555555555555555555555", "66666666666666666666666666666666", protocol.CodingTask{Repository: t.TempDir(), Tests: [][]string{{"true"}}})
	if err == nil || sha != "" || strings.Contains(err.Error(), string(filepath.Separator)) {
		t.Fatalf("no-root result: sha=%q err=%v", sha, err)
	}
}

func TestCandidateRefConflictIsNotOverwritten(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	jobID := "99999999999999999999999999999999"
	attemptID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := "refs/agent-forge/candidates/" + jobID + "/" + attemptID
	git(t, repo, "update-ref", ref, base)

	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, jobID, attemptID, protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
	})
	if err == nil || sha != "" {
		t.Fatalf("conflicting ref accepted: sha=%q err=%v", sha, err)
	}
	if got := git(t, repo, "rev-parse", ref); got != base {
		t.Fatalf("conflicting ref overwritten with %s", got)
	}
}

func TestScopedTestEnvironmentDoesNotInheritWorkerSecret(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\n[ -z \"${FORGE_FAKE_SECRET+x}\" ]\n")
	t.Setenv("FORGE_FAKE_SECRET", "must-not-leak")
	if _, err := executeCodingTask(context.Background(), plugin, []string{repo}, "77777777777777777777777777777777", "88888888888888888888888888888888", protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"./check-env"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPluginEnvironmentDoesNotInheritWorkerSecret(t *testing.T) {
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\n[ -z \"${FORGE_FAKE_SECRET+x}\" ] || exit 9\nprintf '%s\\n' '{\"version\":\"v1\",\"result\":\"ok\"}'\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_FAKE_SECRET", "must-not-leak")
	if got, err := invoke(context.Background(), plugin, pluginRequest{Version: "v1", Input: "hello"}); err != nil || got != "ok" {
		t.Fatalf("invoke = %q, %v", got, err)
	}
}

func codingFixture(t *testing.T, check string) (repo, base, plugin string) {
	t.Helper()
	repo = t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Forge Test")
	git(t, repo, "config", "user.email", "forge@example.invalid")
	write(t, filepath.Join(repo, "answer.txt"), "base\n")
	write(t, filepath.Join(repo, "check-env"), check)
	if err := os.Chmod(filepath.Join(repo, "check-env"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base = git(t, repo, "rev-parse", "HEAD")
	plugin = filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\npython3 -c 'import json,sys,pathlib; r=json.load(sys.stdin); pathlib.Path(r[\"workspace\"], \"answer.txt\").write_text(\"candidate\\n\"); print(json.dumps({\"version\":\"v1\",\"result\":\"edited\"}))'\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, base, plugin
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
