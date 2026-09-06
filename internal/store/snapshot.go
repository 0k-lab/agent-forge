package store

// PreMigrationSnapshotIdentity binds a snapshot to its SQLite schema and bytes.
type PreMigrationSnapshotIdentity struct {
	SchemaVersion int
	Size          int64
	SHA256        [32]byte
}
