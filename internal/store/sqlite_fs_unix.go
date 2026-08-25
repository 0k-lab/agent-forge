//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func prepareSQLiteFile(dsn sqliteDSN) (func() error, error) {
	if rejectSQLiteSymlinkComponents(dsn.path) != nil {
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
