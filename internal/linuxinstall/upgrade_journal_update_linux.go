//go:build linux

package linuxinstall

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type upgradeJournalUpdateOps struct {
	openParent  func(Options) (*os.File, error)
	openCurrent func(*os.File, string) (*os.File, error)
	openUnnamed func(*os.File) (*os.File, error)
	inspect     func(*os.File, string, *unix.Stat_t) error
	publish     func(*os.File, *os.File, string) error
	exchange    func(*os.File, string, string) error
	syncParent  func(*os.File) error
	syncFile    func(*os.File) error
	chmod       func(*os.File, os.FileMode) error
}

func defaultUpgradeJournalUpdateOps() upgradeJournalUpdateOps {
	return upgradeJournalUpdateOps{
		openParent: openUpgradeJournalParent,
		openCurrent: func(parent *os.File, name string) (*os.File, error) {
			fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return nil, err
			}
			return os.NewFile(uintptr(fd), name), nil
		},
		openUnnamed: func(parent *os.File) (*os.File, error) {
			fd, err := unix.Openat(int(parent.Fd()), ".", unix.O_RDWR|unix.O_CLOEXEC|unix.O_TMPFILE, 0o600)
			if err != nil {
				return nil, err
			}
			return os.NewFile(uintptr(fd), "unnamed upgrade journal update"), nil
		},
		inspect: func(parent *os.File, name string, stat *unix.Stat_t) error {
			return unix.Fstatat(int(parent.Fd()), name, stat, unix.AT_SYMLINK_NOFOLLOW)
		},
		publish: func(parent, source *os.File, name string) error {
			return unix.Linkat(int(source.Fd()), "", int(parent.Fd()), name, unix.AT_EMPTY_PATH)
		},
		exchange: func(parent *os.File, a, b string) error {
			return unix.Renameat2(int(parent.Fd()), a, int(parent.Fd()), b, unix.RENAME_EXCHANGE)
		},
		syncParent: func(file *os.File) error { return file.Sync() },
		syncFile:   func(file *os.File) error { return file.Sync() },
		chmod:      func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) },
	}
}

func compareAndSwapUpgradeTransactionJournal(o Options, expected, next upgradeTransactionJournal) (upgradeJournalUpdateOutcome, error) {
	return compareAndSwapUpgradeTransactionJournalWithOps(o, expected, next, defaultUpgradeJournalUpdateOps())
}

