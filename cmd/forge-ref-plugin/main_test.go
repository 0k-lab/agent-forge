package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReferencePluginV1(t *testing.T) {
	id := strings.Repeat("a", 32)
	input := `{"version":"v1","id":"` + id + `","type":"initialize","capabilities":["text"],"limits":{"frame_bytes":1048576,"progress_frames":128,"text_bytes":65536,"progress_text_bytes":1024,"commit_subject_bytes":256}}` + "\n" +
		`{"version":"v1","id":"` + id + `","type":"execute","operation":"text","input":"forge"}` + "\n"
	var output bytes.Buffer
	if err := run(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	want := `{"version":"v1","id":"` + id + `","type":"initialized","capabilities":["text"]}` + "\n" +
		`{"version":"v1","id":"` + id + `","type":"result","output":"FORGE: FORGE"}` + "\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}
