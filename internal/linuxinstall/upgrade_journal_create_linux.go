//go:build linux

package linuxinstall

import (
	"bytes"
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
	syncParent  func(*os.File) error
	inspect     func(*os.File, string, *unix.Stat_t) error
	openUnnamed func(*os.File) (*os.File, error)
	publish     func(*os.File, *os.File, string) error
}

func defaultUpgradeJournalCreateOps() upgradeJournalCreateOps {
	return upgradeJournalCreateOps{
		syncParent: func(f *os.File) error { return f.Sync() },
		inspect: func(parent *os.File, name string, stat *unix.Stat_t) error {
			return unix.Fstatat(int(parent.Fd()), name, stat, unix.AT_SYMLINK_NOFOLLOW)
		},
		openUnnamed: func(parent *os.File) (*os.File, error) {
			fd, err := unix.Openat(int(parent.Fd()), ".", unix.O_RDWR|unix.O_CLOEXEC|unix.O_TMPFILE, 0o600)
			if err != nil {
				return nil, err
			}
			return os.NewFile(uintptr(fd), "unnamed upgrade journal"), nil
		},
		publish: func(parent, source *os.File, name string) error {
			return unix.Linkat(int(source.Fd()), "", int(parent.Fd()), name, unix.AT_EMPTY_PATH)
		},
	}
}

func upgradeJournalPath(o Options) string {
	return rooted(o.Root, prefix+"/var/gate/state/"+upgradeJournalName)
}

func createUpgradeTransactionJournal(o Options, journal upgradeTransactionJournal) (upgradeJournalCreateOutcome, error) {
	return createUpgradeTransactionJournalWithOps(o, journal, defaultUpgradeJournalCreateOps())
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
	defaults := defaultUpgradeJournalCreateOps()
	if ops.inspect == nil {
		ops.inspect = defaults.inspect
	}
	if ops.openUnnamed == nil {
		ops.openUnnamed = defaults.openUnnamed
	}
	if ops.publish == nil {
		ops.publish = defaults.publish
	}

	parent, err := openUpgradeJournalParent(o)
	if err != nil {
		return outcome, err
	}
	defer parent.Close()
	var pinnedParent unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &pinnedParent); err != nil {
		return outcome, fmt.Errorf("inspect upgrade journal directory: %w", err)
	}
	state, _, err := inspectUpgradeJournalName(parent, upgradeJournalName, unix.Stat_t{}, ops)
	if err != nil {
		return outcome, fmt.Errorf("inspect upgrade journal: %w", err)
	}
	if state != upgradeJournalNameAbsent {
		return outcome, errors.New("upgrade journal already exists")
	}

	unnamed, err := ops.openUnnamed(parent)
	if err != nil {
		return outcome, fmt.Errorf("create unnamed upgrade journal: %w", err)
	}
	if unnamed == nil {
		return outcome, errors.New("create unnamed upgrade journal: missing file")
	}
	defer unnamed.Close()
	if _, err := unnamed.Write(body); err != nil {
		return outcome, fmt.Errorf("write unnamed upgrade journal: %w", err)
	}
	if err := unnamed.Chmod(0o400); err != nil {
		return outcome, fmt.Errorf("set unnamed upgrade journal mode: %w", err)
	}
	if err := unnamed.Sync(); err != nil {
		return outcome, fmt.Errorf("sync unnamed upgrade journal: %w", err)
	}
	var expected unix.Stat_t
	if err := unix.Fstat(int(unnamed.Fd()), &expected); err != nil {
		return outcome, fmt.Errorf("inspect unnamed upgrade journal: %w", err)
	}
	if err := validatePinnedUpgradeJournal(unnamed, expected, body, 0); err != nil {
		return outcome, fmt.Errorf("validate unnamed upgrade journal: %w", err)
	}
	if err := validateUpgradeJournalParentIdentityAtPath(o, parent, pinnedParent); err != nil {
		return outcome, fmt.Errorf("upgrade journal directory changed before publish: %w", err)
	}
	state, _, err = inspectUpgradeJournalName(parent, upgradeJournalName, expected, ops)
	if err != nil || state != upgradeJournalNameAbsent {
		if err == nil {
			err = errors.New("upgrade journal appeared before publish")
		}
		return outcome, err
	}
	if err := validatePinnedUpgradeJournal(unnamed, expected, body, 0); err != nil {
		return outcome, fmt.Errorf("upgrade journal changed before publish: %w", err)
	}

	if publishErr := ops.publish(parent, unnamed, upgradeJournalName); publishErr != nil {
		return classifyUpgradeJournalPublishError(o, parent, unnamed, pinnedParent, expected, body, ops, publishErr)
	}
	if err := validatePublishedUpgradeJournal(parent, unnamed, expected, body, ops); err != nil {
		return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("validate published upgrade journal: %w", err), true)
	}
	if err := validateUpgradeJournalParentPath(o, parent, unnamed, pinnedParent, expected, body, ops); err != nil {
		return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("upgrade journal directory changed before sync: %w", err), true)
	}
	if ops.syncParent == nil {
		return upgradeJournalIndeterminate, newUpgradeJournalCreateError(errors.New("sync upgrade journal directory: missing operation"), true)
	}
	if err := ops.syncParent(parent); err != nil {
		return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("sync upgrade journal directory: %w", err), true)
	}
	if err := validateUpgradeJournalParentPath(o, parent, unnamed, pinnedParent, expected, body, ops); err != nil {
		return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("upgrade journal directory changed after sync: %w", err), true)
	}
	if err := validatePublishedUpgradeJournal(parent, unnamed, expected, body, ops); err != nil {
		return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("upgrade journal changed before durable result: %w", err), true)
	}
	if err := validateUpgradeJournalParentPath(o, parent, unnamed, pinnedParent, expected, body, ops); err != nil {
		return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("upgrade journal changed before durable result: %w", err), true)
	}
	return upgradeJournalAppliedDurable, nil
}

