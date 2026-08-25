package worker

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"sync"
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

type pluginFailure struct{ reason string }

func (e pluginFailure) Error() string { return "plugin failed" }

type codingOutcome struct {
	candidateSHA string
	err          error
	evidence     []protocol.AttemptEvidence
	cleanup      func() *protocol.AttemptEvidence
}

type scopedCheckResult struct {
	output    string
	redacted  bool
	truncated bool
	exitCode  *int
	duration  time.Duration
	timedOut  bool
	err       error
}

type scopedCheckRunner func(context.Context, string, []string, []string) scopedCheckResult

var (
	errInvalidTask = errors.New("invalid task")
	errScopedTest  = errors.New("scoped test failed")
)

type WorkerOptions struct {
	HeartbeatInterval time.Duration
}

func DefaultWorkerOptions() WorkerOptions { return WorkerOptions{HeartbeatInterval: 10 * time.Second} }

func (o WorkerOptions) Validate() error {
	if o.HeartbeatInterval <= 0 || o.HeartbeatInterval > time.Hour {
		return errors.New("heartbeat interval must be positive and at most 1h")
	}
	return nil
}

type invalidTaskError struct{ error }

func (invalidTaskError) Is(target error) bool { return target == errInvalidTask }

func invalidTask(err error) error { return invalidTaskError{err} }

type leaseExecutor func(context.Context, protocol.Message) (string, string, error)

type leaseOutcome struct {
	result       string
	candidateSHA string
	err          error
	evidence     []protocol.AttemptEvidence
	cleanup      func() *protocol.AttemptEvidence
}

type outcomeExecutor func(context.Context, protocol.Message) leaseOutcome

func Run(ctx context.Context, gateURL, workerID, token, pluginPath string, repositoryRoots []string) error {
	return RunWithOptions(ctx, gateURL, workerID, token, pluginPath, repositoryRoots, DefaultWorkerOptions())
}

func RunWithOptions(ctx context.Context, gateURL, workerID, token, pluginPath string, repositoryRoots []string, options WorkerOptions) error {
	if err := options.Validate(); err != nil {
		return err
	}
	roots, err := canonicalRepositoryRoots(repositoryRoots)
	if err != nil {
		return err
	}
	return runWithOutcomeExecutor(ctx, gateURL, workerID, token, options, func(ctx context.Context, m protocol.Message) leaseOutcome {
		if m.Task == nil {
			result, err := invoke(ctx, pluginPath, pluginRequest{Version: "v1", Input: m.Input})
			return leaseOutcome{result: result, err: err}
		}
		outcome := executeCodingOutcome(ctx, pluginPath, roots, m.JobID, m.AttemptID, *m.Task)
		return leaseOutcome{candidateSHA: outcome.candidateSHA, err: outcome.err, evidence: outcome.evidence, cleanup: outcome.cleanup}
	})
}

func runWithExecutor(ctx context.Context, gateURL, workerID, token string, options WorkerOptions, execute leaseExecutor) error {
	return runWithOutcomeExecutor(ctx, gateURL, workerID, token, options, func(ctx context.Context, m protocol.Message) leaseOutcome {
		result, candidateSHA, err := execute(ctx, m)
		return leaseOutcome{result: result, candidateSHA: candidateSHA, err: err}
	})
}

