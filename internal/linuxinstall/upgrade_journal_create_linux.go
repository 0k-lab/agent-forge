//go:build linux

package linuxinstall

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	upgradeJournalName     = "upgrade-transaction.json"
	upgradeJournalMaxBytes = 16 << 10
)

type upgradeJournalCreateOps struct {
	syncParent func(*os.File) error
}

func upgradeJournalPath(o Options) string {
	return rooted(o.Root, prefix+"/var/gate/state/"+upgradeJournalName)
}

func createUpgradeTransactionJournal(o Options, journal upgradeTransactionJournal) (upgradeJournalCreateOutcome, error) {
	return createUpgradeTransactionJournalWithOps(o, journal, upgradeJournalCreateOps{syncParent: func(f *os.File) error { return f.Sync() }})
}

func createUpgradeTransactionJournalWithOps(o Options, journal upgradeTransactionJournal, ops upgradeJournalCreateOps) (upgradeJournalCreateOutcome, error) {
	outcome := upgradeJournalNotAppliedDurable
	if journal.Phase != upgradePhasePreparedSnapshotDurable || journal.RestoreOutcome != "" || journal.RestoreResidue != nil {
		return outcome, errors.New("upgrade journal is not a prepared durable snapshot")
	}
	body, err := encodeUpgradeTransactionJournal(journal)
	if err != nil {
		return outcome, err
	}
	parent, err := openUpgradeJournalParent(o)
	if err != nil {
		return outcome, err
	}
	defer parent.Close()

	var target unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), upgradeJournalName, &target, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return outcome, errors.New("upgrade journal already exists")
	} else if !errors.Is(err, unix.ENOENT) {
		return outcome, fmt.Errorf("inspect upgrade journal: %w", err)
	}

	tempName, temp, err := createUpgradeJournalTemp(parent)
	if err != nil {
		return outcome, err
	}
	published := false
	defer func() {
		if !published {
			_ = unix.Unlinkat(int(parent.Fd()), tempName, 0)
		}
	}()
	if _, err = temp.Write(body); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return outcome, fmt.Errorf("persist upgrade journal temporary file: %w", err)
	}
	if err := unix.Renameat2(int(parent.Fd()), tempName, int(parent.Fd()), upgradeJournalName, unix.RENAME_NOREPLACE); err != nil {
		return outcome, fmt.Errorf("publish upgrade journal: %w", err)
	}
	published = true
	if ops.syncParent == nil {
		return upgradeJournalIndeterminate, errors.New("sync upgrade journal directory: missing operation")
	}
	if err := ops.syncParent(parent); err != nil {
		return upgradeJournalIndeterminate, fmt.Errorf("sync upgrade journal directory: %w", err)
	}
	return upgradeJournalAppliedDurable, nil
}

func readUpgradeTransactionJournal(o Options) (upgradeTransactionJournal, error) {
	parent, err := openUpgradeJournalParent(o)
	if err != nil {
		return upgradeTransactionJournal{}, err
	}
	defer parent.Close()
	var parentBefore unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &parentBefore); err != nil {
		return upgradeTransactionJournal{}, fmt.Errorf("inspect upgrade journal directory: %w", err)
	}
	fd, err := unix.Openat(int(parent.Fd()), upgradeJournalName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return upgradeTransactionJournal{}, fmt.Errorf("open upgrade journal: %w", err)
	}
	file := os.NewFile(uintptr(fd), upgradeJournalName)
	defer file.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || !validUpgradeJournalFile(before) {
		return upgradeTransactionJournal{}, errors.New("unsafe upgrade journal")
	}
	if before.Size > upgradeJournalMaxBytes {
		return upgradeTransactionJournal{}, errors.New("upgrade journal is too large")
	}
	body, err := io.ReadAll(io.LimitReader(file, upgradeJournalMaxBytes+1))
	if err != nil || len(body) > upgradeJournalMaxBytes {
		return upgradeTransactionJournal{}, errors.New("read upgrade journal failed")
	}

	var after, visible unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || !sameUpgradeJournalFile(before, after) || !validUpgradeJournalFile(after) ||
		unix.Fstatat(int(parent.Fd()), upgradeJournalName, &visible, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!sameUpgradeJournalFile(after, visible) || !validUpgradeJournalFile(visible) {
		return upgradeTransactionJournal{}, errors.New("upgrade journal changed while reading")
	}
	reopenedParent, err := openUpgradeJournalParent(o)
	if err != nil {
		return upgradeTransactionJournal{}, errors.New("upgrade journal path changed while reading")
	}
	defer reopenedParent.Close()
	var parentAfter, visibleAfter unix.Stat_t
	if unix.Fstat(int(reopenedParent.Fd()), &parentAfter) != nil || !sameUpgradeJournalInode(parentBefore, parentAfter) ||
		unix.Fstatat(int(reopenedParent.Fd()), upgradeJournalName, &visibleAfter, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!sameUpgradeJournalFile(after, visibleAfter) || !validUpgradeJournalFile(visibleAfter) {
		return upgradeTransactionJournal{}, errors.New("upgrade journal path changed while reading")
	}
	return decodeUpgradeTransactionJournal(body)
}

func openUpgradeJournalParent(o Options) (*os.File, error) {
	parentPath := filepath.Clean(filepath.Dir(upgradeJournalPath(o)))
	if !filepath.IsAbs(parentPath) {
		return nil, errors.New("upgrade journal path is not absolute")
	}
	rootFD, err := unix.Open("/", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, strings.TrimPrefix(parentPath, "/"), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open upgrade journal directory: %w", err)
	}
	return os.NewFile(uintptr(fd), parentPath), nil
}

func createUpgradeJournalTemp(parent *os.File) (string, *os.File, error) {
	for range 8 {
		var random [16]byte
		if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
			return "", nil, fmt.Errorf("name upgrade journal temporary file: %w", err)
		}
		name := "." + upgradeJournalName + ".tmp-" + hex.EncodeToString(random[:])
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o400)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("create upgrade journal temporary file: %w", err)
		}
		if err := unix.Fchmod(fd, 0o400); err != nil {
			unix.Close(fd)
			_ = unix.Unlinkat(int(parent.Fd()), name, 0)
			return "", nil, fmt.Errorf("secure upgrade journal temporary file: %w", err)
		}
		return name, os.NewFile(uintptr(fd), name), nil
	}
	return "", nil, errors.New("create upgrade journal temporary file: name collisions")
}

func validUpgradeJournalFile(stat unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0o7777 == 0o400 && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func sameUpgradeJournalInode(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino
}

func sameUpgradeJournalFile(a, b unix.Stat_t) bool {
	return sameUpgradeJournalInode(a, b) && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}
