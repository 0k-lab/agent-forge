package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/pluginprotocol"
	"agent-forge/internal/protocol"
)

func TestCodingOutcomeClassifiesPluginStartFailure(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	outcome := executeCodingOutcome(context.Background(), filepath.Join(t.TempDir(), "missing-plugin"), []string{repo}, strings.Repeat("1", 32), strings.Repeat("2", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
	})
	defer outcome.cleanup()
	if outcome.candidateSHA != "" || outcome.err == nil || len(outcome.evidence) != 1 {
		t.Fatalf("outcome = %#v", outcome)
	}
	record := outcome.evidence[0]
	if record.Phase != protocol.EvidencePhasePlugin || record.Reason != protocol.EvidenceReasonPluginStartFailed || record.BaseSHA != base || record.Output != "" {
		t.Fatalf("plugin start evidence = %#v", record)
	}
	if raw, err := hex.DecodeString(record.EvidenceID); err != nil || len(raw) != 16 || record.EvidenceID != strings.ToLower(record.EvidenceID) {
		t.Fatalf("evidence ID = %q, %v", record.EvidenceID, err)
	}
}

func TestCodingOutcomeClassifiesPluginReportedFailureWithoutRawError(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, workspaceFailurePython("execution_failed"))
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("3", 32), strings.Repeat("4", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "private prompt", Tests: [][]string{{"true"}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 1 || outcome.evidence[0].Reason != protocol.EvidenceReasonPluginReportedFailure {
		t.Fatalf("plugin response evidence = %#v", outcome.evidence)
	}
	if strings.Contains(outcome.evidence[0].Output, "unknown private plugin detail") {
		t.Fatalf("plugin error leaked: %#v", outcome.evidence[0])
	}
}

func TestCodingOutcomeDetectsNoChangesBeforeScopedChecks(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 99\n")
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, workspacePluginPython("pass", ""))
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("5", 32), strings.Repeat("6", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"./check-env"}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 1 || outcome.evidence[0].Phase != protocol.EvidencePhaseWorkspaceValidation || outcome.evidence[0].Reason != protocol.EvidenceReasonNoChanges {
		t.Fatalf("no-change evidence = %#v", outcome.evidence)
	}
}

