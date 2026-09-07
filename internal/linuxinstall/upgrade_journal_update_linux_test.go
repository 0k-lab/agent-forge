//go:build linux

package linuxinstall

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCompareAndSwapUpgradeJournalContract(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged O_TMPFILE publication requires a non-root test process")
	}
	expected := testUpgradeJournal(t)
	next := expected
	next.Phase = upgradePhaseCandidateMayHaveMigrated
	o := setupUpgradeJournalRoot(t)
	if outcome, err := createUpgradeTransactionJournal(o, expected); err != nil || outcome != upgradeJournalAppliedDurable {
		t.Fatalf("create = %q, %v", outcome, err)
	}

	outcome, err := compareAndSwapUpgradeTransactionJournal(o, expected, next)
	if err != nil || outcome != upgradeJournalUpdateAppliedDurable {
		t.Fatalf("compare-and-swap = %q, %v", outcome, err)
	}
	if got, err := readUpgradeTransactionJournal(o); err != nil || got != next {
		t.Fatalf("read = %#v, %v", got, err)
	}
}

func TestCompareAndSwapUpgradeJournalRejectsBeforeMutation(t *testing.T) {
	expected := testUpgradeJournal(t)
	validNext := expected
	validNext.Phase = upgradePhaseCandidateMayHaveMigrated
	for name, mutate := range map[string]func(*upgradeTransactionJournal, *upgradeTransactionJournal){
		"self transition": func(_, next *upgradeTransactionJournal) { next.Phase = expected.Phase },
		"transaction drift": func(_, next *upgradeTransactionJournal) {
			next.TransactionID = "a" + next.TransactionID[1:]
			next.SnapshotRelativePath = "var/gate/state/.forge-pre-migration-" + next.TransactionID + ".db"
		},
		"immutable drift": func(_, next *upgradeTransactionJournal) { next.TargetVersion = "v9.9.9" },
		"invalid restore fields": func(_, next *upgradeTransactionJournal) {
			next.RestoreOutcome = upgradeRestoreAppliedDurable
		},
		"invalid phase": func(_, next *upgradeTransactionJournal) { next.Phase = upgradePhaseOldReady },
	} {
		t.Run(name, func(t *testing.T) {
			next := validNext
			mutate(&expected, &next)
			called := false
			outcome, err := compareAndSwapUpgradeTransactionJournalWithOps(Options{}, expected, next, upgradeJournalUpdateOps{
				openParent: func(Options) (*os.File, error) { called = true; return nil, errors.New("must not open") },
			})
			if err == nil || outcome != upgradeJournalUpdateNotAppliedDurable || called {
				t.Fatalf("compare-and-swap = %q, %v, opened=%v", outcome, err, called)
			}
		})
	}

	t.Run("stale and malformed current journal", func(t *testing.T) {
		for name, body := range map[string][]byte{
			"stale phase": func() []byte { encoded, _ := encodeUpgradeTransactionJournal(validNext); return encoded }(),
			"malformed":   []byte("{}"),
		} {
			t.Run(name, func(t *testing.T) {
				o := setupUpgradeJournalRoot(t)
				if err := os.WriteFile(upgradeJournalPath(o), body, 0o400); err != nil {
					t.Fatal(err)
				}
				before := append([]byte(nil), body...)
				outcome, err := compareAndSwapUpgradeTransactionJournal(o, expected, validNext)
				if err == nil || outcome == upgradeJournalUpdateAppliedDurable {
					t.Fatalf("compare-and-swap = %q, %v", outcome, err)
				}
				after, readErr := os.ReadFile(upgradeJournalPath(o))
				if readErr != nil || string(after) != string(before) {
					t.Fatalf("current journal mutated: %q, %v", after, readErr)
				}
			})
		}
	})
}

