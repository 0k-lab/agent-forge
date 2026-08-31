//go:build linux

package linuxinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	onlineAPIBase       = "https://api.github.com"
	onlineMetadataMax   = 1 << 20
	onlineAssetMax      = 256 << 20
	onlineAssetCountMax = 16
)

var onlineAllowedHosts = map[string]bool{
	"api.github.com": true, "github.com": true, "release-assets.githubusercontent.com": true,
}

type OnlineOptions struct {
	Version                       string
	EnableNow, RunAsRoot, Upgrade bool
}

type onlineAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

type onlineRelease struct {
	TagName         string        `json:"tag_name"`
	TargetCommitish string        `json:"target_commitish"`
	Draft           bool          `json:"draft"`
	Prerelease      bool          `json:"prerelease"`
	Immutable       bool          `json:"immutable"`
	PublishedAt     *time.Time    `json:"published_at"`
	Assets          []onlineAsset `json:"assets"`
}

type onlineGitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type onlineTag struct {
	Name   string          `json:"tag"`
	Object onlineGitObject `json:"object"`
}

type onlineConfig struct {
	apiBase      string
	arch         string
	tempBase     string
	transport    http.RoundTripper
	allowedHosts map[string]bool
	runTarget    func(string, []string, []string) error
}

// InstallOnline resolves one immutable GitHub release and hands verified assets
// to that release's forge binary using the existing offline installer contract.
func InstallOnline(o OnlineOptions) error {
	return installOnline(context.Background(), o, onlineConfig{})
}

func installOnline(ctx context.Context, o OnlineOptions, cfg onlineConfig) error {
	if !versionRE.MatchString(o.Version) {
		return errors.New("invalid online installer arguments")
	}
	if cfg.apiBase == "" {
		cfg.apiBase = onlineAPIBase
	}
	if cfg.arch == "" {
		cfg.arch = runtime.GOARCH
	}
	if cfg.arch != "amd64" && cfg.arch != "arm64" {
		return errors.New("unsupported Linux architecture")
	}
	if cfg.allowedHosts == nil {
		cfg.allowedHosts = onlineAllowedHosts
	}
	if cfg.transport == nil {
		cfg.transport = onlineTransport()
	}
	if cfg.runTarget == nil {
		cfg.runTarget = runOnlineTarget
	}
	client := newOnlineHTTPClient(cfg.transport, cfg.allowedHosts)

	var release onlineRelease
	if err := getOnlineJSON(ctx, client, cfg.apiBase+"/repos/0k-lab/agent-forge/releases/tags/"+o.Version, cfg.allowedHosts, &release); err != nil {
		return err
	}
	if err := validateOnlineRelease(o.Version, release); err != nil {
		return err
	}
	if len(release.Assets) > onlineAssetCountMax {
		return errors.New("release asset count exceeds limit")
	}
	assets, err := selectOnlineAssets(o.Version, cfg.arch, release.Assets)
	if err != nil {
		return err
	}

	var ref struct {
		Object onlineGitObject `json:"object"`
	}
	if err := getOnlineJSON(ctx, client, cfg.apiBase+"/repos/0k-lab/agent-forge/git/ref/tags/"+o.Version, cfg.allowedHosts, &ref); err != nil {
		return err
	}
	if ref.Object.Type != "tag" || !commitRE.MatchString(ref.Object.SHA) {
		return errors.New("release ref is not an annotated tag")
	}
	var tag onlineTag
	if err := getOnlineJSON(ctx, client, cfg.apiBase+"/repos/0k-lab/agent-forge/git/tags/"+ref.Object.SHA, cfg.allowedHosts, &tag); err != nil {
		return err
	}
	commit, err := validateOnlineTag(o.Version, release.TargetCommitish, ref.Object, tag)
	if err != nil {
		return err
	}

	tempBase, err := onlineTempParent(cfg.tempBase)
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp(tempBase, "agent-forge-online-")
	if err != nil {
		return errors.New("online temporary directory failed")
	}
	defer os.RemoveAll(temp)
	if err := os.Chmod(temp, 0o700); err != nil {
		return errors.New("online temporary directory failed")
	}
	for _, name := range requiredOnlineAssetNames(o.Version, cfg.arch) {
		if err := downloadOnlineAsset(ctx, client, assets[name], filepath.Join(temp, name), cfg.allowedHosts); err != nil {
			return err
		}
	}
	manifest, err := readNoFollow(filepath.Join(temp, "SHA256SUMS"), 1<<20)
	if err != nil {
		return errors.New("downloaded SHA256SUMS is invalid")
	}
	anchor := sha256.Sum256(manifest)
	anchorHex := hex.EncodeToString(anchor[:])
	offline := Options{Version: o.Version, Commit: commit, AssetDir: temp, SHA256SUMSSHA256: anchorHex, Arch: cfg.arch}
	archives, err := verifyAssetsIn(offline, temp)
	if err != nil {
		return err
	}
	defer func() {
		for _, archive := range archives {
			_ = os.Remove(archive.file)
		}
	}()
	verified, err := validateArchivesIn(archives, o.Version, commit, temp)
	if err != nil {
		return err
	}
	defer os.RemoveAll(verified)

	argv := []string{"install", "--version", o.Version, "--commit", commit, "--asset-dir", temp, "--sha256sums-sha256", anchorHex}
	if o.EnableNow {
		argv = append(argv, "--enable-now")
	}
	if o.RunAsRoot {
		argv = append(argv, "--run-as-root")
	}
	if o.Upgrade {
		argv = append(argv, "--upgrade")
	}
	return cfg.runTarget(filepath.Join(verified, "forge"), argv, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"})
}

func onlineTempParent(base string) (string, error) {
	production := base == ""
	base = onlineTempBase(base)
	clean := filepath.Clean(base)
	resolved, err := filepath.EvalSymlinks(clean)
	info, statErr := os.Lstat(clean)
	if err != nil || statErr != nil || !filepath.IsAbs(base) || clean != base || resolved != clean || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("unsafe online temporary directory topology")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || production && stat.Uid != 0 {
		return "", errors.New("unsafe online temporary directory owner")
	}
	worldTemp := clean == "/tmp" && stat.Uid == 0 && info.Mode().Perm() == 0o777 && info.Mode()&os.ModeSticky != 0
	if !worldTemp && info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("unsafe online temporary directory mode")
	}
	return clean, nil
}

