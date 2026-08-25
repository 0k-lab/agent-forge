package protocol

import (
	"errors"
	"net/mail"
	"strings"
	"unicode"
)

const (
	MessageLease     = "lease"
	MessageHeartbeat = "heartbeat"
	MessageResult    = "result"
	MessageAck       = "ack"
	MessageError     = "error"

	FailureInvalidTask = "invalid_task"
	FailureScopedTest  = "scoped_test_failed"
	FailureExecution   = "execution_failed"

	DispositionTerminal  = "terminal"
	DispositionRetryable = "retryable"
)

type Message struct {
	Type         string      `json:"type"`
	JobID        string      `json:"job_id,omitempty"`
	AttemptID    string      `json:"attempt_id,omitempty"`
	WorkerID     string      `json:"worker_id,omitempty"`
	Input        string      `json:"input,omitempty"`
	Task         *CodingTask `json:"task,omitempty"`
	Result       string      `json:"result,omitempty"`
	CandidateSHA string      `json:"candidate_sha,omitempty"`
	Error        string      `json:"error,omitempty"`
	Disposition  string      `json:"disposition,omitempty"`
}

type CodingTask struct {
	Repository        string     `json:"repository"`
	BaseSHA           string     `json:"base_sha"`
	Instruction       string     `json:"instruction"`
	Tests             [][]string `json:"tests"`
	CommitAuthorName  string     `json:"commit_author_name,omitempty"`
	CommitAuthorEmail string     `json:"commit_author_email,omitempty"`
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
