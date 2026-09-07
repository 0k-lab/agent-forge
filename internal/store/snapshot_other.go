//go:build !linux

package store

func CreatePreMigrationSnapshot(string, string) (PreMigrationSnapshotIdentity, error) {
	return PreMigrationSnapshotIdentity{}, ErrUnsupportedDatabase
}

func ValidatePreMigrationSnapshot(string, PreMigrationSnapshotIdentity) error {
	return ErrUnsupportedDatabase
}

func RestorePreMigrationSnapshot(string, string, PreMigrationSnapshotIdentity) (PreMigrationRestoreResult, error) {
	return PreMigrationRestoreResult{State: NotAppliedDurable}, ErrUnsupportedDatabase
}
