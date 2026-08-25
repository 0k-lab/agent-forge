package pluginprotocol

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestTextSession(t *testing.T) {
	id := strings.Repeat("a", 32)
	input := strings.Join([]string{
		`{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["text"]}`,
		`{"version":"v1","id":"` + id + `","type":"result","output":"done"}`,
		"",
	}, "\n")
	var sent bytes.Buffer
	result, err := Exchange(&sent, strings.NewReader(input), Request{ID: id, Operation: Text, Input: "hello"}, []Capability{Text})
	if err != nil || result.Output != "done" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	want := `{"version":"v1","id":"` + id + `","type":"initialize","capabilities":["text"],"limits":{"frame_bytes":1048576,"progress_frames":128,"text_bytes":65536,"progress_text_bytes":1024,"commit_subject_bytes":256}}` + "\n" +
		`{"version":"v1","id":"` + id + `","type":"execute","operation":"text","input":"hello"}` + "\n"
	if sent.String() != want {
		t.Fatalf("sent = %q, want %q", sent.String(), want)
	}
}

func TestV1LimitsCannotBeMutatedByCallers(t *testing.T) {
	limits := V1Limits()
	limits.FrameBytes = 1
	if V1Limits().FrameBytes != MaxFrameBytes {
		t.Fatal("caller mutated protocol limits")
	}
}

func TestServeText(t *testing.T) {
	id := strings.Repeat("f", 32)
	wire := `{"version":"v1","id":"` + id + `","type":"initialize","capabilities":["text"],"limits":{"frame_bytes":1048576,"progress_frames":128,"text_bytes":65536,"progress_text_bytes":1024,"commit_subject_bytes":256}}` + "\n" +
		`{"version":"v1","id":"` + id + `","type":"execute","operation":"text","input":"hello"}` + "\n"
	var output bytes.Buffer
	err := Serve(strings.NewReader(wire), &output, []Capability{Text}, func(_ context.Context, request Request) (Result, error) {
		return Result{Output: strings.ToUpper(request.Input)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["text"]}` + "\n" +
		`{"version":"v1","id":"` + id + `","type":"result","output":"HELLO"}` + "\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestServeRejectsUnsupportedOptionalCapabilities(t *testing.T) {
	for _, capability := range []Capability{Cancel, Progress} {
		t.Run(string(capability), func(t *testing.T) {
			err := Serve(strings.NewReader(""), io.Discard, []Capability{Text, capability}, func(context.Context, Request) (Result, error) {
				return Result{}, nil
			})
			if err == nil || err.Error() != "invalid plugin configuration" {
				t.Fatalf("Serve = %v, want invalid plugin configuration", err)
			}
		})
	}
}

func TestCommitSubject(t *testing.T) {
	for _, valid := range []string{"feat: describe the change", "feat: support café ☕"} {
		if err := ValidateCommitSubject(&valid, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateCommitSubject(nil, false); err != nil {
		t.Fatal(err)
	}
	for name, subject := range map[string]string{
		"empty":          "",
		"oversized":      strings.Repeat("x", MaxCommitSubjectBytes+1),
		"leading space":  " subject",
		"trailing space": "subject\u2003",
		"multiline":      "subject\nbody",
		"control":        "subject\u0007",
		"next line":      "subject\u0085body",
		"line separator": "subject\u2028body",
		"bidi embedding": "subject\u202a.txt",
		"bidi override":  "subject\u202egnp.exe",
		"bidi isolate":   "subject\u2066text\u2069",
		"direction mark": "subject\u200e",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCommitSubject(&subject, true); err == nil {
				t.Fatal("accepted invalid subject")
			}
		})
	}
	for _, r := range []rune{'\u200e', '\u200f', '\u202a', '\u202b', '\u202c', '\u202d', '\u202e', '\u2066', '\u2067', '\u2068', '\u2069'} {
		t.Run(fmt.Sprintf("format U+%04X", r), func(t *testing.T) {
			subject := "subject" + string(r)
			if err := ValidateCommitSubject(&subject, true); err == nil {
				t.Fatal("accepted Unicode format character")
			}
		})
	}
	valid := "feat: describe the change"
	if err := ValidateCommitSubject(&valid, false); err == nil {
		t.Fatal("accepted subject without negotiated capability")
	}
}

func TestReadFrameRejectsInvalidWire(t *testing.T) {
	valid := `{"version":"v1","id":"` + strings.Repeat("b", 32) + `","type":"failure","category":"execution_failed"}` + "\n"
	for name, wire := range map[string]string{
		"blank":        "\n",
		"no LF":        strings.TrimSuffix(valid, "\n"),
		"CRLF":         strings.TrimSuffix(valid, "\n") + "\r\n",
		"not compact":  strings.Replace(valid, "{", "{ ", 1),
		"invalid UTF8": string([]byte{'{', 0xff, '}', '\n'}),
		"oversized":    strings.Repeat("x", MaxFrameBytes+1) + "\n",
		"unknown":      strings.TrimSuffix(valid, "}\n") + `,"extra":true}` + "\n",
		"duplicate":    strings.Replace(valid, `"type":"failure"`, `"type":"failure","type":"failure"`, 1),
		"unsupported":  strings.Replace(valid, `"version":"v1"`, `"version":"v2"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadFailure(strings.NewReader(wire)); err == nil {
				t.Fatal("accepted invalid wire")
			}
		})
	}
}

