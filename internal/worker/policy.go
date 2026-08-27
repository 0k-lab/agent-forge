package worker

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"agent-forge/internal/processtree"
	"agent-forge/internal/protocol"
)

type localLease struct {
	repository        string
	pluginArgv        []string
	worktreeRoot      string
	runtimeRoot       string
	pluginEnvironment []string
	checkEnvironment  []string
	policy            protocol.ResolvedPolicy
}

func resolveLease(config Config, message protocol.Message) (localLease, error) {
	return resolveLeaseContext(context.Background(), config, message)
}

func resolveLeaseContext(parent context.Context, config Config, message protocol.Message) (localLease, error) {
	fail := func() (localLease, error) { return localLease{}, errors.New("lease policy rejected") }
	if message.Type != protocol.MessageLease || message.Policy == nil || message.Policy.Validate() != nil {
		return fail()
	}
	policy := *message.Policy
	if config.HeartbeatInterval >= time.Duration(policy.LeaseTTLNanos) {
		return fail()
	}
	e := policy.Execution
	ceilings := config.Ceilings
	if e.PluginTimeoutNanos > int64(ceilings.PluginTimeout) || e.CheckTimeoutNanos > int64(ceilings.CheckTimeout) || e.GitTimeoutNanos > int64(ceilings.GitTimeout) || e.CleanupTimeoutNanos > int64(ceilings.CleanupTimeout) || e.PluginOutputBytes > ceilings.PluginOutputBytes || e.CheckOutputBytes > ceilings.CheckOutputBytes || e.GitOutputBytes > ceilings.GitOutputBytes {
		return fail()
	}
	allowedEnvironment := map[string]bool{}
	for _, name := range config.EnvironmentAllowlist {
		allowedEnvironment[name] = true
	}
	allowedChecks := map[string]bool{}
	for _, name := range config.CheckEnvironmentAllowlist {
		allowedChecks[name] = true
	}
	pluginEnvironment := make([]string, 0, len(e.Environment))
	checkEnvironment := make([]string, 0, len(e.Environment))
	for _, name := range e.Environment {
		if !allowedEnvironment[name] {
			return fail()
		}
		if value, ok := environmentLookup(name); ok {
			entry := name + "=" + value
			pluginEnvironment = append(pluginEnvironment, entry)
			if allowedChecks[name] {
				checkEnvironment = append(checkEnvironment, entry)
			}
		}
	}
	var pluginArgv []string
	for _, registration := range config.Plugins {
		if registration.ID == e.PluginID {
			pluginArgv = append([]string(nil), registration.Argv...)
			break
		}
	}
	if len(pluginArgv) == 0 {
		return fail()
	}
	resolved := localLease{pluginArgv: pluginArgv, worktreeRoot: config.WorktreeRoot, runtimeRoot: config.RuntimeRoot, pluginEnvironment: pluginEnvironment, checkEnvironment: checkEnvironment, policy: policy}
	for _, root := range []*string{&resolved.worktreeRoot, &resolved.runtimeRoot} {
		canonical, err := canonicalDirectory(*root)
		if err != nil || canonical != *root {
			return fail()
		}
	}
	if message.Task == nil {
		if e.RepositoryID != "" || e.DefaultBranch != "" {
			return fail()
		}
		return resolved, nil
	}
	if message.Task.RepositoryID == "" || message.Task.RepositoryID != e.RepositoryID || protocol.ValidateBaseSHA(message.Task.BaseSHA) != nil {
		return fail()
	}
	repository := message.Task.Repository
	if repository != "" {
		canonical, err := canonicalDirectory(repository)
		if err != nil || canonical != repository {
			return fail()
		}
		repository = canonical
	} else {
		for _, registration := range config.Repositories {
			if registration.ID == message.Task.RepositoryID {
				canonical, err := allowedRepository(registration.Path, config.RepositoryRoots)
				if err != nil || canonical != registration.Path {
					return fail()
				}
				repository = canonical
				break
			}
		}
	}
	if repository == "" {
		return fail()
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(e.GitTimeoutNanos))
	defer cancel()
	branch := "refs/heads/" + e.DefaultBranch
	if gitQuiet(ctx, repository, e.GitOutputBytes, "cat-file", "-e", message.Task.BaseSHA+"^{commit}") != nil || gitQuiet(ctx, repository, e.GitOutputBytes, "show-ref", "--verify", "--quiet", branch) != nil || gitQuiet(ctx, repository, e.GitOutputBytes, "merge-base", "--is-ancestor", message.Task.BaseSHA, branch) != nil {
		return fail()
	}
	resolved.repository = repository
	return resolved, nil
}

func executeConfiguredOutcome(ctx context.Context, config Config, message protocol.Message) leaseOutcome {
	resolved, err := resolveLeaseContext(ctx, config, message)
	if err != nil {
		return leaseOutcome{err: invalidTask(err)}
	}
	if message.Task == nil {
		result, err := invokeLocal(ctx, resolved.pluginArgv, pluginRequest{Version: "v1", Input: message.Input}, time.Duration(resolved.policy.Execution.PluginTimeoutNanos), resolved.policy.Execution.PluginOutputBytes, resolved.pluginEnvironment)
		return leaseOutcome{result: result, err: err}
	}
	task := *message.Task
	e := resolved.policy.Execution
	settings := codingSettings{
		pluginArgv: resolved.pluginArgv, repository: resolved.repository, worktreeRoot: resolved.worktreeRoot, runtimeRoot: resolved.runtimeRoot, pluginEnvironment: resolved.pluginEnvironment, checkEnvironment: resolved.checkEnvironment,
		pluginTimeout: time.Duration(e.PluginTimeoutNanos), checkTimeout: time.Duration(e.CheckTimeoutNanos), gitTimeout: time.Duration(e.GitTimeoutNanos), cleanupTimeout: time.Duration(e.CleanupTimeoutNanos),
		pluginOutput: e.PluginOutputBytes, checkOutput: e.CheckOutputBytes, gitOutput: e.GitOutputBytes,
	}
	runCheck := func(ctx context.Context, worktree string, environment, argv []string) scopedCheckResult {
		return runScopedCheckLocal(ctx, worktree, environment, argv, settings.checkTimeout, settings.checkOutput)
	}
	outcome := executeCodingOutcomeSettings(ctx, settings, message.JobID, message.AttemptID, task, runCheck)
	return leaseOutcome{candidateSHA: outcome.candidateSHA, err: outcome.err, evidence: outcome.evidence, cleanup: outcome.cleanup}
}

var environmentLookup = func(name string) (string, bool) {
	return lookupEnv(name)
}

func gitQuiet(ctx context.Context, repository string, outputLimit int64, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", repository}, args...)...)
	cmd.Env = []string{"PATH=" + environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin")}
	budget := &outputBudget{n: outputLimit}
	cmd.Stdout = &limitedWriter{w: discardWriter{}, budget: budget}
	cmd.Stderr = &limitedWriter{w: discardWriter{}, budget: budget}
	return processtree.Run(ctx, cmd)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
