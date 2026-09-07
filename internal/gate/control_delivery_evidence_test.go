package gate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const closedIssueFixture = `{"number":53,"html_url":"https://github.com/0k-lab/agent-forge/issues/53","state":"closed","closed_at":"2026-08-29T20:44:18Z"}`
const mergedTimelineFixture = `[{"event":"cross-referenced","source":{"type":"issue","issue":{"number":55,"html_url":"https://github.com/0k-lab/agent-forge/pull/55","pull_request":{"html_url":"https://github.com/0k-lab/agent-forge/pull/55","merged_at":"2026-08-29T20:44:17Z"}}}}]`
const prDetailFixture = `{"number":55,"html_url":"https://github.com/0k-lab/agent-forge/pull/55","title":"Preserve Unicode","body":"Normalize Unicode input so equivalent names match.","merged_at":"2026-08-29T20:44:17Z","changed_files":2,"additions":18,"deletions":3,"token":"private-secret"}`
const prFilesFixture = `[{"filename":"parser/normalize.go","status":"modified","additions":12,"deletions":3,"patch":"private-patch"},{"filename":"parser/normalize_test.go","status":"added","additions":6,"deletions":0}]`

func TestControlDeliveredChanges(t *testing.T) {
	_, _, h := controlFixture(t)
	calls := 0
	mockIssues(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("unsafe request")
		}
		switch r.URL.Path {
		case "/repos/0k-lab/agent-forge/issues/53":
			return issueResponse(200, closedIssueFixture), nil
		case "/repos/0k-lab/agent-forge/issues/53/timeline":
			return issueResponse(200, mergedTimelineFixture), nil
		case "/repos/0k-lab/agent-forge/pulls/55":
			return issueResponse(200, prDetailFixture), nil
		case "/repos/0k-lab/agent-forge/pulls/55/files":
			if r.URL.Query().Get("per_page") != "20" {
				t.Fatal("unbounded files")
			}
			return issueResponse(200, prFilesFixture), nil
		default:
			t.Fatal("unexpected request", r.URL)
			return nil, nil
		}
	})
	w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues/53/activity", "owner", "")
	var got struct {
		MergedPRs []struct {
			Details struct {
				Title, Summary string
				ChangedFiles   int `json:"changed_files"`
				Files          []struct{ Filename string }
			}
		} `json:"merged_prs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.MergedPRs) != 1 || got.MergedPRs[0].Details.Title != "Preserve Unicode" || got.MergedPRs[0].Details.ChangedFiles != 2 || len(got.MergedPRs[0].Details.Files) != 2 || calls != 4 {
		t.Fatalf("missing delivered changes: %d %s (%d requests)", w.Code, w.Body.String(), calls)
	}
	if !strings.Contains(got.MergedPRs[0].Details.Summary, "equivalent names match") || got.MergedPRs[0].Details.Files[1].Filename != "parser/normalize_test.go" {
		t.Fatal("missing explanation/files")
	}
	for _, bad := range []string{"private-secret", "private-patch"} {
		if strings.Contains(w.Body.String(), bad) {
			t.Fatal("leak")
		}
	}
}

func TestControlDeliveredChangesFailureAndBounds(t *testing.T) {
	_, _, h := controlFixture(t)
	for _, mode := range []string{"redirect", "oversize", "bad-count", "wrong-url", "files-failure", "sanitized"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			mockIssues(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Fatal("credentials or redirect")
				}
				if strings.HasSuffix(r.URL.Path, "/issues/53") {
					return issueResponse(200, closedIssueFixture), nil
				}
				if strings.HasSuffix(r.URL.Path, "/timeline") {
					return issueResponse(200, mergedTimelineFixture), nil
				}
				if strings.HasSuffix(r.URL.Path, "/files") {
					if mode == "files-failure" {
						return issueResponse(429, "private-secret"), nil
					}
					files := strings.TrimSuffix(prFilesFixture, "]") + `,{"filename":"../private","status":"added","additions":1,"deletions":0},{"filename":"bad\u202ename","status":"added","additions":1,"deletions":0}]`
					return issueResponse(200, files), nil
				}
				switch mode {
				case "redirect":
					response := issueResponse(302, "")
					response.Header.Set("Location", "https://evil.example/private")
					return response, nil
				case "oversize":
					return issueResponse(200, strings.Repeat("x", (1<<20)+1)), nil
				case "bad-count":
					return issueResponse(200, strings.Replace(prDetailFixture, `"changed_files":2`, `"changed_files":-1`, 1)), nil
				case "wrong-url":
					return issueResponse(200, strings.ReplaceAll(prDetailFixture, "https://github.com", "https://secret@github.com")), nil
				default:
					return issueResponse(200, prDetailFixture), nil
				}
			})
			w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues/53/activity", "owner", "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"merged_at":"2026-08-29T20:44:17Z"`) {
				t.Fatal("lost basic evidence", w.Body.String())
			}
			if mode == "sanitized" {
				if !strings.Contains(w.Body.String(), `"files_truncated":true`) || strings.Contains(w.Body.String(), "bad") {
					t.Fatal("unsafe file", w.Body.String())
				}
			} else if strings.Contains(w.Body.String(), `"details"`) {
				t.Fatal("invalid detail accepted")
			}
			if calls > 4 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "secret@") {
				t.Fatal("unbounded/unsafe response")
			}
		})
	}
}