func onlineTempBase(configured string) string {
	if configured == "" {
		return "/tmp"
	}
	return configured
}

func validateOnlineRelease(version string, release onlineRelease) error {
	if release.TagName != version || release.PublishedAt == nil || release.PublishedAt.IsZero() || release.Draft || release.Prerelease || !release.Immutable {
		return errors.New("release metadata is not trusted")
	}
	return nil
}

func validateOnlineTag(version, target string, ref onlineGitObject, tag onlineTag) (string, error) {
	if ref.Type != "tag" || !commitRE.MatchString(ref.SHA) || tag.Name != version || tag.Object.Type != "commit" || !commitRE.MatchString(tag.Object.SHA) {
		return "", errors.New("annotated release tag is not exact")
	}
	if commitRE.MatchString(target) && target != tag.Object.SHA {
		return "", errors.New("release commit metadata mismatch")
	}
	return tag.Object.SHA, nil
}

func requiredOnlineAssetNames(version, arch string) []string {
	return []string{
		"SHA256SUMS",
		fmt.Sprintf("agent-forge-cli_%s_linux_%s.tar.gz", version, arch),
		fmt.Sprintf("agent-forge-gate_%s_linux_%s.tar.gz", version, arch),
		fmt.Sprintf("agent-forge-worker_%s_linux_%s.tar.gz", version, arch),
	}
}

