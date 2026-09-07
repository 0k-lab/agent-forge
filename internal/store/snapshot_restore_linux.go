//go:build linux

package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type preMigrationRestoreOps struct {
	copy         func(io.Writer, io.Reader) (int64, error)
	sync         func(*os.File) error
	exchange     func(*os.File, string, string) error
	unlink       func(*os.File, string) error
	validateLive func(string, PreMigrationSnapshotIdentity) error
}

func defaultPreMigrationRestoreOps() preMigrationRestoreOps {
	return preMigrationRestoreOps{
		copy: io.Copy,
		sync: func(file *os.File) error { return file.Sync() },
		exchange: func(directory *os.File, first, second string) error {
			return unix.Renameat2(int(directory.Fd()), first, int(directory.Fd()), second, unix.RENAME_EXCHANGE)
		},
		unlink: func(directory *os.File, name string) error {
			return unix.Unlinkat(int(directory.Fd()), name, 0)
		},
		validateLive: ValidatePreMigrationSnapshot,
	}
}

func RestorePreMigrationSnapshot(databasePath, artifactPath string, expected PreMigrationSnapshotIdentity) (PreMigrationRestoreResult, error) {
	return restorePreMigrationSnapshot(databasePath, artifactPath, expected, defaultPreMigrationRestoreOps())
}

type restorePlacement uint8

const (
	restorePlacementUnknown restorePlacement = iota
	restorePlacementOriginal
	restorePlacementPublished
)

