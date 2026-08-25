package pluginprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunTextProcess(t *testing.T) {
	plugin := pythonPlugin(t, `
import json,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
request=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"result","output":request["input"].upper()},separators=(",",":")),flush=True)
`)
	result, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "hello"}, Options{Timeout: time.Second, OutputBytes: 1 << 20})
	if err != nil || result.Output != "HELLO" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestRunWorkspaceProcess(t *testing.T) {
	plugin := pythonPlugin(t, `
import json,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["workspace_edit","commit_subject"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"result"},separators=(",",":")),flush=True)
`)
	_, err := Run(context.Background(), []string{plugin}, Request{Operation: WorkspaceEdit, Workspace: "/work", Instruction: "edit", TimeoutMS: 1000}, Options{Timeout: time.Second, OutputBytes: 1 << 20, Capabilities: []Capability{CommitSubject}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunFailureRequiresCleanCompletion(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "cleaned")
		plugin := failurePlugin(t, `import time; time.sleep(.1); pathlib.Path(`+strconv.Quote(marker)+`).write_text("done")`)
		_, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20})
		if !errors.Is(err, ErrOperation) {
			t.Fatalf("Run = %v, want operation error", err)
		}
		if body, readErr := os.ReadFile(marker); readErr != nil || string(body) != "done" {
			t.Fatalf("cleanup marker = %q, %v", body, readErr)
		}
	})

	for name, tail := range map[string]string{
		"trailing junk":      `print("junk",flush=True)`,
		"second terminal":    `print(json.dumps({"version":"v1","id":init["id"],"type":"failure","category":"execution_failed"},separators=(",",":")),flush=True)`,
		"nonzero exit":       `raise SystemExit(7)`,
		"output overrun":     `sys.stderr.write("x"*2048); sys.stderr.flush()`,
		"completion timeout": `import time; time.sleep(2)`,
	} {
		t.Run(name, func(t *testing.T) {
			plugin := failurePlugin(t, tail)
			limit := int64(1 << 20)
			if name == "output overrun" {
				limit = 1024
			}
			_, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 3 * time.Second, OutputBytes: limit})
			if err == nil || errors.Is(err, ErrOperation) || errors.Is(err, ErrCancelled) {
				t.Fatalf("Run = %v, want protocol error", err)
			}
		})
	}
}

func TestRunEnforcesHardAggregateOutputLimit(t *testing.T) {
	const limit = int64(1024)
	for name, extra := range map[string]int{"exact": 0, "one byte over": 1} {
		t.Run(name, func(t *testing.T) {
			plugin := pythonPlugin(t, `
import json,sys
init=json.loads(sys.stdin.readline())
ready=json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":"))+"\n"
result=json.dumps({"version":"v1","id":init["id"],"type":"result","output":"ok"},separators=(",",":"))+"\n"
sys.stdout.write(ready); sys.stdout.flush()
json.loads(sys.stdin.readline())
sys.stderr.write("x"*(`+strconv.FormatInt(limit, 10)+`-len(ready.encode())-len(result.encode())+`+strconv.Itoa(extra)+`)); sys.stderr.flush()
sys.stdout.write(result); sys.stdout.flush()
`)
			result, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 5 * time.Second, OutputBytes: limit})
			if extra == 0 && (err != nil || result.Output != "ok") {
				t.Fatalf("exact limit = %#v, %v", result, err)
			}
			if extra != 0 && (err == nil || errors.Is(err, ErrOperation) || errors.Is(err, ErrCancelled)) {
				t.Fatalf("over limit = %v, want protocol error", err)
			}
		})
	}

	t.Run("endless stderr", func(t *testing.T) {
		plugin := pythonPlugin(t, `
import json,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
while True:
 sys.stderr.write("x"*4096); sys.stderr.flush()
`)
		started := time.Now()
		_, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 5 * time.Second, OutputBytes: 256})
		if err == nil || errors.Is(err, ErrCancelled) || time.Since(started) >= time.Second {
			t.Fatalf("Run = %v after %v", err, time.Since(started))
		}
	})

	t.Run("after failure", func(t *testing.T) {
		plugin := failurePlugin(t, `sys.stderr.write("x"*2048); sys.stderr.flush()`)
		_, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 5 * time.Second, OutputBytes: 128})
		if err == nil || errors.Is(err, ErrOperation) || errors.Is(err, ErrCancelled) {
			t.Fatalf("Run = %v, want protocol error", err)
		}
	})

	t.Run("before failure", func(t *testing.T) {
		plugin := pythonPlugin(t, `
import json,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
sys.stderr.write("x"*2048); sys.stderr.flush()
print(json.dumps({"version":"v1","id":init["id"],"type":"failure","category":"execution_failed"},separators=(",",":")),flush=True)
`)
		_, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 5 * time.Second, OutputBytes: 128})
		if err == nil || errors.Is(err, ErrOperation) || errors.Is(err, ErrCancelled) {
			t.Fatalf("Run = %v, want protocol error", err)
		}
	})
}

