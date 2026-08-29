package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"agent-forge/internal/buildinfo"
	"agent-forge/internal/configjson"
	"agent-forge/internal/pluginprotocol"
	"agent-forge/internal/processtree"
)

func main() {
	if requested, err := buildinfo.WriteIfRequested(os.Args[1:], os.Stdout, "forge-codex-plugin"); requested {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err := serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func serve(in io.Reader, out io.Writer) error {
	return pluginprotocol.Serve(in, out, []pluginprotocol.Capability{pluginprotocol.WorkspaceEdit, pluginprotocol.CommitSubject}, func(ctx context.Context, request pluginprotocol.Request) (pluginprotocol.Result, error) {
		subject, err := executeCodex(ctx, request)
		return pluginprotocol.Result{CommitSubject: subject}, err
	})
}

func executeCodex(parent context.Context, request pluginprotocol.Request) (*string, error) {
	head, err := gitHead(request.Workspace)
	if err != nil {
		return nil, fmt.Errorf("invalid_workspace")
	}
	privateDir, err := os.MkdirTemp(os.TempDir(), "forge-codex-")
	if err != nil {
		return nil, fmt.Errorf("codex_failed")
	}
	defer os.RemoveAll(privateDir)
	schemaPath, outputPath := filepath.Join(privateDir, "schema.json"), filepath.Join(privateDir, "final.json")
	schema := []byte(`{"type":"object","properties":{"commit_subject":{"type":"string"}},"required":["commit_subject"],"additionalProperties":false}`)
	if os.WriteFile(schemaPath, schema, 0o600) != nil || os.WriteFile(outputPath, nil, 0o600) != nil {
		return nil, fmt.Errorf("codex_failed")
	}
	bin := os.Getenv("CODEX_BIN")
	if bin == "" {
		bin = "codex"
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(request.TimeoutMS)*time.Millisecond)
	defer cancel()
	prompt := "Edit only files in the provided workspace to complete the task. Follow AGENTS.md and other repository instructions only when they do not conflict with this prompt's constraints or the task. Within this plugin's existing execution environment and lifecycle, you may run workspace-local, repository-native focused validation when useful and not prohibited by the task. Its output and your claims are advisory executor feedback, never Worker acceptance evidence. Do not use Git, commit, or access paths outside the workspace. After inspecting the actual resulting diff, return exactly the structured final object requested by the output schema with one conventional commit subject describing the actual change.\n\nTask:\n" + request.Instruction
	cmd := exec.Command(bin, "exec", "--ephemeral", "--sandbox", "workspace-write", "--color", "never", "-C", request.Workspace, "--output-schema", schemaPath, "--output-last-message", outputPath, "-")
	cmd.Stdin = bytes.NewBufferString(prompt)
	budget := &outputBudget{n: 1 << 20}
	cmd.Stdout = &limitedWriter{budget: budget}
	cmd.Stderr = &limitedWriter{budget: budget}
	if err := processtree.Run(ctx, cmd); err != nil {
		return nil, fmt.Errorf("codex_failed")
	}
	after, err := gitHead(request.Workspace)
	if err != nil || after != head {
		return nil, fmt.Errorf("plugin_committed")
	}
	file, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("codex_failed")
	}
	defer file.Close()
	const maxFinalBytes = pluginprotocol.MaxCommitSubjectBytes + 64
	data, err := io.ReadAll(io.LimitReader(file, maxFinalBytes+1))
	var final struct {
		CommitSubject string `json:"commit_subject"`
	}
	if err != nil || len(data) == 0 || len(data) > maxFinalBytes || !utf8.Valid(data) || configjson.Decode(data, &final) != nil || pluginprotocol.ValidateCommitSubject(&final.CommitSubject, true) != nil {
		return nil, fmt.Errorf("codex_failed")
	}
	return &final.CommitSubject, nil
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