func compareAndSwapUpgradeTransactionJournalWithOps(o Options, expected, next upgradeTransactionJournal, ops upgradeJournalUpdateOps) (upgradeJournalUpdateOutcome, error) {
	expectedBody, nextBody, err := validateUpgradeJournalUpdate(expected, next)
	if err != nil {
		return updateNotApplied(err, false)
	}
	ops, err = completeUpgradeJournalUpdateOps(ops)
	if err != nil {
		return updateNotApplied(err, false)
	}
	witnessName, stageName := upgradeJournalUpdateNames(expected, next)
	parent, err := ops.openParent(o)
	if err != nil {
		return updateNotApplied(err, false)
	}
	defer parent.Close()
	var parentID unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &parentID); err != nil {
		return updateNotApplied(fmt.Errorf("inspect upgrade journal parent: %w", err), false)
	}

	current, err := ops.openCurrent(parent, upgradeJournalName)
	if err != nil || current == nil {
		return updateNotApplied(fmt.Errorf("open current upgrade journal: %w", err), false)
	}
	defer current.Close()
	currentKind, currentID, err := identifyUpgradeJournalFile(current, expectedBody, nextBody)
	if err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, unix.Stat_t{}, witnessName, stageName, ops, fmt.Errorf("validate current upgrade journal: %w", err))
	}

	witnessState, witnessID, err := inspectUpgradeJournalUpdateName(parent, witnessName, ops)
	if err != nil {
		return updateIndeterminate(fmt.Errorf("inspect predecessor witness: %w", err), true)
	}
	stageState, stageID, err := inspectUpgradeJournalUpdateName(parent, stageName, ops)
	if err != nil {
		return updateIndeterminate(fmt.Errorf("inspect staged journal: %w", err), true)
	}

	if witnessState != upgradeJournalNameAbsent || stageState != upgradeJournalNameAbsent {
		return recoverUpgradeJournalUpdate(o, parent, parentID, current, currentKind, currentID, expectedBody, nextBody, witnessName, witnessState, witnessID, stageName, stageState, stageID, ops)
	}
	if currentKind != updateFileExpected || !validUpgradeJournalPinnedMetadata(currentID, 1, int64(len(expectedBody))) {
		return updateNotApplied(errors.New("current upgrade journal does not exactly match expected"), false)
	}
	if err := validateRootedUpdateParent(o, parent, parentID, ops); err != nil {
		return updateIndeterminate(fmt.Errorf("upgrade journal parent changed before witness: %w", err), true)
	}
	if err := validateUpdateNamedFile(parent, upgradeJournalName, current, currentID, expectedBody, 1, ops); err != nil {
		return updateIndeterminate(fmt.Errorf("current upgrade journal changed before witness: %w", err), true)
	}

	if err := ops.publish(parent, current, witnessName); err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, fmt.Errorf("publish predecessor witness: %w", err))
	}
	if err := unix.Fstat(int(current.Fd()), &currentID); err != nil {
		return updateIndeterminate(fmt.Errorf("refresh witnessed upgrade journal: %w", err), true)
	}
	if err := validateExpectedPreExchange(o, parent, parentID, current, currentID, expectedBody, witnessName, stageName, nil, unix.Stat_t{}, nil, ops); err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, err)
	}
	if err := ops.syncParent(parent); err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, fmt.Errorf("sync predecessor witness: %w", err))
	}
	if err := validateExpectedPreExchange(o, parent, parentID, current, currentID, expectedBody, witnessName, stageName, nil, unix.Stat_t{}, nil, ops); err != nil {
		return updateIndeterminate(err, true)
	}

	unnamed, err := ops.openUnnamed(parent)
	if err != nil || unnamed == nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, fmt.Errorf("create unnamed next upgrade journal: %w", err))
	}
	defer unnamed.Close()
	if _, err := unnamed.Write(nextBody); err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, fmt.Errorf("write unnamed next upgrade journal: %w", err))
	}
	if err := ops.chmod(unnamed, 0o400); err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, fmt.Errorf("chmod unnamed next upgrade journal: %w", err))
	}
	if err := ops.syncFile(unnamed); err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, fmt.Errorf("sync unnamed next upgrade journal: %w", err))
	}
	var nextID unix.Stat_t
	if err := unix.Fstat(int(unnamed.Fd()), &nextID); err != nil || validatePinnedUpgradeJournal(unnamed, nextID, nextBody, 0) != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, errors.New("validate unnamed next upgrade journal"))
	}
	if err := validateExpectedPreExchange(o, parent, parentID, current, currentID, expectedBody, witnessName, stageName, nil, unix.Stat_t{}, nil, ops); err != nil {
		return updateIndeterminate(err, true)
	}
	if err := ops.publish(parent, unnamed, stageName); err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, fmt.Errorf("publish staged upgrade journal: %w", err))
	}
	if err := unix.Fstat(int(unnamed.Fd()), &nextID); err != nil {
		return updateIndeterminate(fmt.Errorf("refresh staged upgrade journal: %w", err), true)
	}
	if err := validateExpectedPreExchange(o, parent, parentID, current, currentID, expectedBody, witnessName, stageName, unnamed, nextID, nextBody, ops); err != nil {
		return updateIndeterminate(err, true)
	}
	if err := ops.syncParent(parent); err != nil {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, fmt.Errorf("sync staged upgrade journal: %w", err))
	}
	if err := validateExpectedPreExchange(o, parent, parentID, current, currentID, expectedBody, witnessName, stageName, unnamed, nextID, nextBody, ops); err != nil {
		return updateIndeterminate(err, true)
	}
	return exchangeUpgradeJournalUpdate(o, parent, parentID, current, currentID, unnamed, nextID, expectedBody, nextBody, witnessName, stageName, ops)
}

type updateFileKind uint8

const (
	updateFileOther updateFileKind = iota
	updateFileExpected
	updateFileNext
)

func validateUpgradeJournalUpdate(expected, next upgradeTransactionJournal) ([]byte, []byte, error) {
	expectedBody, err := encodeUpgradeTransactionJournal(expected)
	if err != nil {
		return nil, nil, err
	}
	nextBody, err := encodeUpgradeTransactionJournal(next)
	if err != nil {
		return nil, nil, err
	}
	if expected.TransactionID != next.TransactionID {
		return nil, nil, errors.New("upgrade journal transaction changed")
	}
	a, b := expected, next
	a.Phase, b.Phase = "", ""
	a.RestoreOutcome, b.RestoreOutcome = "", ""
	a.RestoreResidue, b.RestoreResidue = nil, nil
	if a != b {
		return nil, nil, errors.New("immutable upgrade journal field changed")
	}
	if err := validateUpgradeJournalTransition(expected.Phase, next.Phase); err != nil {
		return nil, nil, err
	}
	return expectedBody, nextBody, nil
}

