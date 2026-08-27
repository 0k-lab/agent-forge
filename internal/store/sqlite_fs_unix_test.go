//go:build unix

package store

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestStoreLockLifecycleAndMode(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "forge.db")
	old := syscall.Umask(0o777)
	s, err := Open(path)
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := path + ".lock"
	assertFileMode(t, lockPath, 0o600)
	if other, err := Open(path); other != nil || !errors.Is(err, ErrAlreadyOwned) || err.Error() != "store already owned" || strings.Contains(err.Error(), path) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("second Open = %#v, %q", other, err)
	}
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatalf("active lock disappeared: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatalf("close unlinked lock: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreLockRejectsUnsafeArtifactsWithoutRepair(t *testing.T) {
	for _, kind := range []string{"symlink", "mode"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "forge.db")
			lockPath := path + ".lock"
			if kind == "symlink" {
				target := filepath.Join(filepath.Dir(path), "target")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, lockPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if s, err := Open(path); s != nil || !errors.Is(err, ErrInsecureDatabase) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open = %#v, %v", s, err)
			}
			after, err := os.Lstat(lockPath)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("lock repaired or replaced: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected lock created database: %v", err)
			}
		})
	}
}

func TestStoreLockRejectsSecondProcess(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "forge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreLockHelper$")
	cmd.Env = append(os.Environ(), "FORGE_LOCK_HELPER="+path)
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "store already owned" {
		t.Fatalf("helper = %q, %v", out, err)
	}
}

func TestStoreLockHelper(t *testing.T) {
	path := os.Getenv("FORGE_LOCK_HELPER")
	if path == "" {
		return
	}
	_, err := Open(path)
	if !errors.Is(err, ErrAlreadyOwned) {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString(err.Error())
	os.Exit(0)
}

func TestOpenCreatesDatabaseMode0600UnderCommonUmasks(t *testing.T) {
	for _, mask := range []int{0, 0o022, 0o077, 0o777} {
		t.Run(fmtOctal(mask), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			old := syscall.Umask(mask)
			defer syscall.Umask(old)
			path := filepath.Join(dir, "forge.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			assertFileMode(t, path, 0o600)
		})
	}
}

func TestOpenCreatesOnlyMissingImmediateParentMode0700(t *testing.T) {
	for _, mask := range []int{0, 0o022, 0o077, 0o777} {
		t.Run(fmtOctal(mask), func(t *testing.T) {
			root := privateTempDir(t)
			parent := filepath.Join(root, "state")
			old := syscall.Umask(mask)
			defer syscall.Umask(old)
			s, err := Open(filepath.Join(parent, "forge.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			assertFileMode(t, parent, 0o700)
			info, err := os.Lstat(parent)
			if err != nil || fileUID(info) != uint32(os.Geteuid()) {
				t.Fatalf("parent owner = %d, err=%v", fileUID(info), err)
			}
		})
	}
}

func TestOpenDoesNotCreateMoreThanImmediateParent(t *testing.T) {
	root := privateTempDir(t)
	missing := filepath.Join(root, "missing")
	if s, err := Open(filepath.Join(missing, "state", "forge.db")); err == nil {
		s.Close()
		t.Fatal("Open created multiple missing parent levels")
	} else if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open created ancestor: %v", err)
	}
}

func TestOpenRejectsRacedInsecureParentWithoutRepair(t *testing.T) {
	root := privateTempDir(t)
	parent := filepath.Join(root, "state")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := Open(filepath.Join(parent, "forge.db")); err == nil {
		s.Close()
		t.Fatal("Open accepted raced insecure parent")
	} else if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}
	after, err := os.Lstat(parent)
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o755 {
		t.Fatalf("Open repaired or replaced parent: %v", err)
	}
}

func TestOpenRejectsInsecureParentWithoutCreatingDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "forge.db")
	s, err := Open(path)
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("Open accepted a group/world-accessible parent")
	}
	if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("rejected Open created database: %v", statErr)
	}
	assertFileMode(t, dir, 0o755)
}

func TestOpenRejectsInsecureDatabaseWithoutRepair(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "forge.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE existing (value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("Open accepted an insecure database")
	}
	if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}
	after, statErr := os.Lstat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0o644 {
		t.Fatalf("rejected Open repaired or replaced database: inode same=%v mode=%04o", os.SameFile(before, after), after.Mode().Perm())
	}
}

func TestOpenRejectsDatabaseSpecialModeBits(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "forge.db")
	createSQLiteFile(t, path)
	if err := os.Chmod(path, 0o600|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem did not preserve set-ID bit")
	}
	s, err := Open(path)
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("Open accepted set-ID database mode")
	}
	if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("rejected Open repaired special mode: mode=%v", info.Mode())
	}
}

func TestOpenRejectsUnsafeDatabaseFileTypes(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "forge.db")
			var err error
			switch kind {
			case "symlink":
				target := filepath.Join(dir, "target.db")
				if err = os.WriteFile(target, nil, 0o600); err == nil {
					err = os.Symlink(target, path)
				}
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			s, openErr := Open(path)
			if s != nil {
				s.Close()
			}
			if openErr == nil {
				t.Fatalf("Open accepted %s database", kind)
			}
			if !errors.Is(openErr, ErrInsecureDatabase) {
				t.Fatalf("Open error = %v, want ErrInsecureDatabase", openErr)
			}
		})
	}
}

