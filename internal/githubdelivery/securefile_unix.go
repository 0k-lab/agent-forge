//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package githubdelivery

import (
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openProtected(path string) (*os.File, error) {
	return openProtectedWithOwner(path, owned)
}

func openProtectedExecutable(path string) (*os.File, error) {
	return openProtectedWithOwner(path, executableOwned)
}

func openProtectedWithOwner(path string, ownerPredicate func(os.FileInfo) bool) (*os.File, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errConfig
	}
	parentPath := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(parentPath)
	if err != nil || resolved != parentPath {
		return nil, errConfig
	}
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errConfig
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil || !info.IsDir() || !ownerPredicate(info) || info.Mode().Perm()&0o022 != 0 {
		return nil, errConfig
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errConfig
	}
	return os.NewFile(uintptr(fd), path), nil
}

func readProtectedFile(path string, mode os.FileMode, limit int) ([]byte, error) {
	file, err := openProtected(path)
	if err != nil {
		return nil, errConfig
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != mode {
		return nil, errConfig
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(data) == 0 || len(data) > limit {
		return nil, errConfig
	}
	return data, nil
}
