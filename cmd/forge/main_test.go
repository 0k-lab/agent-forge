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
)

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
