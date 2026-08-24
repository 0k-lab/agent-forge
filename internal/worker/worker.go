package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"agent-forge/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type pluginRequest struct {
	Version     string `json:"version"`
	Input       string `json:"input,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	Instruction string `json:"instruction,omitempty"`
}
type pluginResponse struct {
	Version string `json:"version"`
	Result  string `json:"result"`
	Error   string `json:"error,omitempty"`
}

var errScopedTest = errors.New("scoped test failed")

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
		var result, candidateSHA string
		if m.Task == nil {
			result, err = invoke(ctx, pluginPath, pluginRequest{Version: "v1", Input: m.Input})
		} else {
			candidateSHA, err = executeCodingTask(ctx, pluginPath, *m.Task)
		}
		failure := ""
		if errors.Is(err, errScopedTest) {
			failure = "scoped_test_failed"
		} else if err != nil {
			failure = "execution_failed"
		}
		if err := wsjson.Write(ctx, c, protocol.Message{Type: "result", JobID: m.JobID, AttemptID: m.AttemptID, Result: result, CandidateSHA: candidateSHA, Error: failure}); err != nil {
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
func invoke(parent context.Context, path string, request pluginRequest) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	body, err := json.Marshal(request)
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
	if response.Error != "" {
		return "", errors.New(response.Error)
	}
	return response.Result, nil
}

func executeCodingTask(ctx context.Context, pluginPath string, task protocol.CodingTask) (sha string, err error) {
	if len(task.Tests) == 0 {
		return "", fmt.Errorf("scoped tests required")
	}
	worktree, err := os.MkdirTemp("", "forge-worktree-")
	if err != nil {
		return "", err
	}
	if err := os.Remove(worktree); err != nil {
		return "", err
	}
	defer func() {
		_ = gitCommand(context.Background(), task.Repository, "worktree", "remove", "--force", worktree)
		_ = os.RemoveAll(worktree)
	}()
	if err := gitCommand(ctx, task.Repository, "worktree", "add", "--detach", worktree, task.BaseSHA); err != nil {
		return "", err
	}
	base, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil || base != task.BaseSHA {
		return "", fmt.Errorf("worktree base mismatch")
	}
	if _, err := invoke(ctx, pluginPath, pluginRequest{Version: "v1", Workspace: worktree, Instruction: task.Instruction}); err != nil {
		return "", err
	}
	for _, argv := range task.Tests {
		if len(argv) == 0 {
			return "", fmt.Errorf("empty test command")
		}
		commandCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		cmd := exec.CommandContext(commandCtx, argv[0], argv[1:]...)
		cmd.Dir = worktree
		cmd.Stdout = &limitedWriter{w: io.Discard, n: 1 << 20}
		cmd.Stderr = &limitedWriter{w: io.Discard, n: 1 << 20}
		err := cmd.Run()
		cancel()
		if err != nil {
			return "", errScopedTest
		}
	}
	if err := gitCommand(ctx, worktree, "add", "-A"); err != nil {
		return "", err
	}
	if err := gitCommand(ctx, worktree, "-c", "user.name=Agent Forge", "-c", "user.email=forge@example.invalid", "commit", "-m", "chore: apply coding task"); err != nil {
		return "", err
	}
	sha, err = gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	parent, err := gitOutput(ctx, worktree, "rev-parse", sha+"^")
	if err != nil || parent != task.BaseSHA {
		return "", fmt.Errorf("candidate parent mismatch")
	}
	if err := gitCommand(ctx, task.Repository, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return "", fmt.Errorf("candidate object missing")
	}
	return sha, nil
}

func gitCommand(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout = &limitedWriter{w: io.Discard, n: 1 << 20}
	cmd.Stderr = &limitedWriter{w: io.Discard, n: 1 << 20}
	return cmd.Run()
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: 1 << 20}
	cmd.Stderr = &limitedWriter{w: io.Discard, n: 1 << 20}
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
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