func TestRunCancellationInterruptsBlockedExecuteWrite(t *testing.T) {
	modes := []string{"ordinary"}
	if runtime.GOOS != "windows" {
		modes = append(modes, "setsid")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child-pid")
			newSession := "False"
			if mode == "setsid" {
				newSession = "True"
			}
			plugin := pythonPlugin(t, `
import json,pathlib,subprocess,sys,time
init=json.loads(sys.stdin.readline())
child=subprocess.Popen(["sleep","10"],start_new_session=`+newSession+`)
stat=pathlib.Path("/proc/"+str(child.pid)+"/stat")
pathlib.Path(`+strconv.Quote(pidFile)+`).write_text(str(child.pid)+(" "+stat.read_text().split()[21] if stat.exists() else ""))
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["workspace_edit"]},separators=(",",":")),flush=True)
pathlib.Path(`+strconv.Quote(pidFile+".initialized")+`).write_text("ready")
time.sleep(10)
`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			started := time.Now()
			go func() {
				_, err := Run(ctx, []string{plugin}, Request{Operation: WorkspaceEdit, Workspace: "/work", Instruction: strings.Repeat("\x01", MaxTextBytes), TimeoutMS: 1000}, Options{Timeout: 5 * time.Second, OutputBytes: 1 << 20})
				done <- err
			}()
			child := waitForChild(t, pidFile)
			waitForFile(t, pidFile+".initialized")
			cancel()
			err := <-done
			if !errors.Is(err, ErrCancelled) || time.Since(started) >= time.Second {
				t.Fatalf("Run = %v after %v, want bounded cancellation", err, time.Since(started))
			}
			deadline := time.Now().Add(250 * time.Millisecond)
			for childMarkerRunning(pidFile) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if childMarkerRunning(pidFile) {
				_ = child.Kill()
				t.Fatal("child remains alive after cancellation")
			}
		})
	}
}

func TestRunCancellationInterruptsMarkerSynchronizedBlockedExecuteWrite(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "initialized")
	plugin := pythonPlugin(t, `
import json,pathlib,sys,time
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["workspace_edit"]},separators=(",",":")),flush=True)
pathlib.Path(`+strconv.Quote(marker)+`).write_text("initialized")
time.sleep(10)
`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, []string{plugin}, Request{Operation: WorkspaceEdit, Workspace: "/work", Instruction: strings.Repeat("\x01", MaxTextBytes), TimeoutMS: 1000}, Options{Timeout: 5 * time.Second, OutputBytes: 1 << 20})
		done <- err
	}()
	waitForFile(t, marker)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Run = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunCancellationDuringStartup(t *testing.T) {
	plugin := pythonPlugin(t, `
import time
time.sleep(10)
`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Run = %v, want cancellation", err)
	}
}

func TestRunAlreadyCancelledDoesNotStartPlugin(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	plugin := pythonPlugin(t, `
import pathlib
pathlib.Path(`+strconv.Quote(marker)+`).write_text("started")
`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Run = %v, want cancellation", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cancelled Run started plugin: %v", err)
	}
}

func TestRunCancellationDuringTerminalDrain(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "terminal")
	plugin := pythonPlugin(t, `
import json,pathlib,sys,time
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"result","output":"ok"},separators=(",",":")),flush=True)
pathlib.Path(`+strconv.Quote(marker)+`).write_text("terminal")
time.sleep(.2)
`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20})
		done <- err
	}()
	waitForFile(t, marker)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Run = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("marker was not written")
}

func failurePlugin(t *testing.T, tail string) string {
	t.Helper()
	return pythonPlugin(t, `
import json,pathlib,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"failure","category":"execution_failed"},separators=(",",":")),flush=True)
`+tail)
}

func TestRunRejectsUncleanCompletion(t *testing.T) {
	for name, tail := range map[string]string{
		"blank":     `print()` + "\n",
		"junk":      `print("junk")` + "\n",
		"nonzero":   `sys.exit(7)` + "\n",
		"hang":      `import time; time.sleep(2)` + "\n",
		"stderr":    `sys.stderr.write("x"*2000000)` + "\n",
		"duplicate": `print(json.dumps({"version":"v1","id":init["id"],"type":"result","output":"again"},separators=(",",":")))` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			plugin := pythonPlugin(t, `
import json,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
request=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"result","output":"ok"},separators=(",",":")),flush=True)
`+tail)
			started := time.Now()
			_, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 100 * time.Millisecond, OutputBytes: 1 << 20})
			if err == nil || time.Since(started) > time.Second {
				t.Fatalf("err = %v, elapsed = %v", err, time.Since(started))
			}
		})
	}
}

