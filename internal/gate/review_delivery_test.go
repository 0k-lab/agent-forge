package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-forge/internal/configjson"
	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
)

func TestControlDeliveryPolicySubmission(t *testing.T) {
	s, x, h := controlFixture(t)
	fixture, _, _ := gitHTTPFixture(t)
	original := publicCloneURL
	publicCloneURL = func(publicSource) string { return fixture }
	t.Cleanup(func() { publicCloneURL = original })
	x.config.Delivery = &DeliveryConfig{MaxAttempts: 3}
	generation := strings.Repeat("b", 32)
	at := time.Now().UTC()
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "general", generation, at); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "automatic", "review", "unknown", "Review", "null", "42"} {
		t.Run(value, func(t *testing.T) {
			body := `{"project":"agent-forge","title":"Fix parser","instruction":"Preserve Unicode","check_preset":"go"`
			if value != "" {
				raw, _ := json.Marshal(value)
				if value == "null" || value == "42" {
					raw = []byte(value)
				}
				body += `,"delivery_policy":` + string(raw)
			}
			body += `}`
			w := controlRequest(h, "POST", "/v1/control/jobs", "owner", body)
			valid := value == "" || value == "automatic" || value == "review"
			if !valid {
				if w.Code != 400 {
					t.Fatalf("invalid policy %q: %d %s", value, w.Code, w.Body)
				}
				return
			}
			if w.Code != 201 {
				t.Fatalf("policy %q: %d %s", value, w.Code, w.Body)
			}
			lease, ok, err := s.LeaseNextForPool("worker-1", "general", generation, time.Now().UTC())
			if err != nil || !ok || lease.Policy.DeliveryPolicy != protocol.DeliveryPolicy(value) {
				t.Fatalf("pinned policy=%q ok=%v err=%v", lease.Policy.DeliveryPolicy, ok, err)
			}
			if _, err := s.FailLeaseAt(lease.JobID, lease.AttemptID, "worker-1", generation, protocol.FailureExecution, "terminal", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func heldControlCandidate(t *testing.T) (*store.Store, *server, http.Handler, store.Job) {
	t.Helper()
	s, x, h := controlFixture(t)
	fixture, base, candidate := gitHTTPFixture(t)
	original := publicCloneURL
	publicCloneURL = func(publicSource) string { return fixture }
	t.Cleanup(func() { publicCloneURL = original })
	repo := x.config.Repositories[0]
	x.config.Delivery = &DeliveryConfig{MaxAttempts: 3}
	local, err := provisionPublicRepository(context.Background(), *x.config, repo, base)
	if err != nil {
		t.Fatal(err)
	}
	policy := x.config.resolvedPolicy(repo.WorkerPool, repo.Execution, repo.ID, repo.DefaultBranch)
	policy.DeliveryPolicy = protocol.DeliveryReview
	job, err := s.CreateCodingJobWithPolicyAndSource(protocol.CodingTask{RepositoryID: repo.ID, Repository: local, BaseSHA: base, Instruction: "Fix parser", Tests: [][]string{{"go", "test", "./..."}}}, policy, "https://github.com/0k-lab/agent-forge/issues/89")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	generation := strings.Repeat("b", 32)
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", repo.WorkerPool, generation, at); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", repo.WorkerPool, generation, at)
	if err != nil || !ok {
		t.Fatal("lease", err)
	}
	gitCommand(t, x.config.GitExecutable, local, "update-ref", "refs/agent-forge/candidates/"+job.ID+"/"+lease.AttemptID, candidate)
	lease.Task.Repository = local
	d, err := x.deliveryForCandidate(context.Background(), lease, candidate)
	if err != nil {
		t.Fatal(err)
	}
	index, exit := 0, 0
	if err := s.BindEvidenceLeaseAt(job.ID, lease.AttemptID, "worker-1", generation, []protocol.AttemptEvidence{{EvidenceID: strings.Repeat("d", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckPassed, CheckIndex: &index, ExitCode: &exit, DurationMS: 15, BaseSHA: base, CandidateSHA: candidate, Output: protocol.EvidenceRedactedMarker, OutputRedacted: true}}, at.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	job, err = s.CompleteCandidateDeliveryReportLeaseAt(job.ID, lease.AttemptID, "worker-1", generation, d, `{"summary":"Fix input","changes":["Normalize input"]}`, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return s, x, h, job
}

func TestControlHeldCandidateVisibilityBeforePublisher(t *testing.T) {
	s, x, h, job := heldControlCandidate(t)
	// Any claim reaches registration checking and then the publisher. Poison the
	// registration so an accidental claim fails visibly without network traffic.
	x.config.Repositories = nil
	x.runOneDelivery(context.Background())
	d, err := s.Delivery(job.ID)
	if err != nil || d.Phase != "awaiting_review" || d.Attempts != 0 {
		t.Fatalf("publisher reached: %+v %v", d, err)
	}
	w := controlRequest(h, "GET", "/v1/control/jobs/"+job.ID, "owner", "")
	for _, want := range []string{`"phase":"awaiting_review"`, `"status":"delivering"`, `"summary":"Fix input"`, `"check_count":1`, `"reason":"scoped_check_passed"`, job.CandidateSHA, job.AttemptID, job.SourceRef, `"type":"delivery_review"`} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s: %d %s", want, w.Code, w.Body)
		}
	}
	if strings.Count(w.Body.String(), `"agent_report":`) != 2 {
		t.Fatal("report not bound to run and attempt")
	}
}

func TestControlResumeOwnerAuthenticationAndIdempotency(t *testing.T) {
	s, x, h, job := heldControlCandidate(t)
	path := "/v1/control/jobs/" + job.ID + "/resume-delivery"
	for _, token := range []string{"", "wrong"} {
		w := controlRequest(h, "POST", path, token, "")
		if w.Code != 401 {
			t.Fatalf("unauthorized resume=%d", w.Code)
		}
	}
	d, _ := s.Delivery(job.ID)
	if d.Phase != "awaiting_review" {
		t.Fatal("unauthorized mutation")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := controlRequest(h, "POST", path, "owner", "")
			if w.Code != 200 {
				t.Errorf("resume=%d %s", w.Code, w.Body)
			}
		}()
	}
	wg.Wait()
	d, _ = s.Delivery(job.ID)
	if d.Phase != "pending" || d.Attempts != 0 {
		t.Fatalf("resume must only release existing delivery: %+v", d)
	}
	// Existing delivery logic is entered, without another agent attempt.
	x.config.Repositories = nil
	x.runOneDelivery(context.Background())
	d, _ = s.Delivery(job.ID)
	if d.Phase != "failed" || d.FailureCode != "delivery_registration_changed" || d.Attempts != 1 {
		t.Fatalf("existing pipeline not resumed: %+v", d)
	}
	w := controlRequest(h, "POST", path, "owner", "")
	if w.Code != 200 {
		t.Fatalf("late duplicate=%d %s", w.Code, w.Body)
	}
	attempts, _ := s.Attempts(job.ID)
	if len(attempts) != 1 {
		t.Fatal("agent reran")
	}
	other, err := s.CreateJob("pending")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{other.ID, strings.Repeat("0", 32)} {
		if w := controlRequest(h, "POST", "/v1/control/jobs/"+id+"/resume-delivery", "owner", ""); w.Code != 409 {
			t.Fatalf("non-held resume=%d", w.Code)
		}
	}
}

// Frozen prior-tree v1 policy: strict receivers must not learn Gate-only fields.
type legacyV1Policy struct {
	Version        int                      `json:"version"`
	WorkerPool     string                   `json:"worker_pool"`
	LeaseTTLNanos  int64                    `json:"lease_ttl_nanos"`
	RetryBaseNanos int64                    `json:"retry_base_nanos"`
	MaxAttempts    int                      `json:"max_attempts"`
	RetryAlgorithm string                   `json:"retry_algorithm"`
	RetryMaxNanos  int64                    `json:"retry_max_nanos"`
	Execution      protocol.ExecutionPolicy `json:"execution"`
}

func TestDeliveryLeaseLegacyV1Receiver(t *testing.T) {
	for _, mode := range []protocol.DeliveryPolicy{"", protocol.DeliveryAutomatic, protocol.DeliveryReview} {
		t.Run(string(mode), func(t *testing.T) {
			s, cfg, server := configuredGate(t)
			p := cfg.resolvedPolicy("general", cfg.DefaultExecution, "", "")
			legacy, err := store.CanonicalPolicy(p)
			if err != nil {
				t.Fatal(err)
			}
			p.DeliveryPolicy = mode
			if _, err := s.CreateJobWithPolicy("work", p); err != nil {
				t.Fatal(err)
			}
			c := dialConfiguredSlot(t, server.URL, 0)
			defer c.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, body, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var lease struct {
				Type      string         `json:"type"`
				JobID     string         `json:"job_id"`
				AttemptID string         `json:"attempt_id"`
				Input     string         `json:"input"`
				Policy    legacyV1Policy `json:"policy"`
			}
			if err := configjson.Decode(body, &lease); err != nil {
				t.Fatalf("legacy v1 rejected %q lease: %v", mode, err)
			}
			got, err := json.Marshal(lease.Policy)
			if err != nil || string(got) != string(legacy) {
				t.Fatalf("legacy canonical policy changed: %s want %s (%v)", got, legacy, err)
			}
		})
	}
}

func TestControlResumeRejectsIdentityDrift(t *testing.T) {
	for _, drift := range []string{"repository URL", "default branch", "missing repository", "candidate ref", "candidate tree"} {
		t.Run(drift, func(t *testing.T) {
			s, x, h, job := heldControlCandidate(t)
			switch drift {
			case "repository URL":
				x.config.Repositories[0].RepositoryURL = "https://github.com/octo/other.git"
			case "default branch":
				x.config.Repositories[0].DefaultBranch = "other"
			case "missing repository":
				x.config.Repositories = nil
			case "candidate ref":
				gitCommand(t, x.config.GitExecutable, job.Task.Repository, "update-ref", "refs/agent-forge/candidates/"+job.ID+"/"+job.AttemptID, job.Task.BaseSHA)
			case "candidate tree":
				original := publicGitRunner
				publicGitRunner = func(ctx context.Context, executable, repository string, timeout time.Duration, limit int64, args ...string) (string, error) {
					if len(args) == 2 && args[0] == "rev-parse" && args[1] == job.CandidateSHA+"^{tree}" {
						return strings.Repeat("e", 40), nil
					}
					return original(ctx, executable, repository, timeout, limit, args...)
				}
				t.Cleanup(func() { publicGitRunner = original })
			}
			w := controlRequest(h, "POST", "/v1/control/jobs/"+job.ID+"/resume-delivery", "owner", "")
			if w.Code != 409 {
				t.Errorf("drift resume=%d %s", w.Code, w.Body)
			}
			d, err := s.Delivery(job.ID)
			if err != nil || d.Phase != "awaiting_review" || d.Attempts != 0 {
				t.Errorf("drift released hold: %+v %v", d, err)
			}
		})
	}
}