func TestCodingOutcomeDetectsPluginMutatedHEAD(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	plugin := filepath.Join(t.TempDir(), "plugin")
	// Use a plugin that reads its request once, edits, and commits HEAD itself.
	write(t, plugin, workspacePluginPython(`
pathlib.Path(request["workspace"], "answer.txt").write_text("plugin commit\n")
subprocess.run(["git","-C",request["workspace"],"add","-A"])
subprocess.run(["git","-C",request["workspace"],"-c","user.name=Plugin","-c","user.email=plugin@example.invalid","commit","-qm","bad"])`, ""))
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("7", 32), strings.Repeat("8", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 1 || outcome.evidence[0].Reason != protocol.EvidenceReasonInvalidWorkspaceChange {
		t.Fatalf("mutated HEAD evidence = %#v", outcome.evidence)
	}
}

func TestCodingOutcomeRetainsOrderedChecksThroughLaterFailure(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("9", 32), strings.Repeat("a", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{
			{"sh", "-c", "printf first-pass"},
			{"sh", "-c", "printf second-out; printf second-err >&2; exit 7"},
			{"sh", "-c", "exit 99"},
		},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 2 {
		t.Fatalf("check evidence = %#v", outcome.evidence)
	}
	first, second := outcome.evidence[0], outcome.evidence[1]
	if first.CheckIndex == nil || *first.CheckIndex != 0 || first.Reason != protocol.EvidenceReasonScopedCheckPassed || first.ExitCode == nil || *first.ExitCode != 0 || first.Output != protocol.EvidenceRedactedMarker || !first.OutputRedacted {
		t.Fatalf("first check = %#v", first)
	}
	if second.CheckIndex == nil || *second.CheckIndex != 1 || second.Reason != protocol.EvidenceReasonScopedCheckFailed || second.ExitCode == nil || *second.ExitCode != 7 || second.Output != protocol.EvidenceRedactedMarker || !second.OutputRedacted {
		t.Fatalf("second check = %#v", second)
	}
	if first.EvidenceID == second.EvidenceID || len(first.Argv) != 0 || len(second.Argv) != 0 {
		t.Fatalf("worker supplied argv or reused ID: %#v", outcome.evidence)
	}
}

func TestCodingOutcomeBoundsAndSanitizesFailedCheckOutputWithoutChangingExit(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	command := "import os,sys; print(os.getcwd()); print('Authorization: Bearer " + secret + "'); sys.stderr.write('X'*5000); sys.exit(9)"
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("b", 32), strings.Repeat("c", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"python3", "-c", command}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 1 {
		t.Fatalf("check evidence = %#v", outcome.evidence)
	}
	record := outcome.evidence[0]
	if record.ExitCode == nil || *record.ExitCode != 9 || !record.OutputRedacted || !record.OutputTruncated || len(record.Output) > protocol.MaxEvidenceOutputBytes {
		t.Fatalf("bounded failed check = %#v", record)
	}
	if strings.Contains(record.Output, secret) || strings.Contains(record.Output, repo) || strings.Contains(record.Output, "forge-worktree-") {
		t.Fatalf("private output leaked: %q", record.Output)
	}
}

func TestBoundedCapturePreservesRedactionMarkerAtBoundary(t *testing.T) {
	capture := &boundedCapture{limit: protocol.MaxEvidenceOutputBytes}
	raw := append([]byte(strings.Repeat("x", protocol.MaxEvidenceOutputBytes-1)), 0xff)
	if _, err := capture.Write(raw); err != nil {
		t.Fatal(err)
	}
	output, redacted, truncated := capture.safeOutput()
	if !redacted || truncated || output != protocol.EvidenceRedactedMarker {
		t.Fatalf("output redacted=%v truncated=%v len=%d tail=%q", redacted, truncated, len(output), output[max(0, len(output)-32):])
	}
}

func TestBoundedCaptureReplacesEveryNonEmptyOutputWithFixedMarker(t *testing.T) {
	for _, raw := range []string{
		`["Authorization","Bearer reviewer-json-array-secret"]`,
		`file:///private/reviewer/home/config`,
		`arbitrary-unlabeled-reviewer-secret`,
	} {
		t.Run(raw, func(t *testing.T) {
			capture := &boundedCapture{limit: protocol.MaxEvidenceOutputBytes}
			_, _ = capture.Write([]byte(raw))
			output, redacted, truncated := capture.safeOutput()
			if output != protocol.EvidenceRedactedMarker || !redacted || truncated {
				t.Fatalf("output=%q redacted=%v truncated=%v", output, redacted, truncated)
			}
		})
	}
}

func TestBoundedCaptureMarkerPreservesOnlyCaptureState(t *testing.T) {
	tests := []struct {
		name      string
		raw       []byte
		truncated bool
		output    string
		redacted  bool
	}{
		{"truncation before delimiter", []byte(strings.Repeat("x", protocol.MaxEvidenceOutputBytes) + `file:///private/path`), true, protocol.EvidenceRedactedMarker, true},
		{"UTF-8 split", append([]byte(strings.Repeat("x", protocol.MaxEvidenceOutputBytes-1)), 0xe2, 0x82), true, protocol.EvidenceRedactedMarker, true},
		{"no newline", []byte("arbitrary secret without newline"), false, protocol.EvidenceRedactedMarker, true},
		{"empty", nil, false, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capture := &boundedCapture{limit: protocol.MaxEvidenceOutputBytes}
			_, _ = capture.Write(test.raw)
			output, redacted, truncated := capture.safeOutput()
			if output != test.output || redacted != test.redacted || truncated != test.truncated {
				t.Fatalf("output=%q redacted=%v truncated=%v", output, redacted, truncated)
			}
		})
	}
}

func TestBoundedCaptureDropsAllTruncatedContent(t *testing.T) {
	tests := []struct {
		name, raw string
	}{
		{"reviewer userinfo before at", strings.Repeat("x", 1990) + "https://synthetic-user:synthetic-password-abcdefghijklmnopqrstuvwxyz@example.invalid/" + strings.Repeat("y", 2048)},
		{"credential delimiter incomplete", "safe line\n" + strings.Repeat("x", 2028) + "client secre" + strings.Repeat("y", 64)},
		{"UTF-8 boundary", "safe line\n" + strings.Repeat("x", 2037) + "€" + strings.Repeat("y", 64)},
		{"no newline long line", strings.Repeat("safe", 600)},
		{"safe records then unsafe partial", "first safe\nsecond safe\n" + strings.Repeat("x", 1990) + "https://partial-user:partial-password@example.invalid/" + strings.Repeat("y", 64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capture := &boundedCapture{limit: protocol.MaxEvidenceOutputBytes}
			if _, err := capture.Write([]byte(test.raw)); err != nil {
				t.Fatal(err)
			}
			output, redacted, truncated := capture.safeOutput()
			if output != protocol.EvidenceRedactedMarker || !redacted || !truncated {
				t.Fatalf("output redacted=%v truncated=%v len=%d: %q", redacted, truncated, len(output), output)
			}
		})
	}
}

func TestBoundedCaptureMarksLiteralReservedRedactionMarker(t *testing.T) {
	capture := &boundedCapture{limit: protocol.MaxEvidenceOutputBytes}
	if _, err := capture.Write([]byte("[REDACTED]")); err != nil {
		t.Fatal(err)
	}
	output, redacted, truncated := capture.safeOutput()
	if output != "[REDACTED]" || !redacted || truncated {
		t.Fatalf("output=%q redacted=%v truncated=%v", output, redacted, truncated)
	}
}

func TestBoundedCaptureSanitizesClosedPrivacyFormsBeforeEvidenceTransport(t *testing.T) {
	capture := &boundedCapture{limit: protocol.MaxEvidenceOutputBytes}
	raw := "https://synthetic-user:synthetic-pass@example.invalid/api\nCrEdEnTiAl: synthetic-colon-secret\nworkspace: /synthetic/private/alice/file\nconfig: C:\\synthetic\\private\\alice\\file\n" +
		`{"Authorization":"Bearer synthetic JSON secret","safe":"kept"}` + "\n" +
		`{'api_key': 'synthetic single quoted secret', 'safe': 'kept'}` + "\n" +
		"check --token synthetic CLI token --later tail; safe\n[credential]: synthetic bracket credential\nAccess Token: synthetic-access-token\n[Client Secret]: synthetic-client-secret\ncheck --access token synthetic-cli-token; safe\n" +
		`workspace: \\synthetic-server\private\output` + "\n" + `\\standalone-server\private\output`
	if _, err := capture.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	output, redacted, truncated := capture.safeOutput()
	if !redacted || truncated {
		t.Fatalf("output redacted=%v truncated=%v: %q", redacted, truncated, output)
	}
	for _, private := range []string{"synthetic-user", "synthetic-pass", "synthetic-colon-secret", "/synthetic/private/alice/file", `C:\synthetic\private\alice\file`, "JSON secret", "single quoted secret", "CLI token", "later tail", "bracket credential", "synthetic-access-token", "synthetic-client-secret", "synthetic-cli-token", `\\synthetic-server\private\output`, `\\standalone-server\private\output`} {
		if strings.Contains(output, private) {
			t.Fatalf("worker evidence exposed %q: %q", private, output)
		}
	}
}

func TestCodingOutcomeClassifiesFirstScopedCheckFailure(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("d", 32), strings.Repeat("e", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"sh", "-c", "exit 4"}, {"true"}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 1 || outcome.evidence[0].CheckIndex == nil || *outcome.evidence[0].CheckIndex != 0 || outcome.evidence[0].Reason != protocol.EvidenceReasonScopedCheckFailed || outcome.evidence[0].BaseSHA != base || outcome.evidence[0].CandidateSHA != "" {
		t.Fatalf("first failure evidence = %#v", outcome.evidence)
	}
}

func TestCodingOutcomeClassifiesTimeoutWithoutSleeping(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	runs := 0
	runner := func(context.Context, string, []string, []string) scopedCheckResult {
		runs++
		return scopedCheckResult{duration: 11 * time.Minute, timedOut: true, err: context.DeadlineExceeded}
	}
	outcome := executeCodingOutcomeWithRunner(context.Background(), plugin, []string{repo}, strings.Repeat("f", 32), strings.Repeat("0", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"never-runs"}, {"also-never-runs"}},
	}, runner)
	defer outcome.cleanup()
	if runs != 1 || len(outcome.evidence) != 1 || outcome.evidence[0].Reason != protocol.EvidenceReasonScopedCheckTimeout || outcome.evidence[0].ExitCode != nil || outcome.evidence[0].DurationMS != int64((10*time.Minute)/time.Millisecond) {
		t.Fatalf("timeout runs=%d evidence=%#v", runs, outcome.evidence)
	}
}

