package store

import (
	"testing"
	"time"

	"agent-forge/internal/protocol"
)

func TestDeliveryLifecycleRetriesRecoversAndFinishesWithoutAnotherAttempt(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	job, err := s.CreateCodingJobWithPolicy(protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: strings40("a"), Instruction: "change"}, testResolvedPolicy("coding"))
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
	delivery := Delivery{JobID: job.ID, AttemptID: lease.AttemptID, CandidateSHA: strings40("b"), ExpectedTreeSHA: strings40("c"), ParentSHA: strings40("a"), CandidateRef: "refs/agent-forge/candidates/" + job.ID + "/" + lease.AttemptID, RepositoryID: "agent-forge", RepositoryURL: "https://github.com/octo/repo.git", DefaultBranch: "main", Branch: "forge/" + job.ID, PRTitle: "Agent Forge job " + job.ID, PRBody: "candidate", MaxAttempts: 3}
	completed, err := s.CompleteCandidateDeliveryLeaseAt(job.ID, lease.AttemptID, "worker-1", testGeneration, delivery, start.Add(time.Second))
	if err != nil || completed.Status != "delivering" {
		t.Fatalf("candidate completion = %#v, %v", completed, err)
	}
	claimed, ok, err := s.ClaimDelivery(start.Add(2 * time.Second))
	if err != nil || !ok || claimed.Attempts != 1 || claimed.CandidateSHA != delivery.CandidateSHA {
		t.Fatalf("claim = %#v, %v, %v", claimed, ok, err)
	}
	if err := s.UpdateDelivery(job.ID, "ci", "https://github.com/octo/repo/pull/1", 1, "pending", start.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverDeliveries(start.Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = s.ClaimDelivery(start.Add(4 * time.Second))
	if err != nil || !ok || claimed.Attempts != 2 {
		t.Fatalf("recovered claim = %#v, %v, %v", claimed, ok, err)
	}
	if err := s.UpdateDelivery(job.ID, "merging", "", 0, "success", start.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDelivery(job.ID, strings40("d"), start.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	final, err := s.Job(job.ID)
	if err != nil || final.Status != "succeeded" || final.CandidateSHA != delivery.CandidateSHA {
		t.Fatalf("final job = %#v, %v", final, err)
	}
	attempts, _ := s.Attempts(job.ID)
	if len(attempts) != 1 || attempts[0].Status != "succeeded" {
		t.Fatalf("worker reran: %#v", attempts)
	}
	if err := s.ValidateDeliveries(); err != nil {
		t.Fatal(err)
	}
}
