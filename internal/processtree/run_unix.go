//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package processtree

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

func Run(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			_ = cmd.Process.Kill()
			<-done
		}
		return ctx.Err()
	}
}
