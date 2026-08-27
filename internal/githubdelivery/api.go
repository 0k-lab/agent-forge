package githubdelivery

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type apiClient struct {
	cfg  Config
	op   Options
	base string
	key  *rsa.PrivateKey
}

func newAPI(cfg Config, op Options, key *rsa.PrivateKey) *apiClient {
	base := op.BaseURL
	if base == "" {
		base = cfg.APIBase
	}
	return &apiClient{cfg: cfg, op: op, base: strings.TrimSuffix(base, "/"), key: key}
}

func (a *apiClient) preflight(ctx context.Context, required map[string]string) (int64, error) {
	var repository struct {
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		Owner    struct {
			Type string `json:"type"`
		} `json:"owner"`
	}
	repositoryPath := "/repos/" + escaped(a.cfg.Owner) + "/" + escaped(a.cfg.Repository)
	status, err := a.request(ctx, "GET", repositoryPath, "", nil, &repository)
	if err != nil {
		return 0, err
	}
	if status == http.StatusNotFound || status == http.StatusOK && (repository.Private || !strings.EqualFold(repository.FullName, a.cfg.Owner+"/"+a.cfg.Repository)) {
		return 0, failure("repository_not_found")
	}
	if status != http.StatusOK {
		return 0, classify(status)
	}
	jwt, err := appJWT(a.cfg.AppID, a.key, a.op.Now())
	if err != nil {
		return 0, failure("credential_invalid")
	}
	var installation struct {
		ID          int64             `json:"id"`
		Permissions map[string]string `json:"permissions"`
	}
	status, err = a.request(ctx, "GET", repositoryPath+"/installation", jwt, nil, &installation)
	if err != nil {
		return 0, err
	}
	if status == http.StatusNotFound {
		ownerKind := map[string]string{"Organization": "orgs", "User": "users"}[repository.Owner.Type]
		if ownerKind == "" {
			return 0, failure("api_rejected")
		}
		ownerStatus, ownerErr := a.request(ctx, "GET", "/"+ownerKind+"/"+escaped(a.cfg.Owner)+"/installation", jwt, nil, &struct{}{})
		if ownerErr != nil {
			return 0, ownerErr
		}
		if ownerStatus == http.StatusOK {
			return 0, failure("repository_not_selected")
		}
		if ownerStatus != http.StatusNotFound {
			return 0, classify(ownerStatus)
		}
		return 0, failure("installation_absent")
	}
	if status == http.StatusUnauthorized {
		return 0, failure("credential_expired")
	}
	if status != http.StatusOK || installation.ID < 1 {
		return 0, classify(status)
	}
	for name, permission := range required {
		actual := installation.Permissions[name]
		if actual != permission && !(permission == "read" && actual == "write") {
			return 0, failure("permission_missing")
		}
	}
	return installation.ID, nil
}

func (a *apiClient) mint(ctx context.Context, installationID int64, permissions map[string]string) (string, error) {
	jwt, err := appJWT(a.cfg.AppID, a.key, a.op.Now())
	if err != nil {
		return "", failure("credential_invalid")
	}
	body := map[string]any{"repositories": []string{a.cfg.Repository}, "permissions": permissions}
	var response struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	status, err := a.request(ctx, "POST", "/app/installations/"+number(installationID)+"/access_tokens", jwt, body, &response)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized {
		return "", failure("credential_expired")
	}
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return "", failure("repository_not_selected")
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", classify(status)
	}
	expires, parseErr := time.Parse(time.RFC3339, response.ExpiresAt)
	if response.Token == "" || parseErr != nil || !expires.After(a.op.Now().Add(time.Minute)) {
		return "", failure("credential_expired")
	}
	return response.Token, nil
}

func (a *apiClient) branch(ctx context.Context, token, branch string) (string, bool, error) {
	var response struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	status, err := a.request(ctx, "GET", "/repos/"+escaped(a.cfg.Owner)+"/"+escaped(a.cfg.Repository)+"/git/ref/heads/"+escaped(branch), token, nil, &response)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	if status == http.StatusUnauthorized {
		return "", false, failure("credential_expired")
	}
	if status != http.StatusOK || !hash(response.Object.SHA) {
		return "", false, classify(status)
	}
	return response.Object.SHA, true, nil
}

type pull struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Ref   string `json:"ref"`
		Label string `json:"label"`
		SHA   string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

