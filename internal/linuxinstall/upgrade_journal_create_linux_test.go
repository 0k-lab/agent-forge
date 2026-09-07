//go:build linux

package linuxinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCreateReadUpgradeJournalContract(t *testing.T) {
	journal := upgradeTransactionJournal{
		FormatVersion:         upgradeJournalFormatVersion,
		TransactionID:         strings.Repeat("a", 64),
		SourceVersion:         "v1.2.3",
		TargetVersion:         "v1.2.4",
		TargetCommit:          strings.Repeat("b", 40),
		StoreSchemaVersion:    5,
		SourceReceiptSHA256:   strings.Repeat("c", 64),
		AccountUID:            os.Geteuid(),
		AccountGID:            os.Getegid(),
		DatabaseRelativePath:  "var/gate/state/forge.db",
		SnapshotRelativePath:  "var/gate/state/.forge-pre-migration-" + strings.Repeat("a", 64) + ".db",
		SnapshotSchemaVersion: 4,
		SnapshotSize:          4096,
		SnapshotSHA256:        strings.Repeat("d", 64),
		Phase:                 upgradePhasePreparedSnapshotDurable,
	}
	setup := func(t *testing.T) Options {
		t.Helper()
		o := Options{Root: t.TempDir()}
		if err := os.MkdirAll(filepath.Dir(upgradeJournalPath(o)), 0o700); err != nil {
			t.Fatal(err)
		}
		return o
	}

	t.Run("create once and read canonical journal", func(t *testing.T) {
		o := setup(t)
		wantPath := filepath.Join(o.Root, "opt/agent-forge/var/gate/state/upgrade-transaction.json")
		if got := upgradeJournalPath(o); got != wantPath {
			t.Fatalf("path = %q, want %q", got, wantPath)
		}
		outcome, err := createUpgradeTransactionJournal(o, journal)
		if err != nil || outcome != upgradeJournalAppliedDurable {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		body, err := os.ReadFile(wantPath)
		if err != nil {
			t.Fatal(err)
		}
		canonical, _ := encodeUpgradeTransactionJournal(journal)
		if string(body) != string(canonical) {
			t.Fatalf("journal = %q", body)
		}
		info, err := os.Lstat(wantPath)
		if err != nil || info.Mode().Perm() != 0o400 || !info.Mode().IsRegular() {
			t.Fatalf("journal metadata = %v, %v", info, err)
		}
		got, err := readUpgradeTransactionJournal(o)
		if err != nil || got != journal {
			t.Fatalf("read = %#v, %v", got, err)
		}
	})

	t.Run("directory sync failure is indeterminate and preserves journal", func(t *testing.T) {
		o := setup(t)
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, upgradeJournalCreateOps{
			syncParent: func(*os.File) error { return errors.New("injected directory sync failure") },
		})
		if err == nil || outcome != upgradeJournalIndeterminate {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		if got, err := readUpgradeTransactionJournal(o); err != nil || got != journal {
			t.Fatalf("visible journal = %#v, %v", got, err)
		}
	})

	t.Run("failure before publish is not applied", func(t *testing.T) {
		o := setup(t)
		invalid := journal
		invalid.Phase = upgradePhaseCandidateReady
		outcome, err := createUpgradeTransactionJournal(o, invalid)
		if err == nil || outcome != upgradeJournalNotAppliedDurable {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		if _, err := os.Lstat(upgradeJournalPath(o)); !os.IsNotExist(err) {
			t.Fatalf("journal exists after rejected create: %v", err)
		}
	})

	write := func(t *testing.T, body []byte, mode os.FileMode) Options {
		t.Helper()
		o := setup(t)
		if err := os.WriteFile(upgradeJournalPath(o), body, mode); err != nil {
			t.Fatal(err)
		}
		return o
	}
	canonical, _ := encodeUpgradeTransactionJournal(journal)
	badReads := map[string]func(*testing.T) Options{
		"wrong mode":   func(t *testing.T) Options { return write(t, canonical, 0o600) },
		"noncanonical": func(t *testing.T) Options { return write(t, append(append([]byte(nil), canonical...), '\n'), 0o400) },
		"trailing": func(t *testing.T) Options {
			return write(t, append(append([]byte(nil), canonical...), []byte("{}")...), 0o400)
		},
		"oversized": func(t *testing.T) Options { return write(t, make([]byte, upgradeJournalMaxBytes+1), 0o400) },
		"invalid":   func(t *testing.T) Options { return write(t, []byte("{}"), 0o400) },
		"non-regular": func(t *testing.T) Options {
			o := setup(t)
			if err := os.Mkdir(upgradeJournalPath(o), 0o400); err != nil {
				t.Fatal(err)
			}
			return o
		},
		"hardlink": func(t *testing.T) Options {
			o := write(t, canonical, 0o400)
			if err := os.Link(upgradeJournalPath(o), upgradeJournalPath(o)+".link"); err != nil {
				t.Fatal(err)
			}
			return o
		},
		"symlink leaf": func(t *testing.T) Options {
			o := setup(t)
			target := filepath.Join(o.Root, "journal-target")
			if err := os.WriteFile(target, canonical, 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, upgradeJournalPath(o)); err != nil {
				t.Fatal(err)
			}
			return o
		},
		"symlink component": func(t *testing.T) Options {
			realRoot := t.TempDir()
			linkedRoot := filepath.Join(t.TempDir(), "root")
			if err := os.Symlink(realRoot, linkedRoot); err != nil {
				t.Fatal(err)
			}
			o := Options{Root: realRoot}
			if err := os.MkdirAll(filepath.Dir(upgradeJournalPath(o)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(upgradeJournalPath(o), canonical, 0o400); err != nil {
				t.Fatal(err)
			}
			return Options{Root: linkedRoot}
		},
	}
	if os.Geteuid() == 0 {
		badReads["wrong owner"] = func(t *testing.T) Options {
			o := write(t, canonical, 0o400)
			if err := os.Chown(upgradeJournalPath(o), 1, 1); err != nil {
				t.Fatal(err)
			}
			return o
		}
	}
	for name, arrange := range badReads {
		t.Run("read rejects "+name, func(t *testing.T) {
			if _, err := readUpgradeTransactionJournal(arrange(t)); err == nil {
				t.Fatal("read accepted unsafe journal")
			}
		})
	}
}

func testUpgradeJournal(t *testing.T) upgradeTransactionJournal {
	t.Helper()
	return upgradeTransactionJournal{
		FormatVersion:         upgradeJournalFormatVersion,
		TransactionID:         strings.Repeat("e", 64),
		SourceVersion:         "v2.0.0",
		TargetVersion:         "v2.0.1",
		TargetCommit:          strings.Repeat("f", 40),
		StoreSchemaVersion:    7,
		SourceReceiptSHA256:   strings.Repeat("1", 64),
		AccountUID:            os.Geteuid(),
		AccountGID:            os.Getegid(),
		DatabaseRelativePath:  "var/gate/state/forge.db",
		SnapshotRelativePath:  "var/gate/state/.forge-pre-migration-" + strings.Repeat("e", 64) + ".db",
		SnapshotSchemaVersion: 6,
		SnapshotSize:          8192,
		SnapshotSHA256:        strings.Repeat("2", 64),
		Phase:                 upgradePhasePreparedSnapshotDurable,
	}
}

func setupUpgradeJournalRoot(t *testing.T) Options {
	t.Helper()
	o := Options{Root: t.TempDir()}
	if err := os.MkdirAll(filepath.Dir(upgradeJournalPath(o)), 0o700); err != nil {
		t.Fatal(err)
	}
	return o
}

func realUpgradeJournalCreateOps() upgradeJournalCreateOps {
	return defaultUpgradeJournalCreateOps()
}

func assertCreateResidue(t *testing.T, err error, want bool) {
	t.Helper()
	var createErr *upgradeJournalCreateError
	if !errors.As(err, &createErr) || createErr.Residue != want {
		t.Fatalf("create residue = %T %v, want %v", err, err, want)
	}
}

func assertJournalDirectoryEmpty(t *testing.T, o Options) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(upgradeJournalPath(o)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unexpected journal residue: %v", entries)
	}
}

func TestCreateUpgradeJournalUsesPinnedUnnamedInode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged O_TMPFILE publication requires a non-root test process")
	}
	journal := testUpgradeJournal(t)
	canonical, _ := encodeUpgradeTransactionJournal(journal)
	o := setupUpgradeJournalRoot(t)
	ops := realUpgradeJournalCreateOps()
	realPublish := ops.publish
	ops.publish = func(parent, source *os.File, name string) error {
		entries, err := os.ReadDir(filepath.Dir(upgradeJournalPath(o)))
		if err != nil || len(entries) != 0 {
			t.Fatalf("unnamed inode leaked before publish: %v, %v", entries, err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(int(source.Fd()), &stat); err != nil {
			t.Fatal(err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o400 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 0 || stat.Size != int64(len(canonical)) {
			t.Fatalf("unpublished inode metadata = %#v", stat)
		}
		got := make([]byte, len(canonical)+1)
		n, readErr := source.ReadAt(got, 0)
		if readErr == nil || n != len(canonical) {
			t.Fatalf("read pinned inode = %d, %v", n, readErr)
		}
		if string(got[:n]) != string(canonical) {
			t.Fatalf("pinned inode content = %q", got[:n])
		}
		return realPublish(parent, source, name)
	}
	outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
	if err != nil || outcome != upgradeJournalAppliedDurable {
		t.Fatalf("create = %q, %v", outcome, err)
	}
	if got, err := os.ReadFile(upgradeJournalPath(o)); err != nil || string(got) != string(canonical) {
		t.Fatalf("published journal = %q, %v", got, err)
	}
}

func TestCreateUpgradeJournalPreEffectAndPublishFailures(t *testing.T) {
	journal := testUpgradeJournal(t)
	for name, tc := range map[string]struct {
		arrange       func(*upgradeJournalCreateOps)
		want          upgradeJournalCreateOutcome
		assertResidue bool
	}{
		"O_TMPFILE unsupported before publish": {
			arrange: func(ops *upgradeJournalCreateOps) {
				ops.openUnnamed = func(*os.File) (*os.File, error) { return nil, unix.EOPNOTSUPP }
			},
			want: upgradeJournalNotAppliedDurable,
		},
		"publish call reports unsupported": {
			arrange: func(ops *upgradeJournalCreateOps) {
				ops.publish = func(*os.File, *os.File, string) error { return unix.EOPNOTSUPP }
			},
			want:          upgradeJournalIndeterminate,
			assertResidue: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := setupUpgradeJournalRoot(t)
			ops := realUpgradeJournalCreateOps()
			tc.arrange(&ops)
			outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
			if err == nil || outcome != tc.want {
				t.Fatalf("create = %q, %v, want %q", outcome, err, tc.want)
			}
			if tc.assertResidue {
				assertCreateResidue(t, err, false)
			}
			assertJournalDirectoryEmpty(t, o)
		})
	}
}

func TestCreateUpgradeJournalPublishClassification(t *testing.T) {
	journal := testUpgradeJournal(t)
	canonical, _ := encodeUpgradeTransactionJournal(journal)

	t.Run("real link followed by error is indeterminate with exact residue", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		ops := realUpgradeJournalCreateOps()
		realPublish := ops.publish
		ops.publish = func(parent, source *os.File, name string) error {
			if err := realPublish(parent, source, name); err != nil {
				return err
			}
			return errors.New("injected error after link")
		}
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalIndeterminate {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		assertCreateResidue(t, err, true)
		if got, readErr := os.ReadFile(upgradeJournalPath(o)); readErr != nil || string(got) != string(canonical) {
			t.Fatalf("linked journal = %q, %v", got, readErr)
		}
	})

	t.Run("real link followed by unlink and error is indeterminate without residue", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		ops := realUpgradeJournalCreateOps()
		realPublish := ops.publish
		ops.syncParent = func(*os.File) error {
			t.Fatal("directory sync called after publish error")
			return nil
		}
		ops.publish = func(parent, source *os.File, name string) error {
			if err := realPublish(parent, source, name); err != nil {
				return err
			}
			if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
				return err
			}
			return errors.New("injected error after link and unlink")
		}
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalIndeterminate {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		assertCreateResidue(t, err, false)
		assertJournalDirectoryEmpty(t, o)
	})

	t.Run("raced unrelated final is untouched", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		unrelated := []byte("unrelated")
		ops := realUpgradeJournalCreateOps()
		ops.publish = func(parent, _ *os.File, name string) error {
			fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o400)
			if err != nil {
				return err
			}
			_, writeErr := unix.Write(fd, unrelated)
			closeErr := unix.Close(fd)
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
			return unix.EEXIST
		}
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalIndeterminate {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		assertCreateResidue(t, err, true)
		if got, readErr := os.ReadFile(upgradeJournalPath(o)); readErr != nil || string(got) != string(unrelated) {
			t.Fatalf("unrelated final = %q, %v", got, readErr)
		}
	})

	t.Run("existing final is untouched without opening temporary inode", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		unrelated := []byte("existing")
		if err := os.WriteFile(upgradeJournalPath(o), unrelated, 0o400); err != nil {
			t.Fatal(err)
		}
		ops := realUpgradeJournalCreateOps()
		ops.openUnnamed = func(*os.File) (*os.File, error) { t.Fatal("opened O_TMPFILE"); return nil, nil }
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalNotAppliedDurable {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		if got, readErr := os.ReadFile(upgradeJournalPath(o)); readErr != nil || string(got) != string(unrelated) {
			t.Fatalf("existing final = %q, %v", got, readErr)
		}
	})
}

func TestCreateUpgradeJournalRevalidatesPinnedPublication(t *testing.T) {
	journal := testUpgradeJournal(t)
	canonical, _ := encodeUpgradeTransactionJournal(journal)
	mutations := map[string]func(*os.File, string) error{
		"content": func(parent *os.File, name string) error {
			if err := unix.Fchmodat(int(parent.Fd()), name, 0o600, 0); err != nil {
				return err
			}
			fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			_, writeErr := unix.Pwrite(fd, []byte("X"), 0)
			closeErr := unix.Close(fd)
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
			return unix.Fchmodat(int(parent.Fd()), name, 0o400, 0)
		},
		"mode": func(parent *os.File, name string) error { return unix.Fchmodat(int(parent.Fd()), name, 0o600, 0) },
		"hardlink": func(parent *os.File, name string) error {
			return unix.Linkat(int(parent.Fd()), name, int(parent.Fd()), ".published-hardlink", 0)
		},
		"inode": func(parent *os.File, name string) error {
			if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
				return err
			}
			fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o400)
			if err != nil {
				return err
			}
			_, writeErr := unix.Write(fd, []byte("replacement"))
			closeErr := unix.Close(fd)
			if writeErr != nil {
				return writeErr
			}
			return closeErr
		},
	}
	for name, mutate := range mutations {
		t.Run("published "+name+" mutation", func(t *testing.T) {
			o := setupUpgradeJournalRoot(t)
			ops := realUpgradeJournalCreateOps()
			realPublish := ops.publish
			ops.publish = func(parent, source *os.File, final string) error {
				if err := realPublish(parent, source, final); err != nil {
					return err
				}
				return mutate(parent, final)
			}
			outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
			if err == nil || outcome != upgradeJournalIndeterminate {
				t.Fatalf("create = %q, %v", outcome, err)
			}
			assertCreateResidue(t, err, true)
			if _, statErr := os.Lstat(upgradeJournalPath(o)); statErr != nil {
				t.Fatalf("visible final removed: %v", statErr)
			}
		})
	}

	t.Run("parent swap after link preserves detached final", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		parentPath := filepath.Dir(upgradeJournalPath(o))
		detachedPath := parentPath + ".detached"
		ops := realUpgradeJournalCreateOps()
		realPublish := ops.publish
		ops.publish = func(parent, source *os.File, final string) error {
			if err := realPublish(parent, source, final); err != nil {
				return err
			}
			if err := os.Rename(parentPath, detachedPath); err != nil {
				return err
			}
			return os.Mkdir(parentPath, 0o700)
		}
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalIndeterminate {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		assertCreateResidue(t, err, true)
		if _, statErr := os.Lstat(upgradeJournalPath(o)); !os.IsNotExist(statErr) {
			t.Fatalf("replacement parent gained final: %v", statErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(detachedPath, upgradeJournalName)); readErr != nil || string(got) != string(canonical) {
			t.Fatalf("detached final = %q, %v", got, readErr)
		}
	})

	t.Run("parent swap before link fails closed without journal residue", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		parentPath := filepath.Dir(upgradeJournalPath(o))
		detachedPath := parentPath + ".prelink"
		ops := realUpgradeJournalCreateOps()
		realOpen := ops.openUnnamed
		ops.openUnnamed = func(parent *os.File) (*os.File, error) {
			file, err := realOpen(parent)
			if err != nil {
				return nil, err
			}
			if err := os.Rename(parentPath, detachedPath); err != nil {
				file.Close()
				return nil, err
			}
			if err := os.Mkdir(parentPath, 0o700); err != nil {
				file.Close()
				return nil, err
			}
			return file, nil
		}
		ops.publish = func(*os.File, *os.File, string) error { t.Fatal("published after rooted parent swap"); return nil }
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalNotAppliedDurable {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		assertJournalDirectoryEmpty(t, o)
		entries, readErr := os.ReadDir(detachedPath)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("detached residue = %v, %v", entries, readErr)
		}
	})
}

