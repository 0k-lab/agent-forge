package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var successfulScopedCheckCases = []struct {
	name string
	argv []string
}{
	{"setsid", []string{"/bin/sh", "-c", `setsid /bin/sh -c 'echo $$ > "$PID_FILE"; exec sleep 30' </dev/null >/dev/null 2>&1 & while [ ! -s "$PID_FILE" ]; do :; done`}},
	{"double fork", []string{"/usr/bin/python3", "-c", `
import os
pid=os.fork()
if pid == 0:
 os.setsid()
 pid=os.fork()
 if pid == 0:
  null=os.open(os.devnull,os.O_RDWR)
  for fd in (0,1,2): os.dup2(null,fd)
  open(os.environ["PID_FILE"],"w").write(str(os.getpid()))
  os.execl("/bin/sleep","sleep","30")
 os._exit(0)
os.waitpid(pid,0)
while not os.path.exists(os.environ["PID_FILE"]) or not os.path.getsize(os.environ["PID_FILE"]): pass
`}},
}

func TestSuccessfulScopedCheckCleansDetachedDescendants(t *testing.T) {
	for _, test := range successfulScopedCheckCases {
		t.Run(test.name, func(t *testing.T) {
			for range 10 {
				assertSuccessfulScopedCheckCleans(t, test.argv)
			}
		})
	}
}

func TestSuccessfulScopedCheckCleansDetachedDescendantsConcurrently(t *testing.T) {
	for _, test := range successfulScopedCheckCases {
		t.Run(test.name, func(t *testing.T) {
			const runs = 8
			errCh := make(chan error, runs)
			var wg sync.WaitGroup
			for range runs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					dir, err := os.MkdirTemp("", "scoped-check-cleanup-")
					if err != nil {
						errCh <- err
						return
					}
					defer os.RemoveAll(dir)
					pidFile := filepath.Join(dir, "pid")
					result := runScopedCheckLocal(context.Background(), dir, []string{"PID_FILE=" + pidFile}, test.argv, 5*time.Second, 1024)
					if result.err != nil {
						errCh <- result.err
						return
					}
					if err := pidFileProcessGone(pidFile); err != nil {
						errCh <- err
					}
				}()
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				t.Error(err)
			}
		})
	}
}

func TestSuccessfulScopedCheckFailsClosedForInheritedOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	result := runScopedCheckLocal(context.Background(), t.TempDir(), []string{"PID_FILE=" + pidFile}, []string{"/bin/sh", "-c", `sleep 30 & echo $! > "$PID_FILE"`}, 5*time.Second, 1024)
	if result.err == nil {
		t.Fatal("scoped check with descendant-held output succeeded")
	}
	if err := pidFileProcessGone(pidFile); err != nil {
		t.Fatal(err)
	}
}

func assertSuccessfulScopedCheckCleans(t *testing.T, argv []string) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	result := runScopedCheckLocal(context.Background(), dir, []string{"PID_FILE=" + pidFile}, argv, 5*time.Second, 1024)
	if result.err != nil || result.timedOut {
		t.Fatalf("scoped check = %#v, want success", result)
	}
	if err := pidFileProcessGone(pidFile); err != nil {
		t.Fatal(err)
	}
}

func pidFileProcessGone(pidFile string) error {
	body, err := os.ReadFile(pidFile)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		return err
	}
	if err := syscall.Kill(pid, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return errors.New("descendant remains alive")
	}
	return nil
}