func upgradeJournalUpdateNames(expected, next upgradeTransactionJournal) (string, string) {
	base := ".upgrade-transaction-" + expected.TransactionID + "-" + string(expected.Phase) + "-to-" + string(next.Phase)
	return base + ".predecessor", base + ".stage"
}

func completeUpgradeJournalUpdateOps(ops upgradeJournalUpdateOps) (upgradeJournalUpdateOps, error) {
	d := defaultUpgradeJournalUpdateOps()
	if ops.openParent == nil {
		ops.openParent = d.openParent
	}
	if ops.openCurrent == nil {
		ops.openCurrent = d.openCurrent
	}
	if ops.inspect == nil {
		ops.inspect = d.inspect
	}
	if ops.openUnnamed == nil {
		ops.openUnnamed = d.openUnnamed
	}
	if ops.publish == nil {
		ops.publish = d.publish
	}
	if ops.exchange == nil {
		ops.exchange = d.exchange
	}
	if ops.syncParent == nil {
		ops.syncParent = d.syncParent
	}
	if ops.syncFile == nil {
		ops.syncFile = d.syncFile
	}
	if ops.chmod == nil {
		ops.chmod = d.chmod
	}
	return ops, nil
}

func identifyUpgradeJournalFile(file *os.File, expectedBody, nextBody []byte) (updateFileKind, unix.Stat_t, error) {
	var before, after unix.Stat_t
	if unix.Fstat(int(file.Fd()), &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Mode&0o7777 != 0o400 || before.Uid != uint32(os.Geteuid()) || before.Size > upgradeJournalMaxBytes {
		return updateFileOther, before, errors.New("unsafe upgrade journal metadata")
	}
	body, err := io.ReadAll(io.LimitReader(file, upgradeJournalMaxBytes+1))
	if err != nil || len(body) > upgradeJournalMaxBytes {
		return updateFileOther, before, errors.New("read upgrade journal")
	}
	if unix.Fstat(int(file.Fd()), &after) != nil || !sameUpgradeJournalFile(before, after) {
		return updateFileOther, before, errors.New("upgrade journal changed while reading")
	}
	if _, err := decodeUpgradeTransactionJournal(body); err != nil {
		return updateFileOther, before, err
	}
	if bytes.Equal(body, expectedBody) {
		return updateFileExpected, after, nil
	}
	if bytes.Equal(body, nextBody) {
		return updateFileNext, after, nil
	}
	return updateFileOther, after, nil
}

func inspectUpgradeJournalUpdateName(parent *os.File, name string, ops upgradeJournalUpdateOps) (upgradeJournalNameState, unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := ops.inspect(parent, name, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return upgradeJournalNameAbsent, stat, nil
		}
		return upgradeJournalNameUncertain, stat, err
	}
	return upgradeJournalNameOther, stat, nil
}

func validateRootedUpdateParent(o Options, parent *os.File, parentID unix.Stat_t, ops upgradeJournalUpdateOps) error {
	reopened, err := ops.openParent(o)
	if err != nil {
		return err
	}
	defer reopened.Close()
	return validateUpgradeJournalParentIdentity(reopened, parent, parentID)
}

func validateUpdateNamedFile(parent *os.File, name string, pinned *os.File, id unix.Stat_t, body []byte, nlink uint64, ops upgradeJournalUpdateOps) error {
	var named, pinnedNow unix.Stat_t
	if err := ops.inspect(parent, name, &named); err != nil || !sameUpgradeJournalFile(named, id) || !validUpgradeJournalPinnedMetadata(named, nlink, int64(len(body))) {
		return errors.New("upgrade journal name does not identify pinned file")
	}
	if err := validatePinnedUpgradeJournal(pinned, id, body, nlink); err != nil {
		return err
	}
	if unix.Fstat(int(pinned.Fd()), &pinnedNow) != nil || !sameUpgradeJournalFile(named, pinnedNow) {
		return errors.New("named and pinned upgrade journals differ")
	}
	return nil
}