func TestCompareAndSwapUpgradeJournalFailureBoundaries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged O_TMPFILE publication requires a non-root test process")
	}
	expected := testUpgradeJournal(t)
	next := expected
	next.Phase = upgradePhaseCandidateMayHaveMigrated

	for name, arrange := range map[string]func(*upgradeJournalUpdateOps){
		"O_TMPFILE unsupported": func(ops *upgradeJournalUpdateOps) {
			ops.openUnnamed = func(*os.File) (*os.File, error) { return nil, unix.EOPNOTSUPP }
		},
		"link unsupported": func(ops *upgradeJournalUpdateOps) {
			ops.publish = func(*os.File, *os.File, string) error { return unix.EOPNOTSUPP }
		},
		"pre-exchange fsync failure": func(ops *upgradeJournalUpdateOps) {
			ops.syncParent = func(*os.File) error { return errors.New("injected fsync failure") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := writeUpgradeJournalForUpdate(t, expected)
			ops := defaultUpgradeJournalUpdateOps()
			arrange(&ops)
			outcome, err := compareAndSwapUpgradeTransactionJournalWithOps(o, expected, next, ops)
			if err == nil || outcome == upgradeJournalUpdateAppliedDurable {
				t.Fatalf("compare-and-swap = %q, %v", outcome, err)
			}
			assertUpgradeJournalBody(t, upgradeJournalPath(o), expected)
		})
	}

	t.Run("link effect then error preserves witness", func(t *testing.T) {
		o := writeUpgradeJournalForUpdate(t, expected)
		ops := defaultUpgradeJournalUpdateOps()
		realPublish := ops.publish
		ops.publish = func(parent, source *os.File, name string) error {
			if err := realPublish(parent, source, name); err != nil {
				return err
			}
			return errors.New("injected error after link")
		}
		outcome, err := compareAndSwapUpgradeTransactionJournalWithOps(o, expected, next, ops)
		if err == nil || outcome == upgradeJournalUpdateAppliedDurable {
			t.Fatalf("compare-and-swap = %q, %v", outcome, err)
		}
		witness, _ := upgradeJournalUpdateNames(expected, next)
		assertUpgradeJournalBody(t, filepath.Join(filepath.Dir(upgradeJournalPath(o)), witness), expected)
		assertUpgradeJournalBody(t, upgradeJournalPath(o), expected)
	})
}