func (a *apiClient) reconcilePR(ctx context.Context, token string, input Publication, refresh func() (string, error)) (string, error) {
	query := url.Values{"state": {"open"}, "head": {a.cfg.Owner + ":" + input.NewBranch}, "base": {input.BaseBranch}}
	path := "/repos/" + escaped(a.cfg.Owner) + "/" + escaped(a.cfg.Repository) + "/pulls?" + query.Encode()
	find := func() (*pull, error) {
		var pulls []pull
		status, err := a.request(ctx, "GET", path, token, nil, &pulls)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized {
			return nil, failure("credential_expired")
		}
		if status != http.StatusOK {
			return nil, classify(status)
		}
		if len(pulls) > 1 {
			return nil, failure("pull_request_conflict")
		}
		if len(pulls) == 0 {
			return nil, nil
		}
		pr := pulls[0]
		headOwner, headBranch, exactHead := strings.Cut(pr.Head.Label, ":")
		if !exactHead || !strings.EqualFold(headOwner, a.cfg.Owner) || headBranch != input.NewBranch || pr.Head.Ref != input.NewBranch || pr.Base.Ref != input.BaseBranch {
			return nil, failure("pull_request_conflict")
		}
		return &pr, nil
	}
	findAuthorized := func() (*pull, error) {
		pr, err := find()
		if isCode(err, "credential_expired") {
			if token, err = refresh(); err == nil {
				pr, err = find()
			}
		}
		return pr, err
	}
	var finish func(pull) (string, error)
	finish = func(pr pull) (string, error) {
		if pr.Title != input.PRTitle || pr.Body != input.PRBody {
			var updated pull
			status, err := a.request(ctx, "PATCH", "/repos/"+escaped(a.cfg.Owner)+"/"+escaped(a.cfg.Repository)+"/pulls/"+strconv.Itoa(pr.Number), token, map[string]string{"title": input.PRTitle, "body": input.PRBody}, &updated)
			if err != nil {
				return "", err
			}
			if status == http.StatusUnauthorized {
				if token, err = refresh(); err != nil {
					return "", err
				}
				reconciled, err := find()
				if err != nil {
					return "", err
				}
				if reconciled == nil {
					return "", failure("pull_request_conflict")
				}
				return finish(*reconciled)
			}
			if status != http.StatusOK {
				return "", classify(status)
			}
			pr = updated
		}
		if pr.HTMLURL == "" {
			return "", failure("transient_api")
		}
		return pr.HTMLURL, nil
	}
	pr, err := findAuthorized()
	if err != nil {
		return "", err
	}
	if pr != nil {
		return finish(*pr)
	}
	createPath := "/repos/" + escaped(a.cfg.Owner) + "/" + escaped(a.cfg.Repository) + "/pulls"
	body := map[string]string{"head": input.NewBranch, "base": input.BaseBranch, "title": input.PRTitle, "body": input.PRBody}
	for attempt := 0; attempt < 2; attempt++ {
		var created pull
		status, createErr := a.request(ctx, "POST", createPath, token, body, &created)
		if createErr == nil && (status == http.StatusCreated || status == http.StatusOK) && created.HTMLURL != "" {
			return created.HTMLURL, nil
		}
		if status == http.StatusUnauthorized {
			if token, err = refresh(); err != nil {
				return "", err
			}
		} else if createErr == nil && status != http.StatusUnprocessableEntity && !retryable(status) && !(status >= 200 && status < 300) {
			return "", classify(status)
		}
		pr, err = findAuthorized()
		if err != nil {
			return "", err
		}
		if pr != nil {
			return finish(*pr)
		}
		if status == http.StatusUnprocessableEntity {
			closedQuery := url.Values{"state": {"closed"}, "head": {a.cfg.Owner + ":" + input.NewBranch}, "base": {input.BaseBranch}}
			var closed []pull
			closedStatus, closedErr := a.request(ctx, "GET", "/repos/"+escaped(a.cfg.Owner)+"/"+escaped(a.cfg.Repository)+"/pulls?"+closedQuery.Encode(), token, nil, &closed)
			if closedErr != nil {
				return "", closedErr
			}
			if closedStatus != http.StatusOK {
				return "", classify(closedStatus)
			}
			if len(closed) > 1 || len(closed) == 1 && (!closed[0].Merged || closed[0].Head.SHA != "" && closed[0].Head.SHA != input.CandidateSHA) {
				return "", failure("pull_request_conflict")
			}
			if len(closed) == 1 {
				return finish(closed[0])
			}
		}
		if attempt == 1 {
			if createErr != nil || retryable(status) || status >= 200 && status < 300 {
				return "", failure("transient_api")
			}
			return "", classify(status)
		}
	}
	panic("unreachable")
}

func (a *apiClient) request(ctx context.Context, method, path, bearer string, requestBody, responseBody any) (int, error) {
	var encoded []byte
	if requestBody != nil {
		var err error
		encoded, err = json.Marshal(requestBody)
		if err != nil {
			return 0, failure("request_invalid")
		}
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, a.base+path, bytes.NewReader(encoded))
		if err != nil {
			return 0, failure("request_invalid")
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if requestBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		client := *a.op.HTTPClient
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
		response, err := client.Do(req)
		if err != nil {
			if attempt < attempts-1 && a.op.RetryDelay(ctx, 100*time.Millisecond) == nil {
				continue
			}
			return 0, failure("transient_api")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxOutput+1))
		response.Body.Close()
		if readErr != nil || len(body) > maxOutput {
			if attempt < attempts-1 && a.op.RetryDelay(ctx, 100*time.Millisecond) == nil {
				continue
			}
			return 0, failure("transient_api")
		}
		if retryable(response.StatusCode) && attempt < attempts-1 {
			delay := retryAfter(response.Header.Get("Retry-After"))
			if err := a.op.RetryDelay(ctx, delay); err != nil {
				return 0, failure("transient_api")
			}
			continue
		}
		if responseBody != nil && response.StatusCode >= 200 && response.StatusCode < 300 && (len(body) == 0 || json.Unmarshal(body, responseBody) != nil) {
			return 0, failure("transient_api")
		}
		return response.StatusCode, nil
	}
	return 0, failure("transient_api")
}

func retryable(status int) bool {
	return status == 429 || status == 502 || status == 503 || status == 504
}
func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 100 * time.Millisecond
	}
	return min(time.Duration(seconds)*time.Second, 2*time.Second)
}

func classify(status int) error {
	if retryable(status) {
		return failure("transient_api")
	}
	return failure("api_rejected")
}
