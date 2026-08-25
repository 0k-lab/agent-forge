package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"agent-forge/internal/protocol"
)

var errActiveState = errors.New("active state validation failed")

type persistedJob struct {
	id, status, taskJSON, pool, attemptID, workerID string
	version                                         int
	retryAt                                         int64
	policyBytes                                     []byte
}

type persistedAttempt struct {
	id, jobID, workerID, status, disposition, failureCode, result, candidate, pool, slot, generation string
	ordinal, version                                                                                 int
	leasedAt, deadlineAt, completedAt                                                                int64
	policyBytes                                                                                      []byte
}

func (s *Store) ValidateActivePolicies() error {
	rows, err := s.db.Query(`SELECT id,status,task_json,worker_pool,policy_version,resolved_policy,attempt_id,worker_id,retry_at FROM jobs WHERE status IN ('pending','retry_wait','leased') ORDER BY id`)
	if err != nil {
		return errActiveState
	}
	var active []persistedJob
	for rows.Next() {
		var job persistedJob
		if err := rows.Scan(&job.id, &job.status, &job.taskJSON, &job.pool, &job.version, &job.policyBytes, &job.attemptID, &job.workerID, &job.retryAt); err != nil {
			rows.Close()
			return errActiveState
		}
		active = append(active, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return errActiveState
	}
	if err := rows.Close(); err != nil {
		return errActiveState
	}

	attemptRows, err := s.db.Query(`SELECT id,job_id,ordinal,worker_id,status,leased_at,deadline_at,completed_at,failure_disposition,failure_code,result,candidate_sha,COALESCE(worker_pool,''),COALESCE(slot,''),COALESCE(session_generation,''),COALESCE(policy_version,0),COALESCE(resolved_policy,x'') FROM attempts ORDER BY job_id,ordinal,id`)
	if err != nil {
		return errActiveState
	}
	byJob := map[string][]persistedAttempt{}
	var leased []persistedAttempt
	for attemptRows.Next() {
		var attempt persistedAttempt
		if err := attemptRows.Scan(&attempt.id, &attempt.jobID, &attempt.ordinal, &attempt.workerID, &attempt.status, &attempt.leasedAt, &attempt.deadlineAt, &attempt.completedAt, &attempt.disposition, &attempt.failureCode, &attempt.result, &attempt.candidate, &attempt.pool, &attempt.slot, &attempt.generation, &attempt.version, &attempt.policyBytes); err != nil {
			attemptRows.Close()
			return errActiveState
		}
		byJob[attempt.jobID] = append(byJob[attempt.jobID], attempt)
		if attempt.status == "leased" {
			leased = append(leased, attempt)
		}
	}
	if err := attemptRows.Close(); err != nil {
		return errActiveState
	}

	leasedProjected := map[string]bool{}
	for _, job := range active {
		policy, err := DecodeCanonicalPolicy(job.policyBytes)
		if err != nil || policy.Version != job.version || policy.WorkerPool != job.pool || !canonicalStoredTask(job.taskJSON, policy) {
			return errActiveState
		}
		attempts := byJob[job.id]
		if job.status != "pending" {
			for i, attempt := range attempts {
				if attempt.ordinal != i+1 || i < len(attempts)-1 && !validPersistedAttempt(attempt, job, policy, false) {
					return errActiveState
				}
			}
		}
		switch job.status {
		case "pending":
			if job.attemptID != "" || job.workerID != "" || job.retryAt != 0 || len(attempts) != 0 {
				return errActiveState
			}
		case "retry_wait":
			if job.attemptID == "" || job.workerID != "" || job.retryAt <= 0 || len(attempts) == 0 {
				return errActiveState
			}
			current := attempts[len(attempts)-1]
			if current.id != job.attemptID || current.ordinal >= policy.MaxAttempts || !validPersistedAttempt(current, job, policy, false) {
				return errActiveState
			}
			for _, attempt := range attempts {
				if attempt.status == "leased" {
					return errActiveState
				}
			}
		case "leased":
			if job.retryAt != 0 || job.attemptID == "" || job.workerID == "" || len(attempts) == 0 {
				return errActiveState
			}
			current := attempts[len(attempts)-1]
			if current.id != job.attemptID || current.workerID != job.workerID || current.slot != job.workerID || !validPersistedAttempt(current, job, policy, true) {
				return errActiveState
			}
			count := 0
			for _, attempt := range attempts {
				if attempt.status == "leased" {
					count++
				}
			}
			if count != 1 {
				return errActiveState
			}
			leasedProjected[current.id] = true
		}
	}
	for _, attempt := range leased {
		if !leasedProjected[attempt.id] {
			return errActiveState
		}
	}
	return nil
}

func canonicalStoredTask(taskJSON string, policy ResolvedPolicy) bool {
	if taskJSON == "" {
		return policy.Execution.RepositoryID == ""
	}
	var task protocol.CodingTask
	decoder := json.NewDecoder(strings.NewReader(taskJSON))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&task) != nil || decoder.Decode(&struct{}{}) != io.EOF || validateStoredTask(task, policy) != nil {
		return false
	}
	canonical, err := json.Marshal(task)
	return err == nil && bytes.Equal(canonical, []byte(taskJSON))
}

func validPersistedAttempt(attempt persistedAttempt, job persistedJob, policy ResolvedPolicy, active bool) bool {
	if attempt.jobID != job.id || attempt.pool != job.pool || attempt.version != job.version || !bytes.Equal(attempt.policyBytes, job.policyBytes) || attempt.slot == "" || len(attempt.slot) > 80 || !validSessionGeneration(attempt.generation) || attempt.workerID != attempt.slot || attempt.ordinal < 1 || attempt.ordinal > policy.MaxAttempts || attempt.leasedAt <= 0 || attempt.deadlineAt <= attempt.leasedAt {
		return false
	}
	if decoded, err := DecodeCanonicalPolicy(attempt.policyBytes); err != nil || decoded.Version != attempt.version || decoded.WorkerPool != attempt.pool {
		return false
	}
	if active {
		return attempt.status == "leased" && attempt.completedAt == 0 && attempt.disposition == "" && attempt.failureCode == "" && attempt.result == "" && attempt.candidate == ""
	}
	if attempt.completedAt < attempt.leasedAt {
		return false
	}
	switch attempt.status {
	case "retryable_failed":
		return attempt.completedAt < attempt.deadlineAt && attempt.disposition == "retryable" && attempt.failureCode != "" && len(attempt.failureCode) <= 64 && attempt.result == "" && attempt.candidate == ""
	case "expired":
		return attempt.completedAt >= attempt.deadlineAt && attempt.disposition == "" && attempt.failureCode == "" && attempt.result == "" && attempt.candidate == ""
	default:
		return false
	}
}
