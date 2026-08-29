package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func boundPolicy(tx *sql.Tx, jobID, attemptID string) (ResolvedPolicy, error) {
	var jobPool, attemptPool, slot, jobStatus, attemptStatus, projectedAttempt, projectedSlot string
	var jobVersion, attemptVersion int
	var jobBytes, attemptBytes []byte
	err := tx.QueryRow(`SELECT j.worker_pool,j.policy_version,j.resolved_policy,j.status,j.attempt_id,j.worker_id,
		a.worker_pool,a.policy_version,a.resolved_policy,a.status,a.slot
		FROM jobs j JOIN attempts a ON a.id=? AND a.job_id=j.id WHERE j.id=?`, attemptID, jobID).
		Scan(&jobPool, &jobVersion, &jobBytes, &jobStatus, &projectedAttempt, &projectedSlot, &attemptPool, &attemptVersion, &attemptBytes, &attemptStatus, &slot)
	if err != nil || jobStatus != "leased" || attemptStatus != "leased" || projectedAttempt != attemptID || projectedSlot != slot || jobPool != attemptPool || jobVersion != attemptVersion || !bytes.Equal(jobBytes, attemptBytes) {
		return ResolvedPolicy{}, errors.New("attempt is not active")
	}
	policy, err := DecodeCanonicalPolicy(attemptBytes)
	if err != nil || policy.Version != attemptVersion || policy.WorkerPool != attemptPool {
		return ResolvedPolicy{}, errors.New("corrupt resolved policy")
	}
	return policy, nil
}

func liveSession(tx *sql.Tx, attemptID, slot, generation string) error {
	if !validSessionGeneration(generation) {
		return errors.New("worker session is not active")
	}
	var found int
	err := tx.QueryRow(`SELECT 1 FROM attempts a JOIN workers w ON w.id=a.slot
		WHERE a.id=? AND a.slot=? AND a.session_generation=? AND w.id=? AND w.connected=1 AND w.generation=?`, attemptID, slot, generation, slot, generation).Scan(&found)
	if err != nil {
		return errors.New("worker session is not active")
	}
	return nil
}

func ownedPolicy(tx *sql.Tx, jobID, attemptID, slot, generation string) (ResolvedPolicy, error) {
	var jobPool, attemptPool, projectedAttempt, projectedSlot, attemptSlot, attemptGeneration string
	var jobVersion, attemptVersion int
	var jobBytes, attemptBytes []byte
	err := tx.QueryRow(`SELECT j.worker_pool,j.policy_version,j.resolved_policy,j.attempt_id,j.worker_id,
		a.worker_pool,a.policy_version,a.resolved_policy,a.slot,a.session_generation
		FROM jobs j JOIN attempts a ON a.id=? AND a.job_id=j.id WHERE j.id=?`, attemptID, jobID).
		Scan(&jobPool, &jobVersion, &jobBytes, &projectedAttempt, &projectedSlot, &attemptPool, &attemptVersion, &attemptBytes, &attemptSlot, &attemptGeneration)
	if err != nil || projectedAttempt != attemptID || projectedSlot != slot || attemptSlot != slot || attemptGeneration != generation || jobPool != attemptPool || jobVersion != attemptVersion || !bytes.Equal(jobBytes, attemptBytes) || liveSession(tx, attemptID, slot, generation) != nil {
		return ResolvedPolicy{}, errors.New("attempt is not owned")
	}
	policy, err := DecodeCanonicalPolicy(attemptBytes)
	if err != nil || policy.Version != attemptVersion || policy.WorkerPool != attemptPool {
		return ResolvedPolicy{}, errors.New("corrupt resolved policy")
	}
	return policy, nil
}

func (s *Store) CompleteLeaseAt(jobID, attemptID, slot, generation, result string, at time.Time) (Job, error) {
	return s.terminalOwned(jobID, attemptID, "succeeded", result, "", "", at.UTC(), slot, generation, true)
}

func (s *Store) CompleteCandidateLeaseAt(jobID, attemptID, slot, generation, candidateSHA string, at time.Time) (Job, error) {
	if len(candidateSHA) != 40 {
		return Job{}, errors.New("invalid candidate SHA")
	}
	for _, value := range []byte(candidateSHA) {
		if value < '0' || value > '9' && (value < 'a' || value > 'f') {
			return Job{}, errors.New("invalid candidate SHA")
		}
	}
	return s.terminalOwned(jobID, attemptID, "succeeded", "", candidateSHA, "", at.UTC(), slot, generation, true)
}

func (s *Store) FailLeaseAt(jobID, attemptID, slot, generation, code string, disposition FailureDisposition, at time.Time) (Job, error) {
	if code == "" || len(code) > 64 || disposition != TerminalFailure && disposition != RetryableFailure {
		return Job{}, errors.New("invalid failure")
	}
	if disposition == TerminalFailure {
		return s.terminalOwned(jobID, attemptID, "terminal_failed", "", "", code, at.UTC(), slot, generation, true)
	}
	at = at.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	policy, err := ownedPolicy(tx, jobID, attemptID, slot, generation)
	if err != nil {
		return Job{}, err
	}
	var status, storedCode string
	var deadline int64
	var ordinal int
	if err := tx.QueryRow(`SELECT status,failure_code,deadline_at,ordinal FROM attempts WHERE id=? AND job_id=?`, attemptID, jobID).Scan(&status, &storedCode, &deadline, &ordinal); err != nil {
		return Job{}, err
	}
	if status != "leased" {
		if status != "retryable_failed" || storedCode != code {
			return Job{}, errors.New("result is immutable")
		}
		return scanJob(tx.QueryRow(`SELECT id,input,task_json,status,attempt_id,worker_id,result,candidate_sha,error_text,created_at,updated_at,worker_pool,policy_version,source_ref FROM jobs WHERE id=?`, jobID))
	}
	if at.UnixNano() >= deadline {
		return Job{}, errors.New("attempt lease expired")
	}
	result, err := tx.Exec(`UPDATE attempts SET status='retryable_failed',completed_at=?,failure_disposition='retryable',failure_code=? WHERE id=? AND job_id=? AND status='leased' AND slot=? AND session_generation=?`, at.UnixNano(), code, attemptID, jobID, slot, generation)
	if err != nil {
		return Job{}, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return Job{}, errors.New("attempt is not active")
	}
	stamp := at.Format(time.RFC3339Nano)
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "retryable_failed", fmt.Sprintf("attempt=%s ordinal=%d disposition=retryable failure_code=%s", attemptID, ordinal, code), stamp); err != nil {
		return Job{}, err
	}
	if err = scheduleResolvedRetry(tx, jobID, attemptID, ordinal, at, policy); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, err
	}
	return s.Job(jobID)
}

