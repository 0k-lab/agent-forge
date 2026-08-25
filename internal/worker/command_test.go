package worker

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandTimeoutKillsDescendantPipeHolders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}
	tests := map[string]func(*testing.T, string){
		"plugin": func(t *testing.T, pidFile string) {
			started := time.Now()
			_, err := invokeLocal(context.Background(), []string{"/bin/sh", "-c", `sleep 2 & echo $! > "$PID_FILE"; wait`}, pluginRequest{Version: "v1"}, 20*time.Millisecond, 1024, []string{"PID_FILE=" + pidFile})
			assertTimedOutTree(t, started, pidFile, err)
		},
		"scoped check": func(t *testing.T, pidFile string) {
			started := time.Now()
			result := runScopedCheckLocal(context.Background(), t.TempDir(), []string{"PID_FILE=" + pidFile}, []string{"/bin/sh", "-c", `sleep 2 & echo $! > "$PID_FILE"; wait`}, 20*time.Millisecond, 1024)
			assertTimedOutTree(t, started, pidFile, result.err)
			if !result.timedOut {
				t.Fatal("scoped check did not retain timeout classification")
			}
		},
		"git": func(t *testing.T, pidFile string) {
			repository := t.TempDir()
			git(t, repository, "init")
			started := time.Now()
			_, err := gitOutputLimited(context.Background(), repository, 20*time.Millisecond, 1024, "-c", "alias.hold=!sleep 2 & echo $! > "+pidFile+"; wait", "hold")
			assertTimedOutTree(t, started, pidFile, err)
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) { run(t, t.TempDir()+"/pid") })
	}
}

func assertTimedOutTree(t *testing.T, started time.Time, pidFile string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("command unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("timeout took %v", elapsed)
	}
	body, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("descendant pid: %v", readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	deadline := time.Now().Add(250 * time.Millisecond)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("descendant %d remains alive", pid)
	}
}

func TestPluginAndGitShareStdoutStderrOutputBudget(t *testing.T) {
	request := pluginRequest{Version: "v1"}
	command := `printf '123456789012345678901234567890' >&2; printf '{"version":"v1","result":"ok"}\n'`
	if _, err := invokeLocal(context.Background(), []string{"/bin/sh", "-c", command}, request, time.Second, 50, nil); err == nil {
		t.Fatal("plugin accepted combined output over limit")
	}
	repository := t.TempDir()
	git(t, repository, "init")
	alias := "!printf '%040d' 0; printf '%040d' 0 >&2"
	if _, err := gitOutputLimited(context.Background(), repository, time.Second, 64, "-c", "alias.noisy="+alias, "noisy"); err == nil {
		t.Fatal("git accepted combined output over limit")
	}
}
