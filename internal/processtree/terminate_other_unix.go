//go:build aix || darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package processtree

import "syscall"

func terminateTree(root int) {
	_ = syscall.Kill(-root, syscall.SIGKILL)
	_ = syscall.Kill(root, syscall.SIGKILL)
}
