package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestTargetRejectsRequiresExactStdout(t *testing.T) {
	target := pythonTarget(t, `
import sys
sys.stdout.write("junk\n")
sys.stdout.flush()
sys.exit(7)
`)
	if targetRejects([]string{target}, "\n", "") {
		t.Fatal("accepted stdout before nonzero exit")
	}
}

func TestInvalidScenarioNames(t *testing.T) {
	var got []string
	for _, item := range invalidScenarios("text") {
		got = append(got, item.name)
	}
	want := []string{
		"blank-frame", "malformed-frame", "invalid-utf8", "crlf-frame", "non-compact-frame",
		"invalid-id", "capability-type", "duplicate-capability", "unknown-field", "duplicate-field",
		"unsupported-version", "unsupported-capability", "limit-bool", "limit-float", "non-standard-constant",
		"wrong-id",
		"wrong-order", "unsupported-operation", "input-type", "oversized-input", "execute-unknown-field",
		"oversized-frame", "no-lf-frame", "contradictory-execute", "duplicate-execute",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scenario names = %q, want %q", got, want)
	}
}

func TestPythonExampleRejectsInvalidScenarios(t *testing.T) {
	target := []string{pythonExample(t)}
	for _, item := range invalidScenarios("text") {
		t.Run(item.name, func(t *testing.T) {
			cmd := exec.Command(target[0])
			cmd.Stdin = strings.NewReader(item.wire)
			output, err := cmd.Output()
			wantOutput := item.stdout
			if err == nil || string(output) != wantOutput {
				t.Fatalf("exit = %v, stdout = %q, want nonzero and %q", err, output, wantOutput)
			}
		})
	}
}

func TestPythonExampleAcceptsCompactEscapes(t *testing.T) {
	wire := initialize("text") + `{"version":"v1","id":"` + testID + `","type":"execute","operation":"text","input":"a b\t\/\u00e9\u003c"}` + "\n"
	cmd := exec.Command(pythonExample(t))
	cmd.Stdin = strings.NewReader(wire)
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q", output)
	}
	var result struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &result); err != nil || result.Output != "A B\t/É<" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestPythonExampleRejectsUppercaseExpansionOverLimit(t *testing.T) {
	wire := initialize("text") + `{"version":"v1","id":"` + testID + `","type":"execute","operation":"text","input":"` + strings.Repeat("ΐ", 20_000) + `"}` + "\n"
	cmd := exec.Command(pythonExample(t))
	cmd.Stdin = strings.NewReader(wire)
	output, err := cmd.Output()
	if err == nil || string(output) != initialized("text") {
		t.Fatalf("exit = %v, stdout = %q", err, output)
	}
}

func TestFullSuiteRejectsTargetThatWritesBeforeNonzeroExit(t *testing.T) {
	target := pythonTarget(t, `
import subprocess,sys
data=sys.stdin.buffer.read()
if data==b"\n":
    sys.stdout.write('{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"result","output":"bad"}\n')
    sys.stdout.flush()
    sys.exit(7)
result=subprocess.run([`+strconv.Quote(pythonExample(t))+`],input=data,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
sys.stdout.buffer.write(result.stdout)
sys.stdout.buffer.flush()
sys.exit(result.returncode)
`)
	for _, item := range invalidScenarios("text") {
		if item.name != "blank-frame" && !targetRejects([]string{target}, item.wire, item.stdout) {
			t.Fatalf("fixture unexpectedly failed %s", item.name)
		}
	}
	if suiteConforms([]string{target}, "text") {
		t.Fatal("full suite accepted broken target")
	}
}

func suiteConforms(argv []string, operation string) bool {
	if !validSession(argv, operation) {
		return false
	}
	for _, item := range invalidScenarios(operation) {
		if !targetRejects(argv, item.wire, item.stdout) {
			return false
		}
	}
	return true
}

func pythonExample(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../examples/forge-plugin-v1.py")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func pythonTarget(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env python3\n"+strings.TrimSpace(body)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
