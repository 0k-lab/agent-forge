package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateCommitAuthorEmailDomain(t *testing.T) {
	for _, email := range []string{
		"a@-example.com",
		"a@example-.com",
		"a@.example.com",
		"a@example..com",
		"a@example.com.",
		"a@" + strings.Repeat("a", 64) + ".com",
		"a@example_domain.com",
		"a@exämple.com",
	} {
		err := ValidateCommitAuthor("author", email)
		if err == nil {
			t.Errorf("ValidateCommitAuthor accepted %q", email)
		} else if err.Error() != "invalid commit author" || strings.Contains(err.Error(), email) {
			t.Errorf("ValidateCommitAuthor returned non-generic error %q", err)
		}
	}

	for _, email := range []string{
		"4619899+kricha@users.noreply.github.com",
		"a@example",
	} {
		if err := ValidateCommitAuthor("author", email); err != nil {
			t.Errorf("ValidateCommitAuthor(%q) = %v", email, err)
		}
	}
}

func TestValidateBaseSHARequiresLowercaseHex(t *testing.T) {
	for _, sha := range []string{strings.Repeat("A", 40), strings.Repeat("a", 39) + "B"} {
		if err := ValidateBaseSHA(sha); err == nil {
			t.Fatalf("ValidateBaseSHA accepted %q", sha)
		}
	}
	if err := ValidateBaseSHA(strings.Repeat("a", 40)); err != nil {
		t.Fatalf("ValidateBaseSHA rejected lowercase SHA: %v", err)
	}
}

