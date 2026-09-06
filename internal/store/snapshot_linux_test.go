//go:build linux

package store

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreatePreMigrationSnapshotContract(t *testing.T) {
	databasePath := filepath.Join(privateTempDir(t), "forge.db")
	store, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version=4`); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(privateTempDir(t), "forge.db.snapshot")

	identity, err := CreatePreMigrationSnapshot(databasePath, artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(data)
	if identity.SchemaVersion != 4 || identity.Size != int64(len(data)) || identity.SHA256 != wantHash {
		t.Fatalf("identity = %+v, want schema=4 size=%d sha256=%x", identity, len(data), wantHash)
	}
	if err := ValidatePreMigrationSnapshot(artifactPath, identity); err != nil {
		t.Fatalf("validate created snapshot: %v", err)
	}
	assertFileMode(t, artifactPath, 0o600)

	if _, err := CreatePreMigrationSnapshot(databasePath, artifactPath); err == nil {
		t.Fatal("CreatePreMigrationSnapshot replaced an existing artifact")
	}
	if got, err := os.ReadFile(artifactPath); err != nil || string(got) != string(data) {
		t.Fatalf("existing artifact changed: %v", err)
	}

	owned, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if _, err := CreatePreMigrationSnapshot(databasePath, filepath.Join(filepath.Dir(artifactPath), "owned.snapshot")); !errors.Is(err, ErrAlreadyOwned) {
		t.Fatalf("owned database error = %v, want ErrAlreadyOwned", err)
	}
}

func TestValidatePreMigrationSnapshotRejectsTampering(t *testing.T) {
	databasePath := filepath.Join(privateTempDir(t), "forge.db")
	store, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(privateTempDir(t), "forge.db.snapshot")
	identity, err := CreatePreMigrationSnapshot(databasePath, artifactPath)
	if err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(artifactPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("tampered")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePreMigrationSnapshot(artifactPath, identity); err == nil {
		t.Fatal("ValidatePreMigrationSnapshot accepted tampering")
	}
}
