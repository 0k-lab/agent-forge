package pluginprotocol

import (
	"io"
	"strings"
	"testing"
)

func TestWorkspaceOptionalAgentReport(t *testing.T) {
	id := strings.Repeat("a", 32)
	ready := `{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["workspace_edit","commit_subject","agent_report"]}` + "\n"
	for _, fields := range []string{``, `,"summary":"Normalize input","changes":["Handle equivalent Unicode names"]`} {
		wire := ready + `{"version":"v1","id":"` + id + `","type":"result","commit_subject":"fix: normalize"` + fields + "}\n"
		result, err := Exchange(io.Discard, strings.NewReader(wire), Request{ID: id, Operation: WorkspaceEdit, Workspace: "/work", Instruction: "fix", TimeoutMS: 1000}, []Capability{WorkspaceEdit, CommitSubject, Capability("agent_report")})
		if fields != "" && (result.Report == nil || result.Report.Summary != "Normalize input" || len(result.Report.Changes) != 1) {
			t.Fatal("missing typed report")
		}
		if err != nil || result.CommitSubject == nil {
			t.Fatalf("optional report rejected: %v", err)
		}
	}
}

func TestWorkspaceRejectsMalformedAgentReports(t *testing.T) {
	id := strings.Repeat("a", 32)
	ready := `{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["workspace_edit","agent_report"]}` + "\n"
	for _, fields := range []string{
		`"summary":"x"`, `"changes":["x"]`, `"summary":null,"changes":null`,
		`"summary":" x","changes":["x"]`, `"summary":"x","changes":[""]`,
		`"summary":"x\nsecret","changes":["x"]`, `"summary":"x","changes":["\u202e"]`,
		`"summary":"\ud800","changes":["x"]`, `"summary":"x","changes":["x"],"private":"secret"`,
		`"summary":"` + strings.Repeat("x", 1025) + `","changes":["x"]`,
		`"summary":"x","changes":["` + strings.Repeat("x", 257) + `"]`,
		`"summary":"x","changes":[` + strings.Repeat(`"x",`, 12) + `"x"]`,
		`"summary":"x","summary":"y","changes":["x"]`,
	} {
		wire := ready + `{"version":"v1","id":"` + id + `","type":"result",` + fields + "}\n"
		if _, err := Exchange(io.Discard, strings.NewReader(wire), Request{ID: id, Operation: WorkspaceEdit, Workspace: "/work", Instruction: "fix", TimeoutMS: 1000}, []Capability{WorkspaceEdit, AgentReport}); err == nil {
			t.Fatalf("invalid report accepted: %q", fields)
		}
	}
}

func TestUnnegotiatedReportRejected(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, offered := range [][]Capability{{WorkspaceEdit, CommitSubject}, {WorkspaceEdit, CommitSubject, AgentReport}} {
		for _, fields := range []string{``, `,"summary":"x","changes":["x"]`, `,"summary":null`, `,"changes":null`} {
			wire := `{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["workspace_edit","commit_subject"]}` + "\n" + `{"version":"v1","id":"` + id + `","type":"result","commit_subject":"fix: legacy"` + fields + "}\n"
			_, err := Exchange(io.Discard, strings.NewReader(wire), Request{ID: id, Operation: WorkspaceEdit, Workspace: "/work", Instruction: "fix", TimeoutMS: 1000}, offered)
			if (err != nil) != (fields != "") {
				t.Fatalf("fields=%q err=%v", fields, err)
			}
		}
	}
}
