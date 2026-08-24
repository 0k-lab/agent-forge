package protocol

import (
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