func restorePreMigrationSnapshot(databasePath, artifactPath string, expected PreMigrationSnapshotIdentity, ops preMigrationRestoreOps) (PreMigrationRestoreResult, error) {
	notApplied := PreMigrationRestoreResult{State: NotAppliedDurable}
	if validateSnapshotPath(databasePath) != nil || validateSnapshotPath(artifactPath) != nil || databasePath == artifactPath {
		return notApplied, ErrInvalidDatabaseLocation
	}
	if destinationInfo, statErr := os.Lstat(databasePath); statErr != nil || validateSnapshotFileInfo(destinationInfo) != nil {
		return notApplied, ErrInsecureDatabase
	}
	lockCloser, err := acquireSQLiteLock(databasePath)
	if err != nil {
		return notApplied, err
	}
	defer lockCloser.Close()
	lock, ok := lockCloser.(*os.File)
	if !ok {
		return notApplied, ErrInsecureDatabase
	}

	artifact, err := openSecureSnapshotFile(artifactPath)
	if err != nil {
		return notApplied, err
	}
	defer artifact.Close()
	if rejectSnapshotSidecars(artifactPath) != nil || rejectSnapshotSidecars(databasePath) != nil {
		return notApplied, ErrInsecureDatabase
	}
	actual, err := inspectOpenedPreMigrationSnapshot(artifact)
	if err != nil {
		return notApplied, err
	}
	if actual != expected {
		return notApplied, errInvalidPreMigrationSnapshot
	}
	if err = revalidateOpenedSnapshotPath(artifactPath, artifact); err != nil {
		return notApplied, err
	}

	destination, err := openSecureSnapshotFile(databasePath)
	if err != nil {
		return notApplied, err
	}
	defer destination.Close()
	artifactInfo, artifactStatErr := artifact.Stat()
	destinationInfo, destinationStatErr := destination.Stat()
	if artifactStatErr != nil || destinationStatErr != nil || os.SameFile(artifactInfo, destinationInfo) {
		return notApplied, ErrInvalidDatabaseLocation
	}
	if err = ops.sync(destination); err != nil {
		return notApplied, err
	}
	directory, err := openSecureSnapshotDirectory(filepath.Dir(databasePath))
	if err != nil {
		return notApplied, err
	}
	defer directory.Close()

	stageName, staged, err := createSnapshotTemporary(directory, filepath.Base(databasePath)+".restore")
	if err != nil {
		return notApplied, err
	}
	defer staged.Close()
	stagePath := filepath.Join(filepath.Dir(databasePath), stageName)

	cleanupExpected := func(cause error, expectedFile *os.File) (bool, error) {
		residue, unlinkErr := unlinkExpectedRestoreName(directory, stagePath, stageName, expectedFile, ops)
		if residue {
			return true, errors.Join(cause, unlinkErr)
		}
		syncErr := ops.sync(directory)
		return false, errors.Join(cause, unlinkErr, syncErr)
	}
	failBeforePublish := func(cause error) (PreMigrationRestoreResult, error) {
		residue, cleanupErr := cleanupExpected(cause, staged)
		return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: residue}, cleanupErr
	}
	if _, err = artifact.Seek(0, io.SeekStart); err != nil {
		return failBeforePublish(err)
	}
	if _, err = ops.copy(staged, artifact); err != nil {
		return failBeforePublish(err)
	}
	if err = ops.sync(staged); err != nil {
		return failBeforePublish(err)
	}
	stagedIdentity, err := inspectOpenedPreMigrationSnapshot(staged)
	if err != nil || stagedIdentity != expected {
		if err == nil {
			err = errInvalidPreMigrationSnapshot
		}
		return failBeforePublish(err)
	}

	if revalidateOpenedSnapshotPath(artifactPath, artifact) != nil ||
		revalidateOpenedSnapshotPath(databasePath, destination) != nil ||
		revalidateOpenedSnapshotPath(stagePath, staged) != nil ||
		revalidateOpenedSQLitePath(databasePath+".lock", lock) != nil ||
		rejectSnapshotSidecars(artifactPath) != nil || rejectSnapshotSidecars(databasePath) != nil || rejectSnapshotSidecars(stagePath) != nil {
		return failBeforePublish(ErrInsecureDatabase)
	}

	classify := func() (restorePlacement, bool) {
		return classifyRestorePlacement(databasePath, stagePath, destination, staged)
	}
	compensate := func(cause error) (PreMigrationRestoreResult, error) {
		reverseErr := ops.exchange(directory, stageName, filepath.Base(databasePath))
		placement, residue := classify()
		if placement != restorePlacementOriginal {
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: residue || placement == restorePlacementUnknown}, errors.Join(cause, reverseErr, ErrInsecureDatabase)
		}
		if revalidateOpenedSnapshotPath(databasePath, destination) != nil ||
			revalidateOpenedSnapshotPath(stagePath, staged) != nil ||
			revalidateOpenedSQLitePath(databasePath+".lock", lock) != nil ||
			rejectSnapshotSidecars(databasePath) != nil || rejectSnapshotSidecars(stagePath) != nil {
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: true}, errors.Join(cause, reverseErr, ErrInsecureDatabase)
		}
		if syncErr := ops.sync(directory); syncErr != nil {
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: true}, errors.Join(cause, reverseErr, syncErr)
		}
		if revalidateOpenedSnapshotPath(databasePath, destination) != nil || revalidateOpenedSQLitePath(databasePath+".lock", lock) != nil {
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: true}, errors.Join(cause, reverseErr, ErrInsecureDatabase)
		}
		residue, cleanupErr := cleanupExpected(nil, staged)
		return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: residue}, errors.Join(cause, reverseErr, cleanupErr)
	}

	if err = ops.exchange(directory, stageName, filepath.Base(databasePath)); err != nil {
		placement, residue := classify()
		switch placement {
		case restorePlacementOriginal:
			if syncErr := ops.sync(directory); syncErr != nil {
				return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: true}, errors.Join(err, syncErr)
			}
			cleanupResidue, cleanupErr := cleanupExpected(nil, staged)
			return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: cleanupResidue}, errors.Join(err, cleanupErr)
		case restorePlacementPublished:
			return compensate(err)
		default:
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: residue || placement == restorePlacementUnknown}, errors.Join(err, ErrInsecureDatabase)
		}
	}
	if placement, residue := classify(); placement != restorePlacementPublished {
		return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: residue || placement == restorePlacementUnknown}, ErrInsecureDatabase
	}

	validatePinnedCommitState := func() error {
		if revalidateOpenedSnapshotPath(databasePath, staged) != nil ||
			revalidateOpenedSnapshotPath(stagePath, destination) != nil ||
			revalidateOpenedSnapshotPath(artifactPath, artifact) != nil ||
			revalidateOpenedSQLitePath(databasePath+".lock", lock) != nil ||
			rejectSnapshotSidecars(databasePath) != nil || rejectSnapshotSidecars(stagePath) != nil || rejectSnapshotSidecars(artifactPath) != nil {
			return ErrInsecureDatabase
		}
		artifactIdentity, artifactErr := inspectOpenedPreMigrationSnapshot(artifact)
		stagedIdentity, stagedErr := inspectOpenedPreMigrationSnapshot(staged)
		if artifactErr != nil || stagedErr != nil || artifactIdentity != expected || stagedIdentity != expected {
			return errors.Join(errInvalidPreMigrationSnapshot, artifactErr, stagedErr)
		}
		if revalidateOpenedSnapshotPath(databasePath, staged) != nil ||
			revalidateOpenedSnapshotPath(stagePath, destination) != nil ||
			revalidateOpenedSnapshotPath(artifactPath, artifact) != nil ||
			revalidateOpenedSQLitePath(databasePath+".lock", lock) != nil {
			return ErrInsecureDatabase
		}
		return nil
	}
	if err = ops.validateLive(databasePath, expected); err != nil {
		return compensate(err)
	}
	if err = validatePinnedCommitState(); err != nil {
		return compensate(err)
	}
	if err = ops.sync(directory); err != nil {
		return compensate(err)
	}
	if err = validatePinnedCommitState(); err != nil {
		return compensate(err)
	}

	committedResult := func(residue bool, cause error) (PreMigrationRestoreResult, error) {
		if revalidateOpenedSnapshotPath(databasePath, staged) != nil || revalidateOpenedSQLitePath(databasePath+".lock", lock) != nil {
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: residue || restoreNameResidue(stagePath)}, errors.Join(cause, ErrInsecureDatabase)
		}
		return PreMigrationRestoreResult{State: AppliedDurable, RecoveryResidue: residue}, cause
	}
	residue, unlinkErr := unlinkExpectedRestoreName(directory, stagePath, stageName, destination, ops)
	if residue {
		return committedResult(true, unlinkErr)
	}
	if syncErr := ops.sync(directory); syncErr != nil {
		preserveErr := preserveOldGeneration(directory, stageName, destination, ops)
		preserved := restoreNameResidue(stagePath)
		return committedResult(preserved, errors.Join(unlinkErr, syncErr, preserveErr))
	}
	return committedResult(false, unlinkErr)
}

