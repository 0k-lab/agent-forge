package gate

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
	"github.com/coder/websocket"
)

func TestValidateTaskRejectsInvalidCommitAuthorsWithoutEchoingValues(t *testing.T) {
	valid := protocol.CodingTask{
		Repository:  "/repo",
		BaseSHA:     strings.Repeat("a", 40),
		Instruction: "edit",
		Tests:       [][]string{{"true"}},
	}
	tests := []struct {
		name  string
		value string
		set   func(*protocol.CodingTask, string)
	}{
		{"name only", "kricha", func(task *protocol.CodingTask, value string) { task.CommitAuthorName = value }},
		{"email only", "kricha@example.com", func(task *protocol.CodingTask, value string) { task.CommitAuthorEmail = value }},
		{"oversized name", strings.Repeat("n", 257), func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"oversized email", strings.Repeat("e", 255), func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"name newline", "kricha\nInjected", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"email carriage return", "kricha@example.com\rInjected", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"name control", "k\x00richa", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"name angle bracket", "kricha <admin>", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"email header form", "kricha <kricha@example.com>", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"name header style", "Author: kricha", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"malformed email", "not-an-email", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"name leading whitespace", " kricha", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"name trailing unicode whitespace", "kricha\u2003", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"email leading unicode whitespace", "\u2003kricha@example.com", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"email trailing whitespace", "kricha@example.com ", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := valid
			tt.set(&task, tt.value)
			err := validateTask(task)
			if err == nil {
				t.Fatal("invalid commit author accepted")
			}
			if len(err.Error()) > 64 || strings.Contains(err.Error(), tt.value) {
				t.Fatalf("unsafe validation error %q", err)
			}
		})
	}
}

func TestValidateTaskAcceptsAbsentOrPairedCommitAuthor(t *testing.T) {
	for _, author := range [][2]string{{}, {"kricha", "4619899+kricha@users.noreply.github.com"}} {
		task := protocol.CodingTask{
			Repository:        "/repo",
			BaseSHA:           strings.Repeat("a", 40),
			Instruction:       "edit",
			Tests:             [][]string{{"true"}},
			CommitAuthorName:  author[0],
			CommitAuthorEmail: author[1],
		}
		if err := validateTask(task); err != nil {
			t.Fatalf("author %q <%s> rejected: %v", author[0], author[1], err)
		}
	}
}

func TestWorkerWebSocketRequiresMatchingBearerToken(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(NewHandler(s, map[string]string{"worker-secret": "worker-1"}, "owner-secret"))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/workers/connect?worker_id=worker-1"

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c, _, err := websocket.Dial(ctx, url, nil); err == nil {
		c.CloseNow()
		t.Fatal("missing token accepted")
	}
	h := http.Header{"Authorization": []string{"Bearer wrong"}}
	if c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h}); err == nil {
		c.CloseNow()
		t.Fatal("wrong token accepted")
	}
	h.Set("Authorization", "Bearer worker-secret")
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "done")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if worker, err := s.Worker("worker-1"); err == nil && !worker.Connected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker still connected after WebSocket close")
}

func TestOwnerHTTPAPIRequiresDistinctBearerToken(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(NewHandler(s, map[string]string{"worker-secret": "worker-1"}, "owner-secret"))
	defer ts.Close()

	for _, token := range []string{"", "wrong", "worker-secret"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/jobs", bytes.NewBufferString(`{"input":"hello"}`))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d, want 401", token, res.StatusCode)
		}
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/jobs", bytes.NewBufferString(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer owner-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("owner token status = %d, want 201", res.StatusCode)
	}
}

func TestOwnerHTTPRoutesFailClosedWithoutConfiguredToken(t *testing.T) {
	for _, ownerToken := range []string{"", "worker-secret"} {
		s, err := store.Open(filepath.Join(t.TempDir(), "forge.db"))
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(NewHandler(s, map[string]string{"worker-secret": "worker-1"}, ownerToken))
		for _, path := range []string{"/v1/jobs/missing", "/v1/jobs/missing/events", "/v1/workers/missing"} {
			req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer worker-secret")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("owner token %q, %s status = %d, want 401", ownerToken, path, res.StatusCode)
			}
		}
		ts.Close()
		s.Close()
	}
}
