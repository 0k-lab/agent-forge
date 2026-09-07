package gate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"agent-forge/internal/store"
)

const maxProjectIssues = 50

type controlIssue struct {
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	BodyTruncated bool     `json:"body_truncated"`
	State         string   `json:"state"`
	Labels        []string `json:"labels"`
	ClosedAt      string   `json:"closed_at,omitempty"`
	ClosedBy      string   `json:"closed_by,omitempty"`
}

func boundedIssueText(value string, limit int, multiline bool) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			return -1
		}
		return r
	}, value))
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

// Public issues use a fixed GitHub origin, no authorization, and no redirects.
func fetchControlGitHub(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Agent-Forge-Control")
	client := http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("backlog unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("backlog exceeds limit")
	}
	return body, nil
}

func fetchControlIssues(ctx context.Context, source publicSource) ([]controlIssue, bool, error) {
	body, err := fetchControlGitHub(ctx, "/repos/"+source.Owner+"/"+source.Repository+"/issues?state=all&sort=updated&direction=desc&per_page=50")
	if err != nil {
		return nil, false, err
	}
	var raw []struct {
		Number   int64  `json:"number"`
		URL      string `json:"html_url"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		State    string `json:"state"`
		ClosedAt string `json:"closed_at"`
		ClosedBy struct {
			Login string `json:"login"`
		} `json:"closed_by"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return nil, false, errors.New("invalid backlog")
	}
	truncated := len(raw) >= maxProjectIssues
	if len(raw) > maxProjectIssues {
		raw = raw[:maxProjectIssues]
	}
	issues := []controlIssue{}
	seen := map[int64]bool{}
	for _, item := range raw {
		if len(item.PullRequest) != 0 || item.Number < 1 || item.Number > 9007199254740991 || seen[item.Number] || item.State != "open" && item.State != "closed" {
			continue
		}
		canonical := "https://github.com/" + source.Owner + "/" + source.Repository + "/issues/" + strconv.FormatInt(item.Number, 10)
		if item.URL != canonical {
			continue
		}
		title := boundedIssueText(item.Title, 120, false)
		if title == "" {
			continue
		}
		labels := []string{}
		for _, reserved := range []string{"blocked", "ready-for-agent"} {
			for _, label := range item.Labels {
				if label.Name == reserved {
					labels = append(labels, reserved)
					break
				}
			}
		}
		for _, label := range item.Labels {
			if label.Name == "blocked" || label.Name == "ready-for-agent" {
				continue
			}
			if len(labels) == 20 {
				break
			}
			name := boundedIssueText(label.Name, 50, false)
			// Do not turn malformed labels into scheduling instructions.
			if name != "" && name == label.Name {
				labels = append(labels, name)
			}
		}
		issues = append(issues, controlIssue{Number: int(item.Number), URL: canonical, Title: title, Body: boundedIssueText(item.Body, 8000, true), BodyTruncated: len([]rune(item.Body)) > 8000, State: item.State, Labels: labels, ClosedAt: issueClosedAt(item.State, item.ClosedAt), ClosedBy: issueClosedBy(item.State, item.ClosedBy.Login)})
		seen[item.Number] = true
	}
	return issues, truncated, nil
}

