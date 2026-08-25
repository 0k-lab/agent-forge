package worker

import (
	"context"
	"fmt"
	"time"

	"agent-forge/internal/protocol"
)

func invoke(parent context.Context, path string, request pluginRequest) (string, error) {
	return invokeLocal(parent, []string{path}, request, 15*time.Minute, 1<<20, pluginEnvironment())
}

func executeCodingTask(ctx context.Context, pluginPath string, roots []string, jobID, attemptID string, task protocol.CodingTask) (string, error) {
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
	repository, err := allowedRepository(task.Repository, roots)
	if err != nil {
		return codingOutcome{err: invalidTask(err), evidence: []protocol.AttemptEvidence{newEvidence(task.BaseSHA, protocol.EvidencePhasePreparation, protocol.EvidenceReasonInvalidRepository)}, cleanup: func() *protocol.AttemptEvidence { return nil }}
	}
	environment := pluginEnvironment()
	settings := codingSettings{pluginArgv: []string{pluginPath}, repository: repository, pluginEnvironment: environment, checkEnvironment: environment, pluginTimeout: 15 * time.Minute, checkTimeout: 10 * time.Minute, gitTimeout: 10 * time.Minute, cleanupTimeout: 10 * time.Second, pluginOutput: 1 << 20, checkOutput: protocol.MaxEvidenceOutputBytes, gitOutput: 1 << 20}
	return executeCodingOutcomeSettings(ctx, settings, jobID, attemptID, task, runCheck)
}

func runScopedCheck(parent context.Context, worktree string, env, argv []string) scopedCheckResult {
	return runScopedCheckLocal(parent, worktree, env, argv, 10*time.Minute, protocol.MaxEvidenceOutputBytes)
}

func cleanupCallback(repository, worktree, runtimeDir, baseSHA string, candidate func() string) func() *protocol.AttemptEvidence {
	settings := codingSettings{cleanupTimeout: 10 * time.Second, gitTimeout: 10 * time.Second, gitOutput: 1 << 20}
	return cleanupCallbackSettings(repository, worktree, runtimeDir, baseSHA, candidate, settings)
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