func TestCompareAndSwapUpgradeJournalExchangeUncertaintyAndRecovery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged O_TMPFILE publication requires a non-root test process")
	}
	expected := testUpgradeJournal(t)
	next := expected
	next.Phase = upgradePhaseCandidateMayHaveMigrated

	for _, effect := range []bool{false, true} {
		t.Run(map[bool]string{false: "exchange no-effect error", true: "real exchange followed by error"}[effect], func(t *testing.T) {
			o := writeUpgradeJournalForUpdate(t, expected)
			ops := defaultUpgradeJournalUpdateOps()
			realExchange := ops.exchange
			ops.exchange = func(parent *os.File, a, b string) error {
				if effect {
					if err := realExchange(parent, a, b); err != nil {
						return err
					}
				}
				return errors.New("injected exchange error")
			}
			outcome, err := compareAndSwapUpgradeTransactionJournalWithOps(o, expected, next, ops)
			if err == nil || outcome != upgradeJournalUpdateIndeterminate {
				t.Fatalf("compare-and-swap = %q, %v", outcome, err)
			}
			witness, stage := upgradeJournalUpdateNames(expected, next)
			assertUpgradeJournalBody(t, filepath.Join(filepath.Dir(upgradeJournalPath(o)), witness), expected)
			if effect {
				assertUpgradeJournalBody(t, upgradeJournalPath(o), next)
				assertUpgradeJournalBody(t, filepath.Join(filepath.Dir(upgradeJournalPath(o)), stage), expected)
			} else {
				assertUpgradeJournalBody(t, upgradeJournalPath(o), expected)
				assertUpgradeJournalBody(t, filepath.Join(filepath.Dir(upgradeJournalPath(o)), stage), next)
			}

			outcome, err = compareAndSwapUpgradeTransactionJournal(o, expected, next)
			if err != nil || outcome != upgradeJournalUpdateAppliedDurable {
				t.Fatalf("recovery = %q, %v", outcome, err)
			}
			assertUpgradeJournalBody(t, upgradeJournalPath(o), next)
		})
	}

	t.Run("pre-exchange recovery sync failure does not exchange", func(t *testing.T) {
		o := writeUpgradeJournalForUpdate(t, expected)
		witness, stage := upgradeJournalUpdateNames(expected, next)
		parentPath := filepath.Dir(upgradeJournalPath(o))
		if err := os.Link(upgradeJournalPath(o), filepath.Join(parentPath, witness)); err != nil {
			t.Fatal(err)
		}
		nextBody, err := encodeUpgradeTransactionJournal(next)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parentPath, stage), nextBody, 0o400); err != nil {
			t.Fatal(err)
		}

		ops := defaultUpgradeJournalUpdateOps()
		exchangeCalled := false
		ops.syncParent = func(*os.File) error { return errors.New("injected recovery fsync failure") }
		ops.exchange = func(*os.File, string, string) error {
			exchangeCalled = true
			return errors.New("RENAME_EXCHANGE called before recovery parent sync")
		}
		outcome, err := compareAndSwapUpgradeTransactionJournalWithOps(o, expected, next, ops)
		var updateErr *upgradeJournalUpdateError
		if err == nil || outcome != upgradeJournalUpdateNotAppliedDurable || !errors.As(err, &updateErr) || !updateErr.Residue || exchangeCalled {
			t.Fatalf("recovery = %q, %v; residue=%v, exchange called=%v", outcome, err, updateErr != nil && updateErr.Residue, exchangeCalled)
		}
		assertUpgradeJournalBody(t, upgradeJournalPath(o), expected)
		assertUpgradeJournalBody(t, filepath.Join(parentPath, witness), expected)
		assertUpgradeJournalBody(t, filepath.Join(parentPath, stage), next)
	})

	t.Run("post-exchange fsync failure remains recoverable", func(t *testing.T) {
		o := writeUpgradeJournalForUpdate(t, expected)
		ops := defaultUpgradeJournalUpdateOps()
		realSync := ops.syncParent
		calls := 0
		ops.syncParent = func(parent *os.File) error {
			calls++
			if calls == 3 {
				return errors.New("injected post-exchange fsync failure")
			}
			return realSync(parent)
		}
		outcome, err := compareAndSwapUpgradeTransactionJournalWithOps(o, expected, next, ops)
		if err == nil || outcome != upgradeJournalUpdateIndeterminate {
			t.Fatalf("compare-and-swap = %q, %v", outcome, err)
		}
		assertUpgradeJournalBody(t, upgradeJournalPath(o), next)
		if outcome, err = compareAndSwapUpgradeTransactionJournal(o, expected, next); err != nil || outcome != upgradeJournalUpdateAppliedDurable {
			t.Fatalf("recovery = %q, %v", outcome, err)
		}
	})
}

func TestCompareAndSwapUpgradeJournalPreservesUnrelatedStage(t *testing.T) {
	expected := testUpgradeJournal(t)
	next := expected
	next.Phase = upgradePhaseCandidateMayHaveMigrated
	o := writeUpgradeJournalForUpdate(t, expected)
	_, stage := upgradeJournalUpdateNames(expected, next)
	stagePath := filepath.Join(filepath.Dir(upgradeJournalPath(o)), stage)
	unrelated := []byte("unrelated stage")
	if err := os.WriteFile(stagePath, unrelated, 0o400); err != nil {
		t.Fatal(err)
	}
	outcome, err := compareAndSwapUpgradeTransactionJournal(o, expected, next)
	if err == nil || outcome == upgradeJournalUpdateAppliedDurable {
		t.Fatalf("compare-and-swap = %q, %v", outcome, err)
	}
	if got, readErr := os.ReadFile(stagePath); readErr != nil || string(got) != string(unrelated) {
		t.Fatalf("unrelated stage = %q, %v", got, readErr)
	}
	assertUpgradeJournalBody(t, upgradeJournalPath(o), expected)
}

