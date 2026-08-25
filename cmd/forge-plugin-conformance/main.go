package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"agent-forge/internal/pluginprotocol"
	"agent-forge/internal/processtree"
)

const testID = "0123456789abcdef0123456789abcdef"

func main() {
	operation := flag.String("operation", "text", "text or workspace_edit")
	flag.Parse()
	if flag.NArg() == 0 || *operation != "text" && *operation != "workspace_edit" {
		fmt.Fprintln(os.Stderr, "usage: forge-plugin-conformance [-operation text|workspace_edit] plugin [arg ...]")
		os.Exit(2)
	}
	failed := false
	run := func(name string, check func() bool) {
		if check() {
			fmt.Println("PASS", name)
		} else {
			fmt.Println("FAIL", name)
			failed = true
		}
	}
	argv := flag.Args()
	run("valid-session", func() bool { return validSession(argv, *operation) })
	for _, item := range invalidScenarios(*operation) {
		item := item
		run(item.name, func() bool { return targetRejects(argv, item.wire, item.stdout) })
	}
	if failed {
		os.Exit(1)
	}
}

type scenario struct{ name, wire, stdout string }

func invalidScenarios(operation string) []scenario {
	init := initialize(operation)
	execute := validExecute(operation, testID)
	ready := initialized(operation)
	contradictory := strings.TrimSuffix(execute, "}\n")
	if operation == "text" {
		contradictory += `,"workspace":"/work","instruction":"edit","timeout_ms":1000}` + "\n"
	} else {
		contradictory += `,"input":"x"}` + "\n"
	}
	typed := strings.Replace(execute, `"input":"conformance"`, `"input":1`, 1)
	oversizedInput := strings.Replace(execute, `"input":"conformance"`, `"input":"`+strings.Repeat("x", pluginprotocol.MaxTextBytes+1)+`"`, 1)
	if operation == "workspace_edit" {
		typed = strings.Replace(execute, `"instruction":"edit"`, `"instruction":1`, 1)
		oversizedInput = strings.Replace(execute, `"instruction":"edit"`, `"instruction":"`+strings.Repeat("x", pluginprotocol.MaxTextBytes+1)+`"`, 1)
	}
	return []scenario{
		{"blank-frame", "\n", ""},
		{"malformed-frame", "{\n", ""},
		{"invalid-utf8", string([]byte{'{', 0xff, '}', '\n'}), ""},
		{"crlf-frame", strings.TrimSuffix(init, "\n") + "\r\n", ""},
		{"non-compact-frame", strings.Replace(init, "{", "{ ", 1), ""},
		{"invalid-id", strings.Replace(init, testID, strings.Repeat("A", 32), 1), ""},
		{"capability-type", strings.Replace(init, `"capabilities":["`+operation+`"]`, `"capabilities":[1]`, 1), ""},
		{"duplicate-capability", strings.Replace(init, `"capabilities":["`+operation+`"]`, `"capabilities":["`+operation+`","`+operation+`"]`, 1), ""},
		{"unknown-field", strings.Replace(init, `,"limits":`, `,"unknown":true,"limits":`, 1), ""},
		{"duplicate-field", strings.Replace(init, `"type":"initialize"`, `"type":"initialize","type":"initialize"`, 1), ""},
		{"unsupported-version", strings.Replace(init, `"version":"v1"`, `"version":"v2"`, 1), ""},
		{"unsupported-capability", initialize("unsupported"), ""},
		{"limit-bool", strings.Replace(init, `"frame_bytes":1048576`, `"frame_bytes":true`, 1), ""},
		{"limit-float", strings.Replace(init, `"frame_bytes":1048576`, `"frame_bytes":1048576.0`, 1), ""},
		{"non-standard-constant", strings.Replace(init, `"frame_bytes":1048576`, `"frame_bytes":NaN`, 1), ""},
		{"wrong-id", init + validExecute(operation, strings.Repeat("f", 32)), ready},
		{"wrong-order", init + strings.Replace(execute, `"type":"execute"`, `"type":"result"`, 1), ready},
		{"unsupported-operation", init + strings.Replace(execute, `"operation":"`+operation+`"`, `"operation":"unsupported"`, 1), ready},
		{"input-type", init + typed, ready},
		{"oversized-input", init + oversizedInput, ready},
		{"execute-unknown-field", init + strings.Replace(execute, "}\n", `,"unknown":true}`+"\n", 1), ready},
		{"oversized-frame", strings.Repeat("x", pluginprotocol.MaxFrameBytes+1) + "\n", ""},
		{"no-lf-frame", strings.TrimSuffix(init, "\n"), ""},
		{"contradictory-execute", init + contradictory, ready},
		{"duplicate-execute", init + strings.Replace(execute, `"type":"execute"`, `"type":"execute","type":"execute"`, 1), ready},
	}
}

