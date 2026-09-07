//go:build linux

package linuxinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"

	"agent-forge/internal/configjson"
)

const upgradeJournalFormatVersion = 1

type upgradeJournalPhase string

const (
	upgradePhasePreparedSnapshotDurable  upgradeJournalPhase = "prepared_snapshot_durable"
	upgradePhaseCandidateMayHaveMigrated upgradeJournalPhase = "candidate_may_have_migrated"
	upgradePhaseRestoreInProgress        upgradeJournalPhase = "restore_in_progress"
	upgradePhaseRestoreAppliedDurable    upgradeJournalPhase = "restore_applied_durable"
	upgradePhaseRestoreNotAppliedDurable upgradeJournalPhase = "restore_not_applied_durable"
	upgradePhaseRestoreIndeterminate     upgradeJournalPhase = "restore_indeterminate"
	upgradePhaseCandidateReady           upgradeJournalPhase = "candidate_ready"
	upgradePhaseOldReady                 upgradeJournalPhase = "old_ready"
)

type upgradeRestoreOutcome string

const (
	upgradeRestoreAppliedDurable    upgradeRestoreOutcome = "applied_durable"
	upgradeRestoreNotAppliedDurable upgradeRestoreOutcome = "not_applied_durable"
	upgradeRestoreIndeterminate     upgradeRestoreOutcome = "indeterminate"
)

type upgradeTransactionJournal struct {
	FormatVersion         int                   `json:"format_version"`
	TransactionID         string                `json:"transaction_id"`
	SourceVersion         string                `json:"source_version"`
	TargetVersion         string                `json:"target_version"`
	TargetCommit          string                `json:"target_commit"`
	StoreSchemaVersion    int                   `json:"store_schema_version"`
	SourceReceiptSHA256   string                `json:"source_receipt_sha256"`
	AccountUID            int                   `json:"account_uid"`
	AccountGID            int                   `json:"account_gid"`
	DatabaseRelativePath  string                `json:"database_relative_path"`
	SnapshotRelativePath  string                `json:"snapshot_relative_path"`
	SnapshotSchemaVersion int                   `json:"snapshot_schema_version"`
	SnapshotSize          int64                 `json:"snapshot_size"`
	SnapshotSHA256        string                `json:"snapshot_sha256"`
	Phase                 upgradeJournalPhase   `json:"phase"`
	RestoreOutcome        upgradeRestoreOutcome `json:"restore_outcome,omitempty"`
	RestoreResidue        *bool                 `json:"restore_residue,omitempty"`
}

var (
	upgradeJournalVersionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	upgradeJournalCommitRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	upgradeJournalDigestRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func encodeUpgradeTransactionJournal(journal upgradeTransactionJournal) ([]byte, error) {
	if !validUpgradeTransactionJournal(journal) {
		return nil, errors.New("invalid upgrade transaction journal")
	}
	body, err := json.Marshal(journal)
	if err != nil {
		return nil, errors.New("invalid upgrade transaction journal")
	}
	return body, nil
}

func decodeUpgradeTransactionJournal(body []byte) (upgradeTransactionJournal, error) {
	var journal upgradeTransactionJournal
	if configjson.Decode(body, &journal) != nil || !validUpgradeTransactionJournal(journal) {
		return upgradeTransactionJournal{}, errors.New("corrupt upgrade transaction journal")
	}
	canonical, err := encodeUpgradeTransactionJournal(journal)
	if err != nil || !bytes.Equal(canonical, body) {
		return upgradeTransactionJournal{}, errors.New("corrupt upgrade transaction journal")
	}
	return journal, nil
}

func validUpgradeTransactionJournal(journal upgradeTransactionJournal) bool {
	if journal.FormatVersion != upgradeJournalFormatVersion ||
		!upgradeJournalDigestRE.MatchString(journal.TransactionID) ||
		!upgradeJournalVersionRE.MatchString(journal.SourceVersion) ||
		!upgradeJournalVersionRE.MatchString(journal.TargetVersion) ||
		!upgradeJournalCommitRE.MatchString(journal.TargetCommit) ||
		journal.StoreSchemaVersion <= 0 ||
		!upgradeJournalDigestRE.MatchString(journal.SourceReceiptSHA256) ||
		journal.AccountUID < 0 || uint64(journal.AccountUID) >= uint64(0xffffffff) ||
		journal.AccountGID < 0 || uint64(journal.AccountGID) >= uint64(0xffffffff) ||
		journal.DatabaseRelativePath != "var/gate/state/forge.db" ||
		journal.SnapshotRelativePath != "var/gate/state/.forge-pre-migration-"+journal.TransactionID+".db" ||
		journal.SnapshotSchemaVersion <= 0 || journal.SnapshotSchemaVersion > journal.StoreSchemaVersion ||
		journal.SnapshotSize <= 0 ||
		!upgradeJournalDigestRE.MatchString(journal.SnapshotSHA256) {
		return false
	}

	switch journal.Phase {
	case upgradePhasePreparedSnapshotDurable, upgradePhaseCandidateMayHaveMigrated, upgradePhaseRestoreInProgress, upgradePhaseCandidateReady, upgradePhaseOldReady:
		return journal.RestoreOutcome == "" && journal.RestoreResidue == nil
	case upgradePhaseRestoreAppliedDurable:
		return journal.RestoreOutcome == upgradeRestoreAppliedDurable && journal.RestoreResidue != nil
	case upgradePhaseRestoreNotAppliedDurable:
		return journal.RestoreOutcome == upgradeRestoreNotAppliedDurable && journal.RestoreResidue != nil
	case upgradePhaseRestoreIndeterminate:
		return journal.RestoreOutcome == upgradeRestoreIndeterminate && journal.RestoreResidue != nil
	default:
		return false
	}
}

func validateUpgradeJournalTransition(from, to upgradeJournalPhase) error {
	valid := from == upgradePhasePreparedSnapshotDurable && (to == upgradePhaseCandidateMayHaveMigrated || to == upgradePhaseCandidateReady) ||
		from == upgradePhaseCandidateMayHaveMigrated && (to == upgradePhaseRestoreInProgress || to == upgradePhaseCandidateReady) ||
		from == upgradePhaseRestoreInProgress && (to == upgradePhaseRestoreAppliedDurable || to == upgradePhaseRestoreNotAppliedDurable || to == upgradePhaseRestoreIndeterminate) ||
		from == upgradePhaseRestoreAppliedDurable && to == upgradePhaseOldReady
	if !valid {
		return errors.New("invalid upgrade journal phase transition")
	}
	return nil
}
