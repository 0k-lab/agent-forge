package gate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
)

type issueTransport func(*http.Request) (*http.Response, error)

func (f issueTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func issueResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func mockIssues(t *testing.T, f issueTransport) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = old })
}

func TestControlPublicIssuesReadOnlyAndSanitized(t *testing.T) {
	_, _, h := controlFixture(t)
	calls := 0
	mockIssues(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Host != "api.github.com" || r.URL.Path != "/repos/0k-lab/agent-forge/issues" || r.URL.Query().Get("per_page") != "50" || r.URL.Query().Get("state") != "all" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatalf("unsafe upstream %s %v", r.URL, r.Header)
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("missing bounded timeout")
		}
		return issueResponse(200, `[
   {"number":12,"html_url":"https://github.com/0k-lab/agent-forge/issues/12","title":"  Fix parser  ","body":"Public issue brief","state":"open","labels":[{"name":"ready-for-agent"},{"name":"bug"}],"user":{"token":"private-upstream-token"},"path":"/private/upstream"},
   {"number":13,"html_url":"https://github.com/0k-lab/agent-forge/pull/13","title":"A pull request","state":"open","pull_request":{}},
   {"number":14,"html_url":"https://secret@github.com/0k-lab/agent-forge/issues/14","title":"Credential link","state":"open"},
   {"number":15,"html_url":"https://github.com/other/repo/issues/15","title":"Wrong repository","state":"open"},
   {"number":16,"html_url":"https://github.com/0k-lab/agent-forge/issues/16","title":"Closed issue","state":"closed","labels":[]}
  ]`), nil
	})
	path := "/v1/control/projects/agent-forge/issues"
	if w := controlRequest(h, "GET", path, "", ""); w.Code != 401 {
		t.Fatalf("auth %d", w.Code)
	}
	if calls != 0 {
		t.Fatal("unauthorized fetch")
	}
	w := controlRequest(h, "GET", path, "owner", "")
	var got struct {
		Issues []struct {
			Number                  int
			Title, Body, State, URL string
			Labels                  []string
		}
		Available bool
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !got.Available || len(got.Issues) != 2 || got.Issues[0].Number != 12 || got.Issues[0].Title != "Fix parser" || got.Issues[0].Body != "Public issue brief" || got.Issues[0].Labels[0] != "ready-for-agent" || got.Issues[1].State != "closed" {
		t.Fatalf("issues %d %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-upstream-token", "/private/", "secret@", "pull_request"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("leak %s", secret)
		}
	}
	if w := controlRequest(h, "POST", path, "owner", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
}

func TestControlPublicIssuesBoundedFailure(t *testing.T) {
	_, x, h := controlFixture(t)
	for _, tc := range []struct {
		name, body string
		status     int
		err        error
	}{
		{"rate limited", "private-token /private/path", 429, nil},
		{"oversized", strings.Repeat("x", (1<<20)+1), 200, nil},
		{"invalid json", "not JSON /private/path", 200, nil},
		{"timeout", "", 0, context.DeadlineExceeded},
		{"redirect", "", 302, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockIssues(t, func(*http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return issueResponse(tc.status, tc.body), nil
			})
			w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues", "owner", "")
			if w.Code != 502 || w.Body.Len() > 256 || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("failure %d %s", w.Code, w.Body.String())
			}
		})
	}
	x.config.Repositories[0].RepositoryURL = "https://secret@github.com/org/repo.git"
	mockIssues(t, func(*http.Request) (*http.Response, error) { t.Fatal("invalid repository fetched"); return nil, nil })
	if w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues", "owner", ""); w.Code != 422 {
		t.Fatal(w.Code)
	}
	if w := controlRequest(h, "GET", "/v1/control/projects/missing/issues", "owner", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestControlTasksExcludeUnlinkedAndLinkExactRuns(t *testing.T) {
	s, x, h := controlFixture(t)
	repo := x.config.Repositories[0]
	policy := x.config.resolvedPolicy(repo.WorkerPool, repo.Execution, repo.ID, repo.DefaultBranch)
	source := "https://github.com/0k-lab/agent-forge/issues/12"
	create := func(title, ref string) string {
		t.Helper()
		j, err := s.CreateCodingJobWithPolicyAndSource(protocol.CodingTask{RepositoryID: repo.ID, BaseSHA: strings.Repeat("a", 40), Instruction: title}, policy, ref)
		if err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	first := create("First run", source)
	second := create("Second run", source)
	create("Unlinked historical run", "")
	create("Different exact source", source+"/")
	create("Unsafe source", "https://secret@github.com/0k-lab/agent-forge/issues/12")
	w := controlRequest(h, "GET", "/v1/control/overview", "owner", "")
	var got struct {
		Tasks []struct {
			SourceRef   string `json:"source_ref"`
			Title, Lane string
			RunCount    int                 `json:"run_count"`
			LatestRun   struct{ ID string } `json:"latest_run"`
		}
		Jobs []any
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Tasks) != 1 || len(got.Jobs) != 5 {
		t.Fatalf("tasks %d %s", w.Code, w.Body.String())
	}
	found := false
	for _, task := range got.Tasks {
		if task.SourceRef == source {
			found = true
			if task.LatestRun.ID != second || task.RunCount != 2 || task.Lane != "Ready" {
				t.Fatalf("link %#v", task)
			}
		}
		if strings.Contains(task.Title, "historical") {
			t.Fatal("historical job became task")
		}
	}
	if !found {
		t.Fatal("linked task absent")
	}
	mockIssues(t, func(*http.Request) (*http.Response, error) {
		return issueResponse(200, `[{"number":12,"html_url":"`+source+`","title":"Product issue","state":"open","labels":[]}]`), nil
	})
	w = controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues", "owner", "")
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Tasks) != 1 || got.Tasks[0].Title != "Product issue" || got.Tasks[0].LatestRun.ID != second || got.Tasks[0].RunCount != 2 {
		t.Fatalf("issue linkage %s", w.Body.String())
	}
	path := "/v1/control/projects/agent-forge/runs?source_ref=" + source
	w = controlRequest(h, "GET", path, "owner", "")
	var runs struct {
		Runs []struct {
			ID      string
			Ordinal int
		}
		Total int
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &runs) != nil || runs.Total != 2 || len(runs.Runs) != 2 || runs.Runs[0].ID != first || runs.Runs[0].Ordinal != 1 || runs.Runs[1].ID != second || runs.Runs[1].Ordinal != 2 {
		t.Fatalf("runs %d %s", w.Code, w.Body.String())
	}
	if w := controlRequest(h, "GET", path, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestControlTaskLaneTruth(t *testing.T) {
	for _, tc := range []struct {
		state     string
		labels    []string
		run, want string
	}{
		{"open", nil, "", "Backlog"}, {"open", []string{"ready-for-agent"}, "", "Ready"},
		{"open", nil, "pending", "Ready"}, {"open", nil, "retry_wait", "Ready"}, {"open", nil, "leased", "Working"},
		{"open", nil, "delivering", "Review & CI"}, {"open", nil, "succeeded", "Done"}, {"open", nil, "failed", "Blocked"},
		{"closed", nil, "", "Done"}, {"closed", nil, "failed", "Done"}, {"open", []string{"blocked", "ready-for-agent"}, "", "Blocked"},
	} {
		issue := &controlIssue{State: tc.state, Labels: tc.labels}
		var run *controlJob
		if tc.run != "" {
			run = &controlJob{Status: tc.run}
		}
		if got := controlTaskLane(issue, run); got != tc.want {
			t.Fatalf("%#v: %s", tc, got)
		}
	}
}

func TestControlSubmissionRequiresExactGitHubIssueSource(t *testing.T) {
	s, _, h := controlFixture(t)
	old := publicGitRunner
	publicGitRunner = func(context.Context, string, string, time.Duration, int64, ...string) (string, error) {
		return "", context.DeadlineExceeded
	}
	t.Cleanup(func() { publicGitRunner = old })
	for _, source := range []string{"https://example.com/task/1", "https://github.com/org/repo/pull/1", "https://github.com/org/repo/issues/01", "https://github.com/org/repo/issues/1/", "https://github.com/org/repo/issues/1#comment", "https://github.com/org/repo/issues/1?token=secret"} {
		body, _ := json.Marshal(map[string]string{"project": "agent-forge", "title": "Fix", "instruction": "parser", "check_preset": "go", "source_ref": source})
		w := controlRequest(h, "POST", "/v1/control/jobs", "owner", string(body))
		if w.Code != 400 {
			t.Fatalf("nonexact source %s: %d", source, w.Code)
		}
	}
	page, err := s.RecentDebugJobs(context.Background(), 100, nil)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("invalid source created a run")
	}
}

func TestControlIssuesBoundCountLabelsAndEncodedResponse(t *testing.T) {
	_, _, h := controlFixture(t)
	var raw []map[string]any
	labels := []map[string]string{}
	for i := 0; i < 25; i++ {
		labels = append(labels, map[string]string{"name": "ordinary-" + strings.Repeat("a", i)})
	}
	labels = append(labels, map[string]string{"name": "ready-for-agent"})
	for i := 1; i <= 60; i++ {
		raw = append(raw, map[string]any{"number": i, "html_url": "https://github.com/0k-lab/agent-forge/issues/" + strconv.Itoa(i), "title": strings.Repeat("界", 200), "body": "brief", "state": "open", "labels": labels})
	}
	body, _ := json.Marshal(raw)
	mockIssues(t, func(*http.Request) (*http.Response, error) { return issueResponse(200, string(body)), nil })
	w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues", "owner", "")
	var got struct {
		Issues    []controlIssue
		Tasks     []controlTask
		Truncated bool
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Issues) != 50 || !got.Truncated || len([]rune(got.Issues[0].Title)) != 120 || len(got.Issues[0].Labels) > 20 || got.Tasks[0].Lane != "Ready" {
		t.Fatalf("bounds %d %s", w.Code, w.Body.String()[:min(500, w.Body.Len())])
	}
	// JSON escaping must not turn a bounded upstream body into an unbounded browser response.
	for _, issue := range raw[:50] {
		issue["body"] = strings.Repeat("<", 8000)
	}
	body, _ = json.Marshal(raw[:50])
	// Use literal <, as GitHub may; encoding/json escapes it when producing our response.
	body = []byte(strings.ReplaceAll(string(body), `\u003c`, "<"))
	w = controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues", "owner", "")
	if w.Code != 502 || w.Body.Len() > 256 {
		t.Fatalf("encoded response unbounded: %d %d", w.Code, w.Body.Len())
	}
}

func TestControlLocalPRReferencesAreNotProductTasks(t *testing.T) {
	s, _, h := controlFixture(t)
	for _, ref := range []string{"https://github.com/org/repo/pull/12", "https://github.com/org/repo/issues/012", "https://github.com/org/repo/issues/12/"} {
		if _, err := s.CreateJobWithSource("Historical linked job", ref); err != nil {
			t.Fatal(err)
		}
	}
	w := controlRequest(h, "GET", "/v1/control/overview", "owner", "")
	var got struct {
		Tasks []any
		Jobs  []any
	}
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Tasks) != 0 || len(got.Jobs) != 3 {
		t.Fatalf("nonissue became task %s", w.Body.String())
	}
}

func TestControlIssueBriefTruncationIsExplicit(t *testing.T) {
	_, _, h := controlFixture(t)
	raw, _ := json.Marshal([]map[string]any{{"number": 1, "html_url": "https://github.com/0k-lab/agent-forge/issues/1", "title": "Large brief", "body": strings.Repeat("a", 9000), "state": "open"}})
	mockIssues(t, func(*http.Request) (*http.Response, error) { return issueResponse(200, string(raw)), nil })
	w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues", "owner", "")
	var got struct {
		Issues []struct {
			Body      string
			Truncated bool `json:"body_truncated"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Issues) != 1 || len(got.Issues[0].Body) != 8000 || !got.Issues[0].Truncated {
		t.Fatal("brief was silently truncated")
	}
}

func TestControlLinkedLegacyRunHistoryRemainsReadable(t *testing.T) {
	s, _, h := controlFixture(t)
	source := "https://github.com/org/repo/issues/12"
	job, err := s.CreateJobWithSource("Linked legacy run", source)
	if err != nil {
		t.Fatal(err)
	}
	w := controlRequest(h, "GET", "/v1/control/projects/_unassigned/runs?source_ref="+source, "owner", "")
	var got struct {
		Runs  []controlJob
		Total int
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Total != 1 || got.Runs[0].ID != job.ID {
		t.Fatalf("legacy history %d %s", w.Code, w.Body.String())
	}
}

func TestControlRunHistoryBoundsAndLatestCreationWins(t *testing.T) {
	s, x, h := controlFixture(t)
	repo := x.config.Repositories[0]
	source := "https://github.com/0k-lab/agent-forge/issues/12"
	policy := x.config.resolvedPolicy(repo.WorkerPool, repo.Execution, repo.ID, repo.DefaultBranch)
	var latest string
	for range 24 {
		job, err := s.CreateCodingJobWithPolicyAndSource(protocol.CodingTask{RepositoryID: repo.ID, BaseSHA: strings.Repeat("a", 40), Instruction: "Linked task"}, policy, source)
		if err != nil {
			t.Fatal(err)
		}
		latest = job.ID
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
	if _, err := s.FailLeaseAt(lease.JobID, lease.AttemptID, "worker-1", generation, protocol.FailureInvalidTask, store.TerminalFailure, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	w := controlRequest(h, "GET", "/v1/control/overview", "owner", "")
	var overview struct{ Tasks []controlTask }
	if json.Unmarshal(w.Body.Bytes(), &overview) != nil || len(overview.Tasks) != 1 || overview.Tasks[0].LatestRun.ID != latest || overview.Tasks[0].Lane != "Ready" {
		t.Fatalf("older update replaced latest run: %s", w.Body.String())
	}
	w = controlRequest(h, "GET", "/v1/control/projects/agent-forge/runs?source_ref="+source, "owner", "")
	var group struct {
		Runs      []controlJob
		Total     int
		Truncated bool
	}
	if json.Unmarshal(w.Body.Bytes(), &group) != nil || len(group.Runs) != 20 || group.Total != 24 || !group.Truncated || group.Runs[0].Ordinal != 5 || group.Runs[19].Ordinal != 24 || group.Runs[19].ID != latest {
		t.Fatalf("history bounds: %s", w.Body.String())
	}
}

func TestControlIssueClosedMetadata(t *testing.T) {
	_, _, h := controlFixture(t)
	mockIssues(t, func(*http.Request) (*http.Response, error) {
		return issueResponse(200, `[ {"number":53,"html_url":"https://github.com/0k-lab/agent-forge/issues/53","title":"Completed","state":"closed","closed_at":"2026-08-29T20:44:18Z","closed_by":{"login":"kricha-lab-dev-worker[bot]","token":"secret-token"}} ]`), nil
	})
	w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues", "owner", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"closed_at":"2026-08-29T20:44:18Z"`) || !strings.Contains(w.Body.String(), `"closed_by":"kricha-lab-dev-worker[bot]"`) || strings.Contains(w.Body.String(), "secret-token") {
		t.Fatalf("metadata: %d %s", w.Code, w.Body.String())
	}
}

func TestControlIssueActivityCanonicalBoundedReadOnly(t *testing.T) {
	_, _, h := controlFixture(t)
	calls := 0
	mockIssues(t, func(r *http.Request) (*http.Response, error) {
		calls++
		deadline, ok := r.Context().Deadline()
		if r.Method != "GET" || r.URL.Scheme != "https" || r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || !ok || time.Until(deadline) > 5*time.Second {
			t.Fatalf("unsafe request %v", r)
		}
		if r.URL.Path == "/repos/0k-lab/agent-forge/issues/53" {
			return issueResponse(200, `{"number":53,"html_url":"https://github.com/0k-lab/agent-forge/issues/53","state":"closed","closed_at":"2026-08-29T20:44:18Z","closed_by":{"login":"kricha-lab-dev-worker[bot]"}}`), nil
		}
		if r.URL.Path == "/repos/0k-lab/agent-forge/pulls/55" {
			return issueResponse(502, ""), nil
		}
		if r.URL.Path != "/repos/0k-lab/agent-forge/issues/53/timeline" || r.URL.Query().Get("per_page") != "50" {
			t.Fatalf("unexpected URL %s", r.URL)
		}
		good := `{"event":"cross-referenced","source":{"type":"issue","issue":{"number":55,"html_url":"https://github.com/0k-lab/agent-forge/pull/55","pull_request":{"html_url":"https://github.com/0k-lab/agent-forge/pull/55","merged_at":"2026-08-29T20:44:17Z"}}}}`
		records := []string{good, good, strings.ReplaceAll(good, "0k-lab/agent-forge", "other/repo"), strings.ReplaceAll(good, "https://github.com", "https://secret@github.com"), strings.ReplaceAll(good, "2026-08-29T20:44:17Z", "bad"), strings.ReplaceAll(good, "cross-referenced", "commented"), strings.ReplaceAll(good, `"number":55`, `"number":56`)}
		records = append(records, strings.ReplaceAll(good, `"number":55`, `"number":"bad"`), strings.ReplaceAll(good, `"merged_at":"2026-08-29T20:44:17Z"`, `"merged_at":null`))
		for len(records) < 50 {
			records = append(records, `{}`)
		}
		records = append(records, strings.ReplaceAll(good, "55", "99"))
		return issueResponse(200, "["+strings.Join(records, ",")+"]"), nil
	})
	path := "/v1/control/projects/agent-forge/issues/53/activity"
	if w := controlRequest(h, "GET", path, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := controlRequest(h, "POST", path, "owner", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
	if calls != 0 {
		t.Fatal("unauthorized upstream call")
	}
	w := controlRequest(h, "GET", path, "owner", "")
	var got struct {
		Issue     struct{ State, ClosedAt, ClosedBy string }
		MergedPRs []struct {
			Number int
			URL    string
		} `json:"merged_prs"`
		Truncated bool
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.MergedPRs) != 1 || got.MergedPRs[0].Number != 55 || !got.Truncated || !strings.Contains(w.Body.String(), "kricha-lab-dev-worker[bot]") || calls != 3 {
		t.Fatalf("activity %d %s calls=%d", w.Code, w.Body.String(), calls)
	}
	for _, bad := range []string{"secret@", "other/repo", "/pull/99"} {
		if strings.Contains(w.Body.String(), bad) {
			t.Fatal("unsafe evidence", bad)
		}
	}
}

func TestControlIssueActivityFailures(t *testing.T) {
	_, x, h := controlFixture(t)
	path := "/v1/control/projects/agent-forge/issues/53/activity"
	for _, timeline := range []bool{false, true} {
		for _, mode := range []string{"redirect", "oversize", "timeout", "malformed"} {
			t.Run(strconv.FormatBool(timeline)+"/"+mode, func(t *testing.T) {
				calls := 0
				mockIssues(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if timeline && calls == 1 {
						return issueResponse(200, `{"number":53,"html_url":"https://github.com/0k-lab/agent-forge/issues/53","state":"closed"}`), nil
					}
					limit := 1
					if timeline {
						limit = 2
					}
					if calls > limit {
						t.Fatal("followed redirect")
					}
					switch mode {
					case "redirect":
						response := issueResponse(302, "")
						response.Header.Set("Location", "https://evil.example/private")
						return response, nil
					case "oversize":
						return issueResponse(200, strings.Repeat("x", (1<<20)+1)), nil
					case "timeout":
						return nil, context.DeadlineExceeded
					default:
						return issueResponse(200, `{"private":"/private/token"}`), nil
					}
				})
				w := controlRequest(h, "GET", path, "owner", "")
				if w.Code != 502 || w.Body.Len() > 256 || strings.Contains(w.Body.String(), "private") {
					t.Fatalf("failure %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
	mockIssues(t, func(*http.Request) (*http.Response, error) { t.Fatal("invalid input fetched"); return nil, nil })
	for _, number := range []string{"0", "053", "-1", "9007199254740992", "abc"} {
		if w := controlRequest(h, "GET", strings.Replace(path, "53", number, 1), "owner", ""); w.Code != 400 {
			t.Fatal(number, w.Code)
		}
	}
	x.config.Repositories[0].RepositoryURL = "https://secret@github.com/org/repo.git"
	if w := controlRequest(h, "GET", path, "owner", ""); w.Code != 422 {
		t.Fatal(w.Code)
	}
}

func TestControlSubmissionSourceMatchesSelectedProject(t *testing.T) {
	s, x, h := controlFixture(t)
	other := x.config.Repositories[0]
	other.ID = "other"
	other.RepositoryURL = "https://github.com/other/repo.git"
	x.config.Repositories = append(x.config.Repositories, other)
	calls := 0
	old := publicGitRunner
	publicGitRunner = func(context.Context, string, string, time.Duration, int64, ...string) (string, error) {
		calls++
		return "", context.DeadlineExceeded
	}
	t.Cleanup(func() { publicGitRunner = old })
	for _, source := range []string{
		"https://github.com/other/repo/issues/12", "https://github.com/0k-lab/agent-forge-extra/issues/12",
		"https://github.com/0K-lab/agent-forge/issues/12", "https://github.com/0k-lab/Agent-Forge/issues/12",
		"https://GitHub.com/0k-lab/agent-forge/issues/12", "https://github.com.evil/0k-lab/agent-forge/issues/12",
		"https://github.com/0k-lab%2Fagent-forge/issues/12", "https://github.com/0k-lab/agent-forge/issues/12/extra",
	} {
		body, _ := json.Marshal(map[string]string{"project": "agent-forge", "title": "Fix", "instruction": "parser", "check_preset": "go", "source_ref": source})
		w := controlRequest(h, "POST", "/v1/control/jobs", "owner", string(body))
		if w.Code != 400 || w.Body.Len() > 256 || strings.Contains(w.Body.String(), source) {
			t.Errorf("wrong source %s: %d %s", source, w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("wrong-project requests prepared source: %d", calls)
	}
	page, err := s.RecentDebugJobs(context.Background(), 100, nil)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("invalid source mutated jobs")
	}
}

func TestControlTaskLimitCountsOnlyConfiguredIssueTasks(t *testing.T) {
	for _, validCount := range []int{2, 100, 101} {
		t.Run(strconv.Itoa(validCount), func(t *testing.T) {
			s, x, _ := controlFixture(t)
			repo := x.config.Repositories[0]
			create := func(project, source string) {
				t.Helper()
				_, err := s.CreateCodingJobWithPolicyAndSource(protocol.CodingTask{RepositoryID: project, BaseSHA: strings.Repeat("a", 40), Instruction: "Task"}, x.config.resolvedPolicy(repo.WorkerPool, repo.Execution, project, repo.DefaultBranch), source)
				if err != nil {
					t.Fatal(err)
				}
			}
			for i := 1; i <= validCount; i++ {
				create(repo.ID, "https://github.com/0k-lab/agent-forge/issues/"+strconv.Itoa(i))
			}
			for i := 1; i <= 110; i++ {
				n := strconv.Itoa(i)
				for _, source := range []string{"https://example.com/task/" + n, "https://github.com/other/repo/issues/" + n, "https://github.com/0k-lab/agent-forge/pull/" + n, "https://github.com/0k-lab/agent-forge/issues/0" + n} {
					create(repo.ID, source)
				}
				create("removed-project", "https://github.com/0k-lab/agent-forge/issues/"+n)
			}
			tasks, truncated, err := x.localControlTasks(context.Background())
			if err != nil || len(tasks) != min(100, validCount) || truncated != (validCount > 100) {
				t.Fatalf("valid=%d got=%d truncated=%v err=%v", validCount, len(tasks), truncated, err)
			}
			for _, task := range tasks {
				if task.Project != repo.ID || !strings.HasPrefix(task.SourceRef, "https://github.com/0k-lab/agent-forge/issues/") {
					t.Fatalf("invalid task %#v", task)
				}
			}
		})
	}
}

func TestControlTasksWithoutConfiguration(t *testing.T) {
	s, x, _ := controlFixture(t)
	if _, err := s.CreateJobWithSource("Legacy", "https://github.com/org/repo/issues/1"); err != nil {
		t.Fatal(err)
	}
	x.config = nil
	tasks, truncated, err := x.localControlTasks(context.Background())
	if err != nil || truncated || len(tasks) != 0 {
		t.Fatalf("tasks=%v truncated=%v err=%v", tasks, truncated, err)
	}
}