func TestCodingOutcomeClassifiesCandidateCommitFailure(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	jobID, attemptID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	git(t, repo, "update-ref", "refs/agent-forge/candidates/"+jobID+"/"+attemptID, base)
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, jobID, attemptID, protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 2 || outcome.evidence[0].Reason != protocol.EvidenceReasonScopedCheckPassed || outcome.evidence[0].CandidateSHA != "" || outcome.evidence[1].Phase != protocol.EvidencePhaseCandidateCommit || outcome.evidence[1].Reason != protocol.EvidenceReasonCandidateCommitFailed || !fixedLowerHex(outcome.evidence[1].CandidateSHA, 40) {
		t.Fatalf("candidate failure evidence = %#v", outcome.evidence)
	}
}

func TestCodingOutcomeBindsSuccessfulCandidateToPriorChecks(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("a", 32), strings.Repeat("b", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}, {"sh", "-c", "printf checked"}},
	})
	defer outcome.cleanup()
	if !fixedLowerHex(outcome.candidateSHA, 40) || len(outcome.evidence) != 2 {
		t.Fatalf("outcome = %#v", outcome)
	}
	for _, record := range outcome.evidence {
		if record.BaseSHA != base || record.CandidateSHA != outcome.candidateSHA {
			t.Fatalf("check remained base-bound: %#v", record)
		}
	}
}