func TestOpenRejectsInsecureOrSymlinkSidecarsWithoutRepair(t *testing.T) {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		for _, kind := range []string{"mode", "symlink"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				dir := privateTempDir(t)
				path := filepath.Join(dir, "forge.db")
				createSQLiteFile(t, path)
				sidecar := path + suffix
				if kind == "mode" {
					if err := os.WriteFile(sidecar, []byte("synthetic-secret"), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(sidecar, 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					target := filepath.Join(dir, "target"+suffix)
					if err := os.WriteFile(target, []byte("synthetic-secret"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, sidecar); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.Lstat(sidecar)
				if err != nil {
					t.Fatal(err)
				}
				s, openErr := Open(path)
				if s != nil {
					s.Close()
				}
				if openErr == nil {
					t.Fatalf("Open accepted %s %s", kind, suffix)
				}
				if !errors.Is(openErr, ErrInsecureDatabase) {
					t.Fatalf("Open error = %v, want ErrInsecureDatabase", openErr)
				}
				after, statErr := os.Lstat(sidecar)
				if statErr != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() {
					t.Fatalf("rejected Open removed or replaced sidecar: %v", statErr)
				}
			})
		}
	}
}

func TestOpenRejectsSymlinkPathComponent(t *testing.T) {
	root := privateTempDir(t)
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "private", "forge.db")
	s, err := Open(path)
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("Open accepted a symlinked path component")
	}
	if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("rejected Open created database: %v", statErr)
	}
}

func TestOpenReturnsStablePathFreeInitializationError(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "synthetic-private-name.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("Open accepted corrupt database")
	}
	if !errors.Is(err, ErrDatabaseOpen) {
		t.Fatalf("Open error = %v, want ErrDatabaseOpen", err)
	}
	if got, want := err.Error(), "database open failed"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	for _, private := range []string{path, filepath.Base(path), "not a sqlite database"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("error exposed private value %q", private)
		}
	}
}

func TestOpenParserErrorIsCategorizedAndPathFree(t *testing.T) {
	input := "file:synthetic-private-name.db?token=synthetic-credential"
	_, err := Open(input)
	if !errors.Is(err, ErrInvalidDatabaseLocation) {
		t.Fatalf("Open error = %v, want ErrInvalidDatabaseLocation", err)
	}
	for _, private := range []string{"synthetic-private-name", "synthetic-credential"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("error exposed private value %q", private)
		}
	}
}

func TestOpenPlainPathTreatsQuestionAndFragmentAsLiteral(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "literal?#.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, path, 0o600)
	for _, alias := range []string{filepath.Join(dir, "literal"), filepath.Join(dir, "literal?")} {
		if _, err := os.Lstat(alias); !os.IsNotExist(err) {
			t.Fatalf("Open created truncated alias %q: %v", alias, err)
		}
	}
}

func TestOpenCreatesLiveSQLiteArtifactsMode0600(t *testing.T) {
	for _, mask := range []int{0, 0o022, 0o077} {
		t.Run(fmtOctal(mask), func(t *testing.T) {
			dir := privateTempDir(t)
			old := syscall.Umask(mask)
			defer syscall.Umask(old)
			path := filepath.Join(dir, "forge.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`CREATE TABLE rollback_probe (value TEXT)`); err != nil {
				t.Fatal(err)
			}
			assertFileMode(t, path+"-journal", 0o600)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}

			var journalMode string
			if err := s.db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journalMode); err != nil || journalMode != "wal" {
				t.Fatalf("journal mode = %q, err=%v", journalMode, err)
			}
			if _, err := s.db.Exec(`CREATE TABLE wal_probe (value TEXT)`); err != nil {
				t.Fatal(err)
			}
			assertFileMode(t, path+"-wal", 0o600)
			assertFileMode(t, path+"-shm", 0o600)
		})
	}
}

func TestOpenSupportsValidatedSQLiteDSNs(t *testing.T) {
	for _, dsn := range []string{":memory:", "file::memory:?cache=shared", "file:shared-name?mode=memory&cache=shared", "file:?mode=memory"} {
		t.Run(dsn, func(t *testing.T) {
			s, err := Open(dsn)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	dir := privateTempDir(t)
	path := filepath.Join(dir, "percent decoded.db")
	u := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc&cache=private"}
	s, err := Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, path, 0o600)

}

func TestOpenPreservesSecureDatabaseAcrossRestart(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "forge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
		t.Fatalf("restart replaced or changed database: inode same=%v mode=%04o", os.SameFile(before, after), after.Mode().Perm())
	}
}

func TestOpenRejectsWrongOwnershipWhenAvailable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing ownership safely requires root")
	}
	dir := privateTempDir(t)
	path := filepath.Join(dir, "forge.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 65534, -1); err != nil {
		t.Skipf("cannot create wrong-owner fixture: %v", err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open accepted wrong-owner database")
	} else if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}

	parent := privateTempDir(t)
	if err := os.Chown(parent, 65534, -1); err != nil {
		t.Skipf("cannot create wrong-owner parent fixture: %v", err)
	}
	if s, err := Open(filepath.Join(parent, "forge.db")); err == nil {
		s.Close()
		t.Fatal("Open accepted wrong-owner parent")
	} else if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}

	lockDir := privateTempDir(t)
	lockDatabase := filepath.Join(lockDir, "forge.db")
	lockPath := lockDatabase + ".lock"
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(lockPath, 65534, -1); err != nil {
		t.Skipf("cannot create wrong-owner lock fixture: %v", err)
	}
	if s, err := Open(lockDatabase); err == nil {
		s.Close()
		t.Fatal("Open accepted wrong-owner lock")
	} else if !errors.Is(err, ErrInsecureDatabase) {
		t.Fatalf("Open error = %v, want ErrInsecureDatabase", err)
	}
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if fileUID(info) != 65534 {
		t.Fatalf("Open repaired wrong-owner lock: owner=%d", fileUID(info))
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func createSQLiteFile(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE existing (value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode = %04o, want %04o", got, want)
	}
}

func fmtOctal(mask int) string {
	return "umask_" + strconv.FormatInt(int64(mask), 8)
}