type upgradeJournalNameState uint8

const (
	upgradeJournalNameUncertain upgradeJournalNameState = iota
	upgradeJournalNameAbsent
	upgradeJournalNameExpected
	upgradeJournalNameOther
)

func inspectUpgradeJournalName(parent *os.File, name string, expected unix.Stat_t, ops upgradeJournalCreateOps) (upgradeJournalNameState, unix.Stat_t, error) {
	var stat unix.Stat_t
	if ops.inspect == nil {
		return upgradeJournalNameUncertain, stat, errors.New("missing inspection operation")
	}
	if err := ops.inspect(parent, name, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return upgradeJournalNameAbsent, stat, nil
		}
		return upgradeJournalNameUncertain, stat, err
	}
	if expected.Ino != 0 && sameUpgradeJournalInode(expected, stat) {
		return upgradeJournalNameExpected, stat, nil
	}
	return upgradeJournalNameOther, stat, nil
}

func validUpgradeJournalPinnedMetadata(stat unix.Stat_t, nlink uint64, size int64) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0o7777 == 0o400 &&
		stat.Uid == uint32(os.Geteuid()) && uint64(stat.Nlink) == nlink && stat.Size == size
}

func validatePinnedUpgradeJournal(file *os.File, expected unix.Stat_t, body []byte, nlink uint64) error {
	var before unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return err
	}
	if !sameUpgradeJournalInode(expected, before) || !validUpgradeJournalPinnedMetadata(before, nlink, int64(len(body))) {
		return errors.New("unsafe pinned upgrade journal metadata")
	}
	got := make([]byte, len(body)+1)
	n, readErr := file.ReadAt(got, 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if n != len(body) || !bytes.Equal(got[:n], body) {
		return errors.New("pinned upgrade journal content changed")
	}
	if _, err := decodeUpgradeTransactionJournal(got[:n]); err != nil {
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil || !sameUpgradeJournalFile(before, after) ||
		!validUpgradeJournalPinnedMetadata(after, nlink, int64(len(body))) {
		return errors.New("pinned upgrade journal changed while inspecting")
	}
	return nil
}

func validatePublishedUpgradeJournal(parent, file *os.File, expected unix.Stat_t, body []byte, ops upgradeJournalCreateOps) error {
	state, visible, err := inspectUpgradeJournalName(parent, upgradeJournalName, expected, ops)
	if err != nil || state != upgradeJournalNameExpected || !validUpgradeJournalPinnedMetadata(visible, 1, int64(len(body))) {
		return errors.New("upgrade journal publication placement is uncertain")
	}
	if err := validatePinnedUpgradeJournal(file, expected, body, 1); err != nil {
		return err
	}
	var pinned unix.Stat_t
	if unix.Fstat(int(file.Fd()), &pinned) != nil || !sameUpgradeJournalFile(visible, pinned) {
		return errors.New("visible upgrade journal differs from pinned inode")
	}
	return nil
}

func validateUpgradeJournalParentIdentityAtPath(o Options, pinned *os.File, expected unix.Stat_t) error {
	reopened, err := openUpgradeJournalParent(o)
	if err != nil {
		return err
	}
	defer reopened.Close()
	return validateUpgradeJournalParentIdentity(reopened, pinned, expected)
}

func validateUpgradeJournalParentPath(o Options, parent, file *os.File, expectedParent, expectedFile unix.Stat_t, body []byte, ops upgradeJournalCreateOps) error {
	reopened, err := openUpgradeJournalParent(o)
	if err != nil {
		return err
	}
	defer reopened.Close()
	if err := validateUpgradeJournalParentIdentity(reopened, parent, expectedParent); err != nil {
		return err
	}
	fd, err := unix.Openat(int(reopened.Fd()), upgradeJournalName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("rooted upgrade journal no longer names published file")
	}
	visible := os.NewFile(uintptr(fd), upgradeJournalName)
	defer visible.Close()
	if err := validatePinnedUpgradeJournal(visible, expectedFile, body, 1); err != nil {
		return errors.New("rooted upgrade journal no longer names valid published file")
	}
	var reopenedStat, pinnedStat unix.Stat_t
	if unix.Fstat(int(visible.Fd()), &reopenedStat) != nil || unix.Fstat(int(file.Fd()), &pinnedStat) != nil ||
		!sameUpgradeJournalFile(reopenedStat, pinnedStat) {
		return errors.New("rooted upgrade journal differs from pinned inode")
	}
	state, _, err := inspectUpgradeJournalName(reopened, upgradeJournalName, expectedFile, ops)
	if err != nil || state != upgradeJournalNameExpected {
		return errors.New("rooted upgrade journal path changed while inspecting")
	}
	return nil
}

func validateUpgradeJournalParentIdentity(reopened, pinned *os.File, expected unix.Stat_t) error {
	var current, pinnedNow unix.Stat_t
	if unix.Fstat(int(reopened.Fd()), &current) != nil || unix.Fstat(int(pinned.Fd()), &pinnedNow) != nil ||
		!sameUpgradeJournalInode(expected, current) || !sameUpgradeJournalInode(expected, pinnedNow) {
		return errors.New("rooted upgrade journal parent no longer names pinned directory")
	}
	return nil
}

func classifyUpgradeJournalPublishError(o Options, parent, file *os.File, expectedParent, expectedFile unix.Stat_t, body []byte, ops upgradeJournalCreateOps, publishErr error) (upgradeJournalCreateOutcome, error) {
	state, _, inspectErr := inspectUpgradeJournalName(parent, upgradeJournalName, expectedFile, ops)
	var pinned unix.Stat_t
	pinnedErr := unix.Fstat(int(file.Fd()), &pinned)
	if inspectErr == nil && state == upgradeJournalNameExpected {
		validationErr := validatePublishedUpgradeJournal(parent, file, expectedFile, body, ops)
		pathErr := validateUpgradeJournalParentPath(o, parent, file, expectedParent, expectedFile, body, ops)
		if validationErr != nil || pathErr != nil {
			return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("publish upgrade journal reported an error after effect: %v; validation=%v; path=%v", publishErr, validationErr, pathErr), true)
		}
		return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("publish upgrade journal reported an error after effect: %w", publishErr), true)
	}
	residue := state != upgradeJournalNameAbsent || pinnedErr != nil || pinned.Nlink != 0 || inspectErr != nil
	return upgradeJournalIndeterminate, newUpgradeJournalCreateError(fmt.Errorf("publish upgrade journal placement uncertain: %v (state=%d inspect=%v pinned=%v)", publishErr, state, inspectErr, pinnedErr), residue)
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
