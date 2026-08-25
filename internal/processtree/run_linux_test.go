//go:build linux

package processtree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRunTimeoutKillsSetsidDescendant(t *testing.T) {
	for range 10 {
		if err := runSetsidTimeout(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunBasicBoundaries(t *testing.T) {
	t.Run("quick normal exit", func(t *testing.T) {
		cmd := exec.Command("/bin/sh", "-c", "exit 0")
		if err := Run(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cmd := exec.Command("/bin/sh", "-c", "sleep 3")
		if err := Run(ctx, cmd); !errors.Is(err, context.Canceled) || cmd.Process != nil {
			t.Fatalf("Run = %v, process = %v", err, cmd.Process)
		}
	})
}

func TestRunConcurrentTimeouts(t *testing.T) {
	const runs = 8
	errs := make(chan error, runs)
	for range runs {
		dir := t.TempDir()
		go func() {
			errs <- runSetsidTimeout(dir)
		}()
	}
	for range runs {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcessIdentity(t *testing.T) {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[1], fields[19] = "S", "45", "6789"
	process, err := parseProcessStat([]byte("123 (odd ) name) " + strings.Join(fields, " ")))
	if err != nil || process != (processIdentity{pid: 123, parent: 45, start: 6789}) {
		t.Fatalf("parseProcessStat = %+v, %v", process, err)
	}
	if _, err := parseProcessStat([]byte("broken")); err == nil {
		t.Fatal("parseProcessStat accepted malformed input")
	}

	self, err := readProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	fd, err := openProcess(self)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
	self.start++
	if fd, err := openProcess(self); err == nil {
		_ = unix.Close(fd)
		t.Fatal("openProcess accepted changed identity")
	}
}

func runSetsidTimeout(dir string) error {
	pidFile := filepath.Join(dir, "pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", `setsid /bin/sh -c 'echo $$ > "$PID_FILE"; sleep 3' & wait`)
	cmd.Env = append(os.Environ(), "PID_FILE="+pidFile)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cmd) }()

	var pid int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(body)))
			if err == nil {
				break
			}
			pid = 0
		}
		time.Sleep(time.Millisecond)
	}
	if pid == 0 {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		return errors.New("setsid descendant did not start")
	}
	process, err := readProcess(pid)
	if err != nil {
		return err
	}
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			return fmt.Errorf("Run error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		return errors.New("Run exceeded one-second cleanup bound")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		return fmt.Errorf("Run returned after %v", elapsed)
	}
	deadline = time.Now().Add(cleanupGrace)
	for processRunning(process) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if processRunning(process) {
		return fmt.Errorf("setsid descendant %d remains alive", pid)
	}
	return nil
}

func processRunning(process processIdentity) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(process.pid), "stat"))
	if err != nil {
		return false
	}
	current, err := parseProcessStat(stat)
	if err != nil || current != process {
		return false
	}
	close := strings.LastIndexByte(string(stat), ')')
	if close < 0 {
		return true
	}
	fields := strings.Fields(string(stat[close+1:]))
	return len(fields) == 0 || fields[0] != "Z"
}
