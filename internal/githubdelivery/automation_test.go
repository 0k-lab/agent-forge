package githubdelivery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

type automationHub struct {
	t             *testing.T
	base          string
	remoteBase    string
	candidate     string
	merge         string
	runs          string
	transientRuns int
	changedHead   bool
	merged        bool
	mergeCalls    int
}

func (h *automationHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo":
		io.WriteString(w, `{"full_name":"octo/repo","private":false,"owner":{"type":"User"}}`)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/installation":
		io.WriteString(w, `{"id":7,"permissions":{"actions":"read","contents":"write","pull_requests":"write"}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
		io.WriteString(w, `{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo/repo/git/ref/heads/"):
		sha := h.candidate
		if strings.HasSuffix(r.URL.Path, "/main") {
			sha = h.base
			if h.remoteBase != "" {
				sha = h.remoteBase
			}
		}
		io.WriteString(w, `{"object":{"sha":"`+sha+`"}}`)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/pulls":
		state := r.URL.Query().Get("state")
		if h.merged && state == "open" {
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, `[`+h.pullJSON(false)+`]`)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/repo/pulls" && h.merged:
		w.WriteHeader(http.StatusUnprocessableEntity)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/pulls/42":
		io.WriteString(w, h.pullJSON(h.changedHead))
	case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/actions/runs":
		if h.transientRuns > 0 {
			h.transientRuns--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, h.runs)
	case r.Method == http.MethodPut && r.URL.Path == "/repos/octo/repo/pulls/42/merge":
		h.mergeCalls++
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["sha"] != h.candidate {
			h.t.Fatalf("merge did not lease exact head: %#v", body)
		}
		io.WriteString(w, `{"sha":"`+h.merge+`","merged":true}`)
	default:
		h.t.Errorf("unexpected request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *automationHub) pullJSON(changed bool) string {
	head := h.candidate
	if changed {
		head = strings.Repeat("e", 40)
	}
	return `{"number":42,"html_url":"https://github.com/octo/repo/pull/42","state":"open","merged":` + boolJSON(h.merged) + `,"merge_commit_sha":"` + map[bool]string{true: h.merge}[h.merged] + `","head":{"ref":"forge/job","label":"octo:forge/job","sha":"` + head + `"},"base":{"ref":"main","sha":"` + h.base + `"},"title":"Deliver","body":"Reviewed"}`
}

func boolJSON(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func automationSetup(t *testing.T) (Config, Publication, Options, *automationHub) {
	t.Helper()
	repo, base, candidate, tree, ref := testRepository(t, "README.md")
	hub := &automationHub{t: t, base: base, candidate: candidate, merge: strings.Repeat("d", 40), runs: `{"total_count":1,"workflow_runs":[{"id":1,"head_sha":"` + candidate + `","head_branch":"forge/job","event":"pull_request","status":"completed","conclusion":"success"}]}`}
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "push") {
			t.Fatal("existing exact branch was pushed again")
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	cfg := Config{Version: 1, APIBase: "https://api.github.com", Owner: "octo", Repository: "repo", LocalRepository: repo, GitExecutable: testExecutable(t), AppID: "123", PrivateKeyPath: testKey(t)}
	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
	op := Options{BaseURL: "http://github.test", HTTPClient: testHTTPClient(hub), Run: run, Timeout: time.Minute, RetryDelay: func(context.Context, time.Duration) error { return nil }}
	return cfg, input, op, hub
}

func TestAutomationMergesOnlySuccessfulExactHead(t *testing.T) {
	cfg, input, op, hub := automationSetup(t)
	result, err := DeliverAndMerge(context.Background(), cfg, input, AutomationOptions{Options: op, PollInterval: time.Millisecond, NoRunsGrace: time.Second, Timeout: time.Minute})
	if err != nil || result.MergeSHA != hub.merge || result.CIState != "success" || hub.mergeCalls != 1 {
		t.Fatalf("result=%#v err=%v merge_calls=%d", result, err, hub.mergeCalls)
	}
	hub.merged = true
	hub.remoteBase = hub.merge
	result, err = DeliverAndMerge(context.Background(), cfg, input, AutomationOptions{Options: op, PollInterval: time.Millisecond, NoRunsGrace: time.Second, Timeout: time.Minute})
	if err != nil || result.MergeSHA != hub.merge || hub.mergeCalls != 1 {
		t.Fatalf("idempotent result=%#v err=%v merge_calls=%d", result, err, hub.mergeCalls)
	}
}

func TestAutomationFailsClosedForCIAndHeadLease(t *testing.T) {
	for _, test := range []struct {
		name, runs, code string
		changed          bool
	}{
		{"failed run", `{"total_count":1,"workflow_runs":[{"id":1,"head_sha":"candidate","head_branch":"forge/job","event":"pull_request","status":"completed","conclusion":"failure"}]}`, "ci_failed", false},
		{"changed head", "", "head_changed", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, input, op, hub := automationSetup(t)
			if test.runs != "" {
				hub.runs = strings.ReplaceAll(test.runs, "candidate", input.CandidateSHA)
			}
			hub.changedHead = test.changed
			_, err := DeliverAndMerge(context.Background(), cfg, input, AutomationOptions{Options: op, PollInterval: time.Millisecond, NoRunsGrace: time.Second, Timeout: time.Minute})
			var deliveryErr *Error
			if !AsError(err, &deliveryErr) || deliveryErr.Code != test.code || hub.mergeCalls != 0 {
				t.Fatalf("error=%#v merge_calls=%d", err, hub.mergeCalls)
			}
		})
	}
}

func TestAutomationNoRunsAndTransientRetryAreBounded(t *testing.T) {
	cfg, input, op, hub := automationSetup(t)
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	op.Now = func() time.Time { return now }
	op.RetryDelay = func(_ context.Context, delay time.Duration) error { now = now.Add(delay); return nil }
	hub.runs = `{"total_count":0,"workflow_runs":[]}`
	_, err := DeliverAndMerge(context.Background(), cfg, input, AutomationOptions{Options: op, PollInterval: time.Second, NoRunsGrace: time.Second, Timeout: time.Minute})
	var deliveryErr *Error
	if !AsError(err, &deliveryErr) || deliveryErr.Code != "ci_no_runs" {
		t.Fatalf("no-runs error = %#v", err)
	}

	hub.runs = `{"total_count":1,"workflow_runs":[{"id":1,"head_sha":"` + input.CandidateSHA + `","head_branch":"forge/job","event":"pull_request","status":"completed","conclusion":"success"}]}`
	hub.transientRuns = 3
	_, err = DeliverAndMerge(context.Background(), cfg, input, AutomationOptions{Options: op, PollInterval: time.Millisecond, NoRunsGrace: time.Second, Timeout: time.Minute})
	if !AsError(err, &deliveryErr) || deliveryErr.Code != "transient_api" {
		t.Fatalf("transient error = %#v", err)
	}
	result, err := DeliverAndMerge(context.Background(), cfg, input, AutomationOptions{Options: op, PollInterval: time.Millisecond, NoRunsGrace: time.Second, Timeout: time.Minute})
	if err != nil || result.MergeSHA != hub.merge || hub.mergeCalls != 1 {
		t.Fatalf("retry result=%#v err=%v merge_calls=%d", result, err, hub.mergeCalls)
	}
}