func TestCreateUpgradeJournalDirectoryFsyncBoundary(t *testing.T) {
	journal := testUpgradeJournal(t)

	t.Run("success fsyncs pinned parent exactly once", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		ops := realUpgradeJournalCreateOps()
		realSync := ops.syncParent
		calls := 0
		ops.syncParent = func(parent *os.File) error { calls++; return realSync(parent) }
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err != nil || outcome != upgradeJournalAppliedDurable || calls != 1 {
			t.Fatalf("create = %q, %v, sync calls=%d", outcome, err, calls)
		}
	})

	t.Run("fsync failure is indeterminate and preserves final", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		ops := realUpgradeJournalCreateOps()
		ops.syncParent = func(*os.File) error { return errors.New("injected directory sync failure") }
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalIndeterminate {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		assertCreateResidue(t, err, true)
		if _, statErr := os.Lstat(upgradeJournalPath(o)); statErr != nil {
			t.Fatalf("final missing: %v", statErr)
		}
	})

	t.Run("parent replacement during fsync is indeterminate with detached final", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		parentPath := filepath.Dir(upgradeJournalPath(o))
		detachedPath := parentPath + ".during-sync"
		ops := realUpgradeJournalCreateOps()
		ops.syncParent = func(parent *os.File) error {
			if err := parent.Sync(); err != nil {
				return err
			}
			if err := os.Rename(parentPath, detachedPath); err != nil {
				return err
			}
			return os.Mkdir(parentPath, 0o700)
		}
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalIndeterminate {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		assertCreateResidue(t, err, true)
		if _, statErr := os.Lstat(filepath.Join(detachedPath, upgradeJournalName)); statErr != nil {
			t.Fatalf("detached final missing: %v", statErr)
		}
	})

	t.Run("post-fsync mutation is rejected before AppliedDurable", func(t *testing.T) {
		o := setupUpgradeJournalRoot(t)
		ops := realUpgradeJournalCreateOps()
		ops.syncParent = func(parent *os.File) error {
			if err := parent.Sync(); err != nil {
				return err
			}
			return unix.Fchmodat(int(parent.Fd()), upgradeJournalName, 0o600, 0)
		}
		outcome, err := createUpgradeTransactionJournalWithOps(o, journal, ops)
		if err == nil || outcome != upgradeJournalIndeterminate {
			t.Fatalf("create = %q, %v", outcome, err)
		}
		assertCreateResidue(t, err, true)
	})
}
