package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
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
	cmd := exec.CommandContext(ctx, bin, "exec", "--ephemeral", "--sandbox", "workspace-write", "--color", "never", "-C", r.Workspace, "-")
	cmd.Stdin = bytes.NewBufferString(prompt)
	cmd.Stdout = &limitedWriter{n: 1 << 20}
	cmd.Stderr = &limitedWriter{n: 1 << 20}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex_failed")
	}
	after, err := gitHead(r.Workspace)
	if err != nil || after != head {
		return fmt.Errorf("plugin_committed")
	}
	return nil
}

func gitHead(workspace string) (string, error) {
	returnOutput, err := exec.Command("git", "-C", workspace, "rev-parse", "HEAD").Output()
	return string(bytes.TrimSpace(returnOutput)), err
}

type limitedWriter struct{ n int64 }

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, fmt.Errorf("output_limit")
	}
	l.n -= int64(len(p))
	return len(p), nil
}
