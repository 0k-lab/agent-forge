package store

import (
	"agent-forge/internal/protocol"
	"testing"
	"time"
)

const candidateReport = `{"summary":"Normalize input","changes":["Handle equivalent Unicode names"]}`

func TestCandidateReportPersistsWithMatchingAttempt(t *testing.T) {
	for _, mode := range []string{"legacy", "owned", "delivery"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			at := time.Now().UTC()
			task := protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: strings40("a"), Instruction: "edit"}
			var job Job
			var err error
			if mode == "legacy" {
				job, err = s.CreateCodingJob(task)
			} else {
				job, err = s.CreateCodingJobWithPolicy(task, testResolvedPolicy("coding"))
			}
			if err != nil {
				t.Fatal(err)
			}
			var lease Lease
			var ok bool
			if mode == "legacy" {
				lease, ok, err = s.LeaseNext("worker-1")
			} else {
				if err = s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, at); err != nil {
					t.Fatal(err)
				}
				lease, ok, err = s.LeaseNextForPool("worker-1", "coding", testGeneration, at)
			}
			if err != nil || !ok {
				t.Fatal("lease", err)
			}
			complete := func(report string) (Job, error) {
				var got Job
				var err error
				switch mode {
				case "legacy":
					got, err = s.CompleteCandidateReportAt(job.ID, lease.AttemptID, strings40("b"), report, at.Add(time.Second))
				case "owned":
					got, err = s.CompleteCandidateReportLeaseAt(job.ID, lease.AttemptID, "worker-1", testGeneration, strings40("b"), report, at.Add(time.Second))
				case "delivery":
					delivery := Delivery{JobID: job.ID, AttemptID: lease.AttemptID, CandidateSHA: strings40("b"), ExpectedTreeSHA: strings40("c"), ParentSHA: strings40("a"), CandidateRef: "refs/agent-forge/candidates/" + job.ID + "/" + lease.AttemptID, RepositoryID: "agent-forge", RepositoryURL: "https://github.com/org/repo.git", DefaultBranch: "main", Branch: "forge/" + job.ID, PRTitle: "Normalize input", MaxAttempts: 3}
					got, err = s.CompleteCandidateDeliveryReportLeaseAt(job.ID, lease.AttemptID, "worker-1", testGeneration, delivery, report, at.Add(time.Second))
				}
				return got, err
			}

			for _, bad := range []string{`{"summary":"secret","changes":[]}`, `{"summary":"x","changes":["x"],"private":"secret"}`, `not-json-secret`} {
				if _, err := complete(bad); err == nil {
					t.Fatal("malformed report mutated candidate state")
				}
				before, _ := s.Job(job.ID)
				attempts, _ := s.Attempts(job.ID)
				if before.Status != "leased" || before.Result != "" || before.CandidateSHA != "" || attempts[0].Status != "leased" || attempts[0].Result != "" {
					t.Fatal("invalid report partially persisted")
				}
			}
			got, err := complete(candidateReport)

			attempts, _ := s.Attempts(job.ID)
			if err != nil || got.Result != candidateReport || len(attempts) != 1 || attempts[0].Result != candidateReport || attempts[0].ID != lease.AttemptID {
				t.Fatalf("report lost: %v job=%q attempts=%v", err, got.Result, attempts)
			}
			if mode == "delivery" {
				if _, ok, err := s.ClaimDelivery(at.Add(2 * time.Second)); err != nil || !ok {
					t.Fatal(err)
				}
				if err := s.RecoverDeliveries(at.Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, ok, err := s.ClaimDelivery(at.Add(4 * time.Second)); err != nil || !ok {
					t.Fatal(err)
				}
				if err := s.CompleteDelivery(job.ID, strings40("d"), at.Add(5*time.Second)); err != nil {
					t.Fatal(err)
				}
				final, _ := s.Job(job.ID)
				attempts, _ = s.Attempts(job.ID)
				if final.Result != candidateReport || attempts[0].Result != candidateReport {
					t.Fatal("delivery erased report")
				}
			}
		})
	}
}

func TestCandidateReportRetryIsolation(t *testing.T) {
	s := testStore(t)
	at := time.Now().UTC()
	job, err := s.CreateCodingJobWithPolicy(protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: strings40("a"), Instruction: "edit"}, testResolvedPolicy("coding"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, at); err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, at)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err = s.FailLeaseAt(job.ID, first.AttemptID, "worker-1", testGeneration, "execution_failed", RetryableFailure, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, at.Add(10*time.Second))
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err = s.CompleteCandidateReportLeaseAt(job.ID, first.AttemptID, "worker-1", testGeneration, strings40("b"), candidateReport, at.Add(11*time.Second)); err == nil {
		t.Fatal("stale attempt report accepted")
	}
	before, _ := s.Job(job.ID)
	attempts, _ := s.Attempts(job.ID)
	if before.Result != "" || attempts[0].Result != "" || attempts[1].Result != "" {
		t.Fatal("report carried into retry")
	}
	if _, err = s.CompleteCandidateReportLeaseAt(job.ID, second.AttemptID, "worker-1", testGeneration, strings40("b"), candidateReport, at.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts, _ = s.Attempts(job.ID)
	if attempts[0].Result != "" || attempts[1].Result != candidateReport || attempts[1].ID != second.AttemptID {
		t.Fatal("report bound to wrong attempt")
	}
}