func runWithOutcomeExecutor(ctx context.Context, gateURL, workerID, token string, options WorkerOptions, execute outcomeExecutor) error {
	if err := options.Validate(); err != nil {
		return err
	}
	h := http.Header{"Authorization": []string{"Bearer " + token}}
	c, _, err := websocket.Dial(ctx, gateURL+"/v1/workers/connect?worker_id="+workerID, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return err
	}
	defer c.Close(websocket.StatusNormalClosure, "worker stopping")
	c.SetReadLimit(protocol.MaxWorkerMessageBytes)
	var writeMu sync.Mutex
	for {
		var m protocol.Message
		if err := readGateMessage(ctx, c, &m); err != nil {
			return err
		}
		if m.Type != protocol.MessageLease {
			return errors.New("unexpected Gate message")
		}
		taskCtx, cancelTask := context.WithCancel(ctx)
		stopHeartbeat := make(chan struct{})
		heartbeatDone := make(chan struct{})
		heartbeatErr := make(chan error, 1)
		go func() {
			defer close(heartbeatDone)
			ticker := time.NewTicker(options.HeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stopHeartbeat:
					return
				case <-ticker.C:
					writeMu.Lock()
					err := wsjson.Write(taskCtx, c, protocol.Message{Type: protocol.MessageHeartbeat, JobID: m.JobID, AttemptID: m.AttemptID, WorkerID: workerID})
					writeMu.Unlock()
					if err != nil {
						heartbeatErr <- err
						cancelTask()
						return
					}
				}
			}
		}()
		outcome := execute(taskCtx, m)
		failed := true
		func() {
			defer func() {
				if failed && outcome.cleanup != nil {
					_ = outcome.cleanup()
				}
				close(stopHeartbeat)
				<-heartbeatDone
				cancelTask()
			}()
			send := func(message protocol.Message) error {
				writeMu.Lock()
				err := wsjson.Write(taskCtx, c, message)
				writeMu.Unlock()
				return err
			}
			readACK := func() error {
				var ack protocol.Message
				if err := readGateMessage(taskCtx, c, &ack); err != nil {
					return err
				}
				if ack.Type != protocol.MessageAck || ack.JobID != m.JobID || ack.AttemptID != m.AttemptID || ack.WorkerID != "" || ack.Input != "" || ack.Task != nil || ack.Result != "" || ack.CandidateSHA != "" || ack.Error != "" || ack.Disposition != "" || len(ack.Evidence) != 0 {
					return errors.New("invalid Gate ACK")
				}
				return nil
			}
			bind := func(records []protocol.AttemptEvidence) error {
				if len(records) == 0 {
					return nil
				}
				if err := send(protocol.Message{Type: protocol.MessageEvidence, JobID: m.JobID, AttemptID: m.AttemptID, Evidence: records}); err != nil {
					return err
				}
				return readACK()
			}
			if err = bind(outcome.evidence); err != nil {
				return
			}
			if outcome.cleanup != nil {
				if cleanupEvidence := outcome.cleanup(); cleanupEvidence != nil {
					if err = bind([]protocol.AttemptEvidence{*cleanupEvidence}); err != nil {
						return
					}
				}
			}
			failure, disposition := classifyFailure(outcome.err)
			if err = send(protocol.Message{Type: protocol.MessageResult, JobID: m.JobID, AttemptID: m.AttemptID, Result: outcome.result, CandidateSHA: outcome.candidateSHA, Error: failure, Disposition: disposition}); err != nil {
				return
			}
			if err = readACK(); err != nil {
				return
			}
			failed = false
		}()
		if err != nil {
			return err
		}
		select {
		case heartbeatErr := <-heartbeatErr:
			return heartbeatErr
		default:
		}
	}
}

func readGateMessage(ctx context.Context, c *websocket.Conn, message *protocol.Message) error {
	typ, body, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("Gate message must be text")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(message); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("Gate message has trailing data")
	}
	return nil
}

func classifyFailure(err error) (code, disposition string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, errInvalidTask):
		return protocol.FailureInvalidTask, protocol.DispositionTerminal
	case errors.Is(err, errScopedTest):
		return protocol.FailureScopedTest, protocol.DispositionTerminal
	default:
		return protocol.FailureExecution, protocol.DispositionRetryable
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
		var startErr *exec.Error
		var pathErr *os.PathError
		if errors.As(err, &startErr) || errors.As(err, &pathErr) {
			return "", pluginFailure{protocol.EvidenceReasonPluginStartFailed}
		}
		return "", pluginFailure{protocol.EvidenceReasonPluginProtocolFailed}
	}
	var response pluginResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		return "", pluginFailure{protocol.EvidenceReasonPluginProtocolFailed}
	}
	if response.Version != "v1" {
		return "", pluginFailure{protocol.EvidenceReasonPluginProtocolFailed}
	}
	if response.Error != "" {
		return "", pluginFailure{protocol.EvidenceReasonPluginReportedFailure}
	}
	return response.Result, nil
}

func executeCodingTask(ctx context.Context, pluginPath string, roots []string, jobID, attemptID string, task protocol.CodingTask) (sha string, err error) {
	outcome := executeCodingOutcome(ctx, pluginPath, roots, jobID, attemptID, task)
	if outcome.cleanup != nil {
		_ = outcome.cleanup()
	}
	return outcome.candidateSHA, outcome.err
}

func executeCodingOutcome(ctx context.Context, pluginPath string, roots []string, jobID, attemptID string, task protocol.CodingTask) codingOutcome {
	return executeCodingOutcomeWithRunner(ctx, pluginPath, roots, jobID, attemptID, task, runScopedCheck)
}

