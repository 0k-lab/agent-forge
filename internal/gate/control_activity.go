package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var githubActor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}(\[bot\])?$`)

func issueClosedAt(state, value string) string {
	if state != "closed" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || parsed.IsZero() {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}
func issueClosedBy(state, value string) string {
	if state != "closed" || !githubActor.MatchString(value) {
		return ""
	}
	return value
}

type controlMergedPR struct {
	Number   int64             `json:"number"`
	URL      string            `json:"url"`
	MergedAt string            `json:"merged_at"`
	Details  *controlPRDetails `json:"details,omitempty"`
}

func (x *server) controlIssueActivity(w http.ResponseWriter, r *http.Request) {
	unavailable := func(status int) {
		writeJSON(w, status, map[string]any{"available": false, "error": "public issue activity unavailable"})
	}
	number, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil || number < 1 || number > 9007199254740991 || strconv.FormatInt(number, 10) != r.PathValue("number") {
		unavailable(400)
		return
	}
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
		unavailable(422)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	path := "/repos/" + source.Owner + "/" + source.Repository + "/issues/" + strconv.FormatInt(number, 10)
	canonical := "https://github.com/" + source.Owner + "/" + source.Repository
	body, err := fetchControlGitHub(ctx, path)
	if err != nil {
		unavailable(502)
		return
	}
	var raw struct {
		Number   int64  `json:"number"`
		URL      string `json:"html_url"`
		State    string `json:"state"`
		ClosedAt string `json:"closed_at"`
		ClosedBy struct {
			Login string `json:"login"`
		} `json:"closed_by"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	issueURL := canonical + "/issues/" + strconv.FormatInt(number, 10)
	if json.Unmarshal(body, &raw) != nil || raw.Number != number || raw.URL != issueURL || len(raw.PullRequest) != 0 || (raw.State != "closed" && raw.State != "open") {
		unavailable(502)
		return
	}
	issue := controlIssue{Number: int(number), URL: issueURL, State: raw.State, ClosedAt: issueClosedAt(raw.State, raw.ClosedAt), ClosedBy: issueClosedBy(raw.State, raw.ClosedBy.Login)}
	body, err = fetchControlGitHub(ctx, path+"/timeline?per_page=50")
	if err != nil {
		unavailable(502)
		return
	}
	var events []json.RawMessage
	if json.Unmarshal(body, &events) != nil || events == nil {
		unavailable(502)
		return
	}
	truncated := len(events) >= maxProjectIssues
	if len(events) > maxProjectIssues {
		events = events[:maxProjectIssues]
	}
	prs := []controlMergedPR{}
	seen := map[string]bool{}
	for _, record := range events {
		var event struct {
			Event  string `json:"event"`
			Source struct {
				Type  string `json:"type"`
				Issue struct {
					Number      int64  `json:"number"`
					URL         string `json:"html_url"`
					PullRequest struct {
						URL      string `json:"html_url"`
						MergedAt string `json:"merged_at"`
					} `json:"pull_request"`
				} `json:"issue"`
			} `json:"source"`
		}
		if json.Unmarshal(record, &event) != nil {
			continue
		}
		pr := event.Source.Issue
		if event.Event != "cross-referenced" || event.Source.Type != "issue" || pr.Number < 1 || pr.Number > 9007199254740991 {
			continue
		}
		url := canonical + "/pull/" + strconv.FormatInt(pr.Number, 10)
		merged := issueClosedAt("closed", pr.PullRequest.MergedAt)
		if pr.URL != url || pr.PullRequest.URL != url || merged == "" || seen[url] {
			continue
		}
		seen[url] = true
		if len(prs) == 3 {
			truncated = true
			continue
		}
		prs = append(prs, controlMergedPR{Number: pr.Number, URL: url, MergedAt: merged})
	}
	for i := range prs {
		prs[i].Details = fetchControlPRDetails(ctx, source, prs[i], issue, repository.DefaultBranch)
	}
	writeJSON(w, 200, map[string]any{"available": true, "issue": issue, "merged_prs": prs, "truncated": truncated})
}

type controlPRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int64  `json:"additions"`
	Deletions int64  `json:"deletions"`
}
type controlPRDetails struct {
	DeliveryEvidence bool            `json:"delivery_evidence"`
	Title            string          `json:"title"`
	Summary          string          `json:"summary"`
	SummaryTruncated bool            `json:"summary_truncated"`
	ChangedFiles     int64           `json:"changed_files"`
	Additions        int64           `json:"additions"`
	Deletions        int64           `json:"deletions"`
	Files            []controlPRFile `json:"files"`
	FilesTruncated   bool            `json:"files_truncated"`
}

