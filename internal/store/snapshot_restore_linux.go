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

func restorePreMigrationSnapshot(databasePath, artifactPath string, expected PreMigrationSnapshotIdentity, ops preMigrationRestoreOps) (PreMigrationRestoreResult, error) {
	notApplied := PreMigrationRestoreResult{State: NotAppliedDurable}
	if validateSnapshotPath(databasePath) != nil || validateSnapshotPath(artifactPath) != nil || databasePath == artifactPath {
		return notApplied, ErrInvalidDatabaseLocation
	}
	if destinationInfo, statErr := os.Lstat(databasePath); statErr != nil || validateSnapshotFileInfo(destinationInfo) != nil {
		return notApplied, ErrInsecureDatabase
	}
	lock, err := acquireSQLiteLock(databasePath)
	if err != nil {
		return notApplied, err
	}
	defer lock.Close()

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

	stageName, stage, err := createSnapshotTemporary(directory, filepath.Base(databasePath)+".restore")
	if err != nil {
		return notApplied, err
	}
	stagePath := filepath.Join(filepath.Dir(databasePath), stageName)
	stageExists := true
	cleanupStage := func() error {
		if !stageExists {
			return nil
		}
		if unlinkErr := ops.unlink(directory, stageName); unlinkErr != nil {
			return unlinkErr
		}
		stageExists = false
		return ops.sync(directory)
	}
	failBeforePublish := func(cause error) (PreMigrationRestoreResult, error) {
		if closeErr := stage.Close(); closeErr != nil {
			cause = errors.Join(cause, closeErr)
		}
		if cleanupErr := cleanupStage(); cleanupErr != nil {
			return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: true}, errors.Join(cause, cleanupErr)
		}
		return notApplied, cause
	}
	if _, err = artifact.Seek(0, io.SeekStart); err != nil {
		return failBeforePublish(err)
	}
	if _, err = ops.copy(stage, artifact); err != nil {
		return failBeforePublish(err)
	}
	if err = ops.sync(stage); err != nil {
		return failBeforePublish(err)
	}
	if err = stage.Close(); err != nil {
		return failBeforePublish(err)
	}
	stage = nil

	staged, err := openSecureSnapshotFile(stagePath)
	if err != nil {
		if cleanupErr := cleanupStage(); cleanupErr != nil {
			return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: true}, errors.Join(err, cleanupErr)
		}
		return notApplied, err
	}
	defer staged.Close()
	stagedIdentity, err := inspectOpenedPreMigrationSnapshot(staged)
	if err != nil || stagedIdentity != expected {
		if err == nil {
			err = errInvalidPreMigrationSnapshot
		}
		if cleanupErr := cleanupStage(); cleanupErr != nil {
			return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: true}, errors.Join(err, cleanupErr)
		}
		return notApplied, err
	}

	if revalidateOpenedSnapshotPath(artifactPath, artifact) != nil ||
		revalidateOpenedSnapshotPath(databasePath, destination) != nil ||
		revalidateOpenedSnapshotPath(stagePath, staged) != nil ||
		rejectSnapshotSidecars(artifactPath) != nil || rejectSnapshotSidecars(databasePath) != nil || rejectSnapshotSidecars(stagePath) != nil {
		if cleanupErr := cleanupStage(); cleanupErr != nil {
			return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: true}, errors.Join(ErrInsecureDatabase, cleanupErr)
		}
		return notApplied, ErrInsecureDatabase
	}
	if err = ops.exchange(directory, stageName, filepath.Base(databasePath)); err != nil {
		if cleanupErr := cleanupStage(); cleanupErr != nil {
			return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: true}, errors.Join(err, cleanupErr)
		}
		return notApplied, err
	}

	compensate := func(cause error) (PreMigrationRestoreResult, error) {
		if reverseErr := ops.exchange(directory, stageName, filepath.Base(databasePath)); reverseErr != nil {
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: true}, errors.Join(cause, reverseErr)
		}
		if revalidateOpenedSnapshotPath(databasePath, destination) != nil || revalidateOpenedSnapshotPath(stagePath, staged) != nil ||
			rejectSnapshotSidecars(databasePath) != nil || rejectSnapshotSidecars(stagePath) != nil || rejectSnapshotSidecars(artifactPath) != nil {
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: true}, errors.Join(cause, ErrInsecureDatabase)
		}
		if syncErr := ops.sync(directory); syncErr != nil {
			return PreMigrationRestoreResult{State: Indeterminate, RecoveryResidue: true}, errors.Join(cause, syncErr)
		}
		if cleanupErr := cleanupStage(); cleanupErr != nil {
			return PreMigrationRestoreResult{State: NotAppliedDurable, RecoveryResidue: true}, errors.Join(cause, cleanupErr)
		}
		return notApplied, cause
	}
	if err = ops.validateLive(databasePath, expected); err != nil {
		return compensate(err)
	}
	if err = ops.sync(directory); err != nil {
		return compensate(err)
	}

	stageExists = true
	if err = ops.unlink(directory, stageName); err != nil {
		return PreMigrationRestoreResult{State: AppliedDurable, RecoveryResidue: true}, err
	}
	stageExists = false
	if err = ops.sync(directory); err != nil {
		preserveErr := preserveOldGeneration(directory, stageName, destination, ops)
		return PreMigrationRestoreResult{State: AppliedDurable, RecoveryResidue: true}, errors.Join(err, preserveErr)
	}
	return PreMigrationRestoreResult{State: AppliedDurable}, nil
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
		err = recovery.Sync()
	}
	err = errors.Join(err, recovery.Close())
	if err == nil {
		err = directory.Sync()
	}
	return err
}
