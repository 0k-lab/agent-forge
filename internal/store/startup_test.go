package store

import (
	"testing"
	"time"

	"agent-forge/internal/protocol"
)

func activeState(t *testing.T, status string) (*Store, Job, Lease, ResolvedPolicy) {
	t.Helper()
	s := testStore(t)
	policy := testNoncodingPolicy("coding")
	job, err := s.CreateJobWithPolicy("work", policy)
	if err != nil {
		t.Fatal(err)
	}
	if status == "pending" {
		return s, job, Lease{}, policy
	}
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, start); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, start)
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	if status == "retry_wait" {
		if _, err := s.FailLeaseAt(job.ID, lease.AttemptID, "worker-1", testGeneration, "retry", RetryableFailure, start.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return s, job, lease, policy
}

func activeLineage(t *testing.T, status string) (*Store, Job, Lease, Lease, ResolvedPolicy) {
	t.Helper()
	s, job, previous, policy := activeState(t, "retry_wait")
	start := time.Date(2026, 8, 25, 12, 0, 3, 0, time.UTC)
	if err := s.ClaimWorkerSlot("worker-2", 0, "worker-2", "coding", newTestGeneration, start); err != nil {
		t.Fatal(err)
	}
	current, ok, err := s.LeaseNextForPool("worker-2", "coding", newTestGeneration, start)
	if err != nil || !ok {
		t.Fatalf("second lease = %#v, %v, %v", current, ok, err)
	}
	if status == "retry_wait" {
		if _, err := s.FailLeaseAt(job.ID, current.AttemptID, "worker-2", newTestGeneration, "retry", RetryableFailure, start.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return s, job, previous, current, policy
}

func TestValidateActivePoliciesRejectsMalformedPendingState(t *testing.T) {
	tests := map[string]func(*Store, Job){
		"attempt projection": func(s *Store, job Job) { mustExec(t, s, `UPDATE jobs SET attempt_id='stale' WHERE id=?`, job.ID) },
		"worker projection":  func(s *Store, job Job) { mustExec(t, s, `UPDATE jobs SET worker_id='stale' WHERE id=?`, job.ID) },
		"retry timestamp":    func(s *Store, job Job) { mustExec(t, s, `UPDATE jobs SET retry_at=1 WHERE id=?`, job.ID) },
		"noncanonical policy": func(s *Store, job Job) {
			mustExec(t, s, `UPDATE jobs SET resolved_policy=resolved_policy || ' ' WHERE id=?`, job.ID)
		},
		"existing attempt": func(s *Store, job Job) {
			mustExec(t, s, `INSERT INTO attempts(id,job_id,ordinal,worker_id,status,leased_at,deadline_at) VALUES('stale',?,1,'worker','succeeded',1,2)`, job.ID)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s, job, _, _ := activeState(t, "pending")
			mutate(s, job)
			if err := s.ValidateActivePolicies(); err == nil {
				t.Fatal("accepted malformed pending state")
			}
		})
	}
}

func TestValidateActivePoliciesRejectsMalformedRetryWaitState(t *testing.T) {
	tests := map[string]func(*Store, Job, Lease, ResolvedPolicy){
		"missing attempt": func(s *Store, job Job, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE jobs SET attempt_id='' WHERE id=?`, job.ID)
		},
		"unrelated attempt": func(s *Store, job Job, _ Lease, policy ResolvedPolicy) {
			other, err := s.CreateJobWithPolicy("other", policy)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, s, `UPDATE jobs SET status='failed' WHERE id=?`, other.ID)
			mustExec(t, s, `INSERT INTO attempts(id,job_id,ordinal,worker_id,status,leased_at,deadline_at,completed_at) VALUES('unrelated',?,1,'worker','succeeded',1,2,1)`, other.ID)
			mustExec(t, s, `UPDATE jobs SET attempt_id='unrelated' WHERE id=?`, job.ID)
		},
		"nonlatest attempt": func(s *Store, job Job, lease Lease, policy ResolvedPolicy) {
			body, _ := CanonicalPolicy(policy)
			mustExec(t, s, `INSERT INTO attempts(id,job_id,ordinal,worker_id,status,leased_at,deadline_at,completed_at,worker_pool,slot,session_generation,policy_version,resolved_policy) VALUES('later',?,2,'worker-2','expired',1,2,2,?,'worker-2',?, ?,?)`, job.ID, policy.WorkerPool, testGeneration, policy.Version, body)
			_ = lease
		},
		"wrong status": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET status='succeeded',failure_disposition='',failure_code='',result='done' WHERE id=?`, lease.AttemptID)
		},
		"exhausted budget": func(s *Store, job Job, lease Lease, policy ResolvedPolicy) {
			policy.MaxAttempts = 1
			body, _ := CanonicalPolicy(policy)
			mustExec(t, s, `UPDATE jobs SET resolved_policy=? WHERE id=?`, body, job.ID)
			mustExec(t, s, `UPDATE attempts SET resolved_policy=? WHERE id=?`, body, lease.AttemptID)
		},
		"worker projection": func(s *Store, job Job, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE jobs SET worker_id='stale' WHERE id=?`, job.ID)
		},
		"retry timestamp": func(s *Store, job Job, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE jobs SET retry_at=0 WHERE id=?`, job.ID)
		},
		"bad completion": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET completed_at=0 WHERE id=?`, lease.AttemptID)
		},
		"bad disposition": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET failure_disposition='' WHERE id=?`, lease.AttemptID)
		},
		"policy mismatch": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET worker_pool='other' WHERE id=?`, lease.AttemptID)
		},
		"empty slot": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET slot='' WHERE id=?`, lease.AttemptID)
		},
		"empty generation": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET session_generation='' WHERE id=?`, lease.AttemptID)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s, job, lease, policy := activeState(t, "retry_wait")
			mutate(s, job, lease, policy)
			if err := s.ValidateActivePolicies(); err == nil {
				t.Fatal("accepted malformed retry_wait state")
			}
		})
	}
}

func TestValidateActivePoliciesRequiresCanonicalCodingTask(t *testing.T) {
	s := testStore(t)
	policy := testResolvedPolicy("coding")
	job, err := s.CreateCodingJobWithPolicy(protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: strings40("a"), Instruction: "change", Tests: [][]string{{"true"}}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE jobs SET task_json=task_json || ' ' WHERE id=?`, job.ID)
	if err := s.ValidateActivePolicies(); err == nil {
		t.Fatal("accepted noncanonical coding task")
	}
}

