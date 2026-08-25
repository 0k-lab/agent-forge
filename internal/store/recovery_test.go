package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-forge/internal/protocol"
)

func TestDebugTimelineShowsBoundedRecoveryTransitions(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	policy := testRecoveryPolicy()
	job, _ := s.CreateJob("private raw task")
	first, ok, err := s.LeaseNextAt("worker", start, policy)
	if err != nil || !ok {
		t.Fatalf("lease: %#v %v %v", first, ok, err)
	}
	if _, err := s.FailAt(job.ID, first.AttemptID, protocol.FailureExecution, RetryableFailure, start.Add(time.Second), policy); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.LeaseNextAt("worker", start.Add(time.Second+policy.BaseRetryBackoff), policy)
	if err != nil || !ok {
		t.Fatalf("retry lease: %#v %v %v", second, ok, err)
	}
	if _, err := s.CompleteAt(job.ID, second.AttemptID, "private raw result", start.Add(2*time.Second+policy.BaseRetryBackoff)); err != nil {
		t.Fatal(err)
	}
	timeline, err := s.DebugJobTimeline(context.Background(), job.ID, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	var leased, retryable, scheduled DebugEvent
	for _, event := range timeline.Events {
		switch event.Type {
		case "leased":
			if leased.AttemptOrdinal == 0 {
				leased = event
			}
		case "retryable_failed":
			retryable = event
		case "retry_scheduled":
			scheduled = event
		}
	}
	if leased.AttemptID != first.AttemptID || leased.AttemptOrdinal != 1 || leased.LeaseExpiresAt == nil || !leased.LeaseExpiresAt.Equal(start.Add(policy.LeaseTTL)) {
		t.Fatalf("leased event = %#v", leased)
	}
	if retryable.Disposition != "retryable" || retryable.FailureCode != protocol.FailureExecution || scheduled.AttemptOrdinal != 1 || scheduled.RetryAt == nil || !scheduled.RetryAt.Equal(start.Add(time.Second+policy.BaseRetryBackoff)) {
		t.Fatalf("failure events = %#v %#v", retryable, scheduled)
	}
	body, err := json.Marshal(timeline)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private raw task", "private raw result", `"detail"`} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("timeline exposed %q: %s", forbidden, body)
		}
	}
	if strings.Contains(string(body), `"0001-01-01T00:00:00Z"`) {
		t.Fatalf("timeline exposed zero optional timestamps: %s", body)
	}
}

func testRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{
		LeaseTTL:         10 * time.Second,
		BaseRetryBackoff: 5 * time.Second,
		MaxAttempts:      3,
	}
}

func TestRecoverySurvivesReopen(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "forge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", first, ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.SweepExpired(start.Add(10*time.Second), testRecoveryPolicy()); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.LeaseNextAt("worker-2", start.Add(15*time.Second), testRecoveryPolicy())
	if err != nil || !ok || second.JobID != job.ID || second.AttemptID == first.AttemptID {
		t.Fatalf("recovered lease = %#v, %v, %v", second, ok, err)
	}
}

