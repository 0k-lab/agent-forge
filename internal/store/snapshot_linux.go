//go:build linux

package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var errInvalidPreMigrationSnapshot = errors.New("invalid pre-migration snapshot")

// CreatePreMigrationSnapshot makes an offline, immutable copy without replacing an artifact.
func CreatePreMigrationSnapshot(databasePath, artifactPath string) (PreMigrationSnapshotIdentity, error) {
	if validateSnapshotPath(databasePath) != nil || validateSnapshotPath(artifactPath) != nil || databasePath == artifactPath {
		return PreMigrationSnapshotIdentity{}, ErrInvalidDatabaseLocation
	}
	lock, err := acquireSQLiteLock(databasePath)
	if err != nil {
		return PreMigrationSnapshotIdentity{}, err
	}
	defer lock.Close()

	source, err := openSecureSnapshotFile(databasePath)
	if err != nil {
		return PreMigrationSnapshotIdentity{}, err
	}
	defer source.Close()
	if rejectSnapshotSidecars(databasePath) != nil || rejectSnapshotSidecars(artifactPath) != nil {
		return PreMigrationSnapshotIdentity{}, ErrInsecureDatabase
	}
	if rejectSQLiteSymlinkComponents(artifactPath) != nil {
		return PreMigrationSnapshotIdentity{}, ErrInsecureDatabase
	}

	directory, err := openSecureSnapshotDirectory(filepath.Dir(artifactPath))
	if err != nil {
		return PreMigrationSnapshotIdentity{}, err
	}
	defer directory.Close()
	temporaryName, temporary, err := createSnapshotTemporary(directory, filepath.Base(artifactPath))
	if err != nil {
		return PreMigrationSnapshotIdentity{}, err
	}
	published := false
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			_ = temporary.Close()
		}
		if !published {
			_ = unix.Unlinkat(int(directory.Fd()), temporaryName, 0)
			_ = directory.Sync()
		}
	}()

	if _, err = io.Copy(temporary, source); err != nil {
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	if err = temporary.Sync(); err != nil {
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	if err = temporary.Close(); err != nil {
		temporaryOpen = false
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	temporaryOpen = false

	temporaryPath := filepath.Join(filepath.Dir(artifactPath), temporaryName)
	identity, err := inspectPreMigrationSnapshot(temporaryPath)
	if err != nil {
		return PreMigrationSnapshotIdentity{}, err
	}
	if rejectSnapshotSidecars(databasePath) != nil || rejectSnapshotSidecars(artifactPath) != nil {
		return PreMigrationSnapshotIdentity{}, ErrInsecureDatabase
	}
	if err = unix.Renameat2(int(directory.Fd()), temporaryName, int(directory.Fd()), filepath.Base(artifactPath), unix.RENAME_NOREPLACE); err != nil {
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	published = true
	if err = directory.Sync(); err != nil {
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	if rejectSnapshotSidecars(databasePath) != nil || rejectSnapshotSidecars(artifactPath) != nil {
		return PreMigrationSnapshotIdentity{}, ErrInsecureDatabase
	}
	return identity, nil
}

// ValidatePreMigrationSnapshot verifies a private standalone artifact and its exact identity.
func ValidatePreMigrationSnapshot(artifactPath string, expectedIdentity PreMigrationSnapshotIdentity) error {
	if validateSnapshotPath(artifactPath) != nil {
		return ErrInvalidDatabaseLocation
	}
	actual, err := inspectPreMigrationSnapshot(artifactPath)
	if err != nil {
		return err
	}
	if actual != expectedIdentity {
		return errInvalidPreMigrationSnapshot
	}
	return nil
}

func validateSnapshotPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(os.PathSeparator) {
		return ErrInvalidDatabaseLocation
	}
	return nil
}

func openSecureSnapshotDirectory(path string) (*os.File, error) {
	if rejectSQLiteSymlinkComponents(path) != nil {
		return nil, ErrInsecureDatabase
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrInsecureDatabase
	}
	directory := os.NewFile(uintptr(fd), path)
	info, statErr := directory.Stat()
	current, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || fileUID(info) != uint32(os.Geteuid()) || !os.SameFile(info, current) {
		directory.Close()
		return nil, ErrInsecureDatabase
	}
	return directory, nil
}

func openSecureSnapshotFile(path string) (*os.File, error) {
	if rejectSQLiteSymlinkComponents(path) != nil {
		return nil, ErrInsecureDatabase
	}
	if directory, err := openSecureSnapshotDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	} else {
		directory.Close()
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrInsecureDatabase
	}
	file := os.NewFile(uintptr(fd), path)
	info, statErr := file.Stat()
	current, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || validateSnapshotFileInfo(info) != nil || !os.SameFile(info, current) {
		file.Close()
		return nil, ErrInsecureDatabase
	}
	return file, nil
}

func validateSnapshotFileInfo(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || validateSQLiteFileInfo(info) != nil || stat.Nlink != 1 {
		return ErrInsecureDatabase
	}
	return nil
}

func rejectSnapshotSidecars(path string) error {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return ErrInsecureDatabase
		}
	}
	return nil
}

func createSnapshotTemporary(directory *os.File, artifactName string) (string, *os.File, error) {
	for attempts := 0; attempts < 100; attempts++ {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, errInvalidPreMigrationSnapshot
		}
		name := "." + artifactName + ".tmp-" + hex.EncodeToString(random[:])
		fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, syscall.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, errInvalidPreMigrationSnapshot
		}
		file := os.NewFile(uintptr(fd), name)
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			_ = unix.Unlinkat(int(directory.Fd()), name, 0)
			_ = directory.Sync()
			return "", nil, errInvalidPreMigrationSnapshot
		}
		return name, file, nil
	}
	return "", nil, errInvalidPreMigrationSnapshot
}

func inspectPreMigrationSnapshot(path string) (PreMigrationSnapshotIdentity, error) {
	if rejectSnapshotSidecars(path) != nil {
		return PreMigrationSnapshotIdentity{}, ErrInsecureDatabase
	}
	file, err := openSecureSnapshotFile(path)
	if err != nil {
		return PreMigrationSnapshotIdentity{}, err
	}
	defer file.Close()

	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	dsn := (&url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d", file.Fd()), RawQuery: "immutable=1&mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	defer db.Close()
	var quickCheck string
	if err = db.QueryRow(`PRAGMA quick_check`).Scan(&quickCheck); err != nil || quickCheck != "ok" {
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version < 0 || version > SchemaVersion() {
		return PreMigrationSnapshotIdentity{}, errInvalidPreMigrationSnapshot
	}
	current, err := os.Lstat(path)
	opened, statErr := file.Stat()
	if err != nil || statErr != nil || !os.SameFile(opened, current) || opened.Size() != size || rejectSnapshotSidecars(path) != nil {
		return PreMigrationSnapshotIdentity{}, ErrInsecureDatabase
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return PreMigrationSnapshotIdentity{SchemaVersion: version, Size: size, SHA256: digest}, nil
}
