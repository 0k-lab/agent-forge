//go:build linux

package linuxinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

		unclassified := filepath.Join(filepath.Dir(wantPath), ".upgrade-transaction.json.tmp-unclassified")
		if err := os.WriteFile(unclassified, []byte("leave me"), 0o400); err != nil {
			t.Fatal(err)
		}
		outcome, err = createUpgradeTransactionJournal(o, journal)
		if err == nil || outcome != upgradeJournalNotAppliedDurable {
			t.Fatalf("second create = %q, %v", outcome, err)
		}
		if got, err := os.ReadFile(wantPath); err != nil || string(got) != string(canonical) {
			t.Fatalf("existing journal changed = %q, %v", got, err)
		}
		if got, err := os.ReadFile(unclassified); err != nil || string(got) != "leave me" {
			t.Fatalf("unclassified temp changed = %q, %v", got, err)
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
