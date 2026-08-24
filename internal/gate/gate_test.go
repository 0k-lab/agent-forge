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

	"agent-forge/internal/store"
	"github.com/coder/websocket"
)

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
