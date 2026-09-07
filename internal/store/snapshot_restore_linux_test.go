//go:build linux

package store

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRestorePreMigrationSnapshotContract(t *testing.T) {
	databasePath := filepath.Join(privateTempDir(t), "forge.db")
	store, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(privateTempDir(t), "forge.db.snapshot")
	expected, err := CreatePreMigrationSnapshot(databasePath, artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	artifactBefore, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(databasePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var version [4]byte
	binary.BigEndian.PutUint32(version[:], 4)
	if _, err = file.WriteAt(version[:], 60); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := RestorePreMigrationSnapshot(databasePath, artifactPath, expected)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != AppliedDurable || result.RecoveryResidue {
		t.Fatalf("result = %+v, want AppliedDurable without residue", result)
	}
	if err = ValidatePreMigrationSnapshot(databasePath, expected); err != nil {
		t.Fatalf("restored database: %v", err)
	}
	artifactAfter, err := os.ReadFile(artifactPath)
	if err != nil || string(artifactAfter) != string(artifactBefore) {
		t.Fatalf("artifact changed: %v", err)
	}

	owned, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if result, err = RestorePreMigrationSnapshot(databasePath, artifactPath, expected); !errors.Is(err, ErrAlreadyOwned) || result.State != NotAppliedDurable {
		t.Fatalf("owned restore = (%+v, %v), want NotAppliedDurable, ErrAlreadyOwned", result, err)
	}

	missing := filepath.Join(filepath.Dir(databasePath), "missing.db")
	if result, err = RestorePreMigrationSnapshot(missing, artifactPath, expected); err == nil || result.State != NotAppliedDurable {
		t.Fatalf("missing restore = (%+v, %v), want rejected NotAppliedDurable", result, err)
	}
}

func TestRestorePreMigrationSnapshotFailureOutcomes(t *testing.T) {
	newFixture := func(t *testing.T) (string, string, PreMigrationSnapshotIdentity, []byte) {
		t.Helper()
		databasePath := filepath.Join(privateTempDir(t), "forge.db")
		store, err := Open(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
		artifactPath := filepath.Join(privateTempDir(t), "forge.db.snapshot")
		expected, err := CreatePreMigrationSnapshot(databasePath, artifactPath)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(databasePath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		var version [4]byte
		binary.BigEndian.PutUint32(version[:], uint32((expected.SchemaVersion+1)%(SchemaVersion()+1)))
		if _, err = file.WriteAt(version[:], 60); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		return databasePath, artifactPath, expected, original
	}
	assertOriginal := func(t *testing.T, path string, original []byte) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(original) {
			t.Fatalf("original destination not preserved: %v", err)
		}
	}

	t.Run("prepublication", func(t *testing.T) {
		databasePath, artifactPath, expected, original := newFixture(t)
		ops := defaultPreMigrationRestoreOps()
		ops.exchange = func(*os.File, string, string) error { return errors.New("injected exchange failure") }
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State != NotAppliedDurable || result.RecoveryResidue {
			t.Fatalf("result = (%+v, %v), want clean NotAppliedDurable error", result, err)
		}
		assertOriginal(t, databasePath, original)
	})

	t.Run("compensation uncertain", func(t *testing.T) {
		databasePath, artifactPath, expected, _ := newFixture(t)
		ops := defaultPreMigrationRestoreOps()
		exchanges := 0
		realExchange := ops.exchange
		ops.exchange = func(directory *os.File, first, second string) error {
			exchanges++
			if exchanges == 2 {
				return errors.New("injected compensation failure")
			}
			return realExchange(directory, first, second)
		}
		ops.validateLive = func(string, PreMigrationSnapshotIdentity) error {
			return errors.New("injected live validation failure")
		}
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State != Indeterminate || !result.RecoveryResidue {
			t.Fatalf("result = (%+v, %v), want Indeterminate with residue", result, err)
		}
	})

	t.Run("commit failure compensated", func(t *testing.T) {
		databasePath, artifactPath, expected, original := newFixture(t)
		ops := defaultPreMigrationRestoreOps()
		realSync := ops.sync
		directorySyncs := 0
		ops.sync = func(file *os.File) error {
			if file.Name() == filepath.Dir(databasePath) {
				directorySyncs++
				if directorySyncs == 1 {
					return errors.New("injected commit failure")
				}
			}
			return realSync(file)
		}
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State != NotAppliedDurable || result.RecoveryResidue {
			t.Fatalf("result = (%+v, %v), want compensated NotAppliedDurable", result, err)
		}
		assertOriginal(t, databasePath, original)
	})

	t.Run("committed cleanup failure", func(t *testing.T) {
		databasePath, artifactPath, expected, _ := newFixture(t)
		ops := defaultPreMigrationRestoreOps()
		ops.unlink = func(*os.File, string) error { return errors.New("injected cleanup failure") }
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State != AppliedDurable || !result.RecoveryResidue {
			t.Fatalf("result = (%+v, %v), want AppliedDurable with residue", result, err)
		}
		if err = ValidatePreMigrationSnapshot(databasePath, expected); err != nil {
			t.Fatalf("committed restored database: %v", err)
		}
	})
}
