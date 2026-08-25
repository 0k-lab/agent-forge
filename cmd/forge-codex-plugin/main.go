package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"agent-forge/internal/processtree"
)

type request struct {
	Version     string `json:"version"`
	Workspace   string `json:"workspace"`
	Instruction string `json:"instruction"`
}

type response struct {
	Version string `json:"version"`
	Result  string `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
}

func main() {
	r := response{Version: "v1"}
	if err := run(); err != nil {
		r.Error = err.Error()
	} else {
		r.Result = "edited"
	}
	_ = json.NewEncoder(os.Stdout).Encode(r)
}

func run() error {
	var r request
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&r); err != nil {
		return fmt.Errorf("invalid_request")
	}
	if r.Version != "v1" || r.Workspace == "" || r.Instruction == "" {
		return fmt.Errorf("invalid_request")
	}
	head, err := gitHead(r.Workspace)
	if err != nil {
		return fmt.Errorf("invalid_workspace")
	}
	bin := os.Getenv("CODEX_BIN")
	if bin == "" {
		bin = "codex"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	prompt := "Edit files in the provided workspace to complete the task. Do not run tests, use git, commit, or access paths outside the workspace.\n\nTask:\n" + r.Instruction
	cmd := exec.Command(bin, "exec", "--ephemeral", "--sandbox", "workspace-write", "--color", "never", "-C", r.Workspace, "-")
	cmd.Stdin = bytes.NewBufferString(prompt)
	budget := &outputBudget{n: 1 << 20}
	cmd.Stdout = &limitedWriter{budget: budget}
	cmd.Stderr = &limitedWriter{budget: budget}
	if err := processtree.Run(ctx, cmd); err != nil {
		return fmt.Errorf("codex_failed")
	}
	after, err := gitHead(r.Workspace)
	if err != nil || after != head {
		return fmt.Errorf("plugin_committed")
	}
	return nil
}

func gitHead(workspace string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.Command("git", "-C", workspace, "rev-parse", "HEAD")
	var returnOutput bytes.Buffer
	budget := &outputBudget{n: 1 << 20}
	cmd.Stdout = &limitedWriter{w: &returnOutput, budget: budget}
	cmd.Stderr = &limitedWriter{budget: budget}
	err := processtree.Run(ctx, cmd)
	return string(bytes.TrimSpace(returnOutput.Bytes())), err
}

type outputBudget struct {
	mu sync.Mutex
	n  int64
}

type limitedWriter struct {
	w      io.Writer
	budget *outputBudget
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	if int64(len(p)) > l.budget.n {
		return 0, fmt.Errorf("output_limit")
	}
	l.budget.n -= int64(len(p))
	if l.w == nil {
		return len(p), nil
	}
	return l.w.Write(p)
}
