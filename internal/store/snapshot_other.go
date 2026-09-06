//go:build !linux

package store

func CreatePreMigrationSnapshot(string, string) (PreMigrationSnapshotIdentity, error) {
	return PreMigrationSnapshotIdentity{}, ErrUnsupportedDatabase
}

func ValidatePreMigrationSnapshot(string, PreMigrationSnapshotIdentity) error {
	return ErrUnsupportedDatabase
}
