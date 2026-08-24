package store

import (
	"path/filepath"
	"testing"
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
