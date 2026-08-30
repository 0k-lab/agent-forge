package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-forge/internal/buildinfo"
	"agent-forge/internal/linuxinstall"
)

func TestRollbackCommandRequiresRootNoArgsAndPassesCLIIdentity(t *testing.T) {
	previousRollback, previousUID := rollbackLinux, effectiveUID
	previousVersion, previousCommit := buildinfo.Version, buildinfo.Commit
	t.Cleanup(func() {
		rollbackLinux, effectiveUID = previousRollback, previousUID
		buildinfo.Version, buildinfo.Commit = previousVersion, previousCommit
	})
	buildinfo.Version = "v1.2.4"
	buildinfo.Commit = "89abcdef0123456789abcdef0123456789abcdef"
	effectiveUID = func() int { return 0 }
	var got linuxinstall.RollbackOptions
	rollbackLinux = func(o linuxinstall.RollbackOptions) error { got = o; return nil }
	if err := run([]string{"rollback"}, env(nil), nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got.Version != buildinfo.Version || got.Commit != buildinfo.Commit || got.Arch == "" || got.Account == nil || got.Ownership == nil || got.Services == nil {
		t.Fatalf("rollback options = %#v", got)
	}
	if err := run([]string{"rollback", "extra"}, env(nil), nil, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted rollback arguments")
	}
	effectiveUID = func() int { return 1000 }
	if err := run([]string{"rollback"}, env(nil), nil, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted non-root rollback")
	}
	effectiveUID = func() int { return 0 }
	rollbackLinux = func(linuxinstall.RollbackOptions) error { return linuxinstall.Rollback(linuxinstall.RollbackOptions{}) }
	err := run([]string{"rollback"}, env(nil), nil, io.Discard, io.Discard)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != CodeInvalidInput || cliErr.InstallStage == "" {
		t.Fatalf("rollback error = %#v", err)
	}
}

func TestInstallCommandRequiresOfflineTrustInputsAndExplicitRootMode(t *testing.T) {
	previous := installLinux
	previousUID := effectiveUID
	t.Cleanup(func() { installLinux = previous; effectiveUID = previousUID })
	effectiveUID = func() int { return 0 }
	var got linuxinstall.Options
	installLinux = func(o linuxinstall.Options) error { got = o; return nil }
	args := []string{"install", "--version", "v1.2.3", "--commit", "0123456789abcdef0123456789abcdef01234567", "--asset-dir", "/offline/assets", "--sha256sums-sha256", strings.Repeat("a", 64), "--run-as-root", "--enable-now", "--upgrade"}
	if err := run(args, env(nil), nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got.Version != "v1.2.3" || got.AssetDir != "/offline/assets" || !got.RunAsRoot || !got.EnableNow || !got.Upgrade || got.Account == nil || got.Services == nil {
		t.Fatalf("installer options = %#v", got)
	}
	effectiveUID = func() int { return 1000 }
	if err := run(args, env(nil), nil, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted installer invocation from non-root caller")
	}
	effectiveUID = func() int { return 0 }
	for _, bad := range [][]string{
		{"install", "--version", "latest", "--commit", got.Commit, "--asset-dir", "/offline/assets", "--sha256sums-sha256", strings.Repeat("a", 64)},
		{"install", "--version", "v1.2.3", "--commit", "ABC", "--asset-dir", "relative", "--sha256sums-sha256", "bad"},
	} {
		if err := run(bad, env(nil), nil, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted invalid args: %v", bad)
		}
	}
}

func TestInstallCommandPreservesOnlyCoarseFailureStage(t *testing.T) {
	previous := installLinux
	previousUID := effectiveUID
	t.Cleanup(func() { installLinux = previous; effectiveUID = previousUID })
	effectiveUID = func() int { return 0 }
	installLinux = func(linuxinstall.Options) error { return linuxinstall.Install(linuxinstall.Options{}) }
	err := runInstall([]string{"--version", "v1.2.3", "--commit", "0123456789abcdef0123456789abcdef01234567", "--asset-dir", "/offline/assets", "--sha256sums-sha256", strings.Repeat("a", 64)})
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != CodeInvalidInput || cliErr.InstallStage != "validate" {
		t.Fatalf("error = %#v", err)
	}
}

func TestDoctorCommandRequiresRootAndReportsDistinctUnhealthyResult(t *testing.T) {
	previousDoctor := doctorLinux
	previousUID := effectiveUID
	t.Cleanup(func() { doctorLinux = previousDoctor; effectiveUID = previousUID })

	doctorLinux = func(linuxinstall.DoctorOptions) linuxinstall.DoctorReport {
		return linuxinstall.DoctorReport{Checks: []linuxinstall.DoctorCheck{{ID: "receipt", OK: false}}}
	}
	effectiveUID = func() int { return 0 }
	var out bytes.Buffer
	err := run([]string{"doctor"}, env(nil), nil, &out, io.Discard)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != CodeUnhealthy || out.String() != "FAIL receipt\nRESULT unhealthy\n" || exitStatus(err) != 2 {
		t.Fatalf("error=%#v output=%q exit=%d", err, out.String(), exitStatus(err))
	}

	effectiveUID = func() int { return 1000 }
	if err := run([]string{"doctor"}, env(nil), nil, io.Discard, io.Discard); exitStatus(err) != 1 {
		t.Fatalf("non-root exit=%d, want usage exit 1", exitStatus(err))
	}
}

func TestDoctorUnhealthyDoesNotAppendGenericFailureOutput(t *testing.T) {
	var stderr bytes.Buffer
	err := fail(CodeUnhealthy)
	if status := reportFailure(&stderr, err); status != 2 || stderr.Len() != 0 {
		t.Fatalf("status=%d stderr=%q", status, stderr.String())
	}
}

func TestSubmitReadsStrictTaskAndUsesEnvironmentCredential(t *testing.T) {
	var requests atomic.Int32
	useHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/jobs" || r.Header.Get("Authorization") != "Bearer owner-secret" {
			t.Fatalf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"input":"work"}` {
			t.Fatalf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"0123456789abcdef0123456789abcdef","status":"pending"}`))
	}))
	getenv := env(map[string]string{"FORGE_GATE_URL": "http://gate.test", "FORGE_OWNER_TOKEN": "owner-secret"})
	var stdout, stderr bytes.Buffer
	if err := run([]string{"submit", "-file", "-"}, getenv, strings.NewReader(`{"input":"work"}`), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"status":"pending"`) || strings.Contains(stdout.String()+stderr.String(), "owner-secret") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	err := run([]string{"submit", "-file", "-"}, getenv, strings.NewReader(`{"input":"work","unknown":true}`), io.Discard, io.Discard)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != CodeInvalidInput || requests.Load() != 1 {
		t.Fatalf("strict error = %#v, requests=%d", err, requests.Load())
	}
}

func TestStatusEventsAndResultUseStrictOwnerRoutes(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	useHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("missing owner authorization")
		}
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `"}`))
	}))
	getenv := env(map[string]string{"FORGE_GATE_URL": "http://gate.test", "FORGE_OWNER_TOKEN": "token"})
	for _, tc := range []struct{ command, suffix string }{{"status", "/status"}, {"events", "/events"}, {"result", "/result"}} {
		t.Run(tc.command, func(t *testing.T) {
			var out bytes.Buffer
			if err := run([]string{tc.command, id}, getenv, nil, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "/v1/jobs/"+id+tc.suffix) {
				t.Fatalf("output = %s", out.String())
			}
		})
	}
	if err := run([]string{"status", "../bad"}, getenv, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted invalid job ID")
	}
}

