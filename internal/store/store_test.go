package store

import (
	"path/filepath"
	"testing"

	"agent-forge/internal/protocol"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestResultIsBoundToLeaseAttemptAndIdempotent(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("hello")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if lease.JobID != job.ID {
		t.Fatalf("got job %q want %q", lease.JobID, job.ID)
	}

	if _, err := s.Complete(job.ID, "wrong-attempt", "HELLO"); err == nil {
		t.Fatal("wrong attempt accepted")
	}
	first, err := s.Complete(job.ID, lease.AttemptID, "HELLO")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Complete(job.ID, lease.AttemptID, "HELLO")
	if err != nil {
		t.Fatalf("identical retry must be idempotent: %v", err)
	}
	if first.Result != second.Result || second.Status != "succeeded" {
		t.Fatalf("unexpected results: %#v %#v", first, second)
	}
	if _, err := s.Complete(job.ID, lease.AttemptID, "DIFFERENT"); err == nil {
		t.Fatal("mutable duplicate accepted")
	}
}

func TestWorkerCannotLeaseSecondJobUntilFirstIsTerminal(t *testing.T) {
	s := testStore(t)
	first, err := s.CreateJob("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateJob("second")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok || lease.JobID != first.ID {
		t.Fatalf("first lease = %#v, %v, %v", lease, ok, err)
	}
	if lease, ok, err := s.LeaseNext("worker-1"); err != nil || ok {
		t.Fatalf("second lease while first nonterminal = %#v, %v, %v", lease, ok, err)
	}
	if _, err := s.Complete(first.ID, lease.AttemptID, "done"); err != nil {
		t.Fatal(err)
	}
	lease, ok, err = s.LeaseNext("worker-1")
	if err != nil || !ok || lease.JobID != second.ID {
		t.Fatalf("second lease after first terminal = %#v, %v, %v", lease, ok, err)
	}
}

func TestWorkerLivenessTracksConnectionState(t *testing.T) {
	s := testStore(t)
	if err := s.SetWorkerConnected("w1", true); err != nil {
		t.Fatal(err)
	}
	w, err := s.Worker("w1")
	if err != nil || !w.Connected || w.LastSeen.IsZero() {
		t.Fatalf("connected worker not live: %#v %v", w, err)
	}
	if err := s.SetWorkerConnected("w1", false); err != nil {
		t.Fatal(err)
	}
	w, err = s.Worker("w1")
	if err != nil || w.Connected {
		t.Fatalf("disconnected worker reported live: %#v %v", w, err)
	}
}

func TestCandidateResultIsExactAndImmutable(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateCodingJob(protocol.CodingTask{Instruction: "coding task"})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	want := "1111111111111111111111111111111111111111"
	first, err := s.CompleteCandidate(job.ID, lease.AttemptID, want)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CompleteCandidate(job.ID, lease.AttemptID, want)
	if err != nil {
		t.Fatalf("identical retry must be idempotent: %v", err)
	}
	if first.CandidateSHA != want || second.CandidateSHA != want {
		t.Fatalf("candidate SHA not preserved: %#v %#v", first, second)
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].Detail; got != "candidate_sha="+want {
		t.Fatalf("terminal event detail = %q", got)
	}
	if _, err := s.CompleteCandidate(job.ID, lease.AttemptID, "2222222222222222222222222222222222222222"); err == nil {
		t.Fatal("conflicting candidate accepted")
	}
}

func TestCodingJobRejectsLegacyTextCompletion(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateCodingJob(protocol.CodingTask{Instruction: "coding task"})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if _, err := s.Complete(job.ID, lease.AttemptID, "legacy result"); err == nil {
		t.Fatal("coding job accepted legacy text completion")
	}
}

func TestLegacyJobRejectsCandidateCompletion(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("legacy job")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if _, err := s.CompleteCandidate(job.ID, lease.AttemptID, "1111111111111111111111111111111111111111"); err == nil {
		t.Fatal("legacy job accepted candidate completion")
	}
}

func TestFailureEventNamesFailureCode(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("legacy job")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if _, err := s.Fail(job.ID, lease.AttemptID, "scoped_test_failed"); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].Detail; got != "failure_code=scoped_test_failed" {
		t.Fatalf("terminal failure event detail = %q", got)
	}
	coding, err := s.CreateCodingJob(protocol.CodingTask{Instruction: "coding task"})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err = s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("coding lease: ok=%v err=%v", ok, err)
	}
	if _, err := s.Fail(coding.ID, lease.AttemptID, "execution_failed"); err != nil {
		t.Fatalf("coding failure rejected: %v", err)
	}
}
