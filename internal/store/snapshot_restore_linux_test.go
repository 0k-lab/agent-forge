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

func newRestoreSecurityFixture(t *testing.T) (string, string, PreMigrationSnapshotIdentity, []byte) {
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

func assertRestoreOriginal(t *testing.T, path string, original []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatalf("original destination not preserved: %v", err)
	}
}

func TestRestoreExchangeEffectThenErrorClassifiesBeforeCleanup(t *testing.T) {
	databasePath, artifactPath, expected, original := newRestoreSecurityFixture(t)
	ops := defaultPreMigrationRestoreOps()
	realExchange := ops.exchange
	firstExchange := true
	ops.exchange = func(directory *os.File, firstName, secondName string) error {
		if firstExchange {
			firstExchange = false
			if err := realExchange(directory, firstName, secondName); err != nil {
				return err
			}
			return errors.New("exchange took effect but reported failure")
		}
		return realExchange(directory, firstName, secondName)
	}

	result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
	if err == nil || result.State != NotAppliedDurable || result.RecoveryResidue {
		t.Fatalf("result = (%+v, %v), want durably restored clean NotAppliedDurable", result, err)
	}
	assertRestoreOriginal(t, databasePath, original)
}

func TestRestoreRevalidatesPinnedNamesAndContentBeforeCommit(t *testing.T) {
	t.Run("database pathname swapped", func(t *testing.T) {
		databasePath, artifactPath, expected, _ := newRestoreSecurityFixture(t)
		ops := defaultPreMigrationRestoreOps()
		realValidate := ops.validateLive
		ops.validateLive = func(path string, identity PreMigrationSnapshotIdentity) error {
			if err := realValidate(path, identity); err != nil {
				return err
			}
			if err := os.Rename(databasePath, databasePath+".displaced"); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(artifactPath)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(databasePath, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State == AppliedDurable {
			t.Fatalf("result = (%+v, %v), swapped database pathname must not be AppliedDurable", result, err)
		}
	})

	t.Run("lock pathname replaced", func(t *testing.T) {
		databasePath, artifactPath, expected, _ := newRestoreSecurityFixture(t)
		ops := defaultPreMigrationRestoreOps()
		realValidate := ops.validateLive
		ops.validateLive = func(path string, identity PreMigrationSnapshotIdentity) error {
			if err := realValidate(path, identity); err != nil {
				return err
			}
			lockPath := databasePath + ".lock"
			if err := os.Rename(lockPath, lockPath+".displaced"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State == AppliedDurable {
			t.Fatalf("result = (%+v, %v), replaced lock pathname must not be AppliedDurable", result, err)
		}
	})

	t.Run("database pathname swapped during commit fsync", func(t *testing.T) {
		databasePath, artifactPath, expected, _ := newRestoreSecurityFixture(t)
		ops := defaultPreMigrationRestoreOps()
		realSync := ops.sync
		swapped := false
		ops.sync = func(file *os.File) error {
			if file.Name() == filepath.Dir(databasePath) && !swapped {
				swapped = true
				if err := os.Rename(databasePath, databasePath+".displaced"); err != nil {
					t.Fatal(err)
				}
				contents, err := os.ReadFile(artifactPath)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(databasePath, contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return realSync(file)
		}
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State == AppliedDurable {
			t.Fatalf("result = (%+v, %v), pathname swap around commit fsync must not be AppliedDurable", result, err)
		}
	})

	t.Run("artifact pathname replaced with identical bytes", func(t *testing.T) {
		databasePath, artifactPath, expected, _ := newRestoreSecurityFixture(t)
		ops := defaultPreMigrationRestoreOps()
		realValidate := ops.validateLive
		ops.validateLive = func(path string, identity PreMigrationSnapshotIdentity) error {
			if err := realValidate(path, identity); err != nil {
				return err
			}
			contents, err := os.ReadFile(artifactPath)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(artifactPath, artifactPath+".displaced"); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(artifactPath, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State == AppliedDurable {
			t.Fatalf("result = (%+v, %v), replaced artifact pathname must not be AppliedDurable", result, err)
		}
	})

	t.Run("artifact mutated after live validation", func(t *testing.T) {
		databasePath, artifactPath, expected, _ := newRestoreSecurityFixture(t)
		ops := defaultPreMigrationRestoreOps()
		realValidate := ops.validateLive
		ops.validateLive = func(path string, identity PreMigrationSnapshotIdentity) error {
			if err := realValidate(path, identity); err != nil {
				return err
			}
			artifact, err := os.OpenFile(artifactPath, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := artifact.WriteAt([]byte{0xff}, expected.Size-1)
			closeErr := artifact.Close()
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			return nil
		}
		result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
		if err == nil || result.State == AppliedDurable {
			t.Fatalf("result = (%+v, %v), mutated artifact must not be AppliedDurable", result, err)
		}
	})
}

func TestRestoreRecoveryResidueUsesObservedNameState(t *testing.T) {
	databasePath, artifactPath, expected, _ := newRestoreSecurityFixture(t)
	ops := defaultPreMigrationRestoreOps()
	realUnlink := ops.unlink
	ops.unlink = func(directory *os.File, name string) error {
		if err := realUnlink(directory, name); err != nil {
			return err
		}
		return errors.New("unlink took effect but reported failure")
	}
	result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
	if err == nil || result.State != AppliedDurable || result.RecoveryResidue {
		t.Fatalf("result = (%+v, %v), want AppliedDurable without absent recovery residue", result, err)
	}
}

func TestRestoreDoesNotReportAppliedIfLiveNameChangesDuringCleanup(t *testing.T) {
	databasePath, artifactPath, expected, _ := newRestoreSecurityFixture(t)
	ops := defaultPreMigrationRestoreOps()
	realUnlink := ops.unlink
	ops.unlink = func(directory *os.File, name string) error {
		if err := os.Rename(databasePath, databasePath+".displaced-during-cleanup"); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(artifactPath)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(databasePath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		return realUnlink(directory, name)
	}
	result, err := restorePreMigrationSnapshot(databasePath, artifactPath, expected, ops)
	if err == nil || result.State == AppliedDurable {
		t.Fatalf("result = (%+v, %v), cleanup-time pathname swap must not be AppliedDurable", result, err)
	}
}