func TestCodingOutcomeCleanupIsIdempotentAndLeavesEvidenceInMemory(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	marker := filepath.Join(t.TempDir(), "workspace")
	write(t, plugin, workspacePluginPython(`pathlib.Path(request["workspace"], "answer.txt").write_text("candidate\n"); pathlib.Path(`+strconv.Quote(marker)+`).write_text(request["workspace"])`, ""))
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("3", 32), strings.Repeat("4", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
	})
	workspaceBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	workspace := string(workspaceBytes)
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("worktree removed before outcome returned: %v", err)
	}
	before := append([]protocol.AttemptEvidence(nil), outcome.evidence...)
	if record := outcome.cleanup(); record != nil {
		t.Fatalf("successful cleanup evidence = %#v", record)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("ephemeral worktree remains: %v", err)
	}
	if record := outcome.cleanup(); record != nil || !reflect.DeepEqual(outcome.evidence, before) {
		t.Fatalf("second cleanup=%#v evidence=%#v", record, outcome.evidence)
	}
}

func TestCleanupFailureRecordIsBoundedAndIdempotent(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	base, candidate := strings.Repeat("a", 40), strings.Repeat("b", 40)
	worktree := filepath.Join(t.TempDir(), "not-a-worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	cleanup := cleanupCallback(repo, worktree, t.TempDir(), base, func() string { return candidate })
	first, second := cleanup(), cleanup()
	if first == nil || first != second || first.Phase != protocol.EvidencePhaseCleanup || first.Reason != protocol.EvidenceReasonCleanupFailed || first.BaseSHA != base || first.CandidateSHA != candidate || first.Output != "" || first.DurationMS > int64((10*time.Second)/time.Millisecond) {
		t.Fatalf("cleanup evidence first=%#v second=%#v", first, second)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("failed git cleanup left worktree directory: %v", err)
	}
}

func TestCodingOutcomeRejectsInvalidTaskBeforePlugin(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	marker := filepath.Join(t.TempDir(), "plugin-ran")
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\n: > \""+marker+"\"\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("5", 32), strings.Repeat("6", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Tests: [][]string{{}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 1 || outcome.evidence[0].Phase != protocol.EvidencePhasePreparation || outcome.evidence[0].Reason != protocol.EvidenceReasonInvalidTask {
		t.Fatalf("invalid task evidence = %#v", outcome.evidence)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid task invoked plugin: %v", err)
	}
}

func TestCodingOutcomeRejectsEmptyTestExecutableBeforePlugin(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	marker := filepath.Join(t.TempDir(), "plugin-ran")
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\n: > \""+marker+"\"\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("5", 32), strings.Repeat("7", 32), protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{""}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 1 || outcome.evidence[0].Reason != protocol.EvidenceReasonInvalidTask {
		t.Fatalf("empty executable evidence = %#v", outcome.evidence)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("empty executable invoked plugin: %v", err)
	}
}

func TestCodingTaskBoundsFailBeforePlugin(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	marker := filepath.Join(t.TempDir(), "plugin-ran")
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\n: > \""+marker+"\"\n")
	if err := os.Chmod(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
	tooManyArgs := make([]string, 65)
	for i := range tooManyArgs {
		tooManyArgs[i] = "x"
	}
	for name, task := range map[string]protocol.CodingTask{
		"instruction": {BaseSHA: base, Instruction: strings.Repeat("x", pluginprotocol.MaxTextBytes+1), Tests: [][]string{{"true"}}},
		"argv count":  {BaseSHA: base, Instruction: "edit", Tests: [][]string{tooManyArgs}},
		"argument":    {BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true", strings.Repeat("x", 4097)}}},
		"author":      {BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}, CommitAuthorName: strings.Repeat("x", 257), CommitAuthorEmail: "author@example.invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			task.Repository = repo
			outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("a", 32), strings.Repeat("b", 32), task)
			defer outcome.cleanup()
			if outcome.err == nil || len(outcome.evidence) != 1 || outcome.evidence[0].Reason != protocol.EvidenceReasonInvalidTask {
				t.Fatalf("outcome = %#v", outcome)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("bounded task invoked plugin: %v", err)
			}
		})
	}
}

