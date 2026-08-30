//go:build linux

package linuxinstall

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGateReadyRequiresStrictOwnerBoundProof(t *testing.T) {
	oldURL, oldWindow := gateBaseURL, readinessWindow
	t.Cleanup(func() { gateBaseURL, readinessWindow = oldURL, oldWindow })
	readinessWindow = 20 * time.Millisecond
	owner := "owner-secret"
	version, commit := "v1.2.3", "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request)
		ok    bool
	}{
		{"exact", func(w http.ResponseWriter, r *http.Request) {
			challenge := r.URL.Query().Get("challenge")
			digest := sha256.Sum256([]byte(owner))
			mac := hmac.New(sha256.New, digest[:])
			_, _ = mac.Write([]byte("agent-forge/install-ready/v2\x00" + challenge + "\x00" + version + "\x00" + commit))
			fmt.Fprintf(w, `{"commit":%q,"proof":%q,"status":"ready","version":%q}`+"\n", commit, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), version)
		}, true},
		{"wrong release", func(w http.ResponseWriter, r *http.Request) {
			challenge := r.URL.Query().Get("challenge")
			digest := sha256.Sum256([]byte(owner))
			mac := hmac.New(sha256.New, digest[:])
			_, _ = mac.Write([]byte("agent-forge/install-ready/v2\x00" + challenge + "\x00v9.9.9\x00" + commit))
			fmt.Fprintf(w, `{"commit":%q,"proof":%q,"status":"ready","version":"v9.9.9"}`, commit, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
		}, false},
		{"spoof", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"proof":"wrong","status":"ready"}`))
		}, false},
		{"trailing", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"proof":"wrong","status":"ready"} {}`))
		}, false},
		{"oversized", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 300))) }, false},
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/attacker", http.StatusFound) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(tc.serve))
			defer server.Close()
			gateBaseURL = server.URL
			err := (HostServiceManager{}).GateReady(owner, version, commit)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
	}
}

func TestGateReadyAcceptsCanonicalLegacyV1Proof(t *testing.T) {
	oldURL, oldWindow := gateBaseURL, readinessWindow
	t.Cleanup(func() { gateBaseURL, readinessWindow = oldURL, oldWindow })
	readinessWindow = 20 * time.Millisecond
	owner := "owner-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenge := r.URL.Query().Get("challenge")
		digest := sha256.Sum256([]byte(owner))
		mac := hmac.New(sha256.New, digest[:])
		_, _ = mac.Write([]byte("agent-forge/install-ready/v1\x00" + challenge))
		fmt.Fprintf(w, `{"proof":%q,"status":"ready"}`+"\n", base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	}))
	defer server.Close()
	gateBaseURL = server.URL
	if err := (HostServiceManager{}).GateReady(owner, legacyInstallerVersion, legacyInstallerCommit); err != nil {
		t.Fatal(err)
	}
}

func TestGateReadyRejectsLegacyV1ProofForOtherIdentity(t *testing.T) {
	oldURL, oldWindow := gateBaseURL, readinessWindow
	t.Cleanup(func() { gateBaseURL, readinessWindow = oldURL, oldWindow })
	readinessWindow = 20 * time.Millisecond
	owner := "owner-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenge := r.URL.Query().Get("challenge")
		digest := sha256.Sum256([]byte(owner))
		mac := hmac.New(sha256.New, digest[:])
		_, _ = mac.Write([]byte("agent-forge/install-ready/v1\x00" + challenge))
		fmt.Fprintf(w, `{"proof":%q,"status":"ready"}`+"\n", base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	}))
	defer server.Close()
	gateBaseURL = server.URL
	for _, identity := range []struct{ version, commit string }{
		{"v0.1.4", legacyInstallerCommit},
		{legacyInstallerVersion, "0123456789abcdef0123456789abcdef01234567"},
	} {
		if err := (HostServiceManager{}).GateReady(owner, identity.version, identity.commit); err == nil {
			t.Fatalf("accepted legacy v1 proof for %s %s", identity.version, identity.commit)
		}
	}
}

func TestPrivilegedCommandIgnoresHostilePATHAndSanitizesEnvironment(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("LD_PRELOAD", "/attacker.so")
	cmd := privilegedCommand(systemctlPath, "daemon-reload")
	if cmd.Path != "/usr/bin/systemctl" {
		t.Fatalf("path=%q", cmd.Path)
	}
	joined := strings.Join(cmd.Env, "\n")
	if strings.Contains(joined, os.Getenv("LD_PRELOAD")) || joined != "PATH=/usr/sbin:/usr/bin:/sbin:/bin\nLANG=C\nLC_ALL=C" {
		t.Fatalf("unsafe environment: %q", cmd.Env)
	}
}

func TestValidateServiceAccountRecordRequiresExactIdentity(t *testing.T) {
	valid := "agent-forge:x:991:992::/opt/agent-forge:/usr/sbin/nologin\n"
	if err := validateServiceAccountRecord([]byte(valid), "agent-forge", "/opt/agent-forge", "992"); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"shell":      "agent-forge:x:991:992::/opt/agent-forge:/bin/sh\n",
		"home":       "agent-forge:x:991:992::/tmp:/usr/sbin/nologin\n",
		"group":      "agent-forge:x:991:993::/opt/agent-forge:/usr/sbin/nologin\n",
		"extra line": valid + "other:x:1:1::/:/usr/sbin/nologin\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateServiceAccountRecord([]byte(body), "agent-forge", "/opt/agent-forge", "992"); err == nil {
				t.Fatal("accepted mismatched service account record")
			}
		})
	}
}