func executeCodingOutcomeWithRunner(ctx context.Context, pluginPath string, roots []string, jobID, attemptID string, task protocol.CodingTask, runCheck scopedCheckRunner) codingOutcome {
	noCleanup := func() *protocol.AttemptEvidence { return nil }
	fail := func(phase, reason string, err error) codingOutcome {
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, phase, reason)}, cleanup: noCleanup}
	}
	if err := protocol.ValidateCommitAuthor(task.CommitAuthorName, task.CommitAuthorEmail); err != nil {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(err))
	}
	if len(task.Tests) == 0 || len(task.Tests) > 32 || !fixedLowerHex(task.BaseSHA, 40) {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(errors.New("invalid coding task")))
	}
	for _, argv := range task.Tests {
		if len(argv) == 0 || len(argv) > 64 {
			return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(errors.New("invalid coding task")))
		}
	}
	repository, err := allowedRepository(task.Repository, roots)
	if err != nil {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidRepository, invalidTask(err))
	}
	ref, err := candidateRef(jobID, attemptID)
	if err != nil {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(err))
	}
	runtimeDir, err := os.MkdirTemp("", "forge-runtime-")
	if err != nil {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonRuntimeSetupFailed, err)
	}
	cleanup := cleanupCallback(repository, "", runtimeDir, task.BaseSHA, func() string { return "" })
	for _, name := range []string{"home", "tmp", "cache"} {
		if err := os.Mkdir(filepath.Join(runtimeDir, name), 0o700); err != nil {
			out := fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonRuntimeSetupFailed, err)
			out.cleanup = cleanup
			return out
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
		out := fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonWorktreeSetupFailed, err)
		out.cleanup = cleanup
		return out
	}
	candidate := ""
	cleanup = cleanupCallback(repository, worktree, runtimeDir, task.BaseSHA, func() string { return candidate })
	if err := os.Remove(worktree); err != nil {
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhasePreparation, protocol.EvidenceReasonWorktreeSetupFailed)}, cleanup: cleanup}
	}
	if err := gitCommand(ctx, repository, "worktree", "add", "--detach", worktree, task.BaseSHA); err != nil {
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhasePreparation, protocol.EvidenceReasonWorktreeSetupFailed)}, cleanup: cleanup}
	}
	base, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil || base != task.BaseSHA {
		return codingOutcome{err: errors.New("worktree base mismatch"), evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhasePreparation, protocol.EvidenceReasonWorktreeSetupFailed)}, cleanup: cleanup}
	}
	pluginStarted := time.Now()
	if _, err := invoke(ctx, pluginPath, pluginRequest{Version: "v1", Workspace: worktree, Instruction: task.Instruction}); err != nil {
		reason := protocol.EvidenceReasonPluginProtocolFailed
		var failure pluginFailure
		if errors.As(err, &failure) {
			reason = failure.reason
		}
		record := newEvidence(task.BaseSHA, protocol.EvidencePhasePlugin, reason)
		record.DurationMS = boundedDurationMS(time.Since(pluginStarted), 15*time.Minute)
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{record}, cleanup: cleanup}
	}
	head, headErr := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if headErr != nil || head != task.BaseSHA {
		return codingOutcome{err: errors.New("invalid workspace"), evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhaseWorkspaceValidation, protocol.EvidenceReasonInvalidWorkspaceChange)}, cleanup: cleanup}
	}
	changes, statusErr := gitOutput(ctx, worktree, "status", "--porcelain")
	if statusErr != nil {
		return codingOutcome{err: statusErr, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhaseWorkspaceValidation, protocol.EvidenceReasonInvalidWorkspaceChange)}, cleanup: cleanup}
	}
	if changes == "" {
		return codingOutcome{err: errors.New("no changes"), evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhaseWorkspaceValidation, protocol.EvidenceReasonNoChanges)}, cleanup: cleanup}
	}
	var evidence []protocol.AttemptEvidence
	candidateFailure := func() protocol.AttemptEvidence {
		record := newEvidence(task.BaseSHA, protocol.EvidencePhaseCandidateCommit, protocol.EvidenceReasonCandidateCommitFailed)
		record.CandidateSHA = candidate
		return record
	}
	for index, argv := range task.Tests {
		result := runCheck(ctx, worktree, testEnv, argv)
		record := newEvidence(task.BaseSHA, protocol.EvidencePhaseScopedCheck, protocol.EvidenceReasonScopedCheckPassed)
		record.CheckIndex = &index
		record.DurationMS = boundedDurationMS(result.duration, 10*time.Minute)
		record.Output = result.output
		record.OutputRedacted = result.redacted
		record.OutputTruncated = result.truncated
		record.ExitCode = result.exitCode
		if result.err != nil {
			record.Reason = protocol.EvidenceReasonScopedCheckFailed
			if result.timedOut {
				record.Reason = protocol.EvidenceReasonScopedCheckTimeout
			}
			evidence = append(evidence, record)
			return codingOutcome{err: errScopedTest, evidence: evidence, cleanup: cleanup}
		}
		evidence = append(evidence, record)
	}
	if err := gitCommand(ctx, worktree, "add", "-A"); err != nil {
		return codingOutcome{err: err, evidence: append(evidence, candidateFailure()), cleanup: cleanup}
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
		return codingOutcome{err: err, evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	sha, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return codingOutcome{err: err, evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	candidate = sha
	parent, err := gitOutput(ctx, worktree, "rev-parse", sha+"^")
	if err != nil || parent != task.BaseSHA {
		return codingOutcome{err: errors.New("candidate parent mismatch"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	if err := gitCommand(ctx, repository, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return codingOutcome{err: errors.New("candidate object missing"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	if err := gitCommand(ctx, repository, "update-ref", ref, sha, ""); err != nil {
		return codingOutcome{err: errors.New("candidate ref conflict"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	retained, err := gitOutput(ctx, repository, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || retained != sha {
		return codingOutcome{err: errors.New("candidate ref mismatch"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	parent, err = gitOutput(ctx, repository, "rev-parse", retained+"^")
	if err != nil || parent != task.BaseSHA {
		return codingOutcome{err: errors.New("retained candidate parent mismatch"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	for i := range evidence {
		evidence[i].CandidateSHA = sha
	}
	return codingOutcome{candidateSHA: sha, evidence: evidence, cleanup: cleanup}
}

func boundedDurationMS(duration, limit time.Duration) int64 {
	return max(time.Duration(0), min(duration, limit)).Milliseconds()
}

func fixedLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func runScopedCheck(parent context.Context, worktree string, env, argv []string) scopedCheckResult {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	capture := &boundedCapture{limit: protocol.MaxEvidenceOutputBytes}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = worktree
	cmd.Env = env
	cmd.Stdout = capture
	cmd.Stderr = capture
	err := cmd.Run()
	output, redacted, truncated := capture.safeOutput()
	result := scopedCheckResult{output: output, redacted: redacted, truncated: truncated, duration: time.Since(started), timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), err: err}
	if cmd.ProcessState != nil {
		if exitCode := cmd.ProcessState.ExitCode(); exitCode >= 0 && exitCode <= 255 {
			result.exitCode = &exitCode
		}
	}
	return result
}

type boundedCapture struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (c *boundedCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.limit - len(c.buf)
	if remaining < len(p) {
		c.truncated = true
	}
	if remaining > 0 {
		c.buf = append(c.buf, p[:min(remaining, len(p))]...)
	}
	return len(p), nil
}

func (c *boundedCapture) safeOutput() (string, bool, bool) {
	c.mu.Lock()
	nonempty, truncated := len(c.buf) != 0, c.truncated
	c.mu.Unlock()
	if !nonempty {
		return "", false, truncated
	}
	return protocol.EvidenceRedactedMarker, true, truncated
}

func newEvidence(baseSHA, phase, reason string) protocol.AttemptEvidence {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return protocol.AttemptEvidence{EvidenceID: hex.EncodeToString(id[:]), Phase: phase, Reason: reason, BaseSHA: baseSHA}
}

func cleanupCallback(repository, worktree, runtimeDir, baseSHA string, candidate func() string) func() *protocol.AttemptEvidence {
	var once sync.Once
	var record *protocol.AttemptEvidence
	return func() *protocol.AttemptEvidence {
		once.Do(func() {
			started := time.Now()
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			if worktree != "" {
				err = gitCommand(cleanupCtx, repository, "worktree", "remove", "--force", worktree)
				if removeErr := os.RemoveAll(worktree); err == nil {
					err = removeErr
				}
			}
			if removeErr := os.RemoveAll(runtimeDir); err == nil {
				err = removeErr
			}
			if err != nil {
				value := newEvidence(baseSHA, protocol.EvidencePhaseCleanup, protocol.EvidenceReasonCleanupFailed)
				value.CandidateSHA = candidate()
				value.DurationMS = boundedDurationMS(time.Since(started), 10*time.Second)
				record = &value
			}
		})
		return record
	}
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
