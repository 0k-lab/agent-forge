//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package processtree

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

const cleanupGrace = 250 * time.Millisecond

func Run(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.WaitDelay <= 0 || cmd.WaitDelay > cleanupGrace {
		cmd.WaitDelay = cleanupGrace
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		terminateTree(cmd.Process.Pid)
		timer := time.NewTimer(cleanupGrace + cmd.WaitDelay)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			_ = cmd.Process.Kill()
			timer.Reset(cleanupGrace)
			select {
			case <-done:
			case <-timer.C:
			}
		}
		return ctx.Err()
	}
}
