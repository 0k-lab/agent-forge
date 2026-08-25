package worker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/protocol"
)

func TestInvokeLocalUsesConfiguredArgvEnvironmentTimeoutAndOutput(t *testing.T) {
	request := pluginRequest{Version: "v1", Input: "input"}
	result, err := invokeLocal(context.Background(), []string{"/bin/sh", "-c", `printf '{"version":"v1","result":"%s"}\n' "$FORGE_ALLOWED"`}, request, time.Second, 1024, []string{"FORGE_ALLOWED=visible"})
	if err != nil || result != "visible" {
		t.Fatalf("configured invocation = %q, %v", result, err)
	}
	if _, err := invokeLocal(context.Background(), []string{"/bin/sh", "-c", "sleep 1"}, request, 10*time.Millisecond, 1024, nil); err == nil {
		t.Fatal("plugin timeout was not enforced")
	}
	if _, err := invokeLocal(context.Background(), []string{"/bin/sh", "-c", "head -c 2048 /dev/zero"}, request, time.Second, 16, nil); err == nil {
		t.Fatal("plugin output ceiling was not enforced")
	}
}

func configuredLeasePolicy() protocol.ResolvedPolicy {
	return protocol.ResolvedPolicy{
		Version: 1, WorkerPool: "coding", LeaseTTLNanos: int64(30 * time.Second), RetryBaseNanos: int64(time.Second), MaxAttempts: 3, RetryAlgorithm: "exponential-v1", RetryMaxNanos: int64(24 * time.Hour),
		Execution: protocol.ExecutionPolicy{RepositoryID: "agent-forge", DefaultBranch: "main", PluginID: "codex", Environment: []string{"PATH"}, PluginTimeoutNanos: int64(15 * time.Minute), CheckTimeoutNanos: int64(10 * time.Minute), GitTimeoutNanos: int64(time.Minute), CleanupTimeoutNanos: int64(10 * time.Second), PluginOutputBytes: 1 << 20, CheckOutputBytes: 2048, GitOutputBytes: 1 << 20},
	}
}

func configuredWorkerAndLease(t *testing.T) (Config, protocol.Message, string) {
	t.Helper()
	c, err := ParseConfig([]byte(validWorkerConfig(t)), func(string) string { return "worker-secret" })
	if err != nil {
		t.Fatal(err)
	}
	repository := c.Repositories[0].Path
	git(t, repository, "init", "-b", "main")
	if err := os.WriteFile(repository+"/file.txt", []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "add", "file.txt")
	git(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "base")
	base := git(t, repository, "rev-parse", "HEAD")
	policy := configuredLeasePolicy()
	return c, protocol.Message{Type: protocol.MessageLease, JobID: strings.Repeat("a", 32), AttemptID: strings.Repeat("b", 32), Task: &protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: base, Instruction: "change", Tests: [][]string{{"true"}}}, Policy: &policy}, base
}

func TestResolveConfiguredLeaseUsesOnlyLocalRegistries(t *testing.T) {
	c, message, _ := configuredWorkerAndLease(t)
	resolved, err := resolveLease(c, message)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.repository != c.Repositories[0].Path || resolved.pluginArgv[0] != c.Plugins[0].Argv[0] || resolved.policy.Execution.RepositoryID != "agent-forge" {
		t.Fatalf("resolved lease = %#v", resolved)
	}
	if message.Task.Repository != "" {
		t.Fatal("lease carried a local repository path")
	}
}

func TestResolveConfiguredLeaseFailsClosed(t *testing.T) {
	c, message, base := configuredWorkerAndLease(t)
	tests := map[string]func(*protocol.Message){
		"missing policy": func(m *protocol.Message) { m.Policy = nil },
		"unknown repository": func(m *protocol.Message) {
			m.Task.RepositoryID = "missing"
			m.Policy.Execution.RepositoryID = "missing"
		},
		"unknown plugin":  func(m *protocol.Message) { m.Policy.Execution.PluginID = "missing" },
		"environment":     func(m *protocol.Message) { m.Policy.Execution.Environment = []string{"PRIVATE_SECRET"} },
		"timeout ceiling": func(m *protocol.Message) { m.Policy.Execution.PluginTimeoutNanos = int64(21 * time.Minute) },
		"output ceiling":  func(m *protocol.Message) { m.Policy.Execution.CheckOutputBytes = 2049 },
		"missing branch":  func(m *protocol.Message) { m.Policy.Execution.DefaultBranch = "missing" },
		"non ancestor":    func(m *protocol.Message) { m.Task.BaseSHA = strings.Repeat("a", 40) },
		"path payload":    func(m *protocol.Message) { m.Task.Repository = "/private/repository" },
	}
	_ = base
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			copyMessage := message
			copyTask, copyPolicy := *message.Task, *message.Policy
			copyPolicy.Execution.Environment = append([]string(nil), message.Policy.Execution.Environment...)
			copyMessage.Task, copyMessage.Policy = &copyTask, &copyPolicy
			mutate(&copyMessage)
			_, err := resolveLease(c, copyMessage)
			if err == nil {
				t.Fatal("accepted unsafe lease")
			}
			for _, private := range []string{"PRIVATE_SECRET", "/private/repository", c.Repositories[0].Path, strings.Join(c.Plugins[0].Argv, " ")} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("error leaked private value: %q", err)
				}
			}
		})
	}
}

func TestResolveConfiguredLeaseRejectsCanonicalPathDrift(t *testing.T) {
	c, message, _ := configuredWorkerAndLease(t)
	repository := c.Repositories[0].Path
	moved := repository + "-moved"
	if err := os.Rename(repository, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, repository); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLease(c, message); err == nil {
		t.Fatal("accepted repository symlink drift")
	}
}
