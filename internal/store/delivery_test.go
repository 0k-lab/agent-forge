package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
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

func TestDeliveryPolicyCanonicalCompatibility(t *testing.T) {
	p := testResolvedPolicy("coding")
	old, err := CanonicalPolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "automatic", "review", "unknown", "Review"} {
		body := old
		if value != "" {
			body = []byte(strings.TrimSuffix(string(old), "}") + `,"delivery_policy":"` + value + `"}`)
		}
		var policy ResolvedPolicy
		if err := json.Unmarshal(body, &policy); err != nil {
			t.Fatal(err)
		}
		canonical, err := CanonicalPolicy(policy)
		valid := value == "" || value == "automatic" || value == "review"
		if (err == nil) != valid {
			t.Fatalf("policy %q accepted=%v want=%v", value, err == nil, valid)
		}
		if !valid {
			continue
		}
		if value != "" && !strings.Contains(string(canonical), `"delivery_policy":"`+value+`"`) {
			t.Fatalf("policy %q discarded: %s", value, canonical)
		}
		if value == "" && string(canonical) != string(old) {
			t.Fatal("legacy canonical bytes changed")
		}
		if _, err := DecodeCanonicalPolicy(canonical); err != nil {
			t.Fatal(err)
		}
	}
}

func reviewCandidate(t *testing.T, s *Store) (Job, Delivery, time.Time) {
	t.Helper()
	at := time.Now().UTC()
	policy := testResolvedPolicy("coding")
	policy.DeliveryPolicy = protocol.DeliveryReview
	job, err := s.CreateCodingJobWithPolicyAndSource(protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: strings40("a"), Instruction: "change", Tests: [][]string{{"go", "test", "./..."}}}, policy, "https://github.com/octo/repo/issues/89")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, at); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, at)
	if err != nil || !ok {
		t.Fatal("lease", err)
	}
	d := Delivery{JobID: job.ID, AttemptID: lease.AttemptID, CandidateSHA: strings40("b"), ExpectedTreeSHA: strings40("c"), ParentSHA: strings40("a"), CandidateRef: "refs/agent-forge/candidates/" + job.ID + "/" + lease.AttemptID, RepositoryID: "agent-forge", RepositoryURL: "https://github.com/octo/repo.git", DefaultBranch: "main", Branch: "forge/" + job.ID, PRTitle: "change", MaxAttempts: 3}
	job, err = s.CompleteCandidateDeliveryReportLeaseAt(job.ID, lease.AttemptID, "worker-1", testGeneration, d, `{"summary":"Fix input","changes":["Normalize input"]}`, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return job, d, at.Add(2 * time.Second)
}

func TestReviewCandidateHeldAcrossRestart(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "review.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	job, candidate, at := reviewCandidate(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ValidateDeliveries(); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverDeliveries(at); err != nil {
		t.Fatal(err)
	}
	d, err := s.Delivery(job.ID)
	if err != nil || d.Phase != "awaiting_review" || d.Attempts != 0 || d.CandidateSHA != candidate.CandidateSHA || d.ExpectedTreeSHA != candidate.ExpectedTreeSHA {
		t.Fatalf("hold lost: %+v %v", d, err)
	}
	if _, ok, err := s.ClaimDelivery(at); err != nil || ok {
		t.Fatalf("held candidate claimed: %v %v", ok, err)
	}
	got, _ := s.Job(job.ID)
	attempts, _ := s.Attempts(job.ID)
	if got.Status != "delivering" || got.SourceRef != job.SourceRef || got.Result != job.Result || len(got.Task.Tests) != 1 || len(attempts) != 1 || attempts[0].Result != job.Result || attempts[0].CandidateSHA != candidate.CandidateSHA {
		t.Fatalf("evidence lost: %+v %+v", got, attempts)
	}
	if _, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, at); err != nil || ok {
		t.Fatal("held job re-executed", err)
	}
}

