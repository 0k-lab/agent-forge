package worker

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

func Run(ctx context.Context, gateURL, workerID, token, pluginPath string, repositoryRoots []string) error {
	roots, err := canonicalRepositoryRoots(repositoryRoots)
	if err != nil {
		return err
	}
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
			candidateSHA, err = executeCodingTask(ctx, pluginPath, roots, m.JobID, m.AttemptID, *m.Task)
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
	cmd.Env = pluginEnvironment()
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

func executeCodingTask(ctx context.Context, pluginPath string, roots []string, jobID, attemptID string, task protocol.CodingTask) (sha string, err error) {
	if err := protocol.ValidateCommitAuthor(task.CommitAuthorName, task.CommitAuthorEmail); err != nil {
		return "", err
	}
	repository, err := allowedRepository(task.Repository, roots)
	if err != nil {
		return "", err
	}
	ref, err := candidateRef(jobID, attemptID)
	if err != nil {
		return "", err
	}
	if len(task.Tests) == 0 {
		return "", fmt.Errorf("scoped tests required")
	}
	runtimeDir, err := os.MkdirTemp("", "forge-runtime-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(runtimeDir)
	for _, name := range []string{"home", "tmp", "cache"} {
		if err := os.Mkdir(filepath.Join(runtimeDir, name), 0o700); err != nil {
			return "", err
		}
	}
	testEnv := []string{
		"PATH=" + environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"HOME=" + filepath.Join(runtimeDir, "home"),
		"TMPDIR=" + filepath.Join(runtimeDir, "tmp"),
		"XDG_CACHE_HOME=" + filepath.Join(runtimeDir, "cache"),
	}
	worktree, err := os.MkdirTemp("", "forge-worktree-")
	if err != nil {
		return "", err
	}
	if err := os.Remove(worktree); err != nil {
		return "", err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = gitCommand(cleanupCtx, repository, "worktree", "remove", "--force", worktree)
		_ = os.RemoveAll(worktree)
	}()
	if err := gitCommand(ctx, repository, "worktree", "add", "--detach", worktree, task.BaseSHA); err != nil {
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
		cmd.Env = testEnv
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
	authorName, authorEmail := task.CommitAuthorName, task.CommitAuthorEmail
	if authorName == "" && authorEmail == "" {
		authorName, authorEmail = "Agent Forge", "forge@example.invalid"
	}
	identityEnv := []string{
		"GIT_AUTHOR_NAME=" + authorName,
		"GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=Agent Forge",
		"GIT_COMMITTER_EMAIL=forge@example.invalid",
	}
	if err := gitCommandEnv(ctx, worktree, identityEnv, "commit", "-m", "chore: apply coding task"); err != nil {
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
	if err := gitCommand(ctx, repository, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return "", fmt.Errorf("candidate object missing")
	}
	if err := gitCommand(ctx, repository, "update-ref", ref, sha, ""); err != nil {
		return "", fmt.Errorf("candidate ref conflict")
	}
	retained, err := gitOutput(ctx, repository, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || retained != sha {
		return "", fmt.Errorf("candidate ref mismatch")
	}
	parent, err = gitOutput(ctx, repository, "rev-parse", retained+"^")
	if err != nil || parent != task.BaseSHA {
		return "", fmt.Errorf("retained candidate parent mismatch")
	}
	return sha, nil
}

func canonicalRepositoryRoots(roots []string) ([]string, error) {
	canonical := make([]string, 0, len(roots))
	for _, root := range roots {
		resolved, err := canonicalDirectory(root)
		if err != nil {
			return nil, fmt.Errorf("invalid repository root")
		}
		canonical = append(canonical, resolved)
	}
	return canonical, nil
}

func allowedRepository(repository string, roots []string) (string, error) {
	if len(roots) == 0 {
		return "", fmt.Errorf("repository not allowed")
	}
	resolved, err := canonicalDirectory(repository)
	if err != nil {
		return "", fmt.Errorf("repository not allowed")
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("repository not allowed")
}

func canonicalDirectory(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return filepath.Clean(resolved), nil
}

func candidateRef(jobID, attemptID string) (string, error) {
	for _, id := range []string{jobID, attemptID} {
		if len(id) != 32 {
			return "", fmt.Errorf("invalid candidate identity")
		}
		if _, err := hex.DecodeString(id); err != nil {
			return "", fmt.Errorf("invalid candidate identity")
		}
	}
	return "refs/agent-forge/candidates/" + jobID + "/" + attemptID, nil
}

func pluginEnvironment() []string {
	env := []string{"PATH=" + environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin")}
	for _, name := range []string{"HOME", "CODEX_HOME", "CODEX_BIN", "TMPDIR"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func environmentValue(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func gitCommand(ctx context.Context, dir string, args ...string) error {
	return gitCommandEnv(ctx, dir, nil, args...)
}

func gitCommandEnv(ctx context.Context, dir string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append([]string{"PATH=" + environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin")}, env...)
	cmd.Stdout = &limitedWriter{w: io.Discard, n: 1 << 20}
	cmd.Stderr = &limitedWriter{w: io.Discard, n: 1 << 20}
	return cmd.Run()
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin")}
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
