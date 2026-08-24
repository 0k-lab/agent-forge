package gate

import (
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
	ts := httptest.NewServer(NewHandler(s, map[string]string{"secret": "worker-1"}))
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
	h.Set("Authorization", "Bearer secret")
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "done")
}