func initialize(capability string) string {
	return `{"version":"v1","id":"` + testID + `","type":"initialize","capabilities":["` + capability + `"],"limits":{"frame_bytes":1048576,"progress_frames":128,"text_bytes":65536,"progress_text_bytes":1024,"commit_subject_bytes":256}}` + "\n"
}

func validExecute(operation, id string) string {
	if operation == "workspace_edit" {
		return `{"version":"v1","id":"` + id + `","type":"execute","operation":"workspace_edit","workspace":"/work","instruction":"edit","timeout_ms":1000}` + "\n"
	}
	return `{"version":"v1","id":"` + id + `","type":"execute","operation":"text","input":"conformance"}` + "\n"
}

func initialized(operation string) string {
	return `{"version":"v1","id":"` + testID + `","type":"initialized","capabilities":["` + operation + `"]}` + "\n"
}

func targetRejects(argv []string, wire, expected string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin = strings.NewReader(wire)
	stdout := newCappedBuffer(len(expected) + 1)
	cmd.Stdout, cmd.Stderr = stdout, io.Discard
	err := processtree.Run(ctx, cmd)
	return err != nil && ctx.Err() == nil && !stdout.overflow && stdout.String() == expected
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	buffer := &cappedBuffer{limit: limit}
	buffer.Grow(limit)
	return buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining < len(p) {
		b.overflow = true
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func validSession(argv []string, operation string) bool {
	request := pluginprotocol.Request{Operation: pluginprotocol.Text, Input: "conformance"}
	var capabilities []pluginprotocol.Capability
	if operation == "workspace_edit" {
		workspace, err := os.MkdirTemp("", "forge-plugin-conformance-")
		if err != nil {
			return false
		}
		defer os.RemoveAll(workspace)
		for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Conformance", "-c", "user.email=conformance@example.invalid", "commit", "--allow-empty", "-qm", "base"}} {
			if exec.Command("git", append([]string{"-C", workspace}, args...)...).Run() != nil {
				return false
			}
		}
		request = pluginprotocol.Request{Operation: pluginprotocol.WorkspaceEdit, Workspace: workspace, Instruction: "create answer.txt containing conformance followed by a newline", TimeoutMS: 10_000}
		capabilities = []pluginprotocol.Capability{pluginprotocol.Progress, pluginprotocol.Cancel, pluginprotocol.CommitSubject}
	} else if operation != "text" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := pluginprotocol.Run(ctx, argv, request, pluginprotocol.Options{Timeout: 12 * time.Second, OutputBytes: 1 << 20, Capabilities: capabilities, Environment: os.Environ()})
	if err != nil {
		return false
	}
	if request.Operation == pluginprotocol.Text {
		return result.Output != ""
	}
	status, err := exec.Command("git", "-C", request.Workspace, "status", "--porcelain").Output()
	return err == nil && strings.TrimSpace(string(status)) != "" && (result.CommitSubject == nil || pluginprotocol.ValidateCommitSubject(result.CommitSubject, true) == nil)
}
