package store

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/protocol"
)

const (
	testGeneration    = "00000000000000000000000000000001"
	oldTestGeneration = "00000000000000000000000000000002"
	newTestGeneration = "00000000000000000000000000000003"
)

func testResolvedPolicy(pool string) ResolvedPolicy {
	return ResolvedPolicy{
		Version:        1,
		WorkerPool:     pool,
		LeaseTTLNanos:  int64(10 * time.Second),
		RetryBaseNanos: int64(2 * time.Second),
		MaxAttempts:    3,
		RetryAlgorithm: "exponential-v1",
		RetryMaxNanos:  int64(24 * time.Hour),
		Execution: ExecutionPolicy{
			RepositoryID:        "agent-forge",
			DefaultBranch:       "main",
			PluginID:            "codex",
			Environment:         []string{"PATH"},
			PluginTimeoutNanos:  int64(15 * time.Minute),
			CheckTimeoutNanos:   int64(10 * time.Minute),
			GitTimeoutNanos:     int64(time.Minute),
			CleanupTimeoutNanos: int64(10 * time.Second),
			PluginOutputBytes:   1 << 20,
			CheckOutputBytes:    protocol.MaxEvidenceOutputBytes,
			GitOutputBytes:      1 << 20,
		},
	}
}

func testNoncodingPolicy(pool string) ResolvedPolicy {
	policy := testResolvedPolicy(pool)
	policy.Execution.RepositoryID = ""
	policy.Execution.DefaultBranch = ""
	return policy
}

func TestResolvedPolicyCanonicalRoundTripAndBackoffSaturation(t *testing.T) {
	policy := testResolvedPolicy("coding")
	body, err := CanonicalPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCanonicalPolicy(body)
	if err != nil || decoded.WorkerPool != "coding" {
		t.Fatalf("decoded = %#v, %v", decoded, err)
	}
	if _, err := DecodeCanonicalPolicy(append(append([]byte{}, body...), ' ')); err == nil {
		t.Fatal("accepted noncanonical policy")
	}
	policy.RetryBaseNanos = int64(20 * time.Hour)
	if got := retryDelay(policy, 100); got != 24*time.Hour {
		t.Fatalf("saturated delay = %v", got)
	}
}

