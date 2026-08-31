//go:build linux

package linuxinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOnlineReleaseAndAnnotatedTagTrust(t *testing.T) {
	goodRelease := onlineRelease{TagName: testVersion, Immutable: true, Assets: []onlineAsset{}}
	for name, mutate := range map[string]func(*onlineRelease){
		"draft":      func(r *onlineRelease) { r.Draft = true },
		"prerelease": func(r *onlineRelease) { r.Prerelease = true },
		"mutable":    func(r *onlineRelease) { r.Immutable = false },
		"wrong tag":  func(r *onlineRelease) { r.TagName = "v1.2.4" },
		"unpublished": func(r *onlineRelease) {
			r.PublishedAt = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := goodRelease
			now := time.Now()
			r.PublishedAt = &now
			mutate(&r)
			if err := validateOnlineRelease(testVersion, r); err == nil {
				t.Fatalf("accepted %s release", name)
			}
		})
	}
	now := time.Now()
	goodRelease.PublishedAt = &now
	if err := validateOnlineRelease(testVersion, goodRelease); err != nil {
		t.Fatal(err)
	}

	goodRef := onlineGitObject{Type: "tag", SHA: strings.Repeat("a", 40)}
	goodTag := onlineTag{Name: testVersion, Object: onlineGitObject{Type: "commit", SHA: testCommit}}
	for _, tc := range []struct {
		name   string
		ref    onlineGitObject
		tag    onlineTag
		target string
	}{
		{"lightweight", onlineGitObject{Type: "commit", SHA: testCommit}, goodTag, ""},
		{"nested", goodRef, onlineTag{Name: testVersion, Object: onlineGitObject{Type: "tag", SHA: testCommit}}, ""},
		{"wrong tag", goodRef, onlineTag{Name: "v1.2.4", Object: goodTag.Object}, ""},
		{"malformed commit", goodRef, onlineTag{Name: testVersion, Object: onlineGitObject{Type: "commit", SHA: strings.ToUpper(testCommit)}}, ""},
		{"commit mismatch", goodRef, goodTag, strings.Repeat("b", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateOnlineTag(testVersion, tc.target, tc.ref, tc.tag); err == nil {
				t.Fatalf("accepted %s tag metadata", tc.name)
			}
		})
	}
	if got, err := validateOnlineTag(testVersion, "main", goodRef, goodTag); err != nil || got != testCommit {
		t.Fatalf("branch target_commitish should not assert trust: commit=%q err=%v", got, err)
	}
}