func TestValidateActivePoliciesRejectsMalformedLeasedState(t *testing.T) {
	tests := map[string]func(*Store, Job, Lease, ResolvedPolicy){
		"retry timestamp": func(s *Store, job Job, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE jobs SET retry_at=1 WHERE id=?`, job.ID)
		},
		"job worker": func(s *Store, job Job, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE jobs SET worker_id='other' WHERE id=?`, job.ID)
		},
		"attempt worker": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET worker_id='other' WHERE id=?`, lease.AttemptID)
		},
		"slot": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET slot='other' WHERE id=?`, lease.AttemptID)
		},
		"empty slot": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET worker_id='',slot='' WHERE id=?`, lease.AttemptID)
		},
		"empty generation": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET session_generation='' WHERE id=?`, lease.AttemptID)
		},
		"long generation": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET session_generation=? WHERE id=?`, string(make([]byte, 129)), lease.AttemptID)
		},
		"zero ordinal": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `PRAGMA ignore_check_constraints=ON`)
			mustExec(t, s, `UPDATE attempts SET ordinal=0 WHERE id=?`, lease.AttemptID)
		},
		"over budget": func(s *Store, _ Job, lease Lease, policy ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET ordinal=? WHERE id=?`, policy.MaxAttempts+1, lease.AttemptID)
		},
		"zero leased time": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET leased_at=0 WHERE id=?`, lease.AttemptID)
		},
		"bad deadline": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET deadline_at=leased_at WHERE id=?`, lease.AttemptID)
		},
		"completed": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET completed_at=1 WHERE id=?`, lease.AttemptID)
		},
		"policy mismatch": func(s *Store, _ Job, lease Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET resolved_policy=resolved_policy || ' ' WHERE id=?`, lease.AttemptID)
		},
		"nonlatest": func(s *Store, job Job, _ Lease, policy ResolvedPolicy) {
			body, _ := CanonicalPolicy(policy)
			mustExec(t, s, `INSERT INTO attempts(id,job_id,ordinal,worker_id,status,leased_at,deadline_at,completed_at,worker_pool,slot,session_generation,policy_version,resolved_policy) VALUES('later',?,2,'old','expired',1,2,2,?,'old',?, ?,?)`, job.ID, policy.WorkerPool, testGeneration, policy.Version, body)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s, job, lease, policy := activeState(t, "leased")
			mutate(s, job, lease, policy)
			if err := s.ValidateActivePolicies(); err == nil {
				t.Fatal("accepted malformed leased state")
			}
		})
	}
}

