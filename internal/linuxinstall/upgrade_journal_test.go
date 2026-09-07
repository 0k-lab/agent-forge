//go:build linux

package linuxinstall

import (
	"bytes"
	"strings"
	"testing"
)

func TestUpgradeTransactionJournalContract(t *testing.T) {
	id := strings.Repeat("a", 64)
	digest := strings.Repeat("b", 64)
	journal := upgradeTransactionJournal{
		FormatVersion:         1,
		TransactionID:         id,
		SourceVersion:         "v1.2.3",
		TargetVersion:         "v1.2.4",
		TargetCommit:          strings.Repeat("c", 40),
		StoreSchemaVersion:    5,
		SourceReceiptSHA256:   digest,
		AccountUID:            1000,
		AccountGID:            1001,
		DatabaseRelativePath:  "var/gate/state/forge.db",
		SnapshotRelativePath:  "var/gate/state/.forge-pre-migration-" + id + ".db",
		SnapshotSchemaVersion: 4,
		SnapshotSize:          4096,
		SnapshotSHA256:        strings.Repeat("d", 64),
		Phase:                 upgradePhasePreparedSnapshotDurable,
	}
	body, err := encodeUpgradeTransactionJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"format_version":1,"transaction_id":"` + id + `","source_version":"v1.2.3","target_version":"v1.2.4","target_commit":"` + strings.Repeat("c", 40) + `","store_schema_version":5,"source_receipt_sha256":"` + digest + `","account_uid":1000,"account_gid":1001,"database_relative_path":"var/gate/state/forge.db","snapshot_relative_path":"var/gate/state/.forge-pre-migration-` + id + `.db","snapshot_schema_version":4,"snapshot_size":4096,"snapshot_sha256":"` + strings.Repeat("d", 64) + `","phase":"prepared_snapshot_durable"}`
	if string(body) != want {
		t.Fatalf("canonical journal = %s", body)
	}
	decoded, err := decodeUpgradeTransactionJournal(body)
	if err != nil || decoded != journal {
		t.Fatalf("decoded journal = %#v, %v", decoded, err)
	}
	reencoded, err := encodeUpgradeTransactionJournal(decoded)
	if err != nil || !bytes.Equal(reencoded, body) {
		t.Fatalf("round trip = %s, %v", reencoded, err)
	}

	invalidBodies := map[string][]byte{
		"duplicate":             []byte(strings.Replace(want, `"format_version":1`, `"format_version":1,"format_version":1`, 1)),
		"unknown":               []byte(strings.Replace(want, `"phase":`, `"unknown":true,"phase":`, 1)),
		"null":                  []byte(strings.Replace(want, `"source_version":"v1.2.3"`, `"source_version":null`, 1)),
		"missing":               []byte(strings.Replace(want, `"source_version":"v1.2.3",`, ``, 1)),
		"trailing":              append(append([]byte(nil), body...), []byte(`{}`)...),
		"noncanonical":          append(append([]byte(nil), body...), '\n'),
		"uppercase transaction": []byte(strings.Replace(want, id, strings.Repeat("A", 64), 1)),
	}
	for name, candidate := range invalidBodies {
		t.Run("decode "+name, func(t *testing.T) {
			if _, err := decodeUpgradeTransactionJournal(candidate); err == nil {
				t.Fatal("accepted invalid journal")
			}
		})
	}

	invalidValues := map[string]func(*upgradeTransactionJournal){
		"format":          func(j *upgradeTransactionJournal) { j.FormatVersion = 2 },
		"transaction":     func(j *upgradeTransactionJournal) { j.TransactionID = strings.Repeat("a", 63) },
		"source version":  func(j *upgradeTransactionJournal) { j.SourceVersion = "1.2.3" },
		"target version":  func(j *upgradeTransactionJournal) { j.TargetVersion = "v01.2.4" },
		"commit":          func(j *upgradeTransactionJournal) { j.TargetCommit = strings.Repeat("C", 40) },
		"store schema":    func(j *upgradeTransactionJournal) { j.StoreSchemaVersion = 0 },
		"receipt digest":  func(j *upgradeTransactionJournal) { j.SourceReceiptSHA256 = strings.Repeat("b", 63) },
		"uid":             func(j *upgradeTransactionJournal) { j.AccountUID = -1 },
		"gid":             func(j *upgradeTransactionJournal) { j.AccountGID = -1 },
		"database path":   func(j *upgradeTransactionJournal) { j.DatabaseRelativePath = "/var/gate/state/forge.db" },
		"snapshot path":   func(j *upgradeTransactionJournal) { j.SnapshotRelativePath = "var/gate/state/snapshot.db" },
		"snapshot schema": func(j *upgradeTransactionJournal) { j.SnapshotSchemaVersion = 6 },
		"snapshot size":   func(j *upgradeTransactionJournal) { j.SnapshotSize = 0 },
		"snapshot digest": func(j *upgradeTransactionJournal) { j.SnapshotSHA256 = strings.Repeat("D", 64) },
		"phase":           func(j *upgradeTransactionJournal) { j.Phase = "prepared" },
	}
	for name, mutate := range invalidValues {
		t.Run("encode "+name, func(t *testing.T) {
			candidate := journal
			mutate(&candidate)
			if _, err := encodeUpgradeTransactionJournal(candidate); err == nil {
				t.Fatal("accepted invalid journal")
			}
		})
	}

	falseValue, trueValue := false, true
	restoreCases := []struct {
		phase   upgradeJournalPhase
		outcome upgradeRestoreOutcome
		residue *bool
		valid   bool
	}{
		{upgradePhaseRestoreInProgress, "", nil, true},
		{upgradePhaseRestoreInProgress, upgradeRestoreAppliedDurable, nil, false},
		{upgradePhaseRestoreAppliedDurable, upgradeRestoreAppliedDurable, &falseValue, true},
		{upgradePhaseRestoreAppliedDurable, upgradeRestoreAppliedDurable, &trueValue, true},
		{upgradePhaseRestoreAppliedDurable, upgradeRestoreNotAppliedDurable, &falseValue, false},
		{upgradePhaseRestoreNotAppliedDurable, upgradeRestoreNotAppliedDurable, nil, true},
		{upgradePhaseRestoreIndeterminate, upgradeRestoreIndeterminate, nil, true},
		{upgradePhaseCandidateReady, upgradeRestoreAppliedDurable, nil, false},
		{upgradePhaseRestoreAppliedDurable, upgradeRestoreAppliedDurable, nil, false},
		{upgradePhaseRestoreNotAppliedDurable, upgradeRestoreNotAppliedDurable, &falseValue, false},
	}
	for _, test := range restoreCases {
		candidate := journal
		candidate.Phase, candidate.RestoreOutcome, candidate.RestoreResidue = test.phase, test.outcome, test.residue
		_, err := encodeUpgradeTransactionJournal(candidate)
		if (err == nil) != test.valid {
			t.Errorf("restore fields (%s, %s, %v): %v", test.phase, test.outcome, test.residue, err)
		}
	}

	allowed := map[upgradeJournalPhase][]upgradeJournalPhase{
		upgradePhasePreparedSnapshotDurable:  {upgradePhaseCandidateMayHaveMigrated, upgradePhaseCandidateReady},
		upgradePhaseCandidateMayHaveMigrated: {upgradePhaseRestoreInProgress, upgradePhaseCandidateReady},
		upgradePhaseRestoreInProgress:        {upgradePhaseRestoreAppliedDurable, upgradePhaseRestoreNotAppliedDurable, upgradePhaseRestoreIndeterminate},
		upgradePhaseRestoreAppliedDurable:    {upgradePhaseOldReady},
	}
	phases := []upgradeJournalPhase{upgradePhasePreparedSnapshotDurable, upgradePhaseCandidateMayHaveMigrated, upgradePhaseRestoreInProgress, upgradePhaseRestoreAppliedDurable, upgradePhaseRestoreNotAppliedDurable, upgradePhaseRestoreIndeterminate, upgradePhaseCandidateReady, upgradePhaseOldReady}
	for _, from := range phases {
		for _, to := range phases {
			wantAllowed := false
			for _, candidate := range allowed[from] {
				wantAllowed = wantAllowed || candidate == to
			}
			if err := validateUpgradeJournalTransition(from, to); (err == nil) != wantAllowed {
				t.Errorf("transition %s -> %s: %v", from, to, err)
			}
		}
	}
}
