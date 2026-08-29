package buildinfo

import (
	"bytes"
	"testing"
)

func TestWriteIfRequested(t *testing.T) {
	oldVersion, oldCommit := Version, Commit
	Version = "v1.2.3"
	Commit = "0123456789abcdef0123456789abcdef01234567"
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })
	var output bytes.Buffer
	requested, err := WriteIfRequested([]string{"--version"}, &output, "forge-gate")
	if err != nil || !requested || output.String() != "forge-gate v1.2.3 0123456789abcdef0123456789abcdef01234567\n" {
		t.Fatalf("requested=%v err=%v output=%q", requested, err, output.String())
	}
	requested, err = WriteIfRequested([]string{"-config", "gate.json"}, &output, "forge-gate")
	if err != nil || requested {
		t.Fatalf("ordinary args: requested=%v err=%v", requested, err)
	}
}

func TestLineUsesInjectedVersionAndCommit(t *testing.T) {
	oldVersion, oldCommit := Version, Commit
	Version = "v1.2.3"
	Commit = "0123456789abcdef0123456789abcdef01234567"
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })
	if got := Line("forge-worker"); got != "forge-worker v1.2.3 0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("Line = %q", got)
	}
}
