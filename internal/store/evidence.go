package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agent-forge/internal/protocol"
)

const (
	maxEvidenceRecords     = protocol.MaxEvidenceRecordsPerBatch
	maxEvidenceOutputBytes = protocol.MaxEvidenceOutputBytes
	maxEvidenceTotalOutput = 64 << 10
	maxEvidenceBatchJSON   = protocol.MaxEvidenceBatchBytes
)

func (s *Store) BindEvidenceAt(jobID, attemptID, workerID string, records []protocol.AttemptEvidence, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskJSON string
	var deadline int64
	if err := tx.QueryRow(`SELECT j.task_json,a.deadline_at FROM jobs j JOIN attempts a ON a.id=j.attempt_id AND a.job_id=j.id
		WHERE j.id=? AND j.status='leased' AND j.attempt_id=? AND j.worker_id=? AND a.status='leased' AND a.worker_id=?`,
		jobID, attemptID, workerID, workerID).Scan(&taskJSON, &deadline); err != nil {
		return errors.New("attempt is not active")
	}
	if taskJSON == "" {
		return errors.New("evidence requires a coding task")
	}
	if len(records) == 0 {
		return errors.New("evidence batch is empty")
	}
	if at.UnixNano() >= deadline {
		return errors.New("attempt lease expired")
	}
	var task protocol.CodingTask
	if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
		return errors.New("invalid coding task binding")
	}
	if len(records) > maxEvidenceRecords {
		return errors.New("too many evidence records")
	}
	for i := range records {
		if err := validateEvidence(records[i], task); err != nil {
			return err
		}
	}
	batchJSON, err := json.Marshal(records)
	if err != nil || len(batchJSON) > maxEvidenceBatchJSON {
		return errors.New("evidence batch exceeds limit")
	}
	var storedCount, storedOutput int
	if err := tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(length(CAST(output AS BLOB))),0) FROM attempt_evidence WHERE attempt_id=?`, attemptID).Scan(&storedCount, &storedOutput); err != nil {
		return err
	}
	for _, record := range records {
		record, err = prepareEvidence(record, task)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		payloadHash := sha256.Sum256(payload)
		argvJSON, err := json.Marshal(record.Argv)
		if err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT INTO attempt_evidence(job_id,attempt_id,evidence_id,phase,reason,check_index,exit_code,duration_ms,output,output_redacted,output_truncated,base_sha,candidate_sha,argv_json,argv_redacted,payload_hash,bound_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(attempt_id,evidence_id) DO NOTHING`, jobID, attemptID, record.EvidenceID, record.Phase, record.Reason, record.CheckIndex, record.ExitCode,
			record.DurationMS, record.Output, record.OutputRedacted, record.OutputTruncated, record.BaseSHA, record.CandidateSHA, string(argvJSON), record.ArgvRedacted, payloadHash[:], at.UnixNano())
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			var storedHash []byte
			err := tx.QueryRow(`SELECT payload_hash FROM attempt_evidence WHERE attempt_id=? AND evidence_id=?`, attemptID, record.EvidenceID).Scan(&storedHash)
			if err != nil || !bytes.Equal(storedHash, payloadHash[:]) {
				return errors.New("conflicting evidence replay")
			}
			continue
		}
		storedCount++
		storedOutput += len(record.Output)
		if storedCount > maxEvidenceRecords || storedOutput > maxEvidenceTotalOutput {
			return errors.New("attempt evidence exceeds limit")
		}
	}
	return tx.Commit()
}

func prepareEvidence(record protocol.AttemptEvidence, task protocol.CodingTask) (protocol.AttemptEvidence, error) {
	if record.Phase == protocol.EvidencePhaseScopedCheck {
		if record.CheckIndex == nil || *record.CheckIndex < 0 || *record.CheckIndex >= len(task.Tests) {
			return record, errors.New("invalid evidence check index")
		}
		record.Argv, record.ArgvRedacted = redactArgv(task.Tests[*record.CheckIndex])
		body, err := json.Marshal(record.Argv)
		if err != nil || len(body) > 4096 {
			return record, errors.New("rendered evidence argv exceeds limit")
		}
	} else if record.CheckIndex != nil {
		return record, errors.New("check index requires scoped check evidence")
	}
	if record.Output == "" {
		if record.OutputRedacted || record.OutputTruncated {
			return record, errors.New("inconsistent empty evidence output")
		}
	} else if record.Output != protocol.EvidenceRedactedMarker || !record.OutputRedacted {
		return record, errors.New("evidence output must be fixed redaction marker")
	}
	return record, nil
}

func redactArgv(argv []string) ([]string, bool) {
	out := make([]string, len(argv))
	for i := range out {
		out[i] = protocol.EvidenceRedactedMarker
	}
	return out, len(out) != 0
}

