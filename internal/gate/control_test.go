package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
)

func controlFixture(t *testing.T) (*store.Store, *server, http.Handler) {
	t.Helper()
	database := filepath.Join(secureTempDir(t), "control.db")
	s, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	config := publicGateConfig(t)
	config.Database = database
	x := newServer(s, nil, "owner", DefaultOptions())
	x.config = &config
	return s, x, x.routes()
}

func controlRequest(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestControlAssetsAndAuthentication(t *testing.T) {
	_, _, h := controlFixture(t)
	for path, mime := range map[string]string{"/app": "text/html", "/app/": "text/html", "/app/app.css": "text/css", "/app/app.js": "text/javascript"} {
		w := controlRequest(h, "GET", path, "", "")
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), mime) || w.Body.Len() == 0 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if w.Header().Get("Permissions-Policy") != "camera=(), microphone=(), geolocation=()" {
			t.Fatalf("%s missing permissions policy", path)
		}
		for _, method := range []string{"POST", "HEAD", "PUT", "DELETE"} {
			if w := controlRequest(h, method, path, "", ""); w.Code != 405 {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	if w := controlRequest(h, "GET", "/app/missing", "", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	for path, method := range map[string]string{"/v1/control/overview": "GET", "/v1/control/jobs/" + strings.Repeat("a", 32): "GET", "/v1/control/jobs": "POST"} {
		for _, token := range []string{"", "worker", "wrong"} {
			if w := controlRequest(h, method, path, token, "{}"); w.Code != 401 {
				t.Fatalf("%s: %d", path, w.Code)
			}
		}
	}
	if w := controlRequest(h, "GET", "/debug/", "", ""); w.Code != 200 {
		t.Fatal("debug unavailable")
	}
}

func TestControlOverviewProjectsSlotsAndSafeCards(t *testing.T) {
	s, x, h := controlFixture(t)
	repo := x.config.Repositories[0]
	policy := x.config.resolvedPolicy(repo.WorkerPool, repo.Execution, repo.ID, repo.DefaultBranch)
	task := protocol.CodingTask{RepositoryID: repo.ID, BaseSHA: strings.Repeat("a", 40), Instruction: "\n  Fix the parser  \nDetails", Tests: [][]string{{"go", "test", "./internal/parser"}}}
	job, err := s.CreateCodingJobWithPolicyAndSource(task, policy, "https://github.com/org/repo/issues/12")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", repo.WorkerPool, strings.Repeat("b", 32), at); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", repo.WorkerPool, strings.Repeat("b", 32), at)
	if err != nil || !ok || lease.JobID != job.ID {
		t.Fatalf("lease %v %v", ok, err)
	}
	// The agent belongs to the persisted run, even after config changes.
	x.config.Repositories[0].Execution.PluginID = "replacement"
	w := controlRequest(h, "GET", "/v1/control/overview", "owner", "")
	var got struct {
		Jobs []struct{ ID, Title, Project, Status, SourceRef, WorkerID, Agent string }
	}
	// Decode snake_case through a generic map as a wire-contract assertion.
	var wire map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &wire) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if json.Unmarshal(wire["jobs"], &got.Jobs) != nil || len(got.Jobs) != 1 || got.Jobs[0].Title != "Fix the parser" || got.Jobs[0].Project != repo.ID || got.Jobs[0].Status != "leased" || got.Jobs[0].Agent != "codex" {
		t.Fatalf("jobs: %s", w.Body.String())
	}
	var projects, workers []map[string]any
	json.Unmarshal(wire["projects"], &projects)
	json.Unmarshal(wire["workers"], &workers)
	if len(projects) != 1 || projects[0]["default_branch"] != "main" || projects[0]["worker_pool"] != repo.WorkerPool || projects[0]["public_source"] != true || projects[0]["delivery"] != false {
		t.Fatalf("projects %s", wire["projects"])
	}
	if len(workers) != 2 || workers[0]["base_id"] != "worker-1" || workers[0]["slot"] != float64(0) || workers[0]["connected"] != true || workers[0]["occupied"] != true || workers[0]["active_job_id"] != job.ID || workers[0]["agent"] != "codex" || workers[1]["connected"] != false || workers[1]["occupied"] != false || workers[1]["last_seen"] != nil {
		t.Fatalf("workers %s", wire["workers"])
	}
	for _, secret := range []string{x.config.PublicRepositoryRoot, x.config.Database, strings.Repeat("b", 32), "FORGE_WORKER_TOKEN", "repository_url", "environment"} {
		if secret != "" && strings.Contains(w.Body.String(), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
}

func TestControlTitleAndSourceSafety(t *testing.T) {
	s, _, h := controlFixture(t)
	for _, source := range []string{"javascript:alert(1)", "https://user:secret@example.com/issue", "/private/repo", "https://example.com/issue?token=secret", "https://example.com/#secret"} {
		_, err := s.CreateJobWithSource("\n"+strings.Repeat("界", 200)+"\nsecond", source)
		if err != nil {
			t.Fatal(err)
		}
	}
	w := controlRequest(h, "GET", "/v1/control/overview", "owner", "")
	var got struct {
		Jobs []struct {
			Title  string `json:"title"`
			Source string `json:"source_ref"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Jobs) != 5 {
		t.Fatalf("%s", w.Body.String())
	}
	for _, j := range got.Jobs {
		if len([]rune(j.Title)) != 120 || j.Source != "" {
			t.Fatalf("unsafe card %#v", j)
		}
	}
}

func TestControlDetailActiveEvidenceAndDelivery(t *testing.T) {
	s, x, h := controlFixture(t)
	repo := x.config.Repositories[0]
	base, candidate := strings.Repeat("a", 40), strings.Repeat("c", 40)
	job, err := s.CreateCodingJobWithPolicy(protocol.CodingTask{RepositoryID: repo.ID, Repository: "/private/repository", BaseSHA: base, Instruction: "Fix parser\nKeep Unicode intact", Tests: [][]string{{"check", "/private/check", "--token=secret"}}}, x.config.resolvedPolicy(repo.WorkerPool, repo.Execution, repo.ID, repo.DefaultBranch))
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
		t.Fatal(err)
	}
	index, exit := 0, 0
	if err := s.BindEvidenceLeaseAt(job.ID, lease.AttemptID, "worker-1", generation, []protocol.AttemptEvidence{{EvidenceID: strings.Repeat("d", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckPassed, CheckIndex: &index, ExitCode: &exit, DurationMS: 15, BaseSHA: base, CandidateSHA: candidate, Output: protocol.EvidenceRedactedMarker, OutputRedacted: true}}, at.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	path := "/v1/control/jobs/" + job.ID
	w := controlRequest(h, "GET", path, "owner", "")
	var detail struct {
		Job         struct{ Status string } `json:"job"`
		Instruction string                  `json:"instruction"`
		Attempts    []struct {
			Ordinal  int
			WorkerID string `json:"worker_id"`
			Evidence []safeEvidence
		} `json:"attempts"`
		Timeline    []store.DebugEvent `json:"timeline"`
		Delivery    *safeDelivery      `json:"delivery"`
		Diagnostics struct {
			BaseSHA      string `json:"base_sha"`
			CandidateSHA string `json:"candidate_sha"`
		} `json:"diagnostics"`
		SubagentTelemetry string `json:"subagent_telemetry"`
		CheckCount        int    `json:"check_count"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Job.Status != "leased" || detail.Instruction != job.Task.Instruction || len(detail.Attempts) != 1 || detail.Attempts[0].WorkerID != "worker-1" || len(detail.Attempts[0].Evidence) != 1 || detail.Attempts[0].Evidence[0].DurationMS != 15 || len(detail.Timeline) != 2 || detail.CheckCount != 1 || detail.SubagentTelemetry != "No subagent telemetry reported for this run" {
		t.Fatalf("detail %d %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"/private/", "--token=secret", "private-command-output", "repository_url"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	delivery := store.Delivery{JobID: job.ID, AttemptID: lease.AttemptID, CandidateSHA: candidate, ExpectedTreeSHA: strings.Repeat("e", 40), ParentSHA: base, CandidateRef: "refs/agent-forge/candidates/" + job.ID + "/" + lease.AttemptID, RepositoryID: repo.ID, RepositoryURL: repo.RepositoryURL, DefaultBranch: repo.DefaultBranch, Branch: "forge/" + job.ID, PRTitle: "Fix parser", PRBody: "private-delivery-body", MaxAttempts: 3}
	if _, err := s.CompleteCandidateDeliveryLeaseAt(job.ID, lease.AttemptID, "worker-1", generation, delivery, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimDelivery(at.Add(2 * time.Second)); err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.UpdateDelivery(job.ID, "ci", "https://github.com/org/repo/pull/1", 1, "pending", at.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	w = controlRequest(h, "GET", path, "owner", "")
	json.Unmarshal(w.Body.Bytes(), &detail)
	if w.Code != 200 || detail.Job.Status != "delivering" || detail.Delivery == nil || detail.Delivery.Phase != "ci" || detail.Delivery.CIState != "pending" || detail.Delivery.PRURL != "https://github.com/org/repo/pull/1" || detail.Diagnostics.CandidateSHA != candidate || strings.Contains(w.Body.String(), "private-delivery-body") {
		t.Fatalf("delivery %s", w.Body.String())
	}
	if err := s.UpdateDelivery(job.ID, "merging", "", 0, "success", at.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDelivery(job.ID, strings.Repeat("f", 40), at.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	w = controlRequest(h, "GET", path, "owner", "")
	json.Unmarshal(w.Body.Bytes(), &detail)
	if detail.Job.Status != "succeeded" || detail.Delivery.Phase != "merged" || detail.Delivery.MergeSHA != strings.Repeat("f", 40) {
		t.Fatalf("merge %s", w.Body.String())
	}
	if w := controlRequest(h, "GET", "/v1/control/jobs/"+strings.Repeat("0", 32), "owner", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestControlSubmitPinsCurrentDefaultHead(t *testing.T) {
	s, x, h := controlFixture(t)
	fixture, first, head := gitHTTPFixture(t)
	original := publicCloneURL
	publicCloneURL = func(publicSource) string { return fixture }
	t.Cleanup(func() { publicCloneURL = original })
	// Seed the existing preparation boundary at an older acceptable base.
	if _, err := provisionPublicRepository(context.Background(), *x.config, x.config.Repositories[0], first); err != nil {
		t.Fatal(err)
	}
	w := controlRequest(h, "POST", "/v1/control/jobs", "owner", `{"project":"agent-forge","title":"Fix parser","instruction":"Preserve Unicode","source_ref":"https://github.com/0k-lab/agent-forge/issues/12","check_preset":"go"}`)
	var response struct {
		ID string `json:"id"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatalf("submit %d %s", w.Code, w.Body.String())
	}
	job, err := s.Job(response.ID)
	if err != nil || job.Status != "pending" || job.Task == nil || job.Task.BaseSHA != head || job.Task.BaseSHA == first || job.Task.Instruction != "Fix parser\n\nPreserve Unicode" || job.Task.RepositoryID != "agent-forge" || !strings.HasPrefix(job.Task.Repository, x.config.PublicRepositoryRoot+"/") || job.PolicyVersion != 1 || job.WorkerPool != "general" || job.SourceRef != "https://github.com/0k-lab/agent-forge/issues/12" || len(job.Task.Tests) != 1 || strings.Join(job.Task.Tests[0], " ") != "go test ./..." {
		t.Fatalf("pinned job %#v %v", job, err)
	}
	if strings.Contains(w.Body.String(), x.config.PublicRepositoryRoot) {
		t.Fatal("response leaked path")
	}
	generation := strings.Repeat("b", 32)
	at := time.Now().UTC()
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "general", generation, at); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextForPool("worker-1", "general", generation, at)
	if err != nil || !ok || lease.JobID != job.ID || lease.Task.BaseSHA != head || lease.Policy.Execution.PluginID != "codex" {
		t.Fatalf("normal lease %#v %v", lease, err)
	}
}

func TestControlSubmitRejectsWithoutCreatingJob(t *testing.T) {
	s, x, h := controlFixture(t)
	original := publicGitRunner
	calls := 0
	publicGitRunner = func(context.Context, string, string, time.Duration, int64, ...string) (string, error) {
		calls++
		return "", context.DeadlineExceeded
	}
	t.Cleanup(func() { publicGitRunner = original })
	for _, body := range []string{
		`{"project":"missing","title":"Fix","instruction":"parser","check_preset":"go"}`,
		`{"project":"agent-forge","title":"","instruction":"parser","check_preset":"go"}`,
		`{"project":"agent-forge","title":"Fix","instruction":"parser","base_sha":"abc"}`,
		`{"project":"agent-forge","title":"Fix","instruction":"parser","source_ref":"https://secret@example.com/issue","check_preset":"go"}`,
		`{"project":"agent-forge","title":"Fix","instruction":"parser","check_preset":"unknown"}`,
		`{"project":"agent-forge","title":"Fix","instruction":"parser","check_preset":"go","checks":"go test ./..."}`,
	} {
		if w := controlRequest(h, "POST", "/v1/control/jobs", "owner", body); w.Code != 400 {
			t.Fatalf("invalid submit %d %s", w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("invalid requests reached Git")
	}
	body := `{"project":"agent-forge","title":"Fix","instruction":"parser","checks":"go test ./internal/parser\ngo vet ./internal/parser"}`
	w := controlRequest(h, "POST", "/v1/control/jobs", "owner", body)
	if w.Code != 502 || strings.Contains(w.Body.String(), x.config.PublicRepositoryRoot) || w.Body.Len() > 512 {
		t.Fatalf("unavailable %d %s", w.Code, w.Body.String())
	}
	x.config.Repositories[0].RepositoryURL = ""
	if w := controlRequest(h, "POST", "/v1/control/jobs", "owner", body); w.Code != 422 {
		t.Fatalf("unconfigured %d %s", w.Code, w.Body.String())
	}
	page, err := s.RecentDebugJobs(context.Background(), 100, nil)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("created jobs %#v %v", page, err)
	}
}

func TestControlUIContract(t *testing.T) {
	_, _, h := controlFixture(t)
	js := controlRequest(h, "GET", "/app/app.js", "", "").Body.String()
	for _, mapping := range []string{`pending: 'Ready'`, `retry_wait: 'Ready'`, `leased: 'Working'`, `delivering: 'Review & CI'`, `succeeded: 'Done'`, `failed: 'Blocked'`} {
		if !strings.Contains(js, mapping) {
			t.Fatalf("missing authoritative mapping %s", mapping)
		}
	}
	for _, unsafe := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "localStorage", "sessionStorage", "document.cookie", "/v1/debug/"} {
		if strings.Contains(js, unsafe) {
			t.Fatalf("unsafe UI %s", unsafe)
		}
	}
	for _, needed := range []string{"textContent", "Authorization", "setInterval", "/v1/control/overview", "/v1/control/jobs", "No subagent telemetry reported for this run", "showModal", "submitPending"} {
		if !strings.Contains(js, needed) {
			t.Fatalf("missing UI behavior %s", needed)
		}
	}
	html := controlRequest(h, "GET", "/app/", "", "").Body.String()
	for _, needed := range []string{`type="password"`, `id="task-form"`, `id="project-filter"`, `id="workers-view"`, `aria-live="polite"`, `<dialog`, `<label`} {
		if !strings.Contains(html, needed) {
			t.Fatalf("missing accessible UI %s", needed)
		}
	}
	if strings.Contains(html, "base_sha") || strings.Contains(html, `src="https://`) || strings.Contains(html, `href="https://`) {
		t.Fatal("UI needs SHA or external runtime")
	}
}

func TestControlRunDetailUsesPinnedAgentTimeout(t *testing.T) {
	s, x, h := controlFixture(t)
	repo := x.config.Repositories[0]
	policy := x.config.resolvedPolicy(repo.WorkerPool, repo.Execution, repo.ID, repo.DefaultBranch)
	job, err := s.CreateCodingJobWithPolicy(protocol.CodingTask{RepositoryID: repo.ID, BaseSHA: strings.Repeat("a", 40), Instruction: "Fix"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	x.config.Repositories[0].Execution.PluginTimeout = time.Hour
	w := controlRequest(h, "GET", "/v1/control/jobs/"+job.ID, "owner", "")
	var detail struct {
		Job struct {
			Timeout int64 `json:"plugin_timeout_ms"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Job.Timeout != policy.Execution.PluginTimeoutNanos/int64(time.Millisecond) {
		t.Fatalf("pinned timeout %s", w.Body.String())
	}
}