func (x *server) controlIssues(w http.ResponseWriter, r *http.Request) {
	if x.config == nil {
		http.NotFound(w, r)
		return
	}
	repository, ok := x.repository(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	source, err := canonicalPublicGitHubURL(repository.RepositoryURL)
	if err != nil {
		writeJSON(w, 422, map[string]any{"available": false, "error": "public backlog unavailable"})
		return
	}
	issues, truncated, err := fetchControlIssues(r.Context(), source)
	if err != nil {
		writeJSON(w, 502, map[string]any{"available": false, "error": "public backlog unavailable"})
		return
	}
	tasks := []controlTask{}
	for i := range issues {
		issue := &issues[i]
		refs, err := x.store.ControlSourceRuns(r.Context(), repository.ID, issue.URL)
		if err != nil {
			writeDebugError(w, err)
			return
		}
		task := controlTask{SourceRef: issue.URL, Project: repository.ID, Title: issue.Title, Issue: issue}
		if len(refs) > 0 {
			ref := refs[len(refs)-1]
			run, err := x.controlRun(r.Context(), ref)
			if err != nil {
				writeDebugError(w, err)
				return
			}
			task.LatestRun = &run
			task.RunCount = ref.Count
		}
		task.Lane = controlTaskLane(issue, task.LatestRun)
		tasks = append(tasks, task)
	}
	payload := struct {
		Tasks     []controlTask  `json:"tasks"`
		Available bool           `json:"available"`
		Issues    []controlIssue `json:"issues"`
		Truncated bool           `json:"truncated"`
	}{tasks, true, issues, truncated}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > 1<<20 {
		writeJSON(w, 502, map[string]any{"available": false, "error": "public backlog exceeds response limit"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// Closed issues and explicit blocking labels take precedence over run state.
func controlTaskLane(issue *controlIssue, run *controlJob) string {
	if issue != nil {
		if issue.State == "closed" {
			return "Done"
		}
		for _, label := range issue.Labels {
			if label == "blocked" {
				return "Blocked"
			}
		}
	}
	if run != nil {
		switch run.Status {
		case "pending", "retry_wait":
			return "Ready"
		case "leased":
			return "Working"
		case "delivering":
			return "Review & CI"
		case "succeeded":
			return "Done"
		case "failed":
			return "Blocked"
		}
	}
	if issue != nil {
		for _, label := range issue.Labels {
			if label == "ready-for-agent" {
				return "Ready"
			}
		}
	}
	return "Backlog"
}

type controlTask struct {
	SourceRef string        `json:"source_ref"`
	Project   string        `json:"project"`
	Title     string        `json:"title"`
	Lane      string        `json:"lane"`
	Issue     *controlIssue `json:"issue,omitempty"`
	LatestRun *controlJob   `json:"latest_run,omitempty"`
	RunCount  int           `json:"run_count"`
}

func (x *server) controlRun(ctx context.Context, ref store.ControlRunRef) (controlJob, error) {
	job, err := x.store.Job(ref.ID)
	if err != nil {
		return controlJob{}, err
	}
	run, err := x.controlCard(ctx, job)
	run.Ordinal = ref.Ordinal
	return run, err
}

func (x *server) localControlTasks(ctx context.Context) ([]controlTask, bool, error) {
	refs, truncated, err := x.store.ControlTaskRuns(ctx, func(project, source string) bool {
		if x.config == nil {
			return false
		}
		repository, ok := x.repository(project)
		return ok && projectIssueSource(repository.RepositoryURL, source)
	})
	if err != nil {
		return nil, false, err
	}
	tasks := []controlTask{}
	for _, ref := range refs {
		run, err := x.controlRun(ctx, ref)
		if err != nil {
			return nil, false, err
		}
		tasks = append(tasks, controlTask{SourceRef: run.SourceRef, Project: run.Project, Title: run.Title, Lane: controlTaskLane(nil, &run), LatestRun: &run, RunCount: ref.Count})
	}
	return tasks, truncated, nil
}

func (x *server) controlSourceRuns(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	// Run history survives repository removal; _unassigned cannot be a configured ID.
	if project == "_unassigned" {
		project = ""
	} else if !configID.MatchString(project) {
		http.NotFound(w, r)
		return
	}
	source := r.URL.Query().Get("source_ref")
	if source == "" || controlURL(source) != source {
		writeJSON(w, 400, map[string]string{"error": "invalid source reference"})
		return
	}
	refs, err := x.store.ControlSourceRuns(r.Context(), project, source)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	runs := []controlJob{}
	total := 0
	for _, ref := range refs {
		run, err := x.controlRun(r.Context(), ref)
		if err != nil {
			writeDebugError(w, err)
			return
		}
		runs = append(runs, run)
		total = ref.Count
	}
	writeJSON(w, 200, struct {
		Runs      []controlJob `json:"runs"`
		Total     int          `json:"total"`
		Truncated bool         `json:"truncated"`
	}{runs, total, total > len(runs)})
}

func exactGitHubIssueURL(raw string) bool {
	if raw == "" || controlURL(raw) != raw {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" {
		return false
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 5 || parts[3] != "issues" {
		return false
	}
	source := publicSource{Owner: parts[1], Repository: parts[2]}
	number, err := strconv.ParseInt(parts[4], 10, 64)
	return source.Validate() == nil && err == nil && number > 0 && number <= 9007199254740991 && strconv.FormatInt(number, 10) == parts[4] && u.EscapedPath() == u.Path
}

func projectIssueSource(repositoryURL, sourceRef string) bool {
	source, err := canonicalPublicGitHubURL(repositoryURL)
	return err == nil && exactGitHubIssueURL(sourceRef) && strings.HasPrefix(sourceRef, "https://github.com/"+source.Owner+"/"+source.Repository+"/issues/")
}
