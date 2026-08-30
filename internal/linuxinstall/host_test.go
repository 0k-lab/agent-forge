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
	for _, tc := range []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request)
		ok    bool
	}{
		{"exact", func(w http.ResponseWriter, r *http.Request) {
			challenge := r.URL.Query().Get("challenge")
			digest := sha256.Sum256([]byte(owner))
			mac := hmac.New(sha256.New, digest[:])
			_, _ = mac.Write([]byte("agent-forge/install-ready/v1\x00" + challenge))
			fmt.Fprintf(w, `{"proof":%q,"status":"ready"}`+"\n", base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
		}, true},
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
			err := (HostServiceManager{}).GateReady(owner)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
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