func validateEvidence(record protocol.AttemptEvidence, task protocol.CodingTask) error {
	if !lowerHex(record.EvidenceID, 32) {
		return errors.New("invalid evidence ID")
	}
	allowed := false
	switch record.Phase {
	case protocol.EvidencePhasePreparation:
		allowed = record.Reason == protocol.EvidenceReasonPreparationFailed || record.Reason == protocol.EvidenceReasonInvalidTask || record.Reason == protocol.EvidenceReasonInvalidRepository ||
			record.Reason == protocol.EvidenceReasonRuntimeSetupFailed || record.Reason == protocol.EvidenceReasonWorktreeSetupFailed
	case protocol.EvidencePhasePlugin:
		allowed = record.Reason == protocol.EvidenceReasonPluginFailed || record.Reason == protocol.EvidenceReasonPluginStartFailed ||
			record.Reason == protocol.EvidenceReasonPluginProtocolFailed || record.Reason == protocol.EvidenceReasonPluginReportedFailure
	case protocol.EvidencePhaseWorkspaceValidation:
		allowed = record.Reason == protocol.EvidenceReasonNoChanges || record.Reason == protocol.EvidenceReasonInvalidWorkspaceChange
	case protocol.EvidencePhaseScopedCheck:
		allowed = record.Reason == protocol.EvidenceReasonScopedCheckPassed || record.Reason == protocol.EvidenceReasonScopedCheckFailed || record.Reason == protocol.EvidenceReasonScopedCheckTimeout
	case protocol.EvidencePhaseCandidateCommit:
		allowed = record.Reason == protocol.EvidenceReasonCandidateCommitFailed
	case protocol.EvidencePhaseCleanup:
		allowed = record.Reason == protocol.EvidenceReasonCleanupFailed
	}
	if !allowed {
		return errors.New("invalid evidence phase or reason")
	}
	maxDuration := int64((15 * time.Minute) / time.Millisecond)
	if record.Phase == protocol.EvidencePhaseScopedCheck {
		maxDuration = int64((10 * time.Minute) / time.Millisecond)
	} else if record.Phase == protocol.EvidencePhaseCleanup {
		maxDuration = int64((10 * time.Second) / time.Millisecond)
	}
	if record.DurationMS < 0 || record.DurationMS > maxDuration {
		return errors.New("invalid evidence duration")
	}
	if len(record.Output) > maxEvidenceOutputBytes {
		return errors.New("invalid evidence output")
	}
	if record.ExitCode != nil && (record.Phase != protocol.EvidencePhaseScopedCheck || *record.ExitCode < 0 || *record.ExitCode > 255) {
		return errors.New("invalid evidence exit code")
	}
	if record.BaseSHA != task.BaseSHA || !lowerHex(record.BaseSHA, 40) || record.CandidateSHA != "" && !lowerHex(record.CandidateSHA, 40) {
		return errors.New("invalid evidence SHA binding")
	}
	if record.Phase == protocol.EvidencePhaseScopedCheck && record.Reason != protocol.EvidenceReasonScopedCheckPassed && record.CandidateSHA != "" {
		return errors.New("failed check evidence must remain base-bound")
	}
	if len(record.Argv) != 0 || record.ArgvRedacted {
		return errors.New("worker evidence cannot supply argv")
	}
	return nil
}

func lowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

func (s *Store) AttemptEvidence(jobID, attemptID string) ([]protocol.AttemptEvidence, error) {
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM attempts WHERE id=? AND job_id=?`, attemptID, jobID).Scan(&exists); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT evidence_id,phase,reason,check_index,exit_code,duration_ms,output,output_redacted,output_truncated,base_sha,candidate_sha,argv_json,argv_redacted
		FROM attempt_evidence WHERE job_id=? AND attempt_id=? ORDER BY sequence LIMIT 35`, jobID, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]protocol.AttemptEvidence, 0)
	for rows.Next() {
		record, err := scanEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 34 {
		return nil, errors.New("evidence exceeds limit")
	}
	return out, nil
}

func scanEvidence(row scanner) (protocol.AttemptEvidence, error) {
	var record protocol.AttemptEvidence
	var checkIndex, exitCode sql.NullInt64
	var argvJSON string
	if err := row.Scan(&record.EvidenceID, &record.Phase, &record.Reason, &checkIndex, &exitCode, &record.DurationMS, &record.Output,
		&record.OutputRedacted, &record.OutputTruncated, &record.BaseSHA, &record.CandidateSHA, &argvJSON, &record.ArgvRedacted); err != nil {
		return record, err
	}
	if checkIndex.Valid {
		value := int(checkIndex.Int64)
		record.CheckIndex = &value
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		record.ExitCode = &value
	}
	if err := json.Unmarshal([]byte(argvJSON), &record.Argv); err != nil {
		return record, err
	}
	return record, nil
}
