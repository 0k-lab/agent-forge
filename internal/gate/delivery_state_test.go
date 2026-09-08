package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/githubdelivery"
	"agent-forge/internal/store"
)

type deliveryLogWriter func([]byte) (int, error)

func (f deliveryLogWriter) Write(p []byte) (int, error) { return f(p) }

func TestDeliveryStateWriteFailureRecovery(t *testing.T) {
	for _, mode := range []string{"complete", "retry", "fail", "registration", "complete-live", "ci", "ci-live", "merging", "merging-live"} {
		t.Run(mode, func(t *testing.T) {
			s, x, _, job := heldControlCandidate(t)
			updateFailure := strings.HasPrefix(mode, "ci") || strings.HasPrefix(mode, "merging")
			updatePhase := strings.TrimSuffix(mode, "-live")
			live := strings.HasSuffix(mode, "-live")
			gitCommand(t, x.config.GitExecutable, job.Task.Repository, "update-ref", "refs/heads/main", job.Task.BaseSHA)
			db, err := sql.Open("sqlite", x.config.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := s.ResumeDelivery(job.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			event := "delivery_merged"
			if mode == "retry" {
				event = "delivery_retry"
			}
			if mode == "fail" || mode == "registration" {
				event = "delivery_failed"
			}
			trigger := `CREATE TRIGGER reject_delivery_state BEFORE INSERT ON events WHEN NEW.kind='` + event + `' BEGIN SELECT RAISE(ABORT,'injected delivery state write failure'); END`
			if updateFailure {
				trigger = `CREATE TRIGGER reject_delivery_state BEFORE UPDATE ON deliveries WHEN NEW.phase='` + updatePhase + `' BEGIN SELECT RAISE(ABORT,'injected delivery state write failure'); END`
			}
			if _, err := db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "complete") || updateFailure {
				// Even the final budgeted attempt must reconcile after a crash.
				if _, err := db.Exec(`UPDATE deliveries SET max_attempts=1`); err != nil {
					t.Fatal(err)
				}
			}
			original, err := s.Delivery(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(secureTempDir(t), "app.pem")
			if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
				t.Fatal(err)
			}
			x.config.Delivery = &DeliveryConfig{APIBase: "https://api.github.com", AppID: "123", PrivateKeyPath: keyPath, PollInterval: time.Millisecond, NoRunsGrace: time.Second, Timeout: time.Minute, RetryBase: time.Millisecond, MaxAttempts: 3}
			x.options.LeasePollInterval = time.Millisecond
			merged, mergeCalls, apiCalls := false, 0, 0
			published, pushed := !updateFailure, !updateFailure
			pushCalls, createCalls, ciCalls := 0, 0, 0
			mergeSHA := strings.Repeat("f", 40)
			pull := func() string {
				return fmt.Sprintf(`{"number":42,"html_url":"https://github.com/0k-lab/agent-forge/pull/42","state":"open","merged":%t,"merge_commit_sha":%q,"head":{"ref":%q,"sha":%q,"label":%q},"base":{"ref":"main","sha":%q},"title":%q,"body":%q}`, merged, mergeSHA, "forge/"+job.ID, job.CandidateSHA, "0k-lab:forge/"+job.ID, job.Task.BaseSHA, "Agent Forge job "+job.ID, "Automated delivery for candidate `"+job.CandidateSHA+"`.")
			}
			x.options.Delivery = githubdelivery.Options{
				HTTPClient: &http.Client{Transport: issueTransport(func(r *http.Request) (*http.Response, error) {
					apiCalls++
					body, status := "", 200
					switch {
					case r.Method == "GET" && r.URL.Path == "/repos/0k-lab/agent-forge":
						body = `{"full_name":"0k-lab/agent-forge","private":false,"owner":{"type":"User"}}`
						if mode == "retry" {
							status = 503
						}
						if mode == "fail" {
							status = 404
						}
					case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/installation"):
						body = `{"id":7,"permissions":{"contents":"write","pull_requests":"write"}}`
					case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/access_tokens"):
						body = `{"token":"fake-token","expires_at":"2099-01-01T00:00:00Z"}`
					case r.Method == "GET" && strings.Contains(r.URL.Path, "/git/ref/heads/"):
						if !strings.HasSuffix(r.URL.Path, "/main") && !pushed {
							status = 404
						}
						sha := job.CandidateSHA
						if strings.HasSuffix(r.URL.Path, "/main") {
							sha = job.Task.BaseSHA
						}
						body = `{"object":{"sha":"` + sha + `"}}`
					case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
						body = "[]"
						if published {
							body = "[" + pull() + "]"
						}
					case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
						createCalls++
						published = true
						body, status = pull(), 201
					case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/42"):
						body = pull()
					case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/actions/runs"):
						ciCalls++
						body = `{"total_count":1,"workflow_runs":[{"id":1,"head_sha":"` + job.CandidateSHA + `","head_branch":"forge/` + job.ID + `","event":"pull_request","status":"completed","conclusion":"success"}]}`
					case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/merge"):
						var request struct {
							SHA string `json:"sha"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.SHA != job.CandidateSHA {
							t.Fatalf("merge did not pin exact candidate: %+v %v", request, err)
						}
						mergeCalls++
						merged = true
						body = `{"sha":"` + mergeSHA + `","merged":true}`
					default:
						t.Fatalf("unexpected external operation %s %s", r.Method, r.URL)
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
				RetryDelay: func(context.Context, time.Duration) error { return nil },
				Run: func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
					if slices.Contains(args, "push") {
						if pushed {
							t.Fatal("duplicate external push")
						}
						pushCalls++
						pushed = true
						return nil, nil
					}
					cmd := exec.CommandContext(ctx, name, args...)
					cmd.Dir, cmd.Env = dir, env
					return cmd.CombinedOutput()
				},
			}
			if mode == "registration" {
				x.config.Repositories = nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var logs bytes.Buffer
			sawWriteFailure := false
			x.options.Logger = slog.New(slog.NewTextHandler(deliveryLogWriter(func(p []byte) (int, error) {
				n, _ := logs.Write(p)
				if bytes.Contains(p, []byte("delivery_state_write_failed")) {
					sawWriteFailure = true
					wantCI := 0
					if updatePhase == "merging" {
						wantCI = 1
					}
					if updateFailure && (!published || pushCalls != 1 || createCalls != 1 || mergeCalls != 0 || ciCalls != wantCI) {
						t.Fatalf("wrong failure boundary: published=%t pushes=%d creates=%d CI=%d merges=%d", published, pushCalls, createCalls, ciCalls, mergeCalls)
					}
					if live {
						if _, err := db.Exec(`DROP TRIGGER reject_delivery_state`); err != nil {
							t.Fatal(err)
						}
					} else {
						cancel()
					}
				}
				return n, nil
			}), nil))
			runErr := x.runOneDelivery(ctx)
			if live && runErr != nil {
				t.Fatal(runErr)
			}
			if !live && (runErr == nil || !strings.Contains(runErr.Error(), "injected delivery state write failure")) {
				t.Errorf("write error not propagated: %v", runErr)
			}
			if !sawWriteFailure {
				t.Errorf("state-write error vanished: %s", logs.String())
			}
			d, err := s.Delivery(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !live {
				var terminalEvents int
				if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind IN ('delivery_merged','delivery_failed','delivery_retry')`).Scan(&terminalEvents); err != nil || terminalEvents != 0 {
					t.Errorf("false durable terminal event: %d %v", terminalEvents, err)
				}
				if strings.Contains(logs.String(), "delivery_merged") || strings.Contains(logs.String(), "delivery_failed") {
					t.Errorf("false terminal log: %s", logs.String())
				}
				wantPhase := "publishing"
				if mode == "complete" {
					wantPhase = "merging"
				} else if updatePhase == "merging" {
					wantPhase = "ci"
				}
				if d.Phase != wantPhase {
					t.Errorf("failed write changed durable phase: %+v", d)
				}
				got, err := s.Job(job.ID)
				if err != nil || got.Status != "delivering" {
					t.Errorf("false terminal job: %+v %v", got, err)
				}
				if _, err := db.Exec(`DROP TRIGGER reject_delivery_state`); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = store.Open(x.config.Database)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				x.store = s
				if err := s.ValidateDeliveries(); err != nil {
					t.Fatal(err)
				}
				if err := s.RecoverDeliveries(time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := x.runOneDelivery(context.Background()); err != nil {
					t.Fatal(err)
				}
				d, err = s.Delivery(job.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			want := "merged"
			if mode == "retry" {
				want = "retry_wait"
			}
			if mode == "fail" || mode == "registration" {
				want = "failed"
			}
			if d.Phase != want {
				t.Errorf("recovery phase=%s want=%s", d.Phase, want)
			}
			if (strings.HasPrefix(mode, "complete") || updateFailure) && (mergeCalls != 1 || d.MergeSHA != mergeSHA) {
				t.Errorf("merge calls=%d durable merge=%q", mergeCalls, d.MergeSHA)
			}
			if updateFailure {
				if d.CandidateRef != original.CandidateRef || d.ParentSHA != original.ParentSHA || d.ExpectedTreeSHA != original.ExpectedTreeSHA || d.AttemptID != original.AttemptID {
					t.Errorf("exact candidate identity changed: before=%+v after=%+v", original, d)
				}
				if pushCalls != 1 || createCalls != 1 || d.PRNumber != 42 || d.CandidateSHA != job.CandidateSHA || d.Attempts != 1 || d.MaxAttempts != 1 {
					t.Errorf("duplicate publication or changed candidate/budget: pushes=%d creates=%d delivery=%+v", pushCalls, createCalls, d)
				}
				if strings.Contains(logs.String(), "delivery_failed") {
					t.Errorf("false terminal failure log: %s", logs.String())
				}
			}
			if mode == "registration" && apiCalls != 0 {
				t.Fatal("registration drift reached publisher")
			}
		})
	}
}