func TestSequentialSameLaneFailureDoesNotContaminateNextExecution(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	root, runtimeRoot, state := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "state")
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, `#!/usr/bin/env python3
import json,os,pathlib,sys,time
state=pathlib.Path(os.environ["STATE"])
assert pathlib.Path(os.environ["TMPDIR"]).parent.parent == pathlib.Path(os.environ["RUNTIME_ROOT"])
if not state.exists():
    state.write_text("first")
    time.sleep(2)
    raise SystemExit
initialize=json.loads(sys.stdin.readline())
plugin_id=initialize["id"]
print(json.dumps({"version":"v1","id":plugin_id,"type":"initialized","capabilities":["workspace_edit"]},separators=(",",":")),flush=True)
request=json.loads(sys.stdin.readline())
pathlib.Path(request["workspace"],"answer.txt").write_text("second\n")
print(json.dumps({"version":"v1","id":plugin_id,"type":"result"},separators=(",",":")),flush=True)
`)
	if err := os.Chmod(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := codingSettings{
		pluginArgv: []string{plugin}, repository: repo, worktreeRoot: root, runtimeRoot: runtimeRoot,
		pluginEnvironment: []string{"PATH=" + environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin"), "STATE=" + state, "RUNTIME_ROOT=" + runtimeRoot, "TMPDIR=/unbounded/private/tmp"},
		pluginTimeout:     100 * time.Millisecond, checkTimeout: time.Second, gitTimeout: time.Second, cleanupTimeout: time.Second,
		pluginOutput: 1 << 20, checkOutput: 1024, gitOutput: 1 << 20,
	}
	runCheck := func(context.Context, string, []string, []string) scopedCheckResult { return scopedCheckResult{} }
	firstJob, firstAttempt := strings.Repeat("1", 32), strings.Repeat("2", 32)
	first := executeCodingOutcomeSettings(context.Background(), settings, firstJob, firstAttempt, protocol.CodingTask{BaseSHA: base, Instruction: "first", Tests: [][]string{{"true"}}}, runCheck)
	if first.err == nil || first.candidateSHA != "" || first.cleanup() != nil {
		t.Fatalf("first outcome = %#v", first)
	}
	if exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/agent-forge/candidates/"+firstJob+"/"+firstAttempt).Run() == nil {
		t.Fatal("failed execution retained candidate ref")
	}
	assertEmptyDir(t, root)
	assertEmptyDir(t, runtimeRoot)

	secondJob, secondAttempt := strings.Repeat("3", 32), strings.Repeat("4", 32)
	second := executeCodingOutcomeSettings(context.Background(), settings, secondJob, secondAttempt, protocol.CodingTask{BaseSHA: base, Instruction: "second", Tests: [][]string{{"true"}}}, runCheck)
	if second.err != nil || !fixedLowerHex(second.candidateSHA, 40) {
		t.Fatalf("second outcome = %#v", second)
	}
	if got := git(t, repo, "rev-parse", "refs/agent-forge/candidates/"+secondJob+"/"+secondAttempt); got != second.candidateSHA {
		t.Fatalf("second candidate ref = %q", got)
	}
	if second.cleanup() != nil {
		t.Fatal("second cleanup failed")
	}
	assertEmptyDir(t, root)
	assertEmptyDir(t, runtimeRoot)
}

func assertEmptyDir(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("%s not empty: %v, %v", filepath.Base(path), entries, err)
	}
}

func TestCodingOutcomeClassifiesPluginProtocolFailuresWithoutTranscript(t *testing.T) {
	for name, body := range map[string]string{
		"nonzero":   "#!/bin/sh\nprintf 'Authorization: Bearer plugin-secret' >&2\nexit 7\n",
		"malformed": "#!/bin/sh\nprintf 'private malformed response\\n'\n",
	} {
		t.Run(name, func(t *testing.T) {
			repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
			plugin := filepath.Join(t.TempDir(), "plugin")
			write(t, plugin, body)
			if err := os.Chmod(plugin, 0o755); err != nil {
				t.Fatal(err)
			}
			outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, strings.Repeat("7", 32), strings.Repeat("8", 32), protocol.CodingTask{
				Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
			})
			defer outcome.cleanup()
			if len(outcome.evidence) != 1 || outcome.evidence[0].Reason != protocol.EvidenceReasonPluginProtocolFailed || outcome.evidence[0].Output != "" {
				t.Fatalf("plugin protocol evidence = %#v", outcome.evidence)
			}
		})
	}
}