func validateExpectedPreExchange(o Options, parent *os.File, parentID unix.Stat_t, current *os.File, currentID unix.Stat_t, expectedBody []byte, witnessName, stageName string, next *os.File, nextID unix.Stat_t, nextBody []byte, ops upgradeJournalUpdateOps) error {
	if err := validateRootedUpdateParent(o, parent, parentID, ops); err != nil {
		return err
	}
	if err := validateUpdateNamedFile(parent, upgradeJournalName, current, currentID, expectedBody, 2, ops); err != nil {
		return err
	}
	if err := validateUpdateNamedFile(parent, witnessName, current, currentID, expectedBody, 2, ops); err != nil {
		return err
	}
	if next == nil {
		state, _, err := inspectUpgradeJournalUpdateName(parent, stageName, ops)
		if err != nil || state != upgradeJournalNameAbsent {
			return errors.New("unexpected staged upgrade journal")
		}
		return nil
	}
	return validateUpdateNamedFile(parent, stageName, next, nextID, nextBody, 1, ops)
}

func exchangeUpgradeJournalUpdate(o Options, parent *os.File, parentID unix.Stat_t, expectedFile *os.File, expectedID unix.Stat_t, nextFile *os.File, nextID unix.Stat_t, expectedBody, nextBody []byte, witnessName, stageName string, ops upgradeJournalUpdateOps) (upgradeJournalUpdateOutcome, error) {
	if err := ops.exchange(parent, stageName, upgradeJournalName); err != nil {
		return updateIndeterminate(fmt.Errorf("exchange staged upgrade journal: %w", err), true)
	}
	if err := unix.Fstat(int(expectedFile.Fd()), &expectedID); err != nil {
		return updateIndeterminate(fmt.Errorf("refresh exchanged predecessor: %w", err), true)
	}
	if err := unix.Fstat(int(nextFile.Fd()), &nextID); err != nil {
		return updateIndeterminate(fmt.Errorf("refresh exchanged journal: %w", err), true)
	}
	if err := validateAppliedUpdate(o, parent, parentID, expectedFile, expectedID, nextFile, nextID, expectedBody, nextBody, witnessName, stageName, ops); err != nil {
		return updateIndeterminate(fmt.Errorf("validate exchanged upgrade journal: %w", err), true)
	}
	if err := ops.syncParent(parent); err != nil {
		return updateIndeterminate(fmt.Errorf("sync exchanged upgrade journal: %w", err), true)
	}
	if err := validateAppliedUpdate(o, parent, parentID, expectedFile, expectedID, nextFile, nextID, expectedBody, nextBody, witnessName, stageName, ops); err != nil {
		return updateIndeterminate(fmt.Errorf("revalidate durable upgrade journal: %w", err), true)
	}
	return upgradeJournalUpdateAppliedDurable, nil
}

func validateAppliedUpdate(o Options, parent *os.File, parentID unix.Stat_t, expectedFile *os.File, expectedID unix.Stat_t, nextFile *os.File, nextID unix.Stat_t, expectedBody, nextBody []byte, witnessName, stageName string, ops upgradeJournalUpdateOps) error {
	if err := validateRootedUpdateParent(o, parent, parentID, ops); err != nil {
		return err
	}
	if err := validateUpdateNamedFile(parent, upgradeJournalName, nextFile, nextID, nextBody, 1, ops); err != nil {
		return err
	}
	if err := validateUpdateNamedFile(parent, witnessName, expectedFile, expectedID, expectedBody, 2, ops); err != nil {
		return err
	}
	return validateUpdateNamedFile(parent, stageName, expectedFile, expectedID, expectedBody, 2, ops)
}

