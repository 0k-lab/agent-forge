package githubdelivery

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type AutomationOptions struct {
	Options
	PollInterval time.Duration
	NoRunsGrace  time.Duration
	Timeout      time.Duration
	OnState      func(Result, int, string) error
}

type AutomationResult struct {
	Result
	PRNumber int    `json:"-"`
	CIState  string `json:"ci_state"`
	MergeSHA string `json:"merge_sha"`
}

func DeliverAndMerge(parent context.Context, cfg Config, input Publication, options AutomationOptions) (AutomationResult, error) {
	if options.PollInterval <= 0 || options.NoRunsGrace < 0 || options.Timeout <= 0 {
		return AutomationResult{}, failure("automation_invalid")
	}
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	state, err := localPreflight(ctx, cfg, input, normalizeOptions(options.Options))
	if err != nil {
		return AutomationResult{}, err
	}
	state.sandbox.remove()
	key, err := readPrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return AutomationResult{}, failure("credential_invalid")
	}
	op := normalizeOptions(options.Options)
	api := newAPI(cfg, op, key)
	permissions := map[string]string{"actions": "read", "contents": "write", "pull_requests": "write"}
	installation, err := api.preflight(ctx, permissions)
	if err != nil {
		return AutomationResult{}, err
	}
	token, err := api.mint(ctx, installation, permissions)
	if err != nil {
		return AutomationResult{}, err
	}
	if existing, found, err := api.optionalExactPull(ctx, token, input); err != nil {
		return AutomationResult{}, err
	} else if found && existing.Merged {
		if !hash(existing.MergeCommitSHA) {
			return AutomationResult{}, failure("merge_failed")
		}
		return AutomationResult{Result: Result{Version: 1, CandidateSHA: input.CandidateSHA, Branch: input.NewBranch, PRURL: existing.HTMLURL, Status: "merged"}, PRNumber: existing.Number, CIState: "success", MergeSHA: existing.MergeCommitSHA}, nil
	}
	published, err := Deliver(ctx, cfg, input, options.Options)
	if err != nil {
		return AutomationResult{}, err
	}
	key, err = readPrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return AutomationResult{}, failure("credential_invalid")
	}
	api = newAPI(cfg, op, key)
	installation, err = api.preflight(ctx, permissions)
	if err != nil {
		return AutomationResult{}, err
	}
	token, err = api.mint(ctx, installation, permissions)
	if err != nil {
		return AutomationResult{}, err
	}
	pr, err := api.exactPull(ctx, token, input)
	if err != nil {
		return AutomationResult{}, err
	}
	result := AutomationResult{Result: published, PRNumber: pr.Number}
	if pr.Merged {
		if !hash(pr.MergeCommitSHA) {
			return AutomationResult{}, failure("merge_failed")
		}
		result.Status, result.CIState, result.MergeSHA = "merged", "success", pr.MergeCommitSHA
		return result, nil
	}
	if options.OnState != nil && options.OnState(published, pr.Number, "pending") != nil {
		return AutomationResult{}, failure("state_failed")
	}
	started := op.Now()
	for {
		if err := exactPullIdentity(pr, input); err != nil {
			return AutomationResult{}, err
		}
		runs, err := api.workflowRuns(ctx, token, input)
		if err != nil {
			return AutomationResult{}, err
		}
		if len(runs) == 0 {
			if op.Now().Sub(started) >= options.NoRunsGrace {
				return AutomationResult{}, failure("ci_no_runs")
			}
		} else {
			complete := true
			for _, run := range runs {
				if run.HeadSHA != input.CandidateSHA || run.HeadBranch != input.NewBranch || run.Event != "pull_request" {
					return AutomationResult{}, failure("ci_ambiguous")
				}
				if run.Status != "completed" {
					complete = false
				} else if run.Conclusion != "success" {
					return AutomationResult{}, failure("ci_failed")
				}
			}
			if complete {
				break
			}
		}
		if err := op.RetryDelay(ctx, options.PollInterval); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return AutomationResult{}, failure("ci_timeout")
			}
			return AutomationResult{}, failure("transient_api")
		}
		pr, err = api.pull(ctx, token, pr.Number)
		if err != nil {
			return AutomationResult{}, err
		}
	}
	if options.OnState != nil && options.OnState(published, pr.Number, "success") != nil {
		return AutomationResult{}, failure("state_failed")
	}
	pr, err = api.pull(ctx, token, pr.Number)
	if err != nil {
		return AutomationResult{}, err
	}
	if err := exactPullIdentity(pr, input); err != nil {
		return AutomationResult{}, err
	}
	mergeSHA, err := api.merge(ctx, token, pr.Number, input.CandidateSHA)
	if err != nil {
		return AutomationResult{}, err
	}
	result.Status, result.CIState, result.MergeSHA = "merged", "success", mergeSHA
	return result, nil
}