func TestCodingOutcomeClassifiesInvalidRepositoryDuringPreparation(t *testing.T) {
	base := strings.Repeat("a", 40)
	outcome := executeCodingOutcome(context.Background(), "unused", nil, strings.Repeat("9", 32), strings.Repeat("a", 32), protocol.CodingTask{
		Repository: t.TempDir(), BaseSHA: base, Tests: [][]string{{"true"}},
	})
	defer outcome.cleanup()
	if len(outcome.evidence) != 1 || outcome.evidence[0].Phase != protocol.EvidencePhasePreparation || outcome.evidence[0].Reason != protocol.EvidenceReasonInvalidRepository || outcome.evidence[0].BaseSHA != base {
		t.Fatalf("repository evidence = %#v", outcome.evidence)
	}
}

func TestCodingTaskUsesExactBaseAndCreatesCandidate(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Forge Test")
	git(t, repo, "config", "user.email", "forge@example.invalid")
	write(t, filepath.Join(repo, "answer.txt"), "base\n")
	git(t, repo, "add", "answer.txt")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "answer.txt"), "later\n")
	git(t, repo, "commit", "-qam", "later")

	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, workspacePluginPython(`pathlib.Path(request["workspace"], "answer.txt").write_text("candidate\n")`, "feat: write candidate"))
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}

	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, "11111111111111111111111111111111", "22222222222222222222222222222222", protocol.CodingTask{
		Repository:        repo,
		BaseSHA:           base,
		Instruction:       "write candidate to answer.txt",
		Tests:             [][]string{{"grep", "-qx", "candidate", "answer.txt"}},
		CommitAuthorName:  "kricha",
		CommitAuthorEmail: "4619899+kricha@users.noreply.github.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, repo, "rev-parse", sha+"^"); got != base {
		t.Fatalf("candidate parent = %s, want base %s", got, base)
	}
	if got := git(t, repo, "show", sha+":answer.txt"); got != "candidate" {
		t.Fatalf("candidate content = %q", got)
	}
	if got := git(t, repo, "show", "-s", "--format=%an <%ae>%n%cn <%ce>", sha); got != "kricha <4619899+kricha@users.noreply.github.com>\nAgent Forge <forge@example.invalid>" {
		t.Fatalf("candidate identity = %q", got)
	}
	if got := git(t, repo, "show", "-s", "--format=%s", sha); got != "feat: write candidate" {
		t.Fatalf("candidate subject = %q", got)
	}
	if got := git(t, repo, "status", "--short"); got != "" {
		t.Fatalf("source repository changed: %s", got)
	}
	ref := "refs/agent-forge/candidates/11111111111111111111111111111111/22222222222222222222222222222222"
	if got := git(t, repo, "rev-parse", ref); got != sha {
		t.Fatalf("candidate ref = %s, want %s", got, sha)
	}
	git(t, repo, "reflog", "expire", "--expire=now", "--all")
	git(t, repo, "gc", "--prune=now")
	if got := git(t, repo, "rev-parse", ref); got != sha {
		t.Fatalf("candidate after GC = %s, want %s", got, sha)
	}
}

func TestCodingTaskWithoutCommitAuthorUsesAgentForgeFallback(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccc", protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, repo, "show", "-s", "--format=%an <%ae>%n%cn <%ce>", sha); got != "Agent Forge <forge@example.invalid>\nAgent Forge <forge@example.invalid>" {
		t.Fatalf("fallback identity = %q", got)
	}
}

func TestCodingTaskRejectsInvalidCommitAuthorBeforePluginOrCandidate(t *testing.T) {
	repo, base, _ := codingFixture(t, "#!/bin/sh\nexit 0\n")
	marker := filepath.Join(t.TempDir(), "plugin-invoked")
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, "#!/bin/sh\n: > \""+marker+"\"\nprintf '%s\\n' '{\"version\":\"v1\",\"result\":\"edited\"}'\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	jobID := "dddddddddddddddddddddddddddddddd"
	attemptID := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, jobID, attemptID, protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}, CommitAuthorName: "kricha",
	})
	if err == nil || sha != "" || err.Error() != "invalid commit author" {
		t.Fatalf("invalid author result: sha=%q err=%v", sha, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid author invoked plugin: %v", err)
	}
	if got := git(t, repo, "rev-list", "--all", "--count"); got != "1" {
		t.Fatalf("invalid author created a commit: count=%s", got)
	}
	ref := "refs/agent-forge/candidates/" + jobID + "/" + attemptID
	if err := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", ref).Run(); err == nil {
		t.Fatal("invalid author created a candidate ref")
	}
}

