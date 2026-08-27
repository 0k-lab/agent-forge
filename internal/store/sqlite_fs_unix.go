//go:build unix

package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func acquireSQLiteLock(path string) (io.Closer, error) {
	if rejectSQLiteSymlinkComponents(path) != nil || ensureSQLiteParent(path) != nil {
		return nil, ErrInsecureDatabase
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 || fileUID(parent) != uint32(os.Geteuid()) {
		return nil, ErrInsecureDatabase
	}
	lockPath := path + ".lock"
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	created := err == nil
	if errors.Is(err, syscall.EEXIST) {
		fd, err = unix.Open(lockPath, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, ErrInsecureDatabase
	}
	file := os.NewFile(uintptr(fd), lockPath)
	fail := func(err error) (io.Closer, error) {
		_ = file.Close()
		return nil, err
	}
	if created {
		if err := file.Chmod(0o600); err != nil {
			return fail(ErrInsecureDatabase)
		}
	}
	opened, err := file.Stat()
	if err != nil || validateSQLiteFileInfo(opened) != nil {
		return fail(ErrInsecureDatabase)
	}
	current, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(opened, current) {
		return fail(ErrInsecureDatabase)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fail(ErrAlreadyOwned)
		}
		return fail(ErrInsecureDatabase)
	}
	return file, nil
}

func prepareSQLiteFile(dsn sqliteDSN) (func() error, error) {
	if rejectSQLiteSymlinkComponents(dsn.path) != nil {
		return nil, ErrInsecureDatabase
	}
	if err := ensureSQLiteParent(dsn.path); err != nil {
		return nil, ErrInsecureDatabase
	}
	parent, err := os.Lstat(filepath.Dir(dsn.path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 || fileUID(parent) != uint32(os.Geteuid()) {
		return nil, ErrInsecureDatabase
	}
	if err = validateSQLiteArtifact(dsn.path); err == nil {
		if err = validateSQLiteSidecars(dsn.path); err != nil {
			return nil, ErrInsecureDatabase
		}
		return func() error { return validateSQLiteFiles(dsn.path) }, nil
	} else if !errors.Is(err, os.ErrNotExist) || !dsn.create {
		return nil, ErrInsecureDatabase
	}
	file, err := os.OpenFile(dsn.path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if err = file.Chmod(0o600); err == nil {
			var info os.FileInfo
			if info, err = file.Stat(); err == nil {
				err = validateSQLiteFileInfo(info)
			}
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if errors.Is(err, os.ErrExist) {
		err = validateSQLiteArtifact(dsn.path)
	}
	if err != nil {
		return nil, ErrInsecureDatabase
	}
	if err = validateSQLiteSidecars(dsn.path); err != nil {
		return nil, ErrInsecureDatabase
	}
	return func() error { return validateSQLiteFiles(dsn.path) }, nil
}

func ensureSQLiteParent(path string) error {
	parent := filepath.Dir(path)
	if _, err := os.Lstat(parent); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info, err := os.Lstat(filepath.Dir(parent)); err != nil || !info.IsDir() {
		return ErrInsecureDatabase
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), parent)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.IsDir() || opened.Mode().Perm() != 0o700 || fileUID(opened) != uint32(os.Geteuid()) {
		return ErrInsecureDatabase
	}
	current, err := os.Lstat(parent)
	if err != nil || !os.SameFile(opened, current) {
		return ErrInsecureDatabase
	}
	return nil
}

func fileUID(info os.FileInfo) uint32 { return info.Sys().(*syscall.Stat_t).Uid }

func validateSQLiteArtifact(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validateSQLiteFileInfo(info)
}

func validateSQLiteFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o600 || fileUID(info) != uint32(os.Geteuid()) {
		return ErrInsecureDatabase
	}
	return nil
}

func validateSQLiteFiles(path string) error {
	if rejectSQLiteSymlinkComponents(path) != nil || validateSQLiteArtifact(path) != nil || validateSQLiteSidecars(path) != nil {
		return ErrInsecureDatabase
	}
	return nil
}

func validateSQLiteSidecars(path string) error {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err := validateSQLiteArtifact(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func rejectSQLiteSymlinkComponents(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := string(os.PathSeparator)
	for _, name := range strings.Split(strings.TrimPrefix(filepath.Clean(abs), current), current) {
		current = filepath.Join(current, name)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ErrInsecureDatabase
		}
	}
	return nil
}