func TestReviewResumeConcurrentAndRestartSafe(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "resume.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	job, candidate, at := reviewCandidate(t, s)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ResumeDelivery(job.ID, at); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='delivery_resumed'`, job.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("resume events=%d %v", count, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverDeliveries(at); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeDelivery(job.ID, at); err != nil {
		t.Fatal(err)
	}
	d, ok, err := s.ClaimDelivery(at)
	if err != nil || !ok || d.JobID != job.ID || d.CandidateSHA != candidate.CandidateSHA || d.Attempts != 1 {
		t.Fatalf("resume claim %+v %v %v", d, ok, err)
	}
	if err := s.ResumeDelivery(job.ID, at); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimDelivery(at); err != nil || ok {
		t.Fatal("duplicate claimed", err)
	}
	if err := s.CompleteDelivery(job.ID, strings40("d"), at); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeDelivery(job.ID, at); err != nil {
		t.Fatal(err)
	}
	attempts, _ := s.Attempts(job.ID)
	if len(attempts) != 1 {
		t.Fatal("agent re-executed")
	}
}

func TestReviewRejectsIllegalTransitions(t *testing.T) {
	for _, corruption := range []string{"unknown-review-phase", "unknown-delivery-phase", "active-hold", "wrong-job", "missing-hold", "unknown-policy"} {
		t.Run(corruption, func(t *testing.T) {
			s := testStore(t)
			job, _, at := reviewCandidate(t, s)
			if err := s.UpdateDelivery(job.ID, "ci", "", 0, "pending", at); err == nil {
				t.Fatal("held delivery advanced")
			}
			if err := s.FailDelivery(job.ID, "delivery_failed", at); err == nil {
				t.Fatal("held delivery failed")
			}
			if err := s.CompleteDelivery(job.ID, strings40("d"), at); err == nil {
				t.Fatal("held delivery completed")
			}
			var err error
			switch corruption {
			case "unknown-review-phase":
				_, err = s.db.Exec(`UPDATE events SET detail='phase=surprise' WHERE job_id=? AND kind='delivery_review'`, job.ID)
			case "unknown-delivery-phase":
				_, err = s.db.Exec(`PRAGMA ignore_check_constraints=ON`)
				if err == nil {
					_, err = s.db.Exec(`UPDATE deliveries SET phase='surprise' WHERE job_id=?`, job.ID)
				}
			case "active-hold":
				_, err = s.db.Exec(`UPDATE deliveries SET phase='publishing',attempts=1 WHERE job_id=?`, job.ID)
			case "wrong-job":
				_, err = s.db.Exec(`UPDATE jobs SET status='failed' WHERE id=?`, job.ID)
			case "missing-hold":
				_, err = s.db.Exec(`DELETE FROM events WHERE job_id=? AND kind='delivery_review'`, job.ID)
			case "unknown-policy":
				_, err = s.db.Exec(`UPDATE jobs SET resolved_policy=replace(resolved_policy,'"delivery_policy":"review"','"delivery_policy":"surprise"') WHERE id=?`, job.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ValidateDeliveries(); err == nil {
				t.Fatal("corrupt review state accepted on startup")
			}
			if err := s.ResumeDelivery(job.ID, at); err == nil {
				t.Fatal("corrupt review state resumed")
			}
			if _, ok, _ := s.ClaimDelivery(at); ok {
				t.Fatal("corrupt review state claimed")
			}
		})
	}
	s := testStore(t)
	job, err := s.CreateJob("pending")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{job.ID, strings.Repeat("0", 32)} {
		if err := s.ResumeDelivery(id, time.Now()); err == nil {
			t.Fatal("non-held resume succeeded")
		}
	}
}

func TestReviewCandidateCannotBypassDeliveryHold(t *testing.T) {
	s := testStore(t)
	policy := testResolvedPolicy("coding")
	policy.DeliveryPolicy = protocol.DeliveryReview
	job, err := s.CreateCodingJobWithPolicy(protocol.CodingTask{RepositoryID: "agent-forge", BaseSHA: strings40("a"), Instruction: "change"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "coding", testGeneration, at); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", "coding", testGeneration, at)
	if err != nil || !ok {
		t.Fatal("lease", err)
	}
	if _, err := s.CompleteCandidateReportLeaseAt(job.ID, lease.AttemptID, "worker-1", testGeneration, strings40("b"), "", at.Add(time.Second)); err == nil {
		t.Fatal("review candidate completed without a hold when delivery is unavailable")
	}
	got, _ := s.Job(job.ID)
	if got.Status != "leased" || got.CandidateSHA != "" {
		t.Fatal("rejected candidate mutated job")
	}
}

func TestReviewJournalOrderingFailsClosed(t *testing.T) {
	for _, history := range [][]string{
		{"delivery_resumed", "delivery_review"},
		{"delivery_review", "delivery_resumed", "delivery_review"},
		{"delivery_review", "delivery_resumed", "delivery_resumed"},
		{"delivery_resumed"},
		{"delivery_phase", "delivery_review", "delivery_resumed"},
		{"delivery_review", "delivery_phase", "delivery_resumed"},
		{"delivery_review", "delivery_pending", "delivery_resumed"},
	} {
		t.Run(strings.Join(history, "/"), func(t *testing.T) {
			s := testStore(t)
			job, _, at := reviewCandidate(t, s)
			if _, err := s.db.Exec(`DELETE FROM events WHERE job_id=? AND kind='delivery_review'`, job.ID); err != nil {
				t.Fatal(err)
			}
			for _, kind := range history {
				detail := "phase=pending"
				if kind == "delivery_review" {
					detail = "phase=awaiting_review"
				}
				if _, err := s.db.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, job.ID, kind, detail, at.Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ValidateDeliveries(); err == nil {
				t.Error("out-of-order/duplicate journal validated")
			}
			if _, claimed, _ := s.ClaimDelivery(at); claimed {
				t.Error("out-of-order/duplicate journal claimed")
			}
			if err := s.ResumeDelivery(job.ID, at); err == nil {
				t.Error("out-of-order/duplicate journal resumed")
			}
		})
	}
}

func TestDeliveryIdentityContradictionsFailClosed(t *testing.T) {
	for name, query := range map[string]string{
		"candidate ref":      `UPDATE deliveries SET candidate_ref='refs/agent-forge/candidates/00000000000000000000000000000000/11111111111111111111111111111111'`,
		"parent":             `UPDATE deliveries SET parent_sha='eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee'`,
		"repository":         `UPDATE deliveries SET repository_id='another-repo'`,
		"default branch":     `UPDATE deliveries SET default_branch='other'`,
		"publication branch": `UPDATE deliveries SET branch='forge/other'`,
		"task base":          `UPDATE jobs SET task_json=replace(task_json,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee')`,
		"attempt status":     `UPDATE attempts SET status='leased'`,
		"attempt candidate":  `UPDATE attempts SET candidate_sha='eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee'`,
		"attempt policy":     `UPDATE attempts SET resolved_policy=replace(resolved_policy,'"default_branch":"main"','"default_branch":"other"')`,
		"tree":               `UPDATE deliveries SET expected_tree_sha='invalid'`,
	} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			job, _, at := reviewCandidate(t, s)
			// Release before corruption to exercise ClaimDelivery's shared validation.
			if err := s.ResumeDelivery(job.ID, at); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := s.ValidateDeliveries(); err == nil {
				t.Error("contradictory identity validated")
			}
			if err := s.ResumeDelivery(job.ID, at); err == nil {
				t.Error("contradictory identity resumed")
			}
			if _, ok, _ := s.ClaimDelivery(at); ok {
				t.Error("contradictory identity claimed")
			}
		})
	}
}