func TestPoolLeaseCopiesExactPolicyAndIgnoresLaterConfig(t *testing.T) {
	s := testStore(t)
	policy := testResolvedPolicy("coding")
	task := protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: strings40("a"), Instruction: "change", Tests: [][]string{{"go", "test", "./..."}}}
	job, err := s.CreateCodingJobWithPolicy(task, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorkerSlot("general", 0, "general#0", "general", testGeneration, time.Now()); err != nil {
		t.Fatal(err)
	}
	if lease, ok, err := s.LeaseNextForPool("general#0", "general", testGeneration, time.Now()); err != nil || ok {
		t.Fatalf("wrong-pool lease = %#v, %v, %v", lease, ok, err)
	}
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, start); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, start)
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	var jobBytes, attemptBytes []byte
	if err := s.db.QueryRow(`SELECT resolved_policy FROM jobs WHERE id=?`, job.ID).Scan(&jobBytes); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT resolved_policy FROM attempts WHERE id=?`, lease.AttemptID).Scan(&attemptBytes); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(jobBytes, attemptBytes) {
		t.Fatal("attempt policy was re-marshaled or changed")
	}
	if !reflect.DeepEqual(lease.Policy, policy) || lease.WorkerPool != "coding" || lease.Slot != "worker-1" {
		t.Fatalf("lease policy = %#v", lease)
	}
}

func TestLeaseDecodesTaskAndPolicyBeforeCommit(t *testing.T) {
	s := testStore(t)
	policy := testResolvedPolicy("coding")
	job, err := s.CreateCodingJobWithPolicy(protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: strings40("a"), Instruction: "change", Tests: [][]string{{"true"}}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET task_json='{' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, time.Now()); err == nil || ok {
		t.Fatalf("malformed task lease: ok=%v err=%v", ok, err)
	}
	stored, err := s.Job(job.ID)
	if err == nil || stored.Status == "leased" {
		t.Fatalf("lease committed before decode: %#v, %v", stored, err)
	}
}

func TestPersistedAttemptPolicyDrivesHeartbeatExpiryAndRetry(t *testing.T) {
	s := testStore(t)
	policy := testNoncodingPolicy("coding")
	policy.LeaseTTLNanos = int64(10 * time.Second)
	policy.RetryBaseNanos = int64(3 * time.Second)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJobWithPolicy("work", policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, start); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, start)
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	if err := s.HeartbeatLease(job.ID, lease.AttemptID, "worker-1", testGeneration, start.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || !attempts[0].DeadlineAt.Equal(start.Add(10*time.Second)) {
		t.Fatalf("heartbeat shortened deadline: %#v, %v", attempts, err)
	}
	if err := s.SweepExpiredPolicies(start.Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Job(job.ID)
	if err != nil || stored.Status != "retry_wait" {
		t.Fatalf("expired job = %#v, %v", stored, err)
	}
	var retryAt int64
	if err := s.db.QueryRow(`SELECT retry_at FROM jobs WHERE id=?`, job.ID).Scan(&retryAt); err != nil {
		t.Fatal(err)
	}
	if got := time.Unix(0, retryAt).UTC(); !got.Equal(start.Add(13 * time.Second)) {
		t.Fatalf("retry_at = %v", got)
	}
}

func TestWorkerSlotRejectsLiveDuplicateAndFencesStaleRelease(t *testing.T) {
	s := testStore(t)
	policy := testNoncodingPolicy("coding")
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateJobWithPolicy("work", policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", oldTestGeneration, start); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", "coding", oldTestGeneration, start)
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", newTestGeneration, start.Add(time.Second)); err == nil {
		t.Fatal("live duplicate slot was accepted")
	}
	if err := s.ReleaseWorkerSlot("worker-1", oldTestGeneration, start.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", newTestGeneration, start.Add(3*time.Second)); err != nil {
		t.Fatalf("disconnected slot did not reconnect: %v", err)
	}
	if err := s.ReleaseWorkerSlot("worker-1", oldTestGeneration, start.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	worker, err := s.Worker("worker-1")
	if err != nil || !worker.Connected || worker.Generation != newTestGeneration {
		t.Fatalf("stale release cleared replacement: %#v, %v", worker, err)
	}
	if err := s.HeartbeatLease(job.ID, lease.AttemptID, "worker-1", oldTestGeneration, start.Add(2*time.Second)); err == nil {
		t.Fatal("fenced generation mutated lease")
	}
}

func TestConfiguredStoreRejectsMalformedSessionGenerations(t *testing.T) {
	for name, generation := range map[string]string{
		"empty":     "",
		"short":     "0000000000000000000000000000000",
		"uppercase": "0000000000000000000000000000000A",
		"nonhex":    "0000000000000000000000000000000g",
		"overlong":  "000000000000000000000000000000000",
	} {
		t.Run(name, func(t *testing.T) {
			s, job, lease, _ := activeState(t, "leased")
			at := time.Date(2026, 8, 25, 12, 0, 1, 0, time.UTC)
			checks := map[string]func() error{
				"claim":   func() error { return s.ClaimWorkerSlot("worker-2", 0, "worker-2", "coding", generation, at) },
				"release": func() error { return s.ReleaseWorkerSlot("worker-1", generation, at) },
				"lease": func() error {
					_, _, err := s.LeaseNextForPool("worker-1", "coding", generation, at)
					return err
				},
				"heartbeat": func() error { return s.HeartbeatLease(job.ID, lease.AttemptID, "worker-1", generation, at) },
				"evidence":  func() error { return s.BindEvidenceLeaseAt(job.ID, lease.AttemptID, "worker-1", generation, nil, at) },
				"result": func() error {
					_, err := s.CompleteLeaseAt(job.ID, lease.AttemptID, "worker-1", generation, "done", at)
					return err
				},
				"failure": func() error {
					_, err := s.FailLeaseAt(job.ID, lease.AttemptID, "worker-1", generation, "failed", TerminalFailure, at)
					return err
				},
			}
			for operation, check := range checks {
				if err := check(); err == nil {
					t.Errorf("%s accepted generation %q", operation, generation)
				}
			}
		})
	}
}

func TestActivePolicyMismatchIsCorruptionAndRollsBack(t *testing.T) {
	s := testStore(t)
	policy := testNoncodingPolicy("coding")
	start := time.Now().UTC()
	job, err := s.CreateJobWithPolicy("work", policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, start); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, start)
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	if _, err := s.db.Exec(`UPDATE attempts SET resolved_policy=json(resolved_policy) || ' ' WHERE id=?`, lease.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := s.HeartbeatLease(job.ID, lease.AttemptID, "worker-1", testGeneration, start.Add(time.Second)); err == nil {
		t.Fatal("accepted mismatched policy")
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || !attempts[0].DeadlineAt.Equal(start.Add(10*time.Second)) {
		t.Fatalf("corrupt transition did not roll back: %#v, %v", attempts, err)
	}
}

func TestMigrationVersionAndLegacyStartupBoundary(t *testing.T) {
	s := testStore(t)
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	legacy, err := s.CreateJob("legacy work")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateActivePolicies(); err == nil || strings.Contains(err.Error(), "legacy work") {
		t.Fatalf("active legacy validation = %v", err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET status='failed' WHERE id=?`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateActivePolicies(); err != nil {
		t.Fatalf("terminal legacy history blocked startup: %v", err)
	}
	if job, err := s.Job(legacy.ID); err != nil || job.Input != "legacy work" || job.Status != "failed" {
		t.Fatalf("terminal legacy history = %#v, %v", job, err)
	}
}

func strings40(value string) string {
	result := ""
	for range 40 {
		result += value
	}
	return result
}
