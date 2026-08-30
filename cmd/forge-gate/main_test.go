package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/store"
)

func TestHTTPServerBounds(t *testing.T) {
	server := newHTTPServer(http.NotFoundHandler())
	if server.ReadHeaderTimeout <= 0 || server.ReadHeaderTimeout > 10*time.Second ||
		server.ReadTimeout <= 0 || server.ReadTimeout > time.Minute ||
		server.WriteTimeout <= 0 || server.WriteTimeout > time.Minute ||
		server.IdleTimeout <= 0 || server.IdleTimeout > 2*time.Minute ||
		server.MaxHeaderBytes <= 0 || server.MaxHeaderBytes > 1<<20 || shutdownTimeout <= 0 || shutdownTimeout > 30*time.Second {
		t.Fatalf("unbounded server: %#v shutdown=%s", server, shutdownTimeout)
	}
}

func TestGateJSONLifecycleAndValueSafeStartupError(t *testing.T) {
	var output bytes.Buffer
	logger := newJSONLogger(&output)
	logger.Info("gate lifecycle", "component", "gate", "event", "startup")
	logger.Info("gate lifecycle", "component", "gate", "event", "listening")
	writeStartupError(&output, errors.New("open /private/state/forge.db: permission denied"))
	if strings.Contains(output.String(), "/private/state") {
		t.Fatalf("startup output leaked value: %s", output.String())
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d: %s", len(lines), output.String())
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatal(err)
	}
	if last["component"] != "gate" || last["event"] != "startup_failed" || last["failure_code"] != "startup_failed" {
		t.Fatalf("startup error = %#v", last)
	}
}

func TestAlreadyOwnedStartupErrorIsStable(t *testing.T) {
	var output bytes.Buffer
	writeStartupError(&output, store.ErrAlreadyOwned)
	if !strings.Contains(output.String(), `"failure_code":"already_owned"`) || strings.Contains(output.String(), "forge.db") {
		t.Fatalf("startup error = %s", output.String())
	}
}

func TestRunMakesProcessNonDumpableBeforeConfigOrEnvironment(t *testing.T) {
	want := errors.New("hardened before config")
	original := hardenProcess
	hardenProcess = func() error { return want }
	t.Cleanup(func() { hardenProcess = original })
	if err := run([]string{"-config", "/definitely-not-read"}, newJSONLogger(&bytes.Buffer{})); !errors.Is(err, want) {
		t.Fatalf("run error=%v; process hardening was not first", err)
	}
}