func TestHeartbeatExtendsActiveLease(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	if err := s.Heartbeat(job.ID, lease.AttemptID, "worker-1", start.Add(5*time.Second), testRecoveryPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepExpired(start.Add(10*time.Second), testRecoveryPolicy()); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != "leased" || !attempts[0].DeadlineAt.Equal(start.Add(15*time.Second)) {
		t.Fatalf("heartbeat attempt = %#v, %v", attempts, err)
	}
}

func TestLateOldAttemptOperationsAreRejected(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	old, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("old lease = %#v, %v, %v", old, ok, err)
	}
	if err := s.SweepExpired(start.Add(10*time.Second), testRecoveryPolicy()); err != nil {
		t.Fatal(err)
	}
	assertOldAttemptRejected(t, s, job.ID, old, start.Add(11*time.Second))
	current, ok, err := s.LeaseNextAt("worker-2", start.Add(15*time.Second), testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("retry lease = %#v, %v, %v", current, ok, err)
	}
	if _, err := s.CompleteAt(job.ID, current.AttemptID, "done", start.Add(16*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertOldAttemptRejected(t, s, job.ID, old, start.Add(17*time.Second))
}

func assertOldAttemptRejected(t *testing.T, s *Store, jobID string, old Lease, at time.Time) {
	t.Helper()
	if _, err := s.CompleteAt(jobID, old.AttemptID, "late", at); err == nil {
		t.Fatal("late result accepted")
	}
	if err := s.Heartbeat(jobID, old.AttemptID, "worker-1", at, testRecoveryPolicy()); err == nil {
		t.Fatal("late heartbeat accepted")
	}
	if _, err := s.Fail(jobID, old.AttemptID, "late_failure"); err == nil {
		t.Fatal("late failure accepted")
	}
}

func TestTerminalFailureDoesNotRetry(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	if _, err := s.FailAt(job.ID, lease.AttemptID, "invalid_request", TerminalFailure, start.Add(time.Second), testRecoveryPolicy()); err != nil {
		t.Fatal(err)
	}
	if lease, ok, err := s.LeaseNextAt("worker-2", start.Add(time.Hour), testRecoveryPolicy()); err != nil || ok {
		t.Fatalf("terminal retry = %#v, %v, %v", lease, ok, err)
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != "terminal_failed" || attempts[0].FailureDisposition != "terminal" {
		t.Fatalf("terminal attempt = %#v, %v", attempts, err)
	}
}

func TestTerminalFailureIsExactlyIdempotent(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	first, err := s.FailAt(job.ID, lease.AttemptID, "invalid_request", TerminalFailure, start.Add(time.Second), testRecoveryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.FailAt(job.ID, lease.AttemptID, "invalid_request", TerminalFailure, start.Add(2*time.Second), testRecoveryPolicy())
	if err != nil || second.ID != first.ID || second.Status != "failed" || second.Error != "invalid_request" {
		t.Fatalf("identical terminal failure = %#v, %v", second, err)
	}
	if _, err := s.FailAt(job.ID, lease.AttemptID, "different", TerminalFailure, start.Add(2*time.Second), testRecoveryPolicy()); err == nil {
		t.Fatal("conflicting failure code accepted")
	}
	if _, err := s.CompleteAt(job.ID, lease.AttemptID, "unexpected", start.Add(2*time.Second)); err == nil {
		t.Fatal("conflicting terminal status/result accepted")
	}
	if _, err := s.db.Exec(`UPDATE attempts SET result='corrupt' WHERE id=?`, lease.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailAt(job.ID, lease.AttemptID, "invalid_request", TerminalFailure, start.Add(2*time.Second), testRecoveryPolicy()); err == nil {
		t.Fatal("duplicate accepted mismatched attempt row")
	}
	if _, err := s.db.Exec(`UPDATE attempts SET result='',status='succeeded' WHERE id=?`, lease.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailAt(job.ID, lease.AttemptID, "invalid_request", TerminalFailure, start.Add(2*time.Second), testRecoveryPolicy()); err == nil {
		t.Fatal("duplicate accepted mismatched attempt status")
	}
}

func TestRetryableFailureRetriesUntilMaxAttempts(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	at := start
	for ordinal := 1; ordinal <= testRecoveryPolicy().MaxAttempts; ordinal++ {
		lease, ok, err := s.LeaseNextAt("worker-1", at, testRecoveryPolicy())
		if err != nil || !ok {
			t.Fatalf("lease %d = %#v, %v, %v", ordinal, lease, ok, err)
		}
		failedAt := at.Add(time.Second)
		stored, err := s.FailAt(job.ID, lease.AttemptID, "temporary", RetryableFailure, failedAt, testRecoveryPolicy())
		if err != nil {
			t.Fatalf("failure %d: %v", ordinal, err)
		}
		if ordinal < testRecoveryPolicy().MaxAttempts {
			if stored.Status != "retry_wait" {
				t.Fatalf("failure %d status = %q", ordinal, stored.Status)
			}
			at = failedAt.Add(testRecoveryPolicy().BaseRetryBackoff * time.Duration(1<<(ordinal-1)))
		} else if stored.Status != "failed" || stored.Error != "max_attempts_exceeded" {
			t.Fatalf("max attempt job = %#v", stored)
		}
	}
	if lease, ok, err := s.LeaseNextAt("worker-2", start.Add(24*time.Hour), testRecoveryPolicy()); err != nil || ok {
		t.Fatalf("lease after max = %#v, %v, %v", lease, ok, err)
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || len(attempts) != testRecoveryPolicy().MaxAttempts {
		t.Fatalf("attempts = %#v, %v", attempts, err)
	}
	for i, attempt := range attempts {
		if attempt.Ordinal != i+1 || attempt.Status != "retryable_failed" || attempt.FailureDisposition != "retryable" || attempt.FailureCode != "temporary" {
			t.Fatalf("attempt %d = %#v", i, attempt)
		}
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{"submitted", "leased", "retryable_failed", "retry_scheduled", "leased", "retryable_failed", "retry_scheduled", "leased", "retryable_failed", "failed"}
	if len(events) != len(wantEvents) {
		t.Fatalf("events = %#v", events)
	}
	for i := range wantEvents {
		if events[i].Kind != wantEvents[i] {
			t.Fatalf("event %d = %q, want %q", i, events[i].Kind, wantEvents[i])
		}
	}
}

func TestRetryBudgetDecreaseAfterRestartFailsWaitingJob(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "forge.db")
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	oldPolicy := testRecoveryPolicy()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, oldPolicy)
	if err != nil || !ok {
		t.Fatalf("first lease = %#v, %v, %v", lease, ok, err)
	}
	failedAt := start.Add(time.Second)
	if _, err := s.FailAt(job.ID, lease.AttemptID, "temporary", RetryableFailure, failedAt, oldPolicy); err != nil {
		t.Fatal(err)
	}
	before, err := s.Attempts(job.ID)
	if err != nil || len(before) != 1 || before[0].Ordinal != 1 || before[0].Status != "retryable_failed" || before[0].FailureCode != "temporary" {
		t.Fatalf("attempt before restart = %#v, %v", before, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	policy := oldPolicy
	policy.MaxAttempts = 1
	retryAt := failedAt.Add(oldPolicy.BaseRetryBackoff)
	for range 2 {
		if err := s.SweepExpired(retryAt, policy); err != nil {
			t.Fatal(err)
		}
		if next, ok, err := s.LeaseNextAt("worker-2", retryAt, policy); err != nil || ok {
			t.Fatalf("over-budget lease = %#v, %v, %v", next, ok, err)
		}
	}
	stored, err := s.Job(job.ID)
	if err != nil || stored.Status != "failed" || stored.Error != "max_attempts_exceeded" {
		t.Fatalf("reconciled job = %#v, %v", stored, err)
	}
	if err := s.SweepExpired(retryAt, oldPolicy); err != nil {
		t.Fatal(err)
	}
	if next, ok, err := s.LeaseNextAt("worker-3", retryAt, oldPolicy); err != nil || ok {
		t.Fatalf("policy increase resurrected job = %#v, %v, %v", next, ok, err)
	}
	after, err := s.Attempts(job.ID)
	if err != nil || len(after) != 1 || after[0].ID != before[0].ID || after[0].Ordinal != 1 || after[0].Status != "retryable_failed" || after[0].FailureCode != "temporary" {
		t.Fatalf("attempt after reconciliation = %#v, %v", after, err)
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, event := range events {
		if event.Kind == "failed" {
			failed++
			if !strings.Contains(event.Detail, "failure_code=max_attempts_exceeded") {
				t.Fatalf("failed event = %#v", event)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("failed events = %d; events = %#v", failed, events)
	}
}

func TestLeaseReconcilesRetryBudgetDecreaseWithoutSweep(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	oldPolicy := testRecoveryPolicy()
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, oldPolicy)
	if err != nil || !ok {
		t.Fatalf("first lease = %#v, %v, %v", lease, ok, err)
	}
	failedAt := start.Add(time.Second)
	if _, err := s.FailAt(job.ID, lease.AttemptID, "temporary", RetryableFailure, failedAt, oldPolicy); err != nil {
		t.Fatal(err)
	}
	policy := oldPolicy
	policy.MaxAttempts = 1
	type leaseResult struct {
		lease Lease
		ok    bool
		err   error
	}
	results := make(chan leaseResult, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, ok, err := s.LeaseNextAt(fmt.Sprintf("worker-%d", i+2), failedAt.Add(oldPolicy.BaseRetryBackoff), policy)
			results <- leaseResult{next, ok, err}
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.ok {
			t.Fatalf("over-budget lease = %#v, %v, %v", result.lease, result.ok, result.err)
		}
	}
	stored, err := s.Job(job.ID)
	if err != nil || stored.Status != "failed" || stored.Error != "max_attempts_exceeded" {
		t.Fatalf("reconciled job = %#v, %v", stored, err)
	}
}

func TestActiveAttemptHonorsReducedRetryBudgetOnlyWhenItFinishes(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	oldPolicy := testRecoveryPolicy()
	policy := oldPolicy
	policy.MaxAttempts = 1

	completed, err := s.CreateJob("complete")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-complete", start, oldPolicy)
	if err != nil || !ok {
		t.Fatalf("completion lease = %#v, %v, %v", lease, ok, err)
	}
	if err := s.SweepExpired(start.Add(time.Second), policy); err != nil {
		t.Fatal(err)
	}
	if stored, err := s.CompleteAt(completed.ID, lease.AttemptID, "done", start.Add(2*time.Second)); err != nil || stored.Status != "succeeded" {
		t.Fatalf("active completion = %#v, %v", stored, err)
	}

	failed, err := s.CreateJob("fail")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err = s.LeaseNextAt("worker-fail", start, oldPolicy)
	if err != nil || !ok {
		t.Fatalf("failure lease = %#v, %v, %v", lease, ok, err)
	}
	if stored, err := s.FailAt(failed.ID, lease.AttemptID, "temporary", RetryableFailure, start.Add(time.Second), policy); err != nil || stored.Status != "failed" || stored.Error != "max_attempts_exceeded" {
		t.Fatalf("active retryable failure = %#v, %v", stored, err)
	}

	expired, err := s.CreateJob("expire")
	if err != nil {
		t.Fatal(err)
	}
	if lease, ok, err = s.LeaseNextAt("worker-expire", start, oldPolicy); err != nil || !ok {
		t.Fatalf("expiry lease = %#v, %v, %v", lease, ok, err)
	}
	if err := s.SweepExpired(start.Add(oldPolicy.LeaseTTL), policy); err != nil {
		t.Fatal(err)
	}
	if stored, err := s.Job(expired.ID); err != nil || stored.Status != "failed" || stored.Error != "max_attempts_exceeded" {
		t.Fatalf("expired job = %#v, %v", stored, err)
	}
}

func TestRepeatedSweepIsIdempotent(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy()); err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	for range 2 {
		if err := s.SweepExpired(start.Add(10*time.Second), testRecoveryPolicy()); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"submitted", "leased", "lease_expired", "retry_scheduled"}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for i := range want {
		if events[i].Kind != want[i] {
			t.Fatalf("event %d = %q, want %q", i, events[i].Kind, want[i])
		}
	}
}

func TestSweepRollsBackWhenJobProjectionDoesNotMatchAttempt(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET attempt_id='corrupt' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepExpired(start.Add(testRecoveryPolicy().LeaseTTL), testRecoveryPolicy()); err == nil {
		t.Fatal("sweep accepted mismatched job projection")
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != "leased" {
		t.Fatalf("attempt update was not rolled back: %#v, %v", attempts, err)
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "submitted" || events[1].Kind != "leased" {
		t.Fatalf("events were not rolled back: %#v", events)
	}
}

func TestSchemaRejectsSecondActiveAttemptForJob(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy()); err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	_, err = s.db.Exec(`INSERT INTO attempts(id,job_id,ordinal,worker_id,status,leased_at,deadline_at) VALUES('duplicate',?,2,'worker-2','leased',?,?)`, job.ID, start.UnixNano(), start.Add(time.Second).UnixNano())
	if err == nil {
		t.Fatal("schema accepted two active attempts for one job")
	}
}

func TestLegacyDatabaseMigratesActiveLease(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, input TEXT NOT NULL, status TEXT NOT NULL, attempt_id TEXT NOT NULL DEFAULT '', worker_id TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
		INSERT INTO jobs(id,input,status,attempt_id,worker_id,created_at,updated_at) VALUES('job','work','leased','attempt-1','worker-1',?,?)`, stamp, stamp); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	attempts, err := s.Attempts("job")
	if err != nil || len(attempts) != 1 || attempts[0].Status != "leased" {
		t.Fatalf("migrated attempts = %#v, %v", attempts, err)
	}
	if err := s.SweepExpired(attempts[0].DeadlineAt, testRecoveryPolicy()); err != nil {
		t.Fatal(err)
	}
	if lease, ok, err := s.LeaseNextAt("worker-2", attempts[0].DeadlineAt.Add(testRecoveryPolicy().BaseRetryBackoff), testRecoveryPolicy()); err != nil || !ok || lease.AttemptID == "attempt-1" {
		t.Fatalf("migrated retry = %#v, %v, %v", lease, ok, err)
	}
}

func TestRecoveryPolicyValidationAndDefaults(t *testing.T) {
	first := DefaultRecoveryPolicy()
	if err := first.Validate(); err != nil {
		t.Fatalf("default is invalid: %v", err)
	}
	if first.LeaseTTL != 30*time.Second || first.BaseRetryBackoff != time.Second || first.MaxAttempts != 3 {
		t.Fatalf("defaults = %#v", first)
	}
	first.LeaseTTL = time.Nanosecond
	if later := DefaultRecoveryPolicy(); later.LeaseTTL != 30*time.Second {
		t.Fatalf("mutated default = %#v", later)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			policy := DefaultRecoveryPolicy()
			policy.MaxAttempts = 99
			if DefaultRecoveryPolicy().MaxAttempts != 3 {
				t.Error("concurrent caller changed default")
			}
		}()
	}
	wg.Wait()
	invalid := []RecoveryPolicy{
		{0, time.Second, 3},
		{time.Second, 0, 3},
		{time.Second, time.Second, 0},
		{25 * time.Hour, time.Second, 3},
		{time.Second, 25 * time.Hour, 3},
		{time.Second, time.Second, 101},
	}
	for _, policy := range invalid {
		if err := policy.Validate(); err == nil {
			t.Fatalf("accepted policy %#v", policy)
		}
	}
	if got := (RecoveryPolicy{0, time.Second, 3}).Validate().Error(); got != "lease TTL must be greater than 0 and at most 24h" {
		t.Fatalf("lease TTL error = %q", got)
	}
	if got := (RecoveryPolicy{time.Second, 0, 3}).Validate().Error(); got != "retry backoff must be greater than 0 and at most 24h" {
		t.Fatalf("retry backoff error = %q", got)
	}
}

func TestResultVersusExpiryHasOneAuthority(t *testing.T) {
	for range 20 {
		s := testStore(t)
		start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
		job, err := s.CreateJob("work")
		if err != nil {
			t.Fatal(err)
		}
		lease, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
		if err != nil || !ok {
			t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
		}
		startRace := make(chan struct{})
		var resultErr, sweepErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-startRace
			_, resultErr = s.CompleteAt(job.ID, lease.AttemptID, "done", start.Add(testRecoveryPolicy().LeaseTTL-time.Nanosecond))
		}()
		go func() {
			defer wg.Done()
			<-startRace
			sweepErr = s.SweepExpired(start.Add(testRecoveryPolicy().LeaseTTL), testRecoveryPolicy())
		}()
		close(startRace)
		wg.Wait()
		if sweepErr != nil {
			t.Fatal(sweepErr)
		}
		stored, err := s.Job(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if (stored.Status == "succeeded") != (resultErr == nil) || (stored.Status != "succeeded" && stored.Status != "retry_wait") {
			t.Fatalf("job = %#v, result error = %v", stored, resultErr)
		}
		attempts, err := s.Attempts(job.ID)
		if err != nil || len(attempts) != 1 || (attempts[0].Status != "succeeded" && attempts[0].Status != "expired") {
			t.Fatalf("attempts = %#v, %v", attempts, err)
		}
	}
}

func TestHeartbeatVersusExpiryHasOneAuthority(t *testing.T) {
	for range 20 {
		s := testStore(t)
		start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
		job, err := s.CreateJob("work")
		if err != nil {
			t.Fatal(err)
		}
		lease, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
		if err != nil || !ok {
			t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
		}
		startRace := make(chan struct{})
		var heartbeatErr, sweepErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-startRace
			heartbeatErr = s.Heartbeat(job.ID, lease.AttemptID, "worker-1", start.Add(testRecoveryPolicy().LeaseTTL-time.Nanosecond), testRecoveryPolicy())
		}()
		go func() {
			defer wg.Done()
			<-startRace
			sweepErr = s.SweepExpired(start.Add(testRecoveryPolicy().LeaseTTL), testRecoveryPolicy())
		}()
		close(startRace)
		wg.Wait()
		if sweepErr != nil {
			t.Fatal(sweepErr)
		}
		stored, err := s.Job(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if (stored.Status == "leased") != (heartbeatErr == nil) || (stored.Status != "leased" && stored.Status != "retry_wait") {
			t.Fatalf("job = %#v, heartbeat error = %v", stored, heartbeatErr)
		}
	}
}

func TestDeadlineEqualityIsExpired(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	deadline := start.Add(testRecoveryPolicy().LeaseTTL)
	if _, err := s.CompleteAt(job.ID, lease.AttemptID, "late", deadline); err == nil {
		t.Fatal("result accepted at deadline")
	}
	if err := s.Heartbeat(job.ID, lease.AttemptID, "worker-1", deadline, testRecoveryPolicy()); err == nil {
		t.Fatal("heartbeat accepted at deadline")
	}
}

func TestExpiryAtMaxAttemptsFailsWithoutRetry(t *testing.T) {
	s := testStore(t)
	policy := testRecoveryPolicy()
	policy.MaxAttempts = 1
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LeaseNextAt("worker-1", start, policy); err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if err := s.SweepExpired(start.Add(policy.LeaseTTL), policy); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Job(job.ID)
	if err != nil || stored.Status != "failed" || stored.Error != "max_attempts_exceeded" {
		t.Fatalf("expired max job = %#v, %v", stored, err)
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"submitted", "leased", "lease_expired", "failed"}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for i := range want {
		if events[i].Kind != want[i] {
			t.Fatalf("event %d = %q, want %q", i, events[i].Kind, want[i])
		}
	}
}

func TestCandidateCompletionUsesAttemptLedgerAndDeadline(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateCodingJob(protocol.CodingTask{BaseSHA: strings.Repeat("a", 40), Instruction: "work"})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	candidate := "1111111111111111111111111111111111111111"
	if _, err := s.CompleteCandidateAt(job.ID, lease.AttemptID, candidate, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != "succeeded" || attempts[0].CandidateSHA != candidate || attempts[0].Result != "" {
		t.Fatalf("candidate attempt = %#v, %v", attempts, err)
	}
}

func TestExpiredLeaseRetriesWithNewAttempt(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJob("work")
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.LeaseNextAt("worker-1", start, testRecoveryPolicy())
	if err != nil || !ok {
		t.Fatalf("first lease = %#v, %v, %v", first, ok, err)
	}
	if err := s.SweepExpired(start.Add(testRecoveryPolicy().LeaseTTL), testRecoveryPolicy()); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Job(job.ID)
	if err != nil || stored.Status != "retry_wait" {
		t.Fatalf("expired job = %#v, %v", stored, err)
	}
	if lease, ok, err := s.LeaseNextAt("worker-2", start.Add(14*time.Second), testRecoveryPolicy()); err != nil || ok {
		t.Fatalf("early retry lease = %#v, %v, %v", lease, ok, err)
	}
	second, ok, err := s.LeaseNextAt("worker-2", start.Add(15*time.Second), testRecoveryPolicy())
	if err != nil || !ok || second.AttemptID == first.AttemptID {
		t.Fatalf("second lease = %#v, %v, %v", second, ok, err)
	}
	if _, err := s.CompleteAt(job.ID, second.AttemptID, "done", start.Add(16*time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts = %#v, %v", attempts, err)
	}
	if attempts[0].Ordinal != 1 || attempts[0].Status != "expired" || attempts[1].Ordinal != 2 || attempts[1].Status != "succeeded" || attempts[1].Result != "done" {
		t.Fatalf("attempt history = %#v", attempts)
	}
}
