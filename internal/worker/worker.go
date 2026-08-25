package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent-forge/internal/configjson"
	"agent-forge/internal/pluginprotocol"
	"agent-forge/internal/processtree"
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

type codingSettings struct {
	pluginArgv                           []string
	repository                           string
	worktreeRoot, runtimeRoot            string
	pluginEnvironment, checkEnvironment  []string
	pluginTimeout, checkTimeout          time.Duration
	gitTimeout, cleanupTimeout           time.Duration
	pluginOutput, checkOutput, gitOutput int64
}

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

func RunConfigured(ctx context.Context, config Config) error {
	options := WorkerOptions{HeartbeatInterval: config.HeartbeatInterval}
	if err := options.Validate(); err != nil {
		return err
	}
	var lanes sync.WaitGroup
	for slot := 0; slot < config.Concurrency; slot++ {
		lanes.Add(1)
		go func(slot int) {
			defer lanes.Done()
			superviseSlot(ctx, func() error {
				return runWithOutcomeExecutorSlot(ctx, config.GateURL, config.ID, config.token, slot, options, func(ctx context.Context, message protocol.Message) leaseOutcome {
					return executeConfiguredOutcome(ctx, config, message)
				})
			})
		}(slot)
	}
	lanes.Wait()
	return ctx.Err()
}