func recoverUpgradeJournalUpdate(o Options, parent *os.File, parentID unix.Stat_t, current *os.File, currentKind updateFileKind, currentID unix.Stat_t, expectedBody, nextBody []byte, witnessName string, witnessState upgradeJournalNameState, witnessID unix.Stat_t, stageName string, stageState upgradeJournalNameState, stageID unix.Stat_t, ops upgradeJournalUpdateOps) (upgradeJournalUpdateOutcome, error) {
	if witnessState == upgradeJournalNameAbsent || stageState == upgradeJournalNameAbsent {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, errors.New("incomplete upgrade journal recovery layout"))
	}
	witness, err := ops.openCurrent(parent, witnessName)
	if err != nil || witness == nil {
		return updateIndeterminate(errors.New("open predecessor witness"), true)
	}
	defer witness.Close()
	witnessKind, witnessPinnedID, err := identifyUpgradeJournalFile(witness, expectedBody, nextBody)
	if err != nil || witnessKind != updateFileExpected || !sameUpgradeJournalInode(witnessID, witnessPinnedID) {
		return classifyUpdateBeforeExchange(o, parent, parentID, expectedBody, currentID, witnessName, stageName, ops, errors.New("mismatched predecessor witness"))
	}
	stage, err := ops.openCurrent(parent, stageName)
	if err != nil || stage == nil {
		return updateIndeterminate(errors.New("open staged upgrade journal"), true)
	}
	defer stage.Close()
	stageKind, stagePinnedID, err := identifyUpgradeJournalFile(stage, expectedBody, nextBody)
	if err != nil || !sameUpgradeJournalInode(stageID, stagePinnedID) {
		return updateIndeterminate(errors.New("mismatched staged upgrade journal"), true)
	}
	if currentKind == updateFileExpected && stageKind == updateFileNext && sameUpgradeJournalInode(currentID, witnessPinnedID) && validUpgradeJournalPinnedMetadata(currentID, 2, int64(len(expectedBody))) && validUpgradeJournalPinnedMetadata(stagePinnedID, 1, int64(len(nextBody))) {
		if err := validateExpectedPreExchange(o, parent, parentID, current, currentID, expectedBody, witnessName, stageName, stage, stagePinnedID, nextBody, ops); err != nil {
			return updateIndeterminate(err, true)
		}
		return exchangeUpgradeJournalUpdate(o, parent, parentID, current, currentID, stage, stagePinnedID, expectedBody, nextBody, witnessName, stageName, ops)
	}
	if currentKind == updateFileNext && stageKind == updateFileExpected && sameUpgradeJournalInode(stagePinnedID, witnessPinnedID) && validUpgradeJournalPinnedMetadata(currentID, 1, int64(len(nextBody))) && validUpgradeJournalPinnedMetadata(stagePinnedID, 2, int64(len(expectedBody))) {
		if err := validateAppliedUpdate(o, parent, parentID, stage, stagePinnedID, current, currentID, expectedBody, nextBody, witnessName, stageName, ops); err != nil {
			return updateIndeterminate(err, true)
		}
		if err := ops.syncParent(parent); err != nil {
			return updateIndeterminate(err, true)
		}
		if err := validateAppliedUpdate(o, parent, parentID, stage, stagePinnedID, current, currentID, expectedBody, nextBody, witnessName, stageName, ops); err != nil {
			return updateIndeterminate(err, true)
		}
		return upgradeJournalUpdateAppliedDurable, nil
	}
	return updateIndeterminate(errors.New("unsupported upgrade journal recovery layout"), true)
}

func classifyUpdateBeforeExchange(o Options, parent *os.File, parentID unix.Stat_t, expectedBody []byte, expectedID unix.Stat_t, witnessName, stageName string, ops upgradeJournalUpdateOps, cause error) (upgradeJournalUpdateOutcome, error) {
	if validateRootedUpdateParent(o, parent, parentID, ops) != nil {
		return updateIndeterminate(cause, true)
	}
	current, err := ops.openCurrent(parent, upgradeJournalName)
	if err != nil || current == nil {
		return updateIndeterminate(cause, true)
	}
	defer current.Close()
	kind, id, err := identifyUpgradeJournalFile(current, expectedBody, nil)
	if err != nil || kind != updateFileExpected || (expectedID.Ino != 0 && !sameUpgradeJournalInode(expectedID, id)) {
		return updateIndeterminate(cause, true)
	}
	var residue bool
	for _, name := range []string{witnessName, stageName} {
		state, _, inspectErr := inspectUpgradeJournalUpdateName(parent, name, ops)
		if inspectErr != nil {
			return updateIndeterminate(cause, true)
		}
		residue = residue || state != upgradeJournalNameAbsent
	}
	return updateNotApplied(cause, residue)
}

func updateNotApplied(err error, residue bool) (upgradeJournalUpdateOutcome, error) {
	return upgradeJournalUpdateNotAppliedDurable, newUpgradeJournalUpdateError(err, residue)
}

func updateIndeterminate(err error, residue bool) (upgradeJournalUpdateOutcome, error) {
	return upgradeJournalUpdateIndeterminate, newUpgradeJournalUpdateError(err, residue)
}
