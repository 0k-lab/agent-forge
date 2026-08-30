//go:build linux

package linuxinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-forge/internal/gate"
	"agent-forge/internal/store"
	"agent-forge/internal/worker"
)

func TestReleaseInstallE2E(t *testing.T) {
	assetDir := os.Getenv("AGENT_FORGE_INSTALL_E2E_ASSET_DIR")
	if assetDir == "" {
		t.Skip("release installer E2E assets not supplied")
	}
	version := os.Getenv("AGENT_FORGE_INSTALL_E2E_VERSION")
	commit := os.Getenv("AGENT_FORGE_INSTALL_E2E_COMMIT")
	anchor := os.Getenv("AGENT_FORGE_INSTALL_E2E_SHA256SUMS_SHA256")
	if version == "" || commit == "" || anchor == "" {
		t.Fatal("release installer E2E identity is incomplete")
	}
	manifest, err := os.ReadFile(filepath.Join(assetDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	if got := hex.EncodeToString(sum[:]); got != anchor {
		t.Fatalf("fixture trust anchor = %s, want %s", got, anchor)
	}

	root := os.Getenv("AGENT_FORGE_INSTALL_E2E_ROOT")
	if root == "" {
		root = t.TempDir()
	} else if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	o := Options{
		Version: version, Commit: commit, AssetDir: assetDir, SHA256SUMSSHA256: anchor,
		Root: root, Account: account, Ownership: ownership,
	}
	if err := Install(o); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(root, "opt/agent-forge")
	for _, binary := range []string{"forge", "forge-gate", "forge-worker", "forge-codex-plugin", "forge-ref-plugin"} {
		path := filepath.Join(install, "bin", binary)
		output, runErr := exec.Command(path, "--version").CombinedOutput()
		if runErr != nil || string(output) != fmt.Sprintf("%s %s %s\n", binary, version, commit) {
			t.Fatalf("%s identity = %q, %v", binary, output, runErr)
		}
	}

	gateEnv := parseEnvironmentFile(t, filepath.Join(install, "secrets/gate.env"))
	workerEnv := parseEnvironmentFile(t, filepath.Join(install, "secrets/worker.env"))
	if gateEnv["FORGE_OWNER_TOKEN"] == "" || gateEnv["FORGE_WORKER_TOKEN"] == "" || gateEnv["FORGE_OWNER_TOKEN"] == gateEnv["FORGE_WORKER_TOKEN"] || workerEnv["FORGE_WORKER_TOKEN"] != gateEnv["FORGE_WORKER_TOKEN"] || workerEnv["FORGE_OWNER_TOKEN"] != "" {
		t.Fatal("generated secret split is invalid")
	}
	t.Setenv("FORGE_OWNER_TOKEN", gateEnv["FORGE_OWNER_TOKEN"])
	t.Setenv("FORGE_WORKER_TOKEN", gateEnv["FORGE_WORKER_TOKEN"])
	gateConfig, err := gate.LoadConfig(filepath.Join(install, "etc/gate.json"))
	if err != nil {
		t.Fatalf("generated Gate config: %v", err)
	}
	database, err := store.Open(gateConfig.Database)
	if err != nil {
		t.Fatalf("generated Gate database path: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.LoadConfig(filepath.Join(install, "etc/worker.json")); err != nil {
		t.Fatalf("generated Worker config: %v", err)
	}

	gateLog := &bytes.Buffer{}
	gate := exec.Command(filepath.Join(install, "bin/forge-gate"), "-config", filepath.Join(install, "etc/gate.json"))
	gate.Env = append(os.Environ(), "FORGE_OWNER_TOKEN="+gateEnv["FORGE_OWNER_TOKEN"], "FORGE_WORKER_TOKEN="+gateEnv["FORGE_WORKER_TOKEN"])
	gate.Stdout, gate.Stderr = gateLog, gateLog
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopCommand(t, gate, gateLog)
	waitHTTP(t, "http://127.0.0.1:18080/readyz", "", func(status int, _ []byte) bool { return status == http.StatusOK })

	workerLog := &bytes.Buffer{}
	worker := exec.Command(filepath.Join(install, "bin/forge-worker"), "-config", filepath.Join(install, "etc/worker.json"))
	worker.Env = append(os.Environ(), "FORGE_WORKER_TOKEN="+workerEnv["FORGE_WORKER_TOKEN"])
	worker.Stdout, worker.Stderr = workerLog, workerLog
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopCommand(t, worker, workerLog)
	waitHTTP(t, "http://127.0.0.1:18080/v1/workers/worker-1", gateEnv["FORGE_OWNER_TOKEN"], func(status int, body []byte) bool {
		var state struct {
			Connected bool `json:"connected"`
		}
		return status == http.StatusOK && json.Unmarshal(body, &state) == nil && state.Connected
	})

	gateSecret := filepath.Join(install, "secrets/gate.env")
	before, err := os.Stat(gateSecret)
	if err != nil {
		t.Fatal(err)
	}
	beforeBody, err := os.ReadFile(gateSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(o); err != nil {
		t.Fatalf("same-version no-op: %v", err)
	}
	after, err := os.Stat(gateSecret)
	if err != nil {
		t.Fatal(err)
	}
	afterBody, err := os.ReadFile(gateSecret)
	if err != nil || !os.SameFile(before, after) || !bytes.Equal(beforeBody, afterBody) {
		t.Fatal("same-version install rewrote secrets")
	}

	gateUnit := read(t, filepath.Join(install, "systemd/agent-forge-gate.service"))
	workerUnit := read(t, filepath.Join(install, "systemd/agent-forge-worker.service"))
	for _, invariant := range []string{"NoNewPrivileges=yes", "CapabilityBoundingSet=", "ProtectSystem=strict", "ProtectHome=yes", "ProtectProc=invisible", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6"} {
		if !strings.Contains(gateUnit, invariant) || !strings.Contains(workerUnit, invariant) {
			t.Fatalf("systemd hardening missing: %s", invariant)
		}
	}
	if !strings.Contains(workerUnit, "Requires=agent-forge-gate.service") || !strings.Contains(workerUnit, "After=agent-forge-gate.service") || strings.Contains(workerUnit, "FORGE_OWNER_TOKEN") {
		t.Fatal("Worker unit ordering or secret isolation is invalid")
	}
}

func parseEnvironmentFile(t *testing.T, name string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(read(t, name)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || value == "" || result[key] != "" {
			t.Fatalf("invalid environment line in %s", name)
		}
		result[key] = value
	}
	return result
}

func waitHTTP(t *testing.T, url, token string, accept func(int, []byte) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if readErr == nil && accept(response.StatusCode, body) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("readiness timeout: %s", url)
}

func stopCommand(t *testing.T, command *exec.Cmd, logs *bytes.Buffer) {
	t.Helper()
	if command.Process == nil || command.ProcessState != nil {
		return
	}
	_ = command.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("process shutdown: %v\n%s", err, logs.String())
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		<-done
		t.Errorf("process shutdown timeout\n%s", logs.String())
	}
}
