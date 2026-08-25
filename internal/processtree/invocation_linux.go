package processtree

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const invocationWrapperArg = "__agent_forge_invocation_wrapper_7f21d8c4"

func init() {
	if len(os.Args) < 4 || os.Args[1] != invocationWrapperArg {
		return
	}
	if unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != nil {
		os.Exit(127)
	}
	cmd := &exec.Cmd{Path: os.Args[2], Args: os.Args[3:], Env: os.Environ(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if cmd.Start() != nil {
		os.Exit(127)
	}
	err := cmd.Wait()
	unclean := invocationHoldsOutput(os.Getpid())
	cleanupInvocation(os.Getpid())
	if unclean {
		os.Exit(126)
	}
	if err == nil {
		os.Exit(0)
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ProcessState.ExitCode() >= 0 {
		os.Exit(exit.ProcessState.ExitCode())
	}
	os.Exit(1)
}

func RunInvocation(ctx context.Context, cmd *exec.Cmd) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	info, err := os.Stat(cmd.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return &os.PathError{Op: "fork/exec", Path: cmd.Path, Err: syscall.ENOENT}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	args := append([]string{executable, invocationWrapperArg, cmd.Path}, cmd.Args...)
	cmd.Path, cmd.Args, cmd.Env = executable, args, invocationWrapperEnvironment(cmd.Env)
	return Run(ctx, cmd)
}

func invocationHoldsOutput(root int) bool {
	stdout, _ := os.Readlink("/proc/self/fd/1")
	stderr, _ := os.Readlink("/proc/self/fd/2")
	for _, process := range descendants(root) {
		for fd, inherited := range map[string]string{"1": stdout, "2": stderr} {
			link, err := os.Readlink("/proc/" + strconv.Itoa(process.pid) + "/fd/" + fd)
			if err == nil && inherited != "" && link == inherited {
				return true
			}
		}
	}
	return false
}

func cleanupInvocation(root int) {
	opened := make(map[processIdentity]int)
	for range 4 {
		for _, process := range descendants(root) {
			if _, ok := opened[process]; ok {
				continue
			}
			if fd, err := openProcess(process); err == nil {
				opened[process] = fd
				_ = unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0)
			}
		}
	}
	for _, fd := range opened {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	}
	deadline := time.Now().Add(cleanupGrace)
	for _, fd := range opened {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		_, _ = unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, int(remaining.Milliseconds())+1)
	}
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			break
		}
	}
	for _, fd := range opened {
		_ = unix.Close(fd)
	}
}