func TestScopedTestFailurePreventsCandidateCreation(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Forge Test")
	git(t, repo, "config", "user.email", "forge@example.invalid")
	write(t, filepath.Join(repo, "answer.txt"), "base\n")
	git(t, repo, "add", "answer.txt")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")

	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, workspacePluginPython(`pathlib.Path(request["workspace"], "answer.txt").write_text("wrong\n")`, ""))
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}

	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, "33333333333333333333333333333333", "44444444444444444444444444444444", protocol.CodingTask{
		Repository:  repo,
		BaseSHA:     base,
		Instruction: "edit",
		Tests:       [][]string{{"grep", "-qx", "expected", "answer.txt"}},
	})
	if err == nil || sha != "" {
		t.Fatalf("failed test produced candidate %q, err=%v", sha, err)
	}
	if got := git(t, repo, "rev-list", "--all", "--count"); got != "1" {
		t.Fatalf("failed test created a commit: count=%s", got)
	}
}

func TestScopedCheckMutationCannotProduceCandidate(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nprintf 'mutated by check\\n' > answer.txt\n")
	jobID, attemptID := strings.Repeat("1", 32), strings.Repeat("f", 32)
	outcome := executeCodingOutcome(context.Background(), plugin, []string{repo}, jobID, attemptID, protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"./check-env"}},
	})
	defer outcome.cleanup()
	if outcome.candidateSHA != "" || outcome.err == nil {
		t.Fatalf("mutating check produced candidate: %#v", outcome)
	}
	ref := "refs/agent-forge/candidates/" + jobID + "/" + attemptID
	if err := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", ref).Run(); err == nil {
		t.Fatal("mutating check retained candidate ref")
	}
}

func TestRepositoryCapabilityAllowsChildAndRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	sibling := root + "-sibling"
	outside := t.TempDir()
	for _, dir := range []string{child, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	roots, err := canonicalRepositoryRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := allowedRepository(child, roots); err != nil || got == "" {
		t.Fatalf("allowed child rejected: got=%q err=%v", got, err)
	}
	for _, repository := range []string{sibling, link} {
		if got, err := allowedRepository(repository, roots); err == nil || got != "" {
			t.Fatalf("escape accepted: got=%q err=%v", got, err)
		}
	}
}

func TestCodingTaskFailsWithoutRepositoryRoots(t *testing.T) {
	sha, err := executeCodingTask(context.Background(), "unused", nil, "55555555555555555555555555555555", "66666666666666666666666666666666", protocol.CodingTask{Repository: t.TempDir(), Tests: [][]string{{"true"}}})
	if err == nil || sha != "" || strings.Contains(err.Error(), string(filepath.Separator)) {
		t.Fatalf("no-root result: sha=%q err=%v", sha, err)
	}
}

func TestCandidateRefConflictIsNotOverwritten(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	jobID := "99999999999999999999999999999999"
	attemptID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := "refs/agent-forge/candidates/" + jobID + "/" + attemptID
	git(t, repo, "update-ref", ref, base)

	sha, err := executeCodingTask(context.Background(), plugin, []string{repo}, jobID, attemptID, protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}},
	})
	if err == nil || sha != "" {
		t.Fatalf("conflicting ref accepted: sha=%q err=%v", sha, err)
	}
	if got := git(t, repo, "rev-parse", ref); got != base {
		t.Fatalf("conflicting ref overwritten with %s", got)
	}
}