func validPRCount(n *int64) bool { return n != nil && *n >= 0 && *n <= 9007199254740991 }
func fetchControlPRDetails(ctx context.Context, source publicSource, pr controlMergedPR, issue controlIssue, defaultBranch string) *controlPRDetails {
	endpoint := "/repos/" + source.Owner + "/" + source.Repository + "/pulls/" + strconv.FormatInt(pr.Number, 10)
	body, err := fetchControlGitHub(ctx, endpoint)
	if err != nil {
		return nil
	}
	var raw struct {
		Number   int64  `json:"number"`
		URL      string `json:"html_url"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		MergedAt string `json:"merged_at"`
		Base     struct {
			Ref  string `json:"ref"`
			Repo struct {
				URL string `json:"html_url"`
			} `json:"repo"`
		} `json:"base"`
		ChangedFiles *int64 `json:"changed_files"`
		Additions    *int64 `json:"additions"`
		Deletions    *int64 `json:"deletions"`
	}
	if json.Unmarshal(body, &raw) != nil || raw.Number != pr.Number || raw.URL != pr.URL || issueClosedAt("closed", raw.MergedAt) != pr.MergedAt || !validPRCount(raw.ChangedFiles) || !validPRCount(raw.Additions) || !validPRCount(raw.Deletions) {
		return nil
	}
	title := boundedIssueText(raw.Title, 120, false)
	if title == "" {
		return nil
	}
	detail := &controlPRDetails{Title: title, Summary: boundedIssueText(raw.Body, 2000, true), SummaryTruncated: len([]rune(raw.Body)) > 2000, ChangedFiles: *raw.ChangedFiles, Additions: *raw.Additions, Deletions: *raw.Deletions, Files: []controlPRFile{}}
	merged, mergeErr := time.Parse(time.RFC3339, pr.MergedAt)
	closed, closeErr := time.Parse(time.RFC3339, issue.ClosedAt)
	// Closing evidence requires closure within five minutes of a default-branch merge.
	detail.DeliveryEvidence = issue.State == "closed" && mergeErr == nil && closeErr == nil &&
		!closed.Before(merged) && closed.Sub(merged) <= 5*time.Minute && defaultBranch != "" &&
		raw.Base.Ref == defaultBranch && raw.Base.Repo.URL == "https://github.com/"+source.Owner+"/"+source.Repository &&
		explicitIssueClosure(raw.Body, source, issue.Number)
	body, err = fetchControlGitHub(ctx, endpoint+"/files?per_page=20")
	if err != nil {
		return nil
	}
	var records []json.RawMessage
	if json.Unmarshal(body, &records) != nil || records == nil {
		return nil
	}
	detail.FilesTruncated = len(records) > 20 || detail.ChangedFiles > int64(len(records))
	if len(records) > 20 {
		records = records[:20]
	}
	for _, record := range records {
		var f struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions *int64 `json:"additions"`
			Deletions *int64 `json:"deletions"`
		}
		if json.Unmarshal(record, &f) != nil || !validPRCount(f.Additions) || !validPRCount(f.Deletions) || f.Filename == "" || f.Filename == "." || len(f.Filename) > 256 || boundedIssueText(f.Filename, 256, false) != f.Filename || path.IsAbs(f.Filename) || path.Clean(f.Filename) != f.Filename || strings.HasPrefix(f.Filename, "../") || strings.ContainsAny(f.Filename, "\\:") {
			detail.FilesTruncated = true
			continue
		}
		switch f.Status {
		case "added", "removed", "modified", "renamed", "copied", "changed", "unchanged":
		default:
			detail.FilesTruncated = true
			continue
		}
		detail.Files = append(detail.Files, controlPRFile{Filename: f.Filename, Status: f.Status, Additions: *f.Additions, Deletions: *f.Deletions})
	}
	return detail
}

// Only standalone closing clauses are proof. Ambiguous Markdown/HTML/quotations
// anywhere in the body conservatively leave the PR related, never delivered.
func explicitIssueClosure(body string, source publicSource, number int) bool {
	if strings.ContainsAny(body, "`~<>\"'“”‘’") {
		return false
	}
	ref := "#" + strconv.Itoa(number)
	clause := regexp.MustCompile(`(?m)^(?i:close|closed|closes|fix|fixed|fixes|resolve|resolved|resolves)[ \t]+(?:` + regexp.QuoteMeta(ref) + `|` + regexp.QuoteMeta(source.Owner+"/"+source.Repository+ref) + `)[ \t]*\.?[ \t]*$`)
	return clause.MatchString(body)
}
