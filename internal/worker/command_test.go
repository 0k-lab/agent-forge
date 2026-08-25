package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestScopedCheckResolvesExecutableFromCheckEnvironment(t *testing.T) {
	worktree, ambientBin, checkBin := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(dir, name, output string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '"+output+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(ambientBin, "ambient-only-check", "ambient")
	write(checkBin, "allowed-check", "allowed")
	relativeBin := filepath.Join(worktree, "check-bin")
	if err := os.Mkdir(relativeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	write(relativeBin, "relative-path-check", "relative-path")
	relative := write(worktree, "repo-check", "relative")
	t.Setenv("PATH", ambientBin)

	for _, tt := range []struct {
		name      string
		env, argv []string
		wantErr   bool
	}{
		{"ambient PATH is ignored", []string{"PATH=/bin"}, []string{"ambient-only-check"}, true},
		{"check PATH is used", []string{"PATH=" + checkBin}, []string{"allowed-check"}, false},
		{"relative check PATH uses worktree", []string{"PATH=check-bin"}, []string{"relative-path-check"}, false},
		{"absent PATH fails closed", nil, []string{"ambient-only-check"}, true},
		{"repo-relative is explicit", nil, []string{"./" + filepath.Base(relative)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := runScopedCheckLocal(context.Background(), worktree, tt.env, tt.argv, time.Second, 1024)
			if (result.err != nil) != tt.wantErr || !tt.wantErr && (!result.redacted || result.output != "[REDACTED]") || strings.Contains(result.output, ambientBin) {
				t.Fatalf("result = %#v, want error %v", result, tt.wantErr)
			}
		})
	}
}

func TestCommandTimeoutKillsDescendantPipeHolders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}
	tests := map[string]func(*testing.T, string){
		"plugin setsid escape": func(t *testing.T, pidFile string) {
			started := time.Now()
			_, err := invokeLocal(context.Background(), []string{"/bin/sh", "-c", `setsid /bin/sh -c 'echo $$ > "$PID_FILE"; sleep 3' & wait`}, pluginRequest{Version: "v1"}, 20*time.Millisecond, 1024, []string{"PID_FILE=" + pidFile})
			assertTimedOutTree(t, started, pidFile, err)
		},
		"scoped check setsid escape": func(t *testing.T, pidFile string) {
			started := time.Now()
			result := runScopedCheckLocal(context.Background(), t.TempDir(), []string{"PID_FILE=" + pidFile}, []string{"/bin/sh", "-c", `setsid /bin/sh -c 'echo $$ > "$PID_FILE"; sleep 3' & wait`}, 20*time.Millisecond, 1024)
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
