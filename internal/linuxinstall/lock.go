//go:build linux

package linuxinstall

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquireUpgradeLock(root string) (*os.File, error) {
	name := filepath.Join(filepath.Dir(rooted(root, prefix)), ".agent-forge.install.lock")
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("installer lock unavailable")
	}
	file := os.NewFile(uintptr(fd), name)
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o777 != 0o600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return nil, errors.New("installer lock metadata is not exact")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.New("another installer transaction is active")
	}
	ok = true
	return file, nil
}