func TestOnlineAssetMetadataIsStrictAndArchitectureSpecific(t *testing.T) {
	assets := validOnlineAssets(t, "amd64")
	want := []string{"SHA256SUMS", "agent-forge-cli_" + testVersion + "_linux_amd64.tar.gz", "agent-forge-gate_" + testVersion + "_linux_amd64.tar.gz", "agent-forge-worker_" + testVersion + "_linux_amd64.tar.gz"}
	got, err := selectOnlineAssets(testVersion, "amd64", assets)
	if err != nil || !slices.Equal(sortedOnlineAssetNames(got), want) {
		t.Fatalf("assets=%v err=%v", sortedOnlineAssetNames(got), err)
	}
	arm, err := selectOnlineAssets(testVersion, "arm64", validOnlineAssets(t, "arm64"))
	if err != nil || !strings.Contains(strings.Join(sortedOnlineAssetNames(arm), " "), "linux_arm64") {
		t.Fatalf("arm64 assets=%v err=%v", sortedOnlineAssetNames(arm), err)
	}

	for name, mutate := range map[string]func([]onlineAsset) []onlineAsset{
		"duplicate name": func(a []onlineAsset) []onlineAsset { return append(a, a[0]) },
		"duplicate id": func(a []onlineAsset) []onlineAsset {
			a[1].ID = a[0].ID
			return a
		},
		"absent digest":    func(a []onlineAsset) []onlineAsset { a[0].Digest = ""; return a },
		"unknown digest":   func(a []onlineAsset) []onlineAsset { a[0].Digest = "sha512:" + strings.Repeat("a", 64); return a },
		"uppercase digest": func(a []onlineAsset) []onlineAsset { a[0].Digest = "sha256:" + strings.Repeat("A", 64); return a },
		"malformed digest": func(a []onlineAsset) []onlineAsset { a[0].Digest = "sha256:bad"; return a },
		"missing size":     func(a []onlineAsset) []onlineAsset { a[0].Size = 0; return a },
	} {
		t.Run(name, func(t *testing.T) {
			copy := append([]onlineAsset(nil), assets...)
			if _, err := selectOnlineAssets(testVersion, "amd64", mutate(copy)); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestOnlineRedirectPolicy(t *testing.T) {
	allowed := map[string]bool{"api.github.com": true, "github.com": true, "release-assets.githubusercontent.com": true}
	for _, raw := range []string{
		"http://github.com/file", "https://evil.example/file", "https://user@github.com/file",
		"https://github.com:444/file", "https://github.com/file#fragment",
	} {
		u, _ := url.Parse(raw)
		if err := validateOnlineURL(u, allowed); err == nil {
			t.Fatalf("accepted redirect %s", raw)
		}
	}
	u, _ := url.Parse("https://github.com:443/file")
	if err := validateOnlineURL(u, allowed); err != nil {
		t.Fatal(err)
	}
}

func TestOnlineMetadataRejectsDuplicateJSONKeys(t *testing.T) {
	if err := rejectDuplicateJSONKeys([]byte(`{"tag_name":"v1.2.3","tag_name":"v1.2.3"}`)); err == nil {
		t.Fatal("accepted duplicate JSON key")
	}
	if err := rejectDuplicateJSONKeys([]byte(`{"outer":[{"id":1}],"ok":true}`)); err != nil {
		t.Fatal(err)
	}
}

func TestOnlineInstallVerificationHandoffAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*onlineFixture)
	}{
		{"success", nil},
		{"size mismatch", func(f *onlineFixture) { f.assets[0].Size++ }},
		{"body overrun", func(f *onlineFixture) { f.bodies[f.assets[0].Name] = append(f.bodies[f.assets[0].Name], 'x') }},
		{"api checksum mismatch", func(f *onlineFixture) { f.assets[1].Digest = "sha256:" + strings.Repeat("0", 64) }},
		{"manifest checksum mismatch", func(f *onlineFixture) { f.bodies[f.assets[1].Name][20] ^= 1; f.resetDigest(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newOnlineFixture(t, "amd64")
			if tc.mutate != nil {
				tc.mutate(fixture)
			}
			server := httptest.NewTLSServer(fixture)
			defer server.Close()
			fixture.base = server.URL
			temp := t.TempDir()
			if err := os.Chmod(temp, 0o700); err != nil {
				t.Fatal(err)
			}
			var calls int
			var argv, env []string
			privateModes := true
			err := installOnline(context.Background(), OnlineOptions{Version: testVersion, EnableNow: true, RunAsRoot: true, Upgrade: true}, onlineConfig{
				apiBase: server.URL, arch: "amd64", tempBase: temp, transport: server.Client().Transport,
				allowedHosts: map[string]bool{server.Listener.Addr().String(): true},
				runTarget: func(_ string, gotArgv, gotEnv []string) error {
					calls++
					argv, env = gotArgv, gotEnv
					assetDir := gotArgv[6]
					info, statErr := os.Stat(assetDir)
					privateModes = statErr == nil && info.Mode().Perm() == 0o700
					for _, name := range requiredOnlineAssetNames(testVersion, "amd64") {
						info, statErr = os.Stat(filepath.Join(assetDir, name))
						privateModes = privateModes && statErr == nil && info.Mode().Perm() == 0o600
					}
					return nil
				},
			})
			if tc.name == "success" {
				if err != nil || calls != 1 || !privateModes {
					t.Fatalf("err=%v calls=%d privateModes=%v", err, calls, privateModes)
				}
				want := []string{"install", "--version", testVersion, "--commit", testCommit, "--asset-dir"}
				if len(argv) < 9 || !slices.Equal(argv[:6], want) || argv[7] != "--sha256sums-sha256" || argv[8] != fixture.manifestDigest || !slices.Equal(argv[9:], []string{"--enable-now", "--run-as-root", "--upgrade"}) {
					t.Fatalf("child argv=%q", argv)
				}
				if !slices.Equal(env, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}) {
					t.Fatalf("child env=%q", env)
				}
			} else if err == nil || calls != 0 {
				t.Fatalf("failed verification err=%v calls=%d", err, calls)
			}
			entries, readErr := os.ReadDir(temp)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("temporary cleanup entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestOnlineTempBaseIgnoresEnvironmentAndRejectsUnsafeParent(t *testing.T) {
	hostile := t.TempDir()
	t.Setenv("TMPDIR", hostile)
	if got := onlineTempBase(""); got != "/tmp" {
		t.Fatalf("production temp parent=%q", got)
	}

	unsafe := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(unsafe, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := onlineTempParent(unsafe); err == nil {
		t.Fatal("accepted unsafe online temp parent")
	}
}

func TestOnlineClientTimeoutRedirectLimitAndIgnoresProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)

	redirects := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("/next?n=%s", r.URL.Query().Get("n")+"x"), http.StatusFound)
	}))
	defer redirects.Close()
	client := newOnlineHTTPClient(redirects.Client().Transport, map[string]bool{redirects.Listener.Addr().String(): true})
	response, err := client.Get(redirects.URL + "/next")
	if err == nil {
		response.Body.Close()
		t.Fatal("redirect limit was not enforced")
	}
	if proxyCalls.Load() != 0 {
		t.Fatal("environment proxy was used")
	}

	blocked := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blocked.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, blocked.URL, nil)
	client = newOnlineHTTPClient(blocked.Client().Transport, map[string]bool{blocked.Listener.Addr().String(): true})
	if _, err := client.Do(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}
}