func TestSubmitForwardsRepositoryIDExactly(t *testing.T) {
	want := `{"repository_id":"agent-forge","base_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","instruction":"private","tests":[]}`
	useHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != want {
			t.Fatalf("body = %s", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"0123456789abcdef0123456789abcdef","status":"pending"}`))
	}))
	if err := run([]string{"submit"}, env(map[string]string{"FORGE_GATE_URL": "http://gate.test", "FORGE_OWNER_TOKEN": "token"}), strings.NewReader(want), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitForwardsSourceReferenceExactly(t *testing.T) {
	want := `{"input":"work","source_ref":"development-board/PVTI_opaque@ready-v3"}`
	useHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != want {
			t.Fatalf("body = %s", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"0123456789abcdef0123456789abcdef","status":"pending"}`))
	}))
	if err := run([]string{"submit"}, env(map[string]string{"FORGE_GATE_URL": "http://gate.test", "FORGE_OWNER_TOKEN": "token"}), strings.NewReader(want), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitRejectsMalformedUnicodeSourceBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	useHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	invalidUTF8 := append([]byte(`{"input":"work","source_ref":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	for name, body := range map[string][]byte{
		"invalid UTF-8":  invalidUTF8,
		"lone surrogate": []byte(`{"input":"work","source_ref":"\ud800"}`),
		"null":           []byte(`{"input":"work","source_ref":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			err := run([]string{"submit"}, env(map[string]string{"FORGE_GATE_URL": "http://gate.test", "FORGE_OWNER_TOKEN": "token"}), bytes.NewReader(body), io.Discard, io.Discard)
			var cliErr *CLIError
			if !errors.As(err, &cliErr) || cliErr.Code != CodeInvalidInput {
				t.Fatalf("error = %#v", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("malformed task reached Gate: %d requests", requests.Load())
	}
}

func TestWaitPrintsTerminalAggregateAndFailsForFailedOrTimeout(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	for _, status := range []string{"succeeded", "failed", "pending"} {
		t.Run(status, func(t *testing.T) {
			useHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/result") {
					_, _ = w.Write([]byte(`{"job":{"id":"` + id + `","status":"` + status + `"},"attempts":[],"events":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"` + id + `","status":"` + status + `"}`))
			}))
			var out bytes.Buffer
			err := run([]string{"wait", "-timeout", "20ms", "-poll", "1ms", id}, env(map[string]string{"FORGE_GATE_URL": "http://gate.test", "FORGE_OWNER_TOKEN": "token"}), nil, &out, io.Discard)
			if status == "succeeded" && (err != nil || !strings.Contains(out.String(), `"attempts"`)) {
				t.Fatalf("success = %q, %v", out.String(), err)
			}
			if status != "succeeded" && err == nil {
				t.Fatalf("%s returned success", status)
			}
		})
	}
}

func TestClientRejectsCrossOriginRedirectAndBoundedErrorBody(t *testing.T) {
	useHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "gate.test" {
			t.Fatal("followed cross-origin redirect")
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://other.test/private", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Repeat("private", maxErrorBytes+1)))
	}))
	c, err := newClient("http://gate.test", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.get("/redirect"); err == nil {
		t.Fatal("followed cross-origin redirect")
	}
	_, err = c.get("/error")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != CodeHTTPFailure || len(cliErr.Error()) > 256 {
		t.Fatalf("bounded error = %#v", err)
	}
}

func TestWaitTimeoutBoundsInFlightHTTPRequest(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	useHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	started := time.Now()
	err := run([]string{"wait", "-timeout", "10ms", "-poll", "1ms", id}, env(map[string]string{"FORGE_GATE_URL": "http://gate.test", "FORGE_OWNER_TOKEN": "token"}), nil, io.Discard, io.Discard)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != CodeTimeout || time.Since(started) > 100*time.Millisecond {
		t.Fatalf("bounded wait = %#v after %s", err, time.Since(started))
	}
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func useHandler(t *testing.T, handler http.Handler) {
	t.Helper()
	previous := httpTransport
	httpTransport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Result(), nil
	})
	t.Cleanup(func() { httpTransport = previous })
}