func TestRunSendsOneNegotiatedCancel(t *testing.T) {
	dir := t.TempDir()
	ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "cancel")
	plugin := pythonPlugin(t, `
import json,pathlib,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["workspace_edit","cancel"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
pathlib.Path(`+strconv.Quote(ready)+`).write_text("ready")
cancel=json.loads(sys.stdin.readline())
pathlib.Path(`+strconv.Quote(marker)+`).write_text(json.dumps(cancel))
print(json.dumps({"version":"v1","id":init["id"],"type":"failure","category":"cancelled"},separators=(",",":")),flush=True)
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, []string{plugin}, Request{Operation: WorkspaceEdit, Workspace: "/work", Instruction: "edit", TimeoutMS: 1000}, Options{Timeout: time.Second, OutputBytes: 1 << 20, Capabilities: []Capability{Cancel}})
		done <- err
	}()
	waitForFile(t, ready)
	cancel()
	if err := <-done; !errors.Is(err, ErrCancelled) {
		t.Fatalf("Run = %v, want cancellation", err)
	}
	body, readErr := os.ReadFile(marker)
	var frame cancelFrame
	decodeErr := json.Unmarshal(body, &frame)
	if readErr != nil || decodeErr != nil || frame.Version != Version || frame.Type != "cancel" || !validID(frame.ID) || strings.Count(string(body), `"type": "cancel"`) != 1 {
		t.Fatalf("cancel marker = %q, %v", body, readErr)
	}
}

func TestRunCancellationWinsSuccessRace(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	plugin := pythonPlugin(t, `
import json,pathlib,sys
init=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["workspace_edit","cancel"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
pathlib.Path(`+strconv.Quote(ready)+`).write_text("ready")
json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"result"},separators=(",",":")),flush=True)
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, []string{plugin}, Request{Operation: WorkspaceEdit, Workspace: "/work", Instruction: "edit", TimeoutMS: 1000}, Options{Timeout: time.Second, OutputBytes: 1 << 20, Capabilities: []Capability{Cancel}})
		done <- err
	}()
	waitForFile(t, ready)
	cancel()
	if err := <-done; !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel/success race = %v, want cancelled", err)
	}
}

func TestRunMissingInterpreterIsStartFailure(t *testing.T) {
	plugin := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(plugin, []byte("#!/definitely/missing/interpreter\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20}); !errors.Is(err, ErrStart) {
		t.Fatalf("Run = %v, want start failure", err)
	}
}

func TestRunMissingExecutableIsStartFailure(t *testing.T) {
	plugin := filepath.Join(t.TempDir(), "missing")
	if _, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20}); !errors.Is(err, ErrStart) {
		t.Fatalf("Run = %v, want start failure", err)
	}
}

func TestRunExit127IsNotStartFailure(t *testing.T) {
	plugin := pythonPlugin(t, `raise SystemExit(127)`)
	if _, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: time.Second, OutputBytes: 1 << 20}); err == nil || errors.Is(err, ErrStart) {
		t.Fatalf("Run = %v, want non-start protocol failure", err)
	}
}

func TestRunConcurrentStartupStatusesDoNotCross(t *testing.T) {
	openFDs := -1
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		openFDs = len(entries)
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-interpreter")
	if err := os.WriteFile(missing, []byte("#!/definitely/missing/interpreter\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	exit127 := pythonPlugin(t, `raise SystemExit(127)`)
	var wg sync.WaitGroup
	errs := make(chan string, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plugin := exit127
			wantStart := i%2 == 0
			if wantStart {
				plugin = missing
			}
			_, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 5 * time.Second, OutputBytes: 1 << 20})
			if errors.Is(err, ErrStart) != wantStart {
				errs <- fmt.Sprintf("invocation %d: Run = %T %v, want start failure %t", i, err, err, wantStart)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if openFDs >= 0 {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > openFDs {
			t.Fatalf("open file descriptors = %d, want at most %d", len(entries), openFDs)
		}
	}
}

func TestRunRejectsInvalidRequestBeforeExecutableStarts(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	plugin := pythonPlugin(t, `
import pathlib
pathlib.Path(`+strconv.Quote(marker)+`).write_text("started")
`)
	_, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: strings.Repeat("x", MaxTextBytes+1)}, Options{Timeout: time.Second, OutputBytes: 1 << 20})
	if err == nil {
		t.Fatal("accepted oversized request")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("invalid request started executable: %v", statErr)
	}
}

