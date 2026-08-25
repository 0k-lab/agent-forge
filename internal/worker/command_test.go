package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	tests := map[string]func(context.Context, string, string) (error, bool){
		"plugin ordinary": func(ctx context.Context, pidFile, _ string) (error, bool) {
			_, err := invokeLocal(ctx, []string{"/bin/sh", "-c", `sleep 30 & echo $! > "$PID_FILE"; wait`}, pluginRequest{Version: "v1"}, 5*time.Second, 1024, []string{"PID_FILE=" + pidFile})
			return err, errors.Is(ctx.Err(), context.DeadlineExceeded)
		},
		"plugin setsid escape": func(ctx context.Context, pidFile, _ string) (error, bool) {
			_, err := invokeLocal(ctx, []string{"/bin/sh", "-c", `setsid /bin/sh -c 'echo $$ > "$PID_FILE"; sleep 30' & wait`}, pluginRequest{Version: "v1"}, 5*time.Second, 1024, []string{"PID_FILE=" + pidFile})
			return err, errors.Is(ctx.Err(), context.DeadlineExceeded)
		},
		"scoped check ordinary": func(ctx context.Context, pidFile, workdir string) (error, bool) {
			result := runScopedCheckLocal(ctx, workdir, []string{"PID_FILE=" + pidFile}, []string{"/bin/sh", "-c", `sleep 30 & echo $! > "$PID_FILE"; wait`}, 5*time.Second, 1024)
			return result.err, result.timedOut
		},
		"scoped check setsid escape": func(ctx context.Context, pidFile, workdir string) (error, bool) {
			result := runScopedCheckLocal(ctx, workdir, []string{"PID_FILE=" + pidFile}, []string{"/bin/sh", "-c", `setsid /bin/sh -c 'echo $$ > "$PID_FILE"; sleep 30' & wait`}, 5*time.Second, 1024)
			return result.err, result.timedOut
		},
		"git ordinary": func(ctx context.Context, pidFile, repository string) (error, bool) {
			_, err := gitOutputLimited(ctx, repository, 5*time.Second, 1024, "-c", "alias.hold=!sleep 30 & echo $! > "+pidFile+"; wait", "hold")
			return err, errors.Is(ctx.Err(), context.DeadlineExceeded)
		},
		"git setsid escape": func(ctx context.Context, pidFile, repository string) (error, bool) {
			_, err := gitOutputLimited(ctx, repository, 5*time.Second, 1024, "-c", "alias.hold=!setsid /bin/sh -c 'echo $$ > "+pidFile+"; sleep 30' & wait", "hold")
			return err, errors.Is(ctx.Err(), context.DeadlineExceeded)
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			workdir := t.TempDir()
			if strings.HasPrefix(name, "git ") {
				git(t, workdir, "init")
			}
			ctx := newManualDeadlineContext()
			done := make(chan struct {
				err      error
				timedOut bool
			}, 1)
			go func() {
				err, timedOut := run(ctx, pidFile, workdir)
				done <- struct {
					err      error
					timedOut bool
				}{err, timedOut}
			}()
			pid := waitForPID(t, pidFile)
			started := time.Now()
			ctx.trigger()
			result := <-done
			assertTimedOutTree(t, started, pid, result.err, !strings.HasPrefix(name, "plugin "))
			if !result.timedOut {
				t.Fatal("command did not retain timeout classification")
			}
		})
	}
}

type manualDeadlineContext struct {
	done    chan struct{}
	once    sync.Once
	expired atomic.Bool
}

func newManualDeadlineContext() *manualDeadlineContext {
	return &manualDeadlineContext{done: make(chan struct{})}
}

func (c *manualDeadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *manualDeadlineContext) Done() <-chan struct{}       { return c.done }
func (c *manualDeadlineContext) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return nil
}
func (c *manualDeadlineContext) Value(any) any { return nil }
func (c *manualDeadlineContext) trigger() {
	c.once.Do(func() {
		c.expired.Store(true)
		close(c.done)
	})
}

func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(body))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("descendant pid was not written")
	return 0
}

func assertTimedOutTree(t *testing.T, started time.Time, pid int, err error, wantDeadlineError bool) {
	t.Helper()
	if err == nil || wantDeadlineError && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("command error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("timeout took %v", elapsed)
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