func exactPullIdentity(pr pull, input Publication) error {
	if pr.Number < 1 || pr.Head.Ref != input.NewBranch || pr.Head.SHA != input.CandidateSHA {
		return failure("head_changed")
	}
	if pr.Base.Ref != input.BaseBranch || pr.Base.SHA != input.ExpectedParentSHA {
		return failure("base_changed")
	}
	return nil
}

func (a *apiClient) exactPull(ctx context.Context, token string, input Publication) (pull, error) {
	pr, found, err := a.optionalExactPull(ctx, token, input)
	if err != nil {
		return pull{}, err
	}
	if !found {
		return pull{}, failure("pull_request_conflict")
	}
	return pr, nil
}

func (a *apiClient) optionalExactPull(ctx context.Context, token string, input Publication) (pull, bool, error) {
	query := url.Values{"state": {"all"}, "head": {a.cfg.Owner + ":" + input.NewBranch}, "base": {input.BaseBranch}, "per_page": {"100"}}
	var pulls []pull
	status, err := a.request(ctx, http.MethodGet, "/repos/"+escaped(a.cfg.Owner)+"/"+escaped(a.cfg.Repository)+"/pulls?"+query.Encode(), token, nil, &pulls)
	if err != nil {
		return pull{}, false, err
	}
	if status != http.StatusOK {
		return pull{}, false, classify(status)
	}
	if len(pulls) > 1 {
		return pull{}, false, failure("pull_request_conflict")
	}
	if len(pulls) == 0 {
		return pull{}, false, nil
	}
	if err := exactPullIdentity(pulls[0], input); err != nil {
		return pull{}, false, err
	}
	return pulls[0], true, nil
}

func (a *apiClient) pull(ctx context.Context, token string, number int) (pull, error) {
	var pr pull
	status, err := a.request(ctx, http.MethodGet, "/repos/"+escaped(a.cfg.Owner)+"/"+escaped(a.cfg.Repository)+"/pulls/"+strconv.Itoa(number), token, nil, &pr)
	if err != nil {
		return pull{}, err
	}
	if status != http.StatusOK {
		return pull{}, classify(status)
	}
	return pr, nil
}

type workflowRun struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func (a *apiClient) workflowRuns(ctx context.Context, token string, input Publication) ([]workflowRun, error) {
	query := url.Values{"head_sha": {input.CandidateSHA}, "branch": {input.NewBranch}, "event": {"pull_request"}, "per_page": {"100"}}
	var response struct {
		TotalCount int           `json:"total_count"`
		Runs       []workflowRun `json:"workflow_runs"`
	}
	status, err := a.request(ctx, http.MethodGet, "/repos/"+escaped(a.cfg.Owner)+"/"+escaped(a.cfg.Repository)+"/actions/runs?"+query.Encode(), token, nil, &response)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, classify(status)
	}
	if response.TotalCount != len(response.Runs) || len(response.Runs) > 100 {
		return nil, failure("ci_ambiguous")
	}
	seen := map[int64]bool{}
	for _, run := range response.Runs {
		if run.ID < 1 || seen[run.ID] {
			return nil, failure("ci_ambiguous")
		}
		seen[run.ID] = true
	}
	return response.Runs, nil
}

func (a *apiClient) merge(ctx context.Context, token string, number int, head string) (string, error) {
	var response struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	status, err := a.request(ctx, http.MethodPut, "/repos/"+escaped(a.cfg.Owner)+"/"+escaped(a.cfg.Repository)+"/pulls/"+strconv.Itoa(number)+"/merge", token, map[string]string{"sha": head}, &response)
	if err != nil {
		return "", err
	}
	if status == http.StatusConflict || status == http.StatusUnprocessableEntity {
		return "", failure("head_changed")
	}
	if status != http.StatusOK || !response.Merged || !hash(response.SHA) {
		return "", failure("merge_failed")
	}
	return response.SHA, nil
}
