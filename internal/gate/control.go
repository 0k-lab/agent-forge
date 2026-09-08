package gate

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"agent-forge/internal/configjson"
	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
)

//go:embed app/index.html app/app.css app/app.js
var controlFiles embed.FS

func (x *server) controlAsset(w http.ResponseWriter, r *http.Request) {
	name, mime := "app/index.html", "text/html; charset=utf-8"
	switch r.URL.Path {
	case "/app", "/app/":
	case "/app/app.css":
		name, mime = "app/app.css", "text/css; charset=utf-8"
	case "/app/app.js":
		name, mime = "app/app.js", "text/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	body, err := controlFiles.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Write(body)
}

type controlProject struct {
	ID            string `json:"id"`
	DefaultBranch string `json:"default_branch"`
	WorkerPool    string `json:"worker_pool"`
	Agent         string `json:"agent"`
	PublicSource  bool   `json:"public_source"`
	Delivery      bool   `json:"delivery"`
}

type controlJob struct {
	Ordinal         int           `json:"ordinal,omitempty"`
	PluginTimeoutMS int64         `json:"plugin_timeout_ms,omitempty"`
	ID              string        `json:"id"`
	Title           string        `json:"title"`
	Project         string        `json:"project"`
	Status          string        `json:"status"`
	SourceRef       string        `json:"source_ref,omitempty"`
	WorkerID        string        `json:"worker_id,omitempty"`
	Agent           string        `json:"agent,omitempty"`
	FailureCode     string        `json:"failure_code,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	Delivery        *safeDelivery `json:"delivery,omitempty"`
}

func controlTitle(instruction string) string {
	for _, line := range strings.Split(instruction, "\n") {
		line = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, line))
		if line != "" {
			r := []rune(line)
			if len(r) > 120 {
				r = r[:120]
			}
			return string(r)
		}
	}
	return "Untitled task"
}

func controlURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 512 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "\\\r\n\t") {
		return ""
	}
	return u.String()
}

func (x *server) controlCard(ctx context.Context, job store.Job) (controlJob, error) {
	agent, timeout, err := x.store.ControlAgent(ctx, job.ID)
	if err != nil {
		return controlJob{}, err
	}
	instruction, project := job.Input, ""
	if job.Task != nil {
		instruction, project = job.Task.Instruction, job.Task.RepositoryID
	}
	delivery := x.safeDelivery(job.ID)
	if delivery != nil {
		delivery.PRURL = controlURL(delivery.PRURL)
	}
	return controlJob{PluginTimeoutMS: timeout, ID: job.ID, Title: controlTitle(instruction), Project: project, Status: job.Status, SourceRef: controlURL(job.SourceRef), WorkerID: job.WorkerID, Agent: agent, FailureCode: safeFailureCode(job.Error), CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt, Delivery: delivery}, nil
}

func (x *server) controlOverview(w http.ResponseWriter, r *http.Request) {
	projects := []controlProject{}
	if x.config != nil {
		for _, repo := range x.config.Repositories {
			_, err := canonicalPublicGitHubURL(repo.RepositoryURL)
			public := err == nil && x.config.PublicRepositoryRoot != "" && x.config.GitExecutable != ""
			projects = append(projects, controlProject{repo.ID, repo.DefaultBranch, repo.WorkerPool, repo.Execution.PluginID, public, public && x.config.Delivery != nil})
		}
	}
	workers, truncated, err := x.store.ControlWorkers(r.Context())
	if err != nil {
		writeDebugError(w, err)
		return
	}
	// Configured slots that have never connected have no heartbeat.
	// ponytail: scan at most 256 configured slots; index IDs if the slot ceiling grows.
	if x.config != nil {
		for _, reg := range x.config.Workers {
			for slot := 0; slot < reg.Concurrency; slot++ {
				id := reg.ID
				if slot > 0 {
					id += "#" + strconv.Itoa(slot)
				}
				found := false
				for _, worker := range workers {
					if worker.ID == id {
						found = true
						break
					}
				}
				if !found {
					workers = append(workers, store.ControlWorker{ID: id, BaseID: reg.ID, Slot: slot, Pool: reg.Pool})
				}
			}
		}
	}
	page, err := x.store.RecentDebugJobs(r.Context(), 100, nil)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	jobs := []controlJob{}
	for _, recent := range page.Items {
		job, err := x.store.Job(recent.ID)
		if err != nil {
			writeDebugError(w, err)
			return
		}
		card, err := x.controlCard(r.Context(), job)
		if err != nil {
			writeDebugError(w, err)
			return
		}
		jobs = append(jobs, card)
	}
	tasks, tasksTruncated, err := x.localControlTasks(r.Context())
	if err != nil {
		writeDebugError(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Tasks            []controlTask         `json:"tasks"`
		TasksTruncated   bool                  `json:"tasks_truncated"`
		Projects         []controlProject      `json:"projects"`
		Workers          []store.ControlWorker `json:"workers"`
		Jobs             []controlJob          `json:"jobs"`
		JobsTruncated    bool                  `json:"jobs_truncated"`
		WorkersTruncated bool                  `json:"workers_truncated"`
	}{tasks, tasksTruncated, projects, workers, jobs, page.NextPosition != nil, truncated})
}

type controlAttempt struct {
	Activity    []liveEvent           `json:"activity,omitempty"`
	AgentReport *protocol.AgentReport `json:"agent_report,omitempty"`
	safeAttempt
	WorkerID string `json:"worker_id"`
}

func (x *server) controlDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		http.NotFound(w, r)
		return
	}
	job, err := x.store.Job(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeDebugError(w, err)
		return
	}
	card, err := x.controlCard(r.Context(), job)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	attempts, truncated, err := x.store.ControlAttempts(id)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	safe := []controlAttempt{}
	var runReport *protocol.AgentReport
	for _, a := range attempts {
		records, err := x.store.AttemptEvidence(id, a.ID)
		if err != nil {
			writeDebugError(w, err)
			return
		}
		evidence := []safeEvidence{}
		for _, record := range records {
			evidence = append(evidence, safeEvidence{EvidenceID: record.EvidenceID, Phase: record.Phase, Reason: record.Reason, CheckIndex: record.CheckIndex, ExitCode: record.ExitCode, DurationMS: record.DurationMS, OutputRedacted: record.OutputRedacted, OutputTruncated: record.OutputTruncated, BaseSHA: record.BaseSHA, CandidateSHA: record.CandidateSHA, ArgvRedacted: record.ArgvRedacted})
		}
		var report *protocol.AgentReport
		if job.Task != nil && a.Status == "succeeded" && protocol.ValidateBaseSHA(a.CandidateSHA) == nil {
			report, _ = protocol.DecodeAgentReport(a.Result)
		}
		if a.ID == job.AttemptID && a.CandidateSHA == job.CandidateSHA {
			runReport = report
		}
		safe = append(safe, controlAttempt{Activity: x.projectActivity(a, x.options.Now().UTC()), AgentReport: report, safeAttempt: safeAttempt{ID: a.ID, Ordinal: a.Ordinal, Status: a.Status, FailureDisposition: a.FailureDisposition, FailureCode: safeFailureCode(a.FailureCode), CandidateSHA: a.CandidateSHA, LeasedAt: a.LeasedAt, DeadlineAt: a.DeadlineAt, CompletedAt: a.CompletedAt, Evidence: evidence}, WorkerID: a.WorkerID})
	}
	timeline, err := x.store.DebugJobTimeline(r.Context(), id, 100, nil)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	events := []store.DebugEvent{}
	for _, event := range timeline.Events {
		switch event.Type {
		case "submitted", "leased", "lease_expired", "retryable_failed", "retry_scheduled", "failed", "succeeded", "delivery_review", "delivery_resumed", "delivery_pending", "delivery_phase", "delivery_retry", "delivery_merged", "delivery_failed":
		default:
			continue
		}
		if !validJobID(event.AttemptID) {
			event.AttemptID = ""
		}
		events = append(events, event)
	}
	instruction, base, checks := job.Input, "", 0
	if job.Task != nil {
		instruction, base, checks = job.Task.Instruction, job.Task.BaseSHA, len(job.Task.Tests)
	}
	body, err := json.Marshal(struct {
		Job               controlJob            `json:"job"`
		Instruction       string                `json:"instruction"`
		CheckCount        int                   `json:"check_count"`
		Attempts          []controlAttempt      `json:"attempts"`
		AttemptsTruncated bool                  `json:"attempts_truncated"`
		Timeline          []store.DebugEvent    `json:"timeline"`
		TimelineTruncated bool                  `json:"timeline_truncated"`
		Delivery          *safeDelivery         `json:"delivery,omitempty"`
		Diagnostics       map[string]string     `json:"diagnostics"`
		SubagentTelemetry string                `json:"subagent_telemetry"`
		AgentReport       *protocol.AgentReport `json:"agent_report,omitempty"`
	}{card, instruction, checks, safe, truncated, events, timeline.NextPosition != nil, card.Delivery, map[string]string{"id": id, "attempt_id": job.AttemptID, "base_sha": base, "candidate_sha": job.CandidateSHA}, "No subagent telemetry reported for this run", runReport})
	if err != nil || len(body) > protocol.MaxWorkerMessageBytes {
		writeResultError(w, http.StatusRequestEntityTooLarge, CodeResultTooLarge, "detail exceeds limit")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
func (x *server) controlSubmit(w http.ResponseWriter, r *http.Request) {
	invalid := func() {
		writeJSON(w, 400, map[string]string{"error": "invalid task: choose a project, title, instruction and checks"})
	}
	var in struct {
		DeliveryPolicy configjson.String `json:"delivery_policy"`
		Project        string            `json:"project"`
		Title          string            `json:"title"`
		Instruction    string            `json:"instruction"`
		SourceRef      string            `json:"source_ref"`
		CheckPreset    string            `json:"check_preset"`
		Checks         string            `json:"checks"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil || configjson.Decode(body, &in) != nil || x.config == nil {
		invalid()
		return
	}
	if protocol.DeliveryPolicy(in.DeliveryPolicy).Validate() != nil {
		invalid()
		return
	}
	if in.DeliveryPolicy == configjson.String(protocol.DeliveryReview) && x.config.Delivery == nil {
		writeJSON(w, 422, map[string]string{"error": "delivery is unavailable for this project"})
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Instruction = strings.TrimSpace(in.Instruction)
	in.SourceRef = strings.TrimSpace(in.SourceRef)
	if in.Title == "" || len([]rune(in.Title)) > 120 || strings.IndexFunc(in.Title, unicode.IsControl) >= 0 || in.Instruction == "" || !utf8.ValidString(in.Title+in.Instruction) || in.SourceRef != "" && !exactGitHubIssueURL(in.SourceRef) {
		invalid()
		return
	}
	repository, ok := x.repository(in.Project)
	if !ok {
		invalid()
		return
	}
	if in.SourceRef != "" && !projectIssueSource(repository.RepositoryURL, in.SourceRef) {
		invalid()
		return
	}
	checks := [][]string{}
	switch in.CheckPreset {
	case "go":
		if strings.TrimSpace(in.Checks) != "" {
			invalid()
			return
		}
		checks = append(checks, []string{"go", "test", "./..."})
	case "":
		for _, line := range strings.Split(in.Checks, "\n") {
			argv := strings.Fields(line)
			if len(argv) == 0 {
				continue
			}
			// Explicit checks are argv lines, not shell scripts; quoting is intentionally unsupported.
			if strings.ContainsAny(line, "\"'`\x00") {
				invalid()
				return
			}
			for _, arg := range argv {
				if len(arg) > 4096 {
					invalid()
					return
				}
			}
			checks = append(checks, argv)
		}
		if len(checks) == 0 {
			invalid()
			return
		}
	default:
		invalid()
		return
	}
	task := protocol.CodingTask{RepositoryID: in.Project, BaseSHA: strings.Repeat("0", 40), Instruction: in.Title + "\n\n" + in.Instruction, Tests: checks}
	if validateTask(task) != nil {
		invalid()
		return
	}
	if _, err := canonicalPublicGitHubURL(repository.RepositoryURL); err != nil || x.config.PublicRepositoryRoot == "" || x.config.GitExecutable == "" {
		writeJSON(w, 422, map[string]string{"error": "public source preparation is unavailable for this project"})
		return
	}
	task.Repository, task.BaseSHA, err = preparePublicRepository(r.Context(), *x.config, repository, "")
	if err != nil {
		status := http.StatusUnprocessableEntity
		var preparation preparationError
		if errors.As(err, &preparation) && preparation.retryable {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]string{"error": "repository preparation failed"})
		return
	}
	x.persistConfiguredTask(w, task, repository, in.SourceRef, protocol.DeliveryPolicy(in.DeliveryPolicy))
}

func (x *server) controlResumeDelivery(w http.ResponseWriter, r *http.Request) {
	if x.config == nil || x.config.Delivery == nil {
		writeJSON(w, 422, map[string]string{"error": "delivery is unavailable"})
		return
	}
	d, err := x.store.Delivery(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "delivery is not awaiting review"})
		return
	}
	if d.Phase == "awaiting_review" {
		job, jobErr := x.store.Job(d.JobID)
		if jobErr != nil {
			writeJSON(w, 409, map[string]string{"error": "delivery identity changed"})
			return
		}
		candidate, err := x.deliveryForCandidate(r.Context(), store.Lease{JobID: job.ID, AttemptID: job.AttemptID, Task: job.Task}, d.CandidateSHA)
		if err != nil || candidate.ExpectedTreeSHA != d.ExpectedTreeSHA || candidate.ParentSHA != d.ParentSHA || candidate.RepositoryID != d.RepositoryID || candidate.RepositoryURL != d.RepositoryURL || candidate.DefaultBranch != d.DefaultBranch || candidate.CandidateRef != d.CandidateRef {
			writeJSON(w, 409, map[string]string{"error": "delivery identity changed"})
			return
		}
	}
	if err := x.store.ResumeDelivery(r.PathValue("id"), x.options.Now().UTC()); err != nil {
		writeJSON(w, 409, map[string]string{"error": "delivery is not awaiting review"})
		return
	}
	writeJSON(w, 200, x.safeDelivery(r.PathValue("id")))
}
