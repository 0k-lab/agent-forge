package protocol

import (
	"errors"
	"net/mail"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const (
	MaxEvidenceOutputBytes     = 2048
	MaxEvidenceRecordsPerBatch = 34
	MaxEvidenceBatchBytes      = 96 << 10
	MaxWorkerMessageBytes      = 1 << 20
	EvidenceRedactedMarker     = "[REDACTED]"
)

var evidenceRedactionMarkers = []string{"[INVALID_UTF8]", "[REPOSITORY_PATH]", "[ABSOLUTE_PATH]", EvidenceRedactedMarker}

func EvidenceOutputRedactionMarker(output string) (string, bool) {
	for _, marker := range evidenceRedactionMarkers {
		if strings.Contains(output, marker) {
			return marker, true
		}
	}
	return "", false
}

const evidenceCredentialWord = `[A-Za-z0-9_-]{0,64}(?:authorization|credential|password|secret|token|api[_-]?key)[A-Za-z0-9_-]{0,64}`
const evidenceCredentialLabelPrefix = `(?:[A-Za-z0-9_-]{1,32}[ \t]+){0,2}` + evidenceCredentialWord
const evidenceCredentialLabel = evidenceCredentialLabelPrefix + `(?:[ \t]+[A-Za-z0-9_-]{1,32}){0,2}`
const evidenceCLICredentialLabel = `(?:` + evidenceCredentialWord + `|[A-Za-z0-9_-]{1,32}[ \t]+` + evidenceCredentialWord + `|[A-Za-z0-9_-]{1,32}[ \t]+[A-Za-z0-9_-]{1,32}[ \t]+` + evidenceCredentialWord + `)`

var evidenceSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?im)((?:^|[^A-Za-z0-9_-])["'\[]?` + evidenceCredentialLabel + `["'\]]?\s*[:=]\s*["']?)[^\r\n"',;&#}]+`),
	regexp.MustCompile(`(?im)((?:^|[\s"'(])-{1,2}` + evidenceCLICredentialLabel + `\s+)[^\r\n;&|]+`),
	regexp.MustCompile(`(?i)(\bbearer\s+)\S+`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
	regexp.MustCompile(`\b[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`\b(?:A3T[A-Z0-9]|ABIA|ACCA|AGPA|AIDA|AIPA|AKIA|ANPA|ANVA|AROA|ASCA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----`),
	regexp.MustCompile(`\b([A-Z_][A-Z0-9_]*=)[^\s]+`),
}

var evidenceURLUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s?#]+@`)
var evidenceLabeledPath = regexp.MustCompile(`(?i)(\b[A-Z][A-Z0-9_-]*\s*[:=]\s*)((?:/[^/\s"',;)][^\s"',;)]*)|(?:[A-Z]:[\\/][^\s"',;)]+)|(?:\\\\[^\s\\/"',;)]+\\[^\s"',;)]+))`)
var evidenceAbsolutePath = regexp.MustCompile(`(^|[\s"'(=])((?:/[^\s"',;)]+)|(?:[A-Za-z]:[\\/][^\s"',;)]+)|(?:\\\\[^\s\\/"',;)]+\\[^\s"',;)]+))`)

// SanitizeEvidenceOutput returns deterministic output safe to cross the Worker/Gate boundary.
func SanitizeEvidenceOutput(output string, privatePaths ...string) (string, bool) {
	original := output
	output = strings.ToValidUTF8(output, "[INVALID_UTF8]")
	for _, path := range privatePaths {
		if path != "" {
			output = strings.ReplaceAll(output, filepath.Clean(path), "[REPOSITORY_PATH]")
		}
	}
	output = evidenceURLUserinfo.ReplaceAllString(output, `${1}[REDACTED]@`)
	for _, pattern := range evidenceSecrets {
		output = pattern.ReplaceAllString(output, `${1}[REDACTED]`)
	}
	output = evidenceLabeledPath.ReplaceAllString(output, `${1}[ABSOLUTE_PATH]`)
	output = evidenceAbsolutePath.ReplaceAllString(output, `${1}[ABSOLUTE_PATH]`)
	return output, output != original
}

const (
	MessageLease     = "lease"
	MessageHeartbeat = "heartbeat"
	MessageEvidence  = "evidence"
	MessageResult    = "result"
	MessageAck       = "ack"
	MessageError     = "error"

	FailureInvalidTask = "invalid_task"
	FailureScopedTest  = "scoped_test_failed"
	FailureExecution   = "execution_failed"

	DispositionTerminal  = "terminal"
	DispositionRetryable = "retryable"

	EvidencePhasePreparation         = "preparation"
	EvidencePhasePlugin              = "plugin"
	EvidencePhaseWorkspaceValidation = "workspace_validation"
	EvidencePhaseScopedCheck         = "scoped_check"
	EvidencePhaseCandidateCommit     = "candidate_commit"
	EvidencePhaseCleanup             = "cleanup"

	EvidenceReasonPreparationFailed      = "preparation_failed"
	EvidenceReasonInvalidTask            = "invalid_task"
	EvidenceReasonInvalidRepository      = "invalid_repository"
	EvidenceReasonRuntimeSetupFailed     = "runtime_setup_failed"
	EvidenceReasonWorktreeSetupFailed    = "worktree_setup_failed"
	EvidenceReasonPluginFailed           = "plugin_failed"
	EvidenceReasonPluginStartFailed      = "plugin_start_failed"
	EvidenceReasonPluginProtocolFailed   = "plugin_protocol_failed"
	EvidenceReasonPluginReportedFailure  = "plugin_reported_failure"
	EvidenceReasonNoChanges              = "no_changes"
	EvidenceReasonInvalidWorkspaceChange = "invalid_workspace_change"
	EvidenceReasonScopedCheckPassed      = "scoped_check_passed"
	EvidenceReasonScopedCheckFailed      = "scoped_check_failed"
	EvidenceReasonScopedCheckTimeout     = "scoped_check_timeout"
	EvidenceReasonCandidateCommitFailed  = "candidate_commit_failed"
	EvidenceReasonCleanupFailed          = "cleanup_failed"
)

type AttemptEvidence struct {
	EvidenceID      string   `json:"evidence_id"`
	Phase           string   `json:"phase"`
	Reason          string   `json:"reason"`
	CheckIndex      *int     `json:"check_index,omitempty"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	DurationMS      int64    `json:"duration_ms"`
	Output          string   `json:"output,omitempty"`
	OutputRedacted  bool     `json:"output_redacted,omitempty"`
	OutputTruncated bool     `json:"output_truncated,omitempty"`
	BaseSHA         string   `json:"base_sha"`
	CandidateSHA    string   `json:"candidate_sha,omitempty"`
	Argv            []string `json:"argv,omitempty"`
	ArgvRedacted    bool     `json:"argv_redacted,omitempty"`
}

type Message struct {
	Type         string            `json:"type"`
	JobID        string            `json:"job_id,omitempty"`
	AttemptID    string            `json:"attempt_id,omitempty"`
	WorkerID     string            `json:"worker_id,omitempty"`
	Input        string            `json:"input,omitempty"`
	Task         *CodingTask       `json:"task,omitempty"`
	Result       string            `json:"result,omitempty"`
	CandidateSHA string            `json:"candidate_sha,omitempty"`
	Error        string            `json:"error,omitempty"`
	Disposition  string            `json:"disposition,omitempty"`
	Evidence     []AttemptEvidence `json:"evidence,omitempty"`
	Policy       *ResolvedPolicy   `json:"policy,omitempty"`
}

type CodingTask struct {
	RepositoryID      string     `json:"repository_id,omitempty"`
	Repository        string     `json:"repository,omitempty"`
	BaseSHA           string     `json:"base_sha"`
	Instruction       string     `json:"instruction"`
	Tests             [][]string `json:"tests"`
	CommitAuthorName  string     `json:"commit_author_name,omitempty"`
	CommitAuthorEmail string     `json:"commit_author_email,omitempty"`
}

func ValidateBaseSHA(sha string) error {
	if len(sha) != 40 {
		return errors.New("base_sha must be a full SHA")
	}
	for _, c := range []byte(sha) {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return errors.New("base_sha must be lowercase hexadecimal")
		}
	}
	return nil
}

func ValidateCommitAuthor(name, email string) error {
	invalid := errors.New("invalid commit author")
	if (name == "") != (email == "") {
		return invalid
	}
	if name == "" {
		return nil
	}
	if len(name) > 256 || len(email) > 254 ||
		strings.TrimFunc(name, unicode.IsSpace) != name || strings.TrimFunc(email, unicode.IsSpace) != email {
		return invalid
	}
	for _, value := range []string{name, email} {
		if strings.ContainsAny(value, "<>:") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return invalid
		}
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return invalid
	}
	domain := email[strings.LastIndexByte(email, '@')+1:]
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || !domainAlphaNumeric(label[0]) || !domainAlphaNumeric(label[len(label)-1]) {
			return invalid
		}
		for i := 1; i < len(label)-1; i++ {
			if !domainAlphaNumeric(label[i]) && label[i] != '-' {
				return invalid
			}
		}
	}
	return nil
}

func domainAlphaNumeric(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}
