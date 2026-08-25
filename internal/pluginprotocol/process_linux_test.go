//go:build linux

package pluginprotocol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunCancellationWinsTargetStartupRace(t *testing.T) {
	plugin := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(plugin, []byte("#!/definitely/missing/interpreter\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := make([]string, 1024)
	for i := range env {
		env[i] = fmt.Sprintf("PADDING_%d=%s", i, strings.Repeat("x", 1024))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20, Environment: env}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Run = %v, want cancellation", err)
	}
}

func TestRunCleansDetachedDescendantAfterSuccess(t *testing.T) {
	for _, setsid := range []string{"False", "True"} {
		t.Run(map[string]string{"False": "ordinary", "True": "setsid"}[setsid], func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child-pid")
			plugin := pythonPlugin(t, `
import json,os,pathlib,subprocess,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
child=subprocess.Popen(["sleep","30"],start_new_session=`+setsid+`,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
pathlib.Path(`+strconv.Quote(pidFile)+`).write_text(str(child.pid)+" "+pathlib.Path("/proc/"+str(child.pid)+"/stat").read_text().split()[-20])
print(json.dumps({"version":"v1","id":init["id"],"type":"result","output":"ok"},separators=(",",":")),flush=True)
`)
			result, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20})
			if err != nil || result.Output != "ok" {
				t.Fatalf("Run = %#v, %v", result, err)
			}
			body, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Fields(string(body))
			pid, _ := strconv.Atoi(fields[0])
			if sameProcessRunning(pid, fields[1]) {
				if current, readErr := os.ReadFile(filepath.Join("/proc", fields[0], "stat")); readErr == nil && sameProcessRunning(pid, fields[1]) {
					_ = current
					process, _ := os.FindProcess(pid)
					_ = process.Kill()
				}
				t.Fatalf("invocation descendant %d remains alive", pid)
			}
		})
	}
}

func TestRunCleansDoubleForkSetsidDescendantAfterSuccess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	plugin := pythonPlugin(t, `
import json,pathlib,subprocess,sys,time
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
code='''import os,pathlib,subprocess,sys
if os.fork(): os._exit(0)
os.setsid()
child=subprocess.Popen(["sleep","30"],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
pathlib.Path(sys.argv[1]).write_text(str(child.pid)+" "+pathlib.Path("/proc/"+str(child.pid)+"/stat").read_text().split()[-20])
os._exit(0)
'''
subprocess.Popen([sys.executable,"-c",code,`+strconv.Quote(pidFile)+`],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
while not pathlib.Path(`+strconv.Quote(pidFile)+`).exists(): time.sleep(.001)
print(json.dumps({"version":"v1","id":init["id"],"type":"result","output":"ok"},separators=(",",":")),flush=True)
`)
	result, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20})
	if err != nil || result.Output != "ok" {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	body, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(body))
	pid, _ := strconv.Atoi(fields[0])
	if sameProcessRunning(pid, fields[1]) {
		t.Fatalf("double-fork invocation descendant %d remains alive", pid)
	}
}

func TestRunCleansDetachedDescendantAfterFailure(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	plugin := pythonPlugin(t, `
import json,pathlib,subprocess,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
child=subprocess.Popen(["sleep","30"],start_new_session=True,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
pathlib.Path(`+strconv.Quote(pidFile)+`).write_text(str(child.pid)+" "+pathlib.Path("/proc/"+str(child.pid)+"/stat").read_text().split()[-20])
print(json.dumps({"version":"v1","id":init["id"],"type":"failure","category":"execution_failed"},separators=(",",":")),flush=True)
`)
	if _, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20}); !errors.Is(err, ErrOperation) {
		t.Fatalf("Run = %v, want operation failure", err)
	}
	body, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(body))
	pid, _ := strconv.Atoi(fields[0])
	if sameProcessRunning(pid, fields[1]) {
		t.Fatalf("failed invocation descendant %d remains alive", pid)
	}
}

func sameProcessRunning(pid int, start string) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat))
	return len(fields) >= 22 && fields[len(fields)-20] == start && fields[len(fields)-21] != "Z"
}