func superviseSlot(ctx context.Context, connect func() error) {
	for backoff := 100 * time.Millisecond; ctx.Err() == nil; backoff = min(2*backoff, 5*time.Second) {
		_ = connect()
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func runWithExecutor(ctx context.Context, gateURL, workerID, token string, options WorkerOptions, execute leaseExecutor) error {
	return runWithOutcomeExecutor(ctx, gateURL, workerID, token, options, func(ctx context.Context, m protocol.Message) leaseOutcome {
		result, candidateSHA, err := execute(ctx, m)
		return leaseOutcome{result: result, candidateSHA: candidateSHA, err: err}
	})
}

func runWithOutcomeExecutor(ctx context.Context, gateURL, workerID, token string, options WorkerOptions, execute outcomeExecutor) error {
	return runWithOutcomeExecutorSlot(ctx, gateURL, workerID, token, -1, options, execute)
}

func runWithOutcomeExecutorSlot(ctx context.Context, gateURL, workerID, token string, slot int, options WorkerOptions, execute outcomeExecutor) error {
	if err := options.Validate(); err != nil {
		return err
	}
	h := http.Header{"Authorization": []string{"Bearer " + token}}
	endpoint, err := url.Parse(gateURL)
	if err != nil {
		return err
	}
	endpoint.Path, endpoint.RawPath, endpoint.Fragment = "/v1/workers/connect", "", ""
	query := url.Values{"worker_id": []string{workerID}}
	if slot >= 0 {
		query.Set("slot", strconv.Itoa(slot))
	}
	endpoint.RawQuery = query.Encode()
	c, _, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(protocol.MaxWorkerMessageBytes)
	var writeMu sync.Mutex
	for {
		var m protocol.Message
		if err := readGateMessage(ctx, c, &m); err != nil {
			return err
		}
		if m.Type != protocol.MessageLease || slot >= 0 && m.Policy == nil {
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
				if ack.Type != protocol.MessageAck || ack.JobID != m.JobID || ack.AttemptID != m.AttemptID || ack.WorkerID != "" || ack.Input != "" || ack.Task != nil || ack.Policy != nil || ack.Result != "" || ack.CandidateSHA != "" || ack.Error != "" || ack.Disposition != "" || len(ack.Evidence) != 0 {
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
	return configjson.Decode(body, message)
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
func invokeLocal(parent context.Context, argv []string, request pluginRequest, timeout time.Duration, outputBytes int64, environment []string) (string, error) {
	result, err := invokeLocalResult(parent, argv, request, timeout, outputBytes, environment)
	return result.Output, err
}

func invokeLocalResult(parent context.Context, argv []string, request pluginRequest, timeout time.Duration, outputBytes int64, environment []string) (pluginprotocol.Result, error) {
	operation := pluginprotocol.Text
	protocolRequest := pluginprotocol.Request{Operation: operation, Input: request.Input}
	var capabilities []pluginprotocol.Capability
	if request.Workspace != "" {
		operation = pluginprotocol.WorkspaceEdit
		protocolRequest = pluginprotocol.Request{Operation: operation, Workspace: request.Workspace, Instruction: request.Instruction, TimeoutMS: timeout.Milliseconds()}
		capabilities = []pluginprotocol.Capability{pluginprotocol.Progress, pluginprotocol.Cancel, pluginprotocol.CommitSubject}
	}
	result, err := pluginprotocol.Run(parent, argv, protocolRequest, pluginprotocol.Options{Timeout: timeout, OutputBytes: outputBytes, Capabilities: capabilities, Environment: environment})
	if err != nil {
		reason := protocol.EvidenceReasonPluginProtocolFailed
		if errors.Is(err, pluginprotocol.ErrStart) {
			reason = protocol.EvidenceReasonPluginStartFailed
		} else if errors.Is(err, pluginprotocol.ErrOperation) {
			reason = protocol.EvidenceReasonPluginReportedFailure
		}
		return pluginprotocol.Result{}, pluginFailure{reason}
	}
	return result, nil
}

func executeCodingOutcomeSettings(ctx context.Context, settings codingSettings, jobID, attemptID string, task protocol.CodingTask, runCheck scopedCheckRunner) codingOutcome {
	noCleanup := func() *protocol.AttemptEvidence { return nil }
	fail := func(phase, reason string, err error) codingOutcome {
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, phase, reason)}, cleanup: noCleanup}
	}
	if err := protocol.ValidateCommitAuthor(task.CommitAuthorName, task.CommitAuthorEmail); err != nil {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(err))
	}
	if task.Instruction == "" || len(task.Instruction) > pluginprotocol.MaxTextBytes || !utf8.ValidString(task.Instruction) || len(task.Tests) == 0 || len(task.Tests) > 32 || !fixedLowerHex(task.BaseSHA, 40) {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(errors.New("invalid coding task")))
	}
	for _, argv := range task.Tests {
		if len(argv) == 0 || len(argv) > 64 || argv[0] == "" {
			return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(errors.New("invalid coding task")))
		}
		for _, argument := range argv {
			if len(argument) > 4096 || !utf8.ValidString(argument) || strings.IndexByte(argument, 0) >= 0 {
				return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(errors.New("invalid coding task")))
			}
		}
	}
	repository := settings.repository
	if repository == "" {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidRepository, invalidTask(errors.New("repository not allowed")))
	}
	ref, err := candidateRef(jobID, attemptID)
	if err != nil {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidTask, invalidTask(err))
	}
	runtimeDir, err := os.MkdirTemp(settings.runtimeRoot, "forge-runtime-")
	if err != nil {
		return fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonRuntimeSetupFailed, err)
	}
	cleanup := cleanupCallbackSettings(repository, "", runtimeDir, task.BaseSHA, func() string { return "" }, settings)
	for _, name := range []string{"home", "tmp", "cache"} {
		if err := os.Mkdir(filepath.Join(runtimeDir, name), 0o700); err != nil {
			out := fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonRuntimeSetupFailed, err)
			out.cleanup = cleanup
			return out
		}
	}
	testEnv := make([]string, 0, len(settings.checkEnvironment)+3)
	for _, value := range settings.checkEnvironment {
		if !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "TMPDIR=") && !strings.HasPrefix(value, "XDG_CACHE_HOME=") {
			testEnv = append(testEnv, value)
		}
	}
	testEnv = append(testEnv,
		"HOME="+filepath.Join(runtimeDir, "home"),
		"TMPDIR="+filepath.Join(runtimeDir, "tmp"),
		"XDG_CACHE_HOME="+filepath.Join(runtimeDir, "cache"),
	)
	pluginEnv := make([]string, 0, len(settings.pluginEnvironment)+1)
	for _, value := range settings.pluginEnvironment {
		if !strings.HasPrefix(value, "TMPDIR=") {
			pluginEnv = append(pluginEnv, value)
		}
	}
	pluginEnv = append(pluginEnv, "TMPDIR="+filepath.Join(runtimeDir, "tmp"))
	worktree, err := os.MkdirTemp(settings.worktreeRoot, "forge-worktree-")
	if err != nil {
		out := fail(protocol.EvidencePhasePreparation, protocol.EvidenceReasonWorktreeSetupFailed, err)
		out.cleanup = cleanup
		return out
	}
	candidate := ""
	cleanup = cleanupCallbackSettings(repository, worktree, runtimeDir, task.BaseSHA, func() string { return candidate }, settings)
	if err := os.Remove(worktree); err != nil {
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhasePreparation, protocol.EvidenceReasonWorktreeSetupFailed)}, cleanup: cleanup}
	}
	if err := gitCommandLimited(ctx, repository, settings.gitTimeout, settings.gitOutput, nil, "worktree", "add", "--detach", worktree, task.BaseSHA); err != nil {
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhasePreparation, protocol.EvidenceReasonWorktreeSetupFailed)}, cleanup: cleanup}
	}
	base, err := gitOutputLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, "rev-parse", "HEAD")
	if err != nil || base != task.BaseSHA {
		return codingOutcome{err: errors.New("worktree base mismatch"), evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhasePreparation, protocol.EvidenceReasonWorktreeSetupFailed)}, cleanup: cleanup}
	}
	pluginStarted := time.Now()
	pluginResult, err := invokeLocalResult(ctx, settings.pluginArgv, pluginRequest{Version: "v1", Workspace: worktree, Instruction: task.Instruction}, settings.pluginTimeout, settings.pluginOutput, pluginEnv)
	if err != nil {
		reason := protocol.EvidenceReasonPluginProtocolFailed
		var failure pluginFailure
		if errors.As(err, &failure) {
			reason = failure.reason
		}
		record := newEvidence(task.BaseSHA, protocol.EvidencePhasePlugin, reason)
		record.DurationMS = boundedDurationMS(time.Since(pluginStarted), settings.pluginTimeout)
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{record}, cleanup: cleanup}
	}
	commitSubject := "chore: apply coding task"
	if pluginResult.CommitSubject != nil {
		commitSubject = *pluginResult.CommitSubject
	}
	head, headErr := gitOutputLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, "rev-parse", "HEAD")
	if headErr != nil || head != task.BaseSHA {
		return codingOutcome{err: errors.New("invalid workspace"), evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhaseWorkspaceValidation, protocol.EvidenceReasonInvalidWorkspaceChange)}, cleanup: cleanup}
	}
	changes, statusErr := gitOutputLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, "status", "--porcelain")
	if statusErr != nil {
		return codingOutcome{err: statusErr, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhaseWorkspaceValidation, protocol.EvidenceReasonInvalidWorkspaceChange)}, cleanup: cleanup}
	}
	if changes == "" {
		return codingOutcome{err: errors.New("no changes"), evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhaseWorkspaceValidation, protocol.EvidenceReasonNoChanges)}, cleanup: cleanup}
	}
	if err := gitCommandLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, nil, "add", "-A"); err != nil {
		return codingOutcome{err: err, evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhaseWorkspaceValidation, protocol.EvidenceReasonInvalidWorkspaceChange)}, cleanup: cleanup}
	}
	recordedTree, err := gitOutputLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, "write-tree")
	if err != nil || !fixedLowerHex(recordedTree, 40) {
		return codingOutcome{err: errors.New("invalid candidate tree"), evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhaseWorkspaceValidation, protocol.EvidenceReasonInvalidWorkspaceChange)}, cleanup: cleanup}
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
		record.DurationMS = boundedDurationMS(result.duration, settings.checkTimeout)
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
	postCheckHead, err := gitOutputLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, "rev-parse", "HEAD")
	postCheckTree, treeErr := gitOutputLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, "write-tree")
	untracked, untrackedErr := gitOutputLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, "ls-files", "--others", "--exclude-standard")
	worktreeErr := gitCommandLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, nil, "diff", "--quiet")
	if err != nil || treeErr != nil || untrackedErr != nil || worktreeErr != nil || postCheckHead != task.BaseSHA || postCheckTree != recordedTree || untracked != "" {
		return codingOutcome{err: errors.New("checks mutated candidate"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
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
	sha, err := gitOutputLimitedEnv(ctx, worktree, settings.gitTimeout, settings.gitOutput, identityEnv, "commit-tree", recordedTree, "-p", task.BaseSHA, "-m", commitSubject)
	if err != nil {
		return codingOutcome{err: err, evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	candidate = sha
	candidateTree, err := gitOutputLimited(ctx, repository, settings.gitTimeout, settings.gitOutput, "rev-parse", sha+"^{tree}")
	if err != nil || candidateTree != recordedTree {
		return codingOutcome{err: errors.New("candidate tree mismatch"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	parent, err := gitOutputLimited(ctx, worktree, settings.gitTimeout, settings.gitOutput, "rev-parse", sha+"^")
	if err != nil || parent != task.BaseSHA {
		return codingOutcome{err: errors.New("candidate parent mismatch"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	if err := gitCommandLimited(ctx, repository, settings.gitTimeout, settings.gitOutput, nil, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return codingOutcome{err: errors.New("candidate object missing"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	if err := gitCommandLimited(ctx, repository, settings.gitTimeout, settings.gitOutput, nil, "update-ref", ref, sha, ""); err != nil {
		return codingOutcome{err: errors.New("candidate ref conflict"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	retained, err := gitOutputLimited(ctx, repository, settings.gitTimeout, settings.gitOutput, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || retained != sha {
		return codingOutcome{err: errors.New("candidate ref mismatch"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	parent, err = gitOutputLimited(ctx, repository, settings.gitTimeout, settings.gitOutput, "rev-parse", retained+"^")
	if err != nil || parent != task.BaseSHA {
		return codingOutcome{err: errors.New("retained candidate parent mismatch"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
	}
	retainedTree, err := gitOutputLimited(ctx, repository, settings.gitTimeout, settings.gitOutput, "rev-parse", retained+"^{tree}")
	if err != nil || retainedTree != recordedTree {
		return codingOutcome{err: errors.New("retained candidate tree mismatch"), evidence: append(evidence, candidateFailure()), cleanup: cleanup}
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

func runScopedCheckLocal(parent context.Context, worktree string, env, argv []string, timeout time.Duration, outputBytes int64) scopedCheckResult {
	started := time.Now()
	executable, err := resolveCheckExecutable(worktree, env, argv[0])
	if err != nil {
		return scopedCheckResult{duration: time.Since(started), err: err}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	capture := &boundedCapture{limit: int(outputBytes)}
	cmd := exec.Command(executable, argv[1:]...)
	cmd.Dir = worktree
	cmd.Env = env
	cmd.Stdout = capture
	cmd.Stderr = capture
	err = processtree.Run(ctx, cmd)
	output, redacted, truncated := capture.safeOutput()
	result := scopedCheckResult{output: output, redacted: redacted, truncated: truncated, duration: time.Since(started), timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), err: err}
	if cmd.ProcessState != nil {
		if exitCode := cmd.ProcessState.ExitCode(); exitCode >= 0 && exitCode <= 255 {
			result.exitCode = &exitCode
		}
	}
	return result
}

func resolveCheckExecutable(worktree string, env []string, name string) (string, error) {
	fail := func() (string, error) { return "", errors.New("scoped check executable unavailable") }
	if filepath.IsAbs(name) {
		return name, nil
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return filepath.Join(worktree, name), nil
	}
	var pathValue string
	found := false
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			pathValue, found = strings.TrimPrefix(entry, "PATH="), true
			break
		}
	}
	if !found {
		return fail()
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(worktree, directory)
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return fail()
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

func cleanupCallbackSettings(repository, worktree, runtimeDir, baseSHA string, candidate func() string, settings codingSettings) func() *protocol.AttemptEvidence {
	var once sync.Once
	var record *protocol.AttemptEvidence
	return func() *protocol.AttemptEvidence {
		once.Do(func() {
			started := time.Now()
			cleanupCtx, cancel := context.WithTimeout(context.Background(), settings.cleanupTimeout)
			defer cancel()
			var err error
			if worktree != "" {
				err = gitCommandLimited(cleanupCtx, repository, settings.gitTimeout, settings.gitOutput, nil, "worktree", "remove", "--force", worktree)
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
				value.DurationMS = boundedDurationMS(time.Since(started), settings.cleanupTimeout)
				record = &value
			}
		})
		return record
	}
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

func gitCommandLimited(parent context.Context, dir string, timeout time.Duration, outputBytes int64, env []string, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append([]string{"PATH=" + environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin")}, env...)
	budget := &outputBudget{n: outputBytes}
	cmd.Stdout = &limitedWriter{w: io.Discard, budget: budget}
	cmd.Stderr = &limitedWriter{w: io.Discard, budget: budget}
	return processtree.Run(ctx, cmd)
}

func gitOutputLimited(parent context.Context, dir string, timeout time.Duration, outputBytes int64, args ...string) (string, error) {
	return gitOutputLimitedEnv(parent, dir, timeout, outputBytes, nil, args...)
}

func gitOutputLimitedEnv(parent context.Context, dir string, timeout time.Duration, outputBytes int64, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append([]string{"PATH=" + environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin")}, env...)
	var out bytes.Buffer
	budget := &outputBudget{n: outputBytes}
	cmd.Stdout = &limitedWriter{w: &out, budget: budget}
	cmd.Stderr = &limitedWriter{w: io.Discard, budget: budget}
	err := processtree.Run(ctx, cmd)
	return strings.TrimSpace(out.String()), err
}

type limitedWriter struct {
	w      io.Writer
	budget *outputBudget
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	if int64(len(p)) > l.budget.n {
		return 0, fmt.Errorf("plugin output exceeds limit")
	}
	n, err := l.w.Write(p)
	l.budget.n -= int64(n)
	return n, err
}

type outputBudget struct {
	mu sync.Mutex
	n  int64
}
