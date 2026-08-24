package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"

	"agent-forge/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type pluginRequest struct {
	Version string `json:"version"`
	Input   string `json:"input"`
}
type pluginResponse struct {
	Version string `json:"version"`
	Result  string `json:"result"`
}

func Run(ctx context.Context, gateURL, workerID, token, pluginPath string) error {
	h := http.Header{"Authorization": []string{"Bearer " + token}}
	c, _, err := websocket.Dial(ctx, gateURL+"/v1/workers/connect?worker_id="+workerID, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return err
	}
	defer c.Close(websocket.StatusNormalClosure, "worker stopping")
	for {
		var m protocol.Message
		if err := wsjson.Read(ctx, c, &m); err != nil {
			return err
		}
		if m.Type != "lease" {
			continue
		}
		result, err := invoke(ctx, pluginPath, m.Input)
		if err != nil {
			return fmt.Errorf("plugin: %w", err)
		}
		if err := wsjson.Write(ctx, c, protocol.Message{Type: "result", JobID: m.JobID, AttemptID: m.AttemptID, Result: result}); err != nil {
			return err
		}
		var ack protocol.Message
		if err := wsjson.Read(ctx, c, &ack); err != nil {
			return err
		}
		if ack.Type != "ack" {
			return fmt.Errorf("gate rejected result: %s", ack.Error)
		}
	}
}
func invoke(parent context.Context, path, input string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	body, err := json.Marshal(pluginRequest{Version: "v1", Input: input})
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, path)
	cmd.Stdin = bytes.NewReader(append(body, '\n'))
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: 1 << 20}
	cmd.Stderr = &limitedWriter{w: io.Discard, n: 1 << 20}
	if err := cmd.Run(); err != nil {
		return "", err
	}
	var response pluginResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		return "", err
	}
	if response.Version != "v1" {
		return "", fmt.Errorf("unsupported plugin response version %q", response.Version)
	}
	return response.Result, nil
}

type limitedWriter struct {
	w io.Writer
	n int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, fmt.Errorf("plugin output exceeds limit")
	}
	n, err := l.w.Write(p)
	l.n -= int64(n)
	return n, err
}