func revalidateOpenedSQLitePath(path string, file *os.File) error {
	opened, statErr := file.Stat()
	current, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || validateSQLiteFileInfo(opened) != nil || validateSQLiteFileInfo(current) != nil || !os.SameFile(opened, current) {
		return ErrInsecureDatabase
	}
	return nil
}

func classifyRestorePlacement(databasePath, stagePath string, destination, staged *os.File) (restorePlacement, bool) {
	databaseInfo, databaseErr := os.Lstat(databasePath)
	stageInfo, stageErr := os.Lstat(stagePath)
	residue := stageErr == nil || !errors.Is(stageErr, os.ErrNotExist)
	if databaseErr != nil || stageErr != nil || validateSnapshotFileInfo(databaseInfo) != nil || validateSnapshotFileInfo(stageInfo) != nil {
		return restorePlacementUnknown, residue
	}
	destinationInfo, destinationErr := destination.Stat()
	stagedInfo, stagedErr := staged.Stat()
	if destinationErr != nil || stagedErr != nil {
		return restorePlacementUnknown, true
	}
	switch {
	case os.SameFile(databaseInfo, destinationInfo) && os.SameFile(stageInfo, stagedInfo):
		return restorePlacementOriginal, true
	case os.SameFile(databaseInfo, stagedInfo) && os.SameFile(stageInfo, destinationInfo):
		return restorePlacementPublished, true
	default:
		return restorePlacementUnknown, true
	}
}

func unlinkExpectedRestoreName(directory *os.File, path, name string, expected *os.File, ops preMigrationRestoreOps) (bool, error) {
	if err := revalidateOpenedSnapshotPath(path, expected); err != nil {
		return restoreNameResidue(path), err
	}
	unlinkErr := ops.unlink(directory, name)
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return false, unlinkErr
	}
	if statErr != nil || validateSnapshotFileInfo(info) != nil {
		return true, errors.Join(unlinkErr, ErrInsecureDatabase)
	}
	expectedInfo, expectedErr := expected.Stat()
	if expectedErr != nil || !os.SameFile(info, expectedInfo) {
		return true, errors.Join(unlinkErr, ErrInsecureDatabase)
	}
	if unlinkErr == nil {
		unlinkErr = ErrInsecureDatabase
	}
	return true, unlinkErr
}

func restoreNameResidue(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func preserveOldGeneration(directory *os.File, name string, original *os.File, ops preMigrationRestoreOps) error {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	recovery := os.NewFile(uintptr(fd), name)
	if _, err = original.Seek(0, io.SeekStart); err == nil {
		_, err = ops.copy(recovery, original)
	}
	if err == nil {
		err = ops.sync(recovery)
	}
	err = errors.Join(err, recovery.Close())
	if err == nil {
		err = ops.sync(directory)
	}
	return err
}