func selectOnlineAssets(version, arch string, all []onlineAsset) (map[string]onlineAsset, error) {
	if len(all) > onlineAssetCountMax {
		return nil, errors.New("release asset count exceeds limit")
	}
	required := map[string]bool{}
	for _, name := range requiredOnlineAssetNames(version, arch) {
		required[name] = true
	}
	known := map[string]bool{fmt.Sprintf("agent-forge_%s_linux.spdx.json", version): true}
	for _, knownArch := range []string{"amd64", "arm64"} {
		for _, name := range requiredOnlineAssetNames(version, knownArch)[1:] {
			known[name] = true
		}
	}
	known["SHA256SUMS"] = true
	seenNames, seenIDs := map[string]bool{}, map[int64]bool{}
	out := map[string]onlineAsset{}
	for _, asset := range all {
		if asset.Name == "" || asset.ID < 1 || seenNames[asset.Name] || seenIDs[asset.ID] {
			return nil, errors.New("release assets are ambiguous")
		}
		seenNames[asset.Name], seenIDs[asset.ID] = true, true
		if !known[asset.Name] {
			return nil, errors.New("release asset name is unexpected")
		}
		if !required[asset.Name] {
			continue
		}
		if asset.Size < 1 || asset.Size > onlineAssetMax || !strings.HasPrefix(asset.Digest, "sha256:") || !digestRE.MatchString(strings.TrimPrefix(asset.Digest, "sha256:")) || asset.URL == "" {
			return nil, errors.New("release asset metadata is invalid")
		}
		out[asset.Name] = asset
	}
	if len(out) != len(required) {
		return nil, errors.New("required release assets are missing")
	}
	return out, nil
}

func onlineTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true, DisableCompression: true,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second,
	}
}

func newOnlineHTTPClient(transport http.RoundTripper, allowed map[string]bool) *http.Client {
	if base, ok := transport.(*http.Transport); ok {
		clone := base.Clone()
		clone.Proxy = nil
		clone.DisableCompression = true
		transport = clone
	}
	return &http.Client{
		Transport: transport, Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || validateOnlineURL(req.URL, allowed) != nil {
				return errors.New("online redirect rejected")
			}
			return nil
		},
	}
}

func validateOnlineURL(u *url.URL, allowed map[string]bool) error {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || !(allowed[u.Host] || allowed[u.Hostname()] && (u.Port() == "" || u.Port() == "443")) {
		return errors.New("online URL rejected")
	}
	return nil
}

func getOnlineJSON(ctx context.Context, client *http.Client, raw string, allowed map[string]bool, dst any) error {
	request, err := onlineRequest(ctx, raw, allowed)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("GitHub metadata request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" {
		return errors.New("GitHub metadata response rejected")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, onlineMetadataMax+1))
	if err != nil || len(body) > onlineMetadataMax {
		return errors.New("GitHub metadata body rejected")
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return errors.New("GitHub metadata JSON rejected")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(dst); err != nil {
		return errors.New("GitHub metadata JSON rejected")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("GitHub metadata JSON rejected")
	}
	return nil
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return errors.New("duplicate JSON key")
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func onlineRequest(ctx context.Context, raw string, allowed map[string]bool) (*http.Request, error) {
	u, err := url.Parse(raw)
	if err != nil || validateOnlineURL(u, allowed) != nil {
		return nil, errors.New("online URL rejected")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("online request rejected")
	}
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "agent-forge-installer")
	return request, nil
}

func downloadOnlineAsset(ctx context.Context, client *http.Client, asset onlineAsset, destination string, allowed map[string]bool) error {
	request, err := onlineRequest(ctx, asset.URL, allowed)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("release asset download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" || response.ContentLength > asset.Size {
		return errors.New("release asset response rejected")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("release asset staging failed")
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, asset.Size+1))
	closeErr := file.Close()
	want := strings.TrimPrefix(asset.Digest, "sha256:")
	if copyErr != nil || closeErr != nil || n != asset.Size || hex.EncodeToString(hash.Sum(nil)) != want {
		return errors.New("release asset verification failed")
	}
	return nil
}

func runOnlineTarget(binary string, argv, env []string) error {
	args := append([]string{"-c", `umask 077; exec "$@"`, "forge", binary}, argv...)
	cmd := exec.Command("/bin/sh", args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