func TestScopedTestEnvironmentDoesNotInheritWorkerSecret(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\n[ -z \"${FORGE_FAKE_SECRET+x}\" ]\n")
	t.Setenv("FORGE_FAKE_SECRET", "must-not-leak")
	if _, err := executeCodingTask(context.Background(), plugin, []string{repo}, "77777777777777777777777777777777", "88888888888888888888888888888888", protocol.CodingTask{
		Repository: repo, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"./check-env"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodingSeparatesPluginAndCheckEnvironments(t *testing.T) {
	repo, base, plugin := codingFixture(t, "#!/bin/sh\nexit 0\n")
	write(t, plugin, workspacePluginPython(`
env=__import__("os").environ
assert env["CODEX_HOME"] == "/plugin/home" and env["CODEX_BIN"] == "/plugin/bin" and env["UNLISTED"] == "plugin-only"
pathlib.Path(request["workspace"], "answer.txt").write_text("candidate\n")`, ""))
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	worktrees, runtimeRoot := t.TempDir(), t.TempDir()
	safePath := environmentValue("PATH", "/usr/local/bin:/usr/bin:/bin")
	settings := codingSettings{
		pluginArgv: []string{plugin}, repository: repo, worktreeRoot: worktrees, runtimeRoot: runtimeRoot,
		pluginEnvironment: []string{"PATH=" + safePath, "CODEX_HOME=/plugin/home", "CODEX_BIN=/plugin/bin", "UNLISTED=plugin-only"},
		checkEnvironment:  []string{"PATH=" + safePath},
		pluginTimeout:     time.Second, checkTimeout: time.Second, gitTimeout: time.Second, cleanupTimeout: time.Second,
		pluginOutput: 1 << 20, checkOutput: 1024, gitOutput: 1 << 20,
	}
	runner := func(_ context.Context, _ string, env, _ []string) scopedCheckResult {
		values := map[string]string{}
		for _, entry := range env {
			name, value, _ := strings.Cut(entry, "=")
			values[name] = value
		}
		if values["PATH"] != safePath || values["HOME"] == "" || values["TMPDIR"] == "" || values["XDG_CACHE_HOME"] == "" || values["CODEX_HOME"] != "" || values["CODEX_BIN"] != "" || values["UNLISTED"] != "" {
			return scopedCheckResult{err: errors.New("unsafe check environment")}
		}
		return scopedCheckResult{}
	}
	outcome := executeCodingOutcomeSettings(context.Background(), settings, strings.Repeat("c", 32), strings.Repeat("d", 32), protocol.CodingTask{BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}}, runner)
	defer outcome.cleanup()
	if outcome.err != nil || outcome.candidateSHA == "" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestPluginEnvironmentDoesNotInheritWorkerSecret(t *testing.T) {
	plugin := filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, `#!/usr/bin/env python3
import json,os,sys
assert "FORGE_FAKE_SECRET" not in os.environ
initialize=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":initialize["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":initialize["id"],"type":"result","output":"ok"},separators=(",",":")),flush=True)
`)
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_FAKE_SECRET", "must-not-leak")
	if got, err := invoke(context.Background(), plugin, pluginRequest{Version: "v1", Input: "hello"}); err != nil || got != "ok" {
		t.Fatalf("invoke = %q, %v", got, err)
	}
}

func codingFixture(t *testing.T, check string) (repo, base, plugin string) {
	t.Helper()
	repo = t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Forge Test")
	git(t, repo, "config", "user.email", "forge@example.invalid")
	write(t, filepath.Join(repo, "answer.txt"), "base\n")
	write(t, filepath.Join(repo, "check-env"), check)
	if err := os.Chmod(filepath.Join(repo, "check-env"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base = git(t, repo, "rev-parse", "HEAD")
	plugin = filepath.Join(t.TempDir(), "plugin")
	write(t, plugin, workspacePluginPython(`pathlib.Path(request["workspace"], "answer.txt").write_text("candidate\n")`, ""))
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, base, plugin
}

func workspacePluginPython(action, subject string) string {
	result := `{"version":"v1","id":plugin_id,"type":"result"}`
	if subject != "" {
		result = `{"version":"v1","id":plugin_id,"type":"result","commit_subject":` + strconv.Quote(subject) + `}`
	}
	return "#!/usr/bin/env python3\n" + `import json,pathlib,subprocess,sys
initialize=json.loads(sys.stdin.readline())
plugin_id=initialize["id"]
capabilities=["workspace_edit"]
if "commit_subject" in initialize["capabilities"]:
    capabilities.append("commit_subject")
print(json.dumps({"version":"v1","id":plugin_id,"type":"initialized","capabilities":capabilities},separators=(",",":")),flush=True)
request=json.loads(sys.stdin.readline())
` + action + `
print(json.dumps(` + result + `,separators=(",",":")),flush=True)
`
}

func workspaceFailurePython(category string) string {
	return "#!/usr/bin/env python3\n" + `import json,sys
initialize=json.loads(sys.stdin.readline())
plugin_id=initialize["id"]
print(json.dumps({"version":"v1","id":plugin_id,"type":"initialized","capabilities":["workspace_edit"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":plugin_id,"type":"failure","category":` + strconv.Quote(category) + `},separators=(",",":")),flush=True)
`
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