func TestRunRejectsMalformedWorkerOutput(t *testing.T) {
	ready := `json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":"))`
	for name, body := range map[string]string{
		"malformed":        `sys.stdout.buffer.write(b"{\\n"); sys.stdout.flush()`,
		"invalid UTF-8":    `sys.stdout.buffer.write(b"{\\xff}\\n"); sys.stdout.flush()`,
		"oversized":        `sys.stdout.write("x"*1048577+"\\n"); sys.stdout.flush()`,
		"no LF":            `sys.stdout.write(` + ready + `); sys.stdout.flush()`,
		"wrong version":    `print(json.dumps({"version":"v2","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)`,
		"wrong capability": `print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["progress"]},separators=(",",":")),flush=True)`,
		"wrong order":      `print(json.dumps({"version":"v1","id":init["id"],"type":"result","output":"early"},separators=(",",":")),flush=True)`,
		"missing terminal": `print(` + ready + `,flush=True); sys.stdin.readline()`,
		"malformed terminal": `print(` + ready + `,flush=True); sys.stdin.readline();
sys.stdout.buffer.write(b"{\\n"); sys.stdout.flush()`,
	} {
		t.Run(name, func(t *testing.T) {
			plugin := pythonPlugin(t, "import json,sys\ninit=json.loads(sys.stdin.readline())\n"+body)
			if _, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 300 * time.Millisecond, OutputBytes: 2 << 20}); err == nil {
				t.Fatal("accepted malformed worker output")
			}
		})
	}
}

func TestRunBoundsDrainAfterTerminalWithInheritedPipe(t *testing.T) {
	testInheritedPipeDrain(t, false)
}

func TestRunBoundsDrainAfterMalformedOutputWithInheritedPipe(t *testing.T) {
	testInheritedPipeDrain(t, true)
}

func testInheritedPipeDrain(t *testing.T, malformed bool) {
	t.Helper()
	modes := []string{"ordinary"}
	if runtime.GOOS != "windows" {
		modes = append(modes, "setsid")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child-pid")
			newSession := "False"
			if mode == "setsid" {
				newSession = "True"
			}
			terminal := `print("{",flush=True)`
			if !malformed {
				terminal = `print(json.dumps({"version":"v1","id":init["id"],"type":"initialized","capabilities":["text"]},separators=(",",":")),flush=True)
json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":init["id"],"type":"result","output":"ok"},separators=(",",":")),flush=True)`
			}
			plugin := pythonPlugin(t, `
import json,pathlib,subprocess,sys
init=json.loads(sys.stdin.readline())
child=subprocess.Popen(["sleep","10"],start_new_session=`+newSession+`)
pathlib.Path(`+strconv.Quote(pidFile)+`).write_text(str(child.pid))
`+terminal)

			type response struct {
				result Result
				err    error
			}
			done := make(chan response, 1)
			started := time.Now()
			go func() {
				result, err := Run(context.Background(), []string{plugin}, Request{Operation: Text, Input: "x"}, Options{Timeout: 3 * time.Second, OutputBytes: 1 << 20})
				done <- response{result, err}
			}()
			child := waitForChild(t, pidFile)
			t.Cleanup(func() { _ = child.Kill() })

			deadline := time.NewTimer(time.Until(started.Add(time.Second)))
			defer deadline.Stop()
			select {
			case response := <-done:
				if response.err == nil || response.err.Error() != "plugin protocol failed" {
					t.Fatalf("Run = %#v, %v; want protocol error", response.result, response.err)
				}
			case <-deadline.C:
				_ = child.Kill()
				<-done
				t.Fatalf("Run exceeded one-second inherited-pipe drain bound")
			}
		})
	}
}

func waitForChild(t *testing.T, path string) *os.Process {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(body))
			if len(fields) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			pid, parseErr := strconv.Atoi(fields[0])
			if parseErr == nil && pid > 0 {
				process, findErr := os.FindProcess(pid)
				if findErr == nil {
					return process
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("child did not start")
	return nil
}

func childMarkerRunning(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return false
	}
	if len(fields) > 1 {
		stat, err := os.ReadFile(filepath.Join("/proc", fields[0], "stat"))
		if err != nil {
			return false
		}
		current := strings.Fields(string(stat))
		return len(current) >= 22 && current[21] == fields[1] && current[2] != "Z"
	}
	process, _ := os.FindProcess(pid)
	return process.Signal(os.Signal(nil)) == nil
}

func TestWaitForChildRetriesIncompleteMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child-pid")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}()
	if process := waitForChild(t, path); process.Pid != os.Getpid() {
		t.Fatalf("pid = %d, want %d", process.Pid, os.Getpid())
	}
}

func pythonPlugin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env python3\n"+strings.TrimSpace(body)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