func TestCompareAndSwapUpgradeJournalSubstitutionRacesPreserveEveryInode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged O_TMPFILE publication requires a non-root test process")
	}
	expected := testUpgradeJournal(t)
	next := expected
	next.Phase = upgradePhaseCandidateMayHaveMigrated
	unrelated := []byte("unexpected journal inode")

	for _, racedName := range []string{upgradeJournalName, "stage"} {
		t.Run(racedName+" substitution", func(t *testing.T) {
			o := writeUpgradeJournalForUpdate(t, expected)
			parentPath := filepath.Dir(upgradeJournalPath(o))
			decoyName := ".unrelated-racer"
			decoyPath := filepath.Join(parentPath, decoyName)
			if err := os.WriteFile(decoyPath, unrelated, 0o400); err != nil {
				t.Fatal(err)
			}
			ops := defaultUpgradeJournalUpdateOps()
			realExchange := ops.exchange
			ops.exchange = func(parent *os.File, stage, final string) error {
				target := final
				if racedName == "stage" {
					target = stage
				}
				if err := unix.Renameat2(int(parent.Fd()), decoyName, int(parent.Fd()), target, unix.RENAME_EXCHANGE); err != nil {
					return err
				}
				return realExchange(parent, stage, final)
			}
			outcome, err := compareAndSwapUpgradeTransactionJournalWithOps(o, expected, next, ops)
			if err == nil || outcome != upgradeJournalUpdateIndeterminate {
				t.Fatalf("compare-and-swap = %q, %v", outcome, err)
			}
			_, stage := upgradeJournalUpdateNames(expected, next)
			if racedName == upgradeJournalName {
				if got, readErr := os.ReadFile(filepath.Join(parentPath, stage)); readErr != nil || string(got) != string(unrelated) {
					t.Fatalf("displaced final = %q, %v", got, readErr)
				}
				assertUpgradeJournalBody(t, decoyPath, expected)
			} else {
				if got, readErr := os.ReadFile(upgradeJournalPath(o)); readErr != nil || string(got) != string(unrelated) {
					t.Fatalf("displaced stage = %q, %v", got, readErr)
				}
				assertUpgradeJournalBody(t, decoyPath, next)
			}
		})
	}

	t.Run("parent substitution", func(t *testing.T) {
		o := writeUpgradeJournalForUpdate(t, expected)
		parentPath := filepath.Dir(upgradeJournalPath(o))
		detachedPath := parentPath + ".detached-update"
		ops := defaultUpgradeJournalUpdateOps()
		realExchange := ops.exchange
		ops.exchange = func(parent *os.File, stage, final string) error {
			if err := os.Rename(parentPath, detachedPath); err != nil {
				return err
			}
			if err := os.Mkdir(parentPath, 0o700); err != nil {
				return err
			}
			return realExchange(parent, stage, final)
		}
		outcome, err := compareAndSwapUpgradeTransactionJournalWithOps(o, expected, next, ops)
		if err == nil || outcome != upgradeJournalUpdateIndeterminate {
			t.Fatalf("compare-and-swap = %q, %v", outcome, err)
		}
		assertUpgradeJournalBody(t, filepath.Join(detachedPath, upgradeJournalName), next)
		witness, stage := upgradeJournalUpdateNames(expected, next)
		assertUpgradeJournalBody(t, filepath.Join(detachedPath, witness), expected)
		assertUpgradeJournalBody(t, filepath.Join(detachedPath, stage), expected)
	})
}

func writeUpgradeJournalForUpdate(t *testing.T, journal upgradeTransactionJournal) Options {
	t.Helper()
	o := setupUpgradeJournalRoot(t)
	body, err := encodeUpgradeTransactionJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upgradeJournalPath(o), body, 0o400); err != nil {
		t.Fatal(err)
	}
	return o
}

func assertUpgradeJournalBody(t *testing.T, path string, journal upgradeTransactionJournal) {
	t.Helper()
	want, _ := encodeUpgradeTransactionJournal(journal)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("%s = %q, %v", path, got, err)
	}
}
