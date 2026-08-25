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