func TestControlDeliveredChangesCapsPRsFilesAndSummary(t *testing.T) {
	_, _, h := controlFixture(t)
	calls := 0
	mockIssues(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("auth forwarded")
		}
		if strings.HasSuffix(r.URL.Path, "/issues/53") {
			return issueResponse(200, closedIssueFixture), nil
		}
		if strings.HasSuffix(r.URL.Path, "/timeline") {
			records := []string{}
			for _, n := range []string{"55", "56", "57", "58"} {
				records = append(records, strings.TrimSuffix(strings.TrimPrefix(strings.ReplaceAll(mergedTimelineFixture, "55", n), "["), "]"))
			}
			return issueResponse(200, "["+strings.Join(records, ",")+"]"), nil
		}
		if strings.HasSuffix(r.URL.Path, "/files") {
			var files []map[string]any
			for i := 0; i < 25; i++ {
				files = append(files, map[string]any{"filename": fmt.Sprintf("src/file%d.go", i), "status": "modified", "additions": 1, "deletions": 0})
			}
			body, _ := json.Marshal(files)
			return issueResponse(200, string(body)), nil
		}
		parts := strings.Split(r.URL.Path, "/")
		number := parts[len(parts)-1]
		body := strings.ReplaceAll(prDetailFixture, "55", number)
		body = strings.Replace(body, `"changed_files":2`, `"changed_files":25`, 1)
		body = strings.Replace(body, "Normalize Unicode input so equivalent names match.", strings.Repeat("x", 2100), 1)
		return issueResponse(200, body), nil
	})
	w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues/53/activity", "owner", "")
	var got struct {
		MergedPRs []controlMergedPR `json:"merged_prs"`
		Truncated bool
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.MergedPRs) != 3 || calls != 8 || !got.Truncated {
		t.Fatal("PR bound", w.Code, calls, w.Body.String())
	}
	for _, pr := range got.MergedPRs {
		if pr.Details == nil || len(pr.Details.Files) != 20 || !pr.Details.FilesTruncated || len(pr.Details.Summary) != 2000 || !pr.Details.SummaryTruncated {
			t.Fatal("missing bounds", pr)
		}
	}
}

func TestCrossReferenceRequiresStrictClosingProof(t *testing.T) {
	for _, tc := range []struct {
		name, body, branch, repo, state, closed string
		want                                    bool
	}{
		{"unrelated", "Improve docs", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"plain mention", "#53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"wrong number", "Closes #530", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"other issue", "Fixes #54", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"other repo", "Closes other/repo#53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"inline", "`Closes #53`", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"inline ref", "Closes `#53`", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"fence", "```text\nCloses #53\n```", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"tilde fence", "~~~\nCloses #53\n~~~", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"quote", "> Closes #53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"quotation", "\"Closes #53\"", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"indented code", "    Closes #53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"ordinary", "This mentions #53 but does not fix it", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"branch", "Closes #53", "develop", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", false},
		{"base repo", "Closes #53", "main", "other/repo", "closed", "2026-08-29T20:44:18Z", false},
		{"open", "Closes #53", "main", "0k-lab/agent-forge", "open", "2026-08-29T20:44:18Z", false},
		{"after closure", "Closes #53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:16Z", false},
		{"too late", "Closes #53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:49:18Z", false},
		{"missing time", "Closes #53", "main", "0k-lab/agent-forge", "closed", "", false},
		{"exact", "Closes #53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", true},
		{"canonical", "Fixes 0k-lab/agent-forge#53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:44:18Z", true},
		{"window boundary", "Resolves #53", "main", "0k-lab/agent-forge", "closed", "2026-08-29T20:49:17Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, h := controlFixture(t)
			mockIssues(t, func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/issues/53"):
					b, _ := json.Marshal(map[string]any{"number": 53, "html_url": "https://github.com/0k-lab/agent-forge/issues/53", "state": tc.state, "closed_at": tc.closed})
					return issueResponse(200, string(b)), nil
				case strings.HasSuffix(r.URL.Path, "/timeline"):
					return issueResponse(200, mergedTimelineFixture), nil
				case strings.HasSuffix(r.URL.Path, "/files"):
					return issueResponse(200, prFilesFixture), nil
				default:
					var raw map[string]any
					json.Unmarshal([]byte(prDetailFixture), &raw)
					raw["body"] = tc.body
					raw["base"] = map[string]any{"ref": tc.branch, "repo": map[string]string{"html_url": "https://github.com/" + tc.repo}}
					b, _ := json.Marshal(raw)
					return issueResponse(200, string(b)), nil
				}
			})
			w := controlRequest(h, "GET", "/v1/control/projects/agent-forge/issues/53/activity", "owner", "")
			var got struct {
				PRs []struct{ Details map[string]any } `json:"merged_prs"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.PRs) != 1 || got.PRs[0].Details == nil {
				t.Fatalf("lost related details %s", w.Body.String())
			}
			proof, ok := got.PRs[0].Details["delivery_evidence"].(bool)
			if !ok || proof != tc.want {
				t.Fatalf("delivery_evidence=%v present=%v want=%v", proof, ok, tc.want)
			}
		})
	}
}

func TestExplicitIssueClosingKeywords(t *testing.T) {
	source := publicSource{Owner: "0k-lab", Repository: "agent-forge"}
	for _, keyword := range []string{"close", "closed", "closes", "fix", "fixed", "fixes", "resolve", "resolved", "resolves"} {
		for _, ref := range []string{"#53", "0k-lab/agent-forge#53"} {
			if !explicitIssueClosure("Description.\n\n"+strings.ToUpper(keyword)+" "+ref+".\n", source, 53) {
				t.Fatalf("closing keyword rejected: %s %s", keyword, ref)
			}
		}
	}
}