func TestValidateActivePoliciesRejectsMalformedPriorAttempts(t *testing.T) {
	tests := map[string]func(*Store, Job, Lease, Lease, ResolvedPolicy){
		"succeeded": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET status='succeeded',failure_disposition='',failure_code='',result='done' WHERE id=?`, previous.AttemptID)
		},
		"terminal failed": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET status='terminal_failed',failure_disposition='terminal',failure_code='failed' WHERE id=?`, previous.AttemptID)
		},
		"leased": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET status='leased',completed_at=0,failure_disposition='',failure_code='' WHERE id=?`, previous.AttemptID)
		},
		"corrupt fields": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET result='unexpected' WHERE id=?`, previous.AttemptID)
		},
		"skipped ordinal": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `DELETE FROM attempts WHERE id=?`, previous.AttemptID)
		},
		"pool mismatch": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET worker_pool='other' WHERE id=?`, previous.AttemptID)
		},
		"policy mismatch": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET resolved_policy=resolved_policy || ' ' WHERE id=?`, previous.AttemptID)
		},
		"slot mismatch": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET slot='other' WHERE id=?`, previous.AttemptID)
		},
		"generation syntax": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET session_generation='0000000000000000000000000000000A' WHERE id=?`, previous.AttemptID)
		},
		"timestamps": func(s *Store, _ Job, previous, _ Lease, _ ResolvedPolicy) {
			mustExec(t, s, `UPDATE attempts SET deadline_at=leased_at WHERE id=?`, previous.AttemptID)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s, job, previous, current, policy := activeLineage(t, "retry_wait")
			mutate(s, job, previous, current, policy)
			if err := s.ValidateActivePolicies(); err == nil {
				t.Fatal("accepted malformed prior attempt")
			}
		})
	}
}

func TestValidateActivePoliciesValidatesLeasedAttemptLineage(t *testing.T) {
	s, _, previous, _, _ := activeLineage(t, "leased")
	mustExec(t, s, `UPDATE attempts SET session_generation='short' WHERE id=?`, previous.AttemptID)
	if err := s.ValidateActivePolicies(); err == nil {
		t.Fatal("accepted malformed prior attempt for leased job")
	}
}

func TestValidateActivePoliciesRejectsLeasedAttemptWithoutLeasedProjection(t *testing.T) {
	for _, status := range []string{"pending", "retry_wait"} {
		t.Run(status, func(t *testing.T) {
			s, job, lease, _ := activeState(t, "leased")
			mustExec(t, s, `UPDATE jobs SET status=?,worker_id='',retry_at=? WHERE id=?`, status, map[string]int64{"pending": 0, "retry_wait": 1}[status], job.ID)
			if status == "pending" {
				mustExec(t, s, `UPDATE jobs SET attempt_id='' WHERE id=?`, job.ID)
			}
			if err := s.ValidateActivePolicies(); err == nil {
				t.Fatalf("accepted leased attempt %s projected by %s job", lease.AttemptID, status)
			}
		})
	}
}

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