func (s *Store) HeartbeatLease(jobID, attemptID, slot, generation string, at time.Time) error {
	at = at.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	policy, err := boundPolicy(tx, jobID, attemptID)
	if err != nil || liveSession(tx, attemptID, slot, generation) != nil {
		return errors.New("attempt is not active")
	}
	var oldDeadline int64
	if err := tx.QueryRow(`SELECT deadline_at FROM attempts WHERE id=? AND job_id=? AND slot=? AND session_generation=? AND status='leased'`, attemptID, jobID, slot, generation).Scan(&oldDeadline); err != nil || at.UnixNano() >= oldDeadline {
		return errors.New("attempt is not active")
	}
	deadline := at.Add(time.Duration(policy.LeaseTTLNanos)).UnixNano()
	if deadline < oldDeadline {
		deadline = oldDeadline
	}
	result, err := tx.Exec(`UPDATE attempts SET deadline_at=? WHERE id=? AND job_id=? AND slot=? AND session_generation=? AND status='leased' AND deadline_at>?`, deadline, attemptID, jobID, slot, generation, at.UnixNano())
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("attempt is not active")
	}
	result, err = tx.Exec(`UPDATE jobs SET updated_at=? WHERE id=? AND status='leased' AND attempt_id=? AND worker_id=?`, at.Format(time.RFC3339Nano), jobID, attemptID, slot)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("attempt is not active")
	}
	return tx.Commit()
}

func (s *Store) SweepExpiredPolicies(at time.Time) error {
	at = at.UTC()
	rows, err := s.db.Query(`SELECT id,job_id FROM attempts WHERE status='leased' AND deadline_at<=? ORDER BY deadline_at,id`, at.UnixNano())
	if err != nil {
		return err
	}
	var due [][2]string
	for rows.Next() {
		var item [2]string
		if err := rows.Scan(&item[0], &item[1]); err != nil {
			rows.Close()
			return err
		}
		due = append(due, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range due {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		policy, policyErr := boundPolicy(tx, item[1], item[0])
		if policyErr != nil {
			tx.Rollback()
			return policyErr
		}
		var ordinal int
		var deadline int64
		if err = tx.QueryRow(`SELECT ordinal,deadline_at FROM attempts WHERE id=? AND job_id=? AND status='leased'`, item[0], item[1]).Scan(&ordinal, &deadline); err != nil {
			tx.Rollback()
			return err
		}
		if deadline > at.UnixNano() {
			tx.Rollback()
			continue
		}
		result, err := tx.Exec(`UPDATE attempts SET status='expired',completed_at=? WHERE id=? AND job_id=? AND status='leased' AND deadline_at<=?`, at.UnixNano(), item[0], item[1], at.UnixNano())
		if err != nil {
			tx.Rollback()
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			tx.Rollback()
			if err != nil {
				return err
			}
			continue
		}
		stamp := at.Format(time.RFC3339Nano)
		detail := fmt.Sprintf("attempt=%s ordinal=%d lease_expires=%s", item[0], ordinal, time.Unix(0, deadline).UTC().Format(time.RFC3339Nano))
		if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, item[1], "lease_expired", detail, stamp); err == nil {
			err = scheduleResolvedRetry(tx, item[1], item[0], ordinal, at, policy)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func scheduleResolvedRetry(tx *sql.Tx, jobID, attemptID string, ordinal int, at time.Time, policy ResolvedPolicy) error {
	stamp := at.UTC().Format(time.RFC3339Nano)
	if ordinal >= policy.MaxAttempts {
		result, err := tx.Exec(`UPDATE jobs SET status='failed',worker_id='',retry_at=0,error_text='max_attempts_exceeded',updated_at=? WHERE id=? AND attempt_id=? AND status='leased'`, stamp, jobID, attemptID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return errors.New("job retry update failed")
		}
		_, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "failed", fmt.Sprintf("attempt=%s ordinal=%d disposition=terminal failure_code=max_attempts_exceeded", attemptID, ordinal), stamp)
		return err
	}
	retryAt := at.Add(retryDelay(policy, ordinal))
	result, err := tx.Exec(`UPDATE jobs SET status='retry_wait',worker_id='',retry_at=?,updated_at=? WHERE id=? AND attempt_id=? AND status='leased'`, retryAt.UnixNano(), stamp, jobID, attemptID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("job retry update failed")
	}
	_, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "retry_scheduled", fmt.Sprintf("attempt=%s ordinal=%d retry_at=%s", attemptID, ordinal, retryAt.Format(time.RFC3339Nano)), stamp)
	return err
}