func TestOnlineInstallCancellationCleansPrivateDownload(t *testing.T) {
	fixture := newOnlineFixture(t, "amd64")
	fixture.blockName = "SHA256SUMS"
	server := httptest.NewTLSServer(fixture)
	defer server.Close()
	fixture.base = server.URL
	temp := t.TempDir()
	if err := os.Chmod(temp, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	calls := 0
	err := installOnline(ctx, OnlineOptions{Version: testVersion}, onlineConfig{
		apiBase: server.URL, arch: "amd64", tempBase: temp, transport: server.Client().Transport,
		allowedHosts: map[string]bool{server.Listener.Addr().String(): true},
		runTarget:    func(string, []string, []string) error { calls++; return nil },
	})
	entries, readErr := os.ReadDir(temp)
	if err == nil || calls != 0 || readErr != nil || len(entries) != 0 {
		t.Fatalf("err=%v calls=%d cleanup=%v readErr=%v", err, calls, entries, readErr)
	}
}

type onlineFixture struct {
	t              *testing.T
	base           string
	blockName      string
	assets         []onlineAsset
	bodies         map[string][]byte
	manifestDigest string
}

func newOnlineFixture(t *testing.T, arch string) *onlineFixture {
	dir, anchor := releaseFixture(t, nil)
	f := &onlineFixture{t: t, bodies: map[string][]byte{}, manifestDigest: anchor}
	for i, name := range requiredOnlineAssetNames(testVersion, arch) {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		f.bodies[name] = body
		f.assets = append(f.assets, onlineAsset{ID: int64(i + 1), Name: name, Size: int64(len(body)), Digest: "sha256:" + hex.EncodeToString(sum[:]), URL: "https://github.com/download/" + name})
	}
	return f
}

func (f *onlineFixture) resetDigest(index int) {
	sum := sha256.Sum256(f.bodies[f.assets[index].Name])
	f.assets[index].Digest = "sha256:" + hex.EncodeToString(sum[:])
}

func (f *onlineFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	switch r.URL.Path {
	case "/repos/0k-lab/agent-forge/releases/tags/" + testVersion:
		assets := append([]onlineAsset(nil), f.assets...)
		for i := range assets {
			assets[i].URL = f.base + "/assets/" + assets[i].Name
		}
		_ = json.NewEncoder(w).Encode(onlineRelease{TagName: testVersion, TargetCommitish: testCommit, Immutable: true, PublishedAt: &now, Assets: assets})
	case "/repos/0k-lab/agent-forge/git/ref/tags/" + testVersion:
		_ = json.NewEncoder(w).Encode(struct {
			Object onlineGitObject `json:"object"`
		}{onlineGitObject{Type: "tag", SHA: strings.Repeat("a", 40)}})
	case "/repos/0k-lab/agent-forge/git/tags/" + strings.Repeat("a", 40):
		_ = json.NewEncoder(w).Encode(onlineTag{Name: testVersion, Object: onlineGitObject{Type: "commit", SHA: testCommit}})
	default:
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if name == f.blockName {
			<-r.Context().Done()
			return
		}
		body, ok := f.bodies[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}
}

func validOnlineAssets(t *testing.T, arch string) []onlineAsset {
	t.Helper()
	return newOnlineFixture(t, arch).assets
}

func sortedOnlineAssetNames(assets map[string]onlineAsset) []string {
	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

var _ io.Reader