func TestWorkspaceSessionWithProgressAndSubject(t *testing.T) {
	id := strings.Repeat("c", 32)
	input := strings.Join([]string{
		`{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["workspace_edit","progress","commit_subject"]}`,
		`{"version":"v1","id":"` + id + `","type":"progress","sequence":1,"stage":"started","text":"editing"}`,
		`{"version":"v1","id":"` + id + `","type":"progress","sequence":2,"stage":"finalizing","text":"done"}`,
		`{"version":"v1","id":"` + id + `","type":"result","commit_subject":"feat: edit answer"}`,
		"",
	}, "\n")
	var sent bytes.Buffer
	result, err := Exchange(&sent, strings.NewReader(input), Request{ID: id, Operation: WorkspaceEdit, Workspace: "/work", Instruction: "edit", TimeoutMS: 1000}, []Capability{WorkspaceEdit, Progress, Cancel, CommitSubject})
	if err != nil || result.CommitSubject == nil || *result.CommitSubject != "feat: edit answer" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	wantExecute := `{"version":"v1","id":"` + id + `","type":"execute","operation":"workspace_edit","workspace":"/work","instruction":"edit","timeout_ms":1000}` + "\n"
	if !strings.HasSuffix(sent.String(), wantExecute) {
		t.Fatalf("sent = %q", sent.String())
	}
}

func TestSessionRejectsInvalidProgressAndFailure(t *testing.T) {
	id := strings.Repeat("d", 32)
	ready := `{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["text"]}` + "\n"
	for name, terminal := range map[string]string{
		"progress unnegotiated": `{"version":"v1","id":"` + id + `","type":"progress","sequence":1,"stage":"working","text":"x"}`,
		"wrong id":              `{"version":"v1","id":"` + strings.Repeat("e", 32) + `","type":"result","output":"x"}`,
		"unknown failure":       `{"version":"v1","id":"` + id + `","type":"failure","category":"retry"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var sent bytes.Buffer
			if _, err := Exchange(&sent, strings.NewReader(ready+terminal+"\n"), Request{ID: id, Operation: Text, Input: "x"}, []Capability{Text}); err == nil {
				t.Fatal("accepted invalid session")
			}
		})
	}
}

func TestSessionRejectsProgressSequenceAndLimitViolations(t *testing.T) {
	id := strings.Repeat("1", 32)
	ready := `{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["text","progress"]}` + "\n"
	progress := func(sequence int) string {
		return fmt.Sprintf(`{"version":"v1","id":"%s","type":"progress","sequence":%d,"stage":"working","text":"x"}`+"\n", id, sequence)
	}
	t.Run("non-monotonic", func(t *testing.T) {
		if _, err := Exchange(io.Discard, strings.NewReader(ready+progress(2)), Request{ID: id, Operation: Text, Input: "x"}, []Capability{Text, Progress}); err == nil {
			t.Fatal("accepted non-monotonic progress")
		}
	})
	t.Run("over limit", func(t *testing.T) {
		var wire strings.Builder
		wire.WriteString(ready)
		for sequence := 1; sequence <= MaxProgressFrames+1; sequence++ {
			wire.WriteString(progress(sequence))
		}
		if _, err := Exchange(io.Discard, strings.NewReader(wire.String()), Request{ID: id, Operation: Text, Input: "x"}, []Capability{Text, Progress}); err == nil {
			t.Fatal("accepted too many progress frames")
		}
	})
}

func TestWorkerOutputConformanceFixtures(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	request := Request{ID: id, Operation: Text, Input: "hello"}
	valid, err := os.ReadFile("testdata/valid-worker-progress.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := Exchange(io.Discard, bytes.NewReader(valid), request, []Capability{Text, Progress}); err != nil || result.Output != "done" {
		t.Fatalf("valid fixture = %#v, %v", result, err)
	}
	for _, path := range []string{"testdata/invalid-worker-order.ndjson", "testdata/invalid-worker-progress.ndjson"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Exchange(io.Discard, bytes.NewReader(body), request, []Capability{Text}); err == nil {
			t.Fatalf("accepted invalid fixture %s", path)
		}
	}
}