func TestFailureAndHeartbeatMessageContract(t *testing.T) {
	body, err := json.Marshal(Message{Type: MessageHeartbeat, JobID: "job", AttemptID: "attempt", WorkerID: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"type":"heartbeat","job_id":"job","attempt_id":"attempt","worker_id":"worker"}` {
		t.Fatalf("heartbeat JSON = %s", body)
	}
	if FailureInvalidTask != "invalid_task" || FailureScopedTest != "scoped_test_failed" || FailureExecution != "execution_failed" || DispositionTerminal != "terminal" || DispositionRetryable != "retryable" {
		t.Fatal("failure constants changed")
	}
	body, err = json.Marshal(Message{Type: MessageResult, Error: FailureExecution, Disposition: DispositionRetryable})
	if err != nil || !bytes.Contains(body, []byte(`"disposition":"retryable"`)) {
		t.Fatalf("failure JSON = %s, %v", body, err)
	}
}

func TestEvidenceMessageContractIsOptionalAndBounded(t *testing.T) {
	record := AttemptEvidence{EvidenceID: strings.Repeat("a", 32), Phase: EvidencePhasePlugin, Reason: EvidenceReasonPluginFailed, BaseSHA: strings.Repeat("b", 40)}
	body, err := json.Marshal(Message{Type: MessageEvidence, JobID: "job", AttemptID: "attempt", Evidence: []AttemptEvidence{record}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"evidence","job_id":"job","attempt_id":"attempt","evidence":[{"evidence_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","phase":"plugin","reason":"plugin_failed","duration_ms":0,"base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`
	if string(body) != want {
		t.Fatalf("evidence message JSON = %s", body)
	}
	body, err = json.Marshal(Message{Type: MessageResult, Result: "legacy"})
	if err != nil || strings.Contains(string(body), "evidence") {
		t.Fatalf("legacy result JSON = %s, %v", body, err)
	}
	if MaxEvidenceRecordsPerBatch != 34 || MaxEvidenceBatchBytes != 96<<10 || MaxWorkerMessageBytes != 1<<20 {
		t.Fatal("evidence protocol limits changed")
	}
}

func TestAttemptEvidenceJSONContractIsStructuredAndClosed(t *testing.T) {
	checkIndex, exitCode := 1, 7
	record := AttemptEvidence{
		EvidenceID:      strings.Repeat("a", 32),
		Phase:           EvidencePhaseScopedCheck,
		Reason:          EvidenceReasonScopedCheckFailed,
		CheckIndex:      &checkIndex,
		ExitCode:        &exitCode,
		DurationMS:      1250,
		Output:          "bounded output",
		OutputRedacted:  true,
		OutputTruncated: true,
		BaseSHA:         strings.Repeat("b", 40),
		CandidateSHA:    strings.Repeat("c", 40),
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"evidence_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","phase":"scoped_check","reason":"scoped_check_failed","check_index":1,"exit_code":7,"duration_ms":1250,"output":"bounded output","output_redacted":true,"output_truncated":true,"base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","candidate_sha":"cccccccccccccccccccccccccccccccccccccccc"}`
	if string(body) != want {
		t.Fatalf("evidence JSON = %s", body)
	}
	if EvidencePhasePreparation != "preparation" || EvidencePhasePlugin != "plugin" || EvidencePhaseWorkspaceValidation != "workspace_validation" ||
		EvidencePhaseCandidateCommit != "candidate_commit" || EvidencePhaseCleanup != "cleanup" ||
		EvidenceReasonPluginFailed != "plugin_failed" || EvidenceReasonNoChanges != "no_changes" || EvidenceReasonInvalidWorkspaceChange != "invalid_workspace_change" ||
		EvidenceReasonScopedCheckPassed != "scoped_check_passed" || EvidenceReasonScopedCheckTimeout != "scoped_check_timeout" ||
		EvidenceReasonCandidateCommitFailed != "candidate_commit_failed" || EvidenceReasonCleanupFailed != "cleanup_failed" {
		t.Fatal("evidence allowlist constants changed")
	}
}

func TestCodingEvidenceReasonTaxonomyIsClosed(t *testing.T) {
	want := []string{
		EvidenceReasonInvalidTask, EvidenceReasonInvalidRepository, EvidenceReasonSourcePolicyInvalid, EvidenceReasonRepositoryStateUnsafe, EvidenceReasonCloneFailed, EvidenceReasonFetchFailed, EvidenceReasonBaseUnavailable, EvidenceReasonRuntimeSetupFailed, EvidenceReasonWorktreeSetupFailed,
		EvidenceReasonPluginStartFailed, EvidenceReasonPluginProtocolFailed, EvidenceReasonPluginReportedFailure,
		EvidenceReasonNoChanges, EvidenceReasonInvalidWorkspaceChange,
		EvidenceReasonScopedCheckPassed, EvidenceReasonScopedCheckFailed, EvidenceReasonScopedCheckTimeout,
		EvidenceReasonCandidateCommitFailed, EvidenceReasonCleanupFailed,
	}
	if strings.Join(want, ",") != "invalid_task,invalid_repository,source_policy_invalid,repository_state_unsafe,clone_failed,fetch_failed,base_unavailable,runtime_setup_failed,worktree_setup_failed,plugin_start_failed,plugin_protocol_failed,plugin_reported_failure,no_changes,invalid_workspace_change,scoped_check_passed,scoped_check_failed,scoped_check_timeout,candidate_commit_failed,cleanup_failed" {
		t.Fatalf("evidence reason taxonomy changed: %v", want)
	}
}

func TestSanitizeEvidenceOutputRejectsPrivateAndCredentialForms(t *testing.T) {
	repository := "/private/alice/repository"
	unsafe := []string{
		"Authorization: Bearer secret",
		"authorization: Basic secret",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github_pat_abcdefghijklmnopqrstuvwxyz_0123456789",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature",
		"AKIAIOSFODNN7EXAMPLE",
		"-----BEGIN PRIVATE KEY-----",
		"PASSWORD=hunter2",
		repository + "/secret.txt",
		"/tmp/forge-worktree/private.txt",
		string([]byte{'b', 'a', 'd', 0xff}),
	}
	for _, input := range unsafe {
		t.Run(input[:min(len(input), 20)], func(t *testing.T) {
			got, changed := SanitizeEvidenceOutput(input, repository)
			if !changed || got == input {
				t.Fatalf("unsafe output unchanged: %q", got)
			}
			if again, changed := SanitizeEvidenceOutput(got, repository); changed || again != got {
				t.Fatalf("sanitizer is not idempotent: first=%q second=%q", got, again)
			}
		})
	}
	if got, changed := SanitizeEvidenceOutput("go test passed", repository); changed || got != "go test passed" {
		t.Fatalf("safe output = %q, changed=%v", got, changed)
	}
}

func TestSanitizeEvidenceOutputRedactsClosedPrivacyForms(t *testing.T) {
	tests := []struct {
		name, input string
		private     []string
	}{
		{"URL userinfo", "fetch https://synthetic-user:synthetic-pass@example.invalid/api", []string{"synthetic-user", "synthetic-pass"}},
		{"URL userinfo with at", "fetch https://synthetic-user@synthetic-pass@example.invalid/api", []string{"synthetic-user", "synthetic-pass"}},
		{"URI username only", "clone ssh://synthetic-name@example.invalid/repo", []string{"synthetic-name"}},
		{"colon credential", "CrEdEnTiAl : synthetic-colon-secret", []string{"synthetic-colon-secret"}},
		{"equals credential", "password = synthetic-equals-secret", []string{"synthetic-equals-secret"}},
		{"equals authorization", "AUTHORIZATION = synthetic-equals-auth", []string{"synthetic-equals-auth"}},
		{"spaced colon authorization", "Authorization: Bearer synthetic colon secret; safe=kept", []string{"synthetic colon secret"}},
		{"spaced equals authorization query", "https://example.invalid/?authorization=Bearer synthetic equals secret&safe=kept#fragment", []string{"synthetic equals secret"}},
		{"spaced credential query", "https://example.invalid/?credential=synthetic value with spaces&safe=kept", []string{"value with spaces"}},
		{"reviewer credential", "OPENAI_API_KEY: synthetic-reviewer-key", []string{"synthetic-reviewer-key"}},
		{"compound labels", "DbPaSsWoRd: synthetic-db-password\nSERVICE-CREDENTIAL=synthetic-service-credential", []string{"synthetic-db-password", "synthetic-service-credential"}},
		{"query credentials", "https://example.invalid/?token=synthetic-token&api_key=synthetic-key&authorization=synthetic-auth&client_secret=synthetic-client-secret&refreshToken=synthetic-refresh-token&x-api-key=synthetic-prefixed-key&safe=kept", []string{"synthetic-token", "synthetic-key", "synthetic-auth", "synthetic-client-secret", "synthetic-refresh-token", "synthetic-prefixed-key"}},
		{"POSIX path after colon", "workspace: /synthetic/private/alice/file.go", []string{"/synthetic/private/alice/file.go"}},
		{"POSIX path after equals", "config = /synthetic/private/config.json", []string{"/synthetic/private/config.json"}},
		{"Windows path after colon", `workspace: C:\synthetic\private\alice\file.go`, []string{`C:\synthetic\private\alice\file.go`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed := SanitizeEvidenceOutput(test.input)
			if !changed {
				t.Fatalf("unsafe output unchanged: %q", got)
			}
			for _, private := range test.private {
				if strings.Contains(got, private) {
					t.Fatalf("private value %q leaked in %q", private, got)
				}
			}
			if _, marked := EvidenceOutputRedactionMarker(got); !marked {
				t.Fatalf("redacted output has no marker: %q", got)
			}
			if again, changed := SanitizeEvidenceOutput(got); changed || again != got {
				t.Fatalf("sanitizer is not idempotent: first=%q second=%q", got, again)
			}
		})
	}
	if got, changed := SanitizeEvidenceOutput("https://example.invalid/?client_secret=hidden&safe=kept&mode=fast"); !changed || !strings.Contains(got, "&safe=kept&mode=fast") {
		t.Fatalf("safe neighboring query parameters changed: %q", got)
	}
	for _, delimiter := range []string{"&safe=kept", "#fragment", "; safe=kept"} {
		input := "Authorization: Bearer synthetic secret" + delimiter
		if got, changed := SanitizeEvidenceOutput(input); !changed || !strings.HasSuffix(got, delimiter) {
			t.Fatalf("safe delimiter %q changed: %q", delimiter, got)
		}
	}
}

func TestSanitizeEvidenceOutputRedactsStructuredCLIAndUNCForms(t *testing.T) {
	tests := []struct {
		name, input, safe string
		private           []string
	}{
		{"JSON authorization", `{"Authorization":"Bearer synthetic JSON secret","safe":"kept"}`, `,"safe":"kept"}`,
			[]string{"synthetic", "JSON secret"}},
		{"single-quoted structured key", `{'api_key': 'synthetic single quoted secret', 'safe': 'kept'}`, `, 'safe': 'kept'}`,
			[]string{"synthetic single quoted secret"}},
		{"single-hyphen credential flag", "check -credential synthetic single-hyphen credential --later may-leak; printf safe", "; printf safe",
			[]string{"synthetic single-hyphen credential", "may-leak"}},
		{"CLI token", "check --token synthetic CLI token --later may-leak; printf safe", "; printf safe",
			[]string{"synthetic CLI token", "may-leak"}},
		{"CLI authorization", `check --authorization "Bearer synthetic CLI authorization" && printf safe`, "&& printf safe",
			[]string{"synthetic", "CLI authorization"}},
		{"bracket credential label", "[credential]: synthetic bracket credential\nsafe line", "\nsafe line",
			[]string{"synthetic bracket credential"}},
		{"labeled UNC path", `workspace: \\synthetic-server\private\output; safe=kept`, "; safe=kept",
			[]string{`\\synthetic-server\private\output`}},
		{"standalone UNC path", `open \\synthetic-server\private\output, safe=kept`, ", safe=kept",
			[]string{`\\synthetic-server\private\output`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed := SanitizeEvidenceOutput(test.input)
			if !changed {
				t.Fatalf("unsafe output unchanged: %q", got)
			}
			for _, private := range test.private {
				if strings.Contains(got, private) {
					t.Fatalf("private value %q leaked in %q", private, got)
				}
			}
			if !strings.Contains(got, test.safe) {
				t.Fatalf("safe delimiter %q removed from %q", test.safe, got)
			}
			if _, marked := EvidenceOutputRedactionMarker(got); !marked {
				t.Fatalf("redacted output has no marker: %q", got)
			}
			if again, changed := SanitizeEvidenceOutput(got); changed || again != got {
				t.Fatalf("sanitizer is not idempotent: first=%q second=%q", got, again)
			}
		})
	}
}

func TestSanitizeEvidenceOutputRedactsMultiWordCredentialLabels(t *testing.T) {
	tests := []struct {
		input, private, safe string
	}{
		{"Authorization Header: Basic synthetic-review-value; safe=kept", "synthetic-review-value", "; safe=kept"},
		{"Database Password Value: synthetic-db-value; safe=kept", "synthetic-db-value", "; safe=kept"},
		{"Access Token: synthetic-access-token; safe=kept", "synthetic-access-token", "; safe=kept"},
		{"Client Secret = synthetic-client-secret&safe=kept", "synthetic-client-secret", "&safe=kept"},
		{`"Access Token": "synthetic-quoted-token", "safe":"kept"`, "synthetic-quoted-token", `, "safe":"kept"`},
		{"[Client Secret]: synthetic-bracketed-secret\nsafe line", "synthetic-bracketed-secret", "\nsafe line"},
		{"check --access token synthetic-cli-token; safe", "synthetic-cli-token", "; safe"},
		{"check --client secret synthetic-cli-secret && safe", "synthetic-cli-secret", "&& safe"},
		{"check --token synthetic-existing-value secret --later unsafe; safe", "synthetic-existing-value", "; safe"},
	}
	for _, test := range tests {
		got, changed := SanitizeEvidenceOutput(test.input)
		if !changed || strings.Contains(got, test.private) || !strings.Contains(got, test.safe) {
			t.Fatalf("multi-word label unsafe or delimiter lost: %q", got)
		}
		if again, changed := SanitizeEvidenceOutput(got); changed || again != got {
			t.Fatalf("sanitizer is not idempotent: first=%q second=%q", got, again)
		}
	}
}

func TestEvidenceOutputRedactionMarkerRecognizesReservedMarkers(t *testing.T) {
	for _, marker := range []string{"[REDACTED]", "[REPOSITORY_PATH]", "[ABSOLUTE_PATH]", "[INVALID_UTF8]"} {
		if got, ok := EvidenceOutputRedactionMarker("literal " + marker + " output"); !ok || got != marker {
			t.Fatalf("marker %q = %q, %v", marker, got, ok)
		}
	}
	if marker, ok := EvidenceOutputRedactionMarker("go test passed"); ok || marker != "" {
		t.Fatalf("safe output marker = %q, %v", marker, ok)
	}
}

func TestValidateBranchNameMatchesGitCheckRefFormat(t *testing.T) {
	for _, branch := range []string{"main", "feature/nested-topic", "release/v1.2.3"} {
		if err := ValidateBranchName(branch); err != nil {
			t.Errorf("valid branch %q: %v", branch, err)
		}
	}
	for _, branch := range []string{
		"", "@", ".hidden", "topic.lock", "topic/.hidden", "-leading", "topic\x00control", "topic\x7fdel",
		"topic/./leaf", "topic/.lock/leaf", "topic//leaf", "topic..leaf", "topic@{leaf", "topic\\leaf",
		"topic leaf", "topic~leaf", "topic^leaf", "topic:leaf", "topic?leaf", "topic*leaf", "topic[leaf",
		"/topic", "topic/", "topic.",
	} {
		if err := ValidateBranchName(branch); err == nil {
			t.Errorf("accepted invalid branch %q", branch)
		}
	}
}
