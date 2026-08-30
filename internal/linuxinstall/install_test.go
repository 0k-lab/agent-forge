package linuxinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-forge/internal/gate"
	"agent-forge/internal/worker"
)

const (
	testVersion = "v1.2.3"
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
)

type fakeAccount struct {
	calls, validations, rollbacks int
	uid, gid                      int
}

func (f *fakeAccount) Validate(name, home string) (int, int, error) {
	f.validations++
	return f.identity(name, home)
}

func (f *fakeAccount) Ensure(name, home string) (AccountState, error) {
	f.calls++
	uid, gid, err := f.identity(name, home)
	return AccountState{UID: uid, GID: gid}, err
}

func (f *fakeAccount) Rollback(_ string, _ AccountState) error {
	f.rollbacks++
	return nil
}

func (f *fakeAccount) identity(name, home string) (int, int, error) {
	if name != "agent-forge" || home != "/opt/agent-forge" {
		return 0, 0, fmt.Errorf("unexpected account request")
	}
	if f.uid == 0 {
		f.uid = os.Getuid()
		if f.uid == 0 {
			f.uid = 12345
		}
	}
	if f.gid == 0 {
		f.gid = os.Getgid()
		if f.gid == 0 {
			f.gid = 12345
		}
	}
	return f.uid, f.gid, nil
}

type fakeOwnership struct{ owners map[string][2]int }

func newFakeOwnership() *fakeOwnership {
	return &fakeOwnership{owners: map[string][2]int{}}
}

func (f *fakeOwnership) Chown(name string, uid, gid int) error {
	f.owners[ownershipKey(name)] = [2]int{uid, gid}
	return nil
}

func (f *fakeOwnership) Owner(name string) (int, int, error) {
	if owner, ok := f.owners[ownershipKey(name)]; ok {
		return owner[0], owner[1], nil
	}
	return 0, 0, nil
}

func ownershipKey(name string) string {
	clean := filepath.Clean(name)
	if marker := strings.Index(clean, "/.agent-forge.install-"); marker >= 0 {
		rest := clean[marker+1:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			return rest[slash+1:]
		}
		return ""
	}
	if marker := strings.Index(clean, "/opt/agent-forge"); marker >= 0 {
		return strings.TrimPrefix(clean[marker+len("/opt/agent-forge"):], "/")
	}
	return clean
}

type fakeServices struct {
	calls  [][]string
	ready  int
	failAt string
}

func (f *fakeServices) Run(argv ...string) error {
	f.calls = append(f.calls, append([]string(nil), argv...))
	if strings.Join(argv, " ") == f.failAt {
		return errors.New("injected service failure")
	}
	return nil
}

func (f *fakeServices) GateReady(ownerToken string) error {
	if ownerToken == "" {
		return fmt.Errorf("empty owner token")
	}
	f.calls = append(f.calls, []string{"gate-ready"})
	if f.failAt == "gate-ready" {
		return errors.New("injected Gate readiness failure")
	}
	f.ready++
	return nil
}

func (f *fakeServices) WorkerReady(ownerToken string) error {
	if ownerToken == "" {
		return fmt.Errorf("empty owner token")
	}
	f.calls = append(f.calls, []string{"worker-ready"})
	if f.failAt == "worker-ready" {
		return errors.New("injected Worker readiness failure")
	}
	f.ready++
	return nil
}

func TestEnableNowStartsInOrderAndChecksConnectedReadiness(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	services := &fakeServices{}
	ownership := newFakeOwnership()
	err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: t.TempDir(), Arch: "amd64", Account: &fakeAccount{}, EnableNow: true, Services: services, Ownership: ownership, Random: strings.NewReader(strings.Repeat("x", 32) + strings.Repeat("y", 32))})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(services.calls))
	for i := range services.calls {
		got[i] = strings.Join(services.calls[i], " ")
	}
	want := []string{"daemon-reload", "enable --now agent-forge-gate.service", "gate-ready", "enable --now agent-forge-worker.service", "worker-ready"}
	if strings.Join(got, "|") != strings.Join(want, "|") || services.ready != 2 {
		t.Fatalf("calls=%v ready=%d", got, services.ready)
	}
}

func TestActivationFailuresPreserveValidStagedInstall(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	for _, point := range []string{"daemon-reload", "enable --now agent-forge-gate.service", "gate-ready", "enable --now agent-forge-worker.service", "worker-ready"} {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			account := &fakeAccount{}
			ownership := newFakeOwnership()
			services := &fakeServices{failAt: point}
			o := Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Services: services, EnableNow: true, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
			if err := Install(o); err == nil {
				t.Fatal("activation failure was not propagated")
			}
			secret := rooted(root, prefix+"/secrets/gate.env")
			before, err := os.ReadFile(secret)
			if err != nil {
				t.Fatal("published installation was not preserved")
			}
			o.EnableNow = false
			o.Services = &fakeServices{}
			if err := Install(o); err != nil {
				t.Fatalf("preserved install failed validation: %v", err)
			}
			after, _ := os.ReadFile(secret)
			if !bytes.Equal(before, after) {
				t.Fatal("validation rerun changed secrets")
			}
		})
	}
}

type failingOwnership struct{ *fakeOwnership }

func (f failingOwnership) Chown(string, int, int) error {
	return errors.New("injected ownership failure")
}

func TestFreshAccountRollsBackOnPrepublicationFailure(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	account := &fakeAccount{}
	account.uid, account.gid = 12345, 12345
	// Mark this identity as installer-created so the transaction owns rollback.
	created := &createdFakeAccount{fakeAccount: account}
	err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: t.TempDir(), Arch: "amd64", Account: created, Ownership: failingOwnership{newFakeOwnership()}, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))})
	if err == nil || created.rollbacks != 1 {
		t.Fatalf("err=%v rollbacks=%d", err, created.rollbacks)
	}
	account2 := &fakeAccount{uid: 12346, gid: 12346}
	created2 := &createdFakeAccount{fakeAccount: account2, rollbackErr: errors.New("injected rollback failure")}
	err = Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: t.TempDir(), Arch: "amd64", Account: created2, Ownership: failingOwnership{newFakeOwnership()}, Random: strings.NewReader(strings.Repeat("c", 32) + strings.Repeat("d", 32))})
	if err == nil || !strings.Contains(err.Error(), "rollback failed") || created2.rollbacks != 1 {
		t.Fatalf("rollback failure not surfaced: err=%v rollbacks=%d", err, created2.rollbacks)
	}
}

type createdFakeAccount struct {
	*fakeAccount
	rollbackErr error
}

func (f *createdFakeAccount) Ensure(name, home string) (AccountState, error) {
	uid, gid, err := f.identity(name, home)
	return AccountState{UID: uid, GID: gid, UserCreated: true, GroupCreated: true}, err
}

func (f *createdFakeAccount) Rollback(_ string, _ AccountState) error {
	f.rollbacks++
	return f.rollbackErr
}

func TestInstallPublishesVerifiedDedicatedAccountLayout(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	root := t.TempDir()
	account := &fakeAccount{}
	services := &fakeServices{}
	ownership := newFakeOwnership()

	err := Install(Options{
		Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor,
		Root: root, Arch: "amd64", Account: account, Services: services, Ownership: ownership,
		Random: strings.NewReader(strings.Repeat("r", 32) + strings.Repeat("s", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(root, "opt/agent-forge")
	for _, name := range []string{"forge", "forge-gate", "forge-worker", "forge-codex-plugin", "forge-ref-plugin"} {
		assertMode(t, filepath.Join(prefix, "bin", name), 0o755)
	}
	assertMode(t, filepath.Join(prefix, "etc/gate.json"), 0o440)
	assertMode(t, filepath.Join(prefix, "etc/worker.json"), 0o440)
	assertOwnership(t, ownership, filepath.Join(prefix, "etc/gate.json"), 0, account.gid)
	assertOwnership(t, ownership, filepath.Join(prefix, "etc/worker.json"), 0, account.gid)
	assertMode(t, filepath.Join(prefix, "secrets/gate.env"), 0o600)
	assertMode(t, filepath.Join(prefix, "secrets/worker.env"), 0o600)
	assertMode(t, filepath.Join(prefix, "install-receipt.json"), 0o400)
	for _, name := range []string{"var/gate/state", "var/repositories", "var/worker/worktrees", "var/worker/runtime"} {
		assertMode(t, filepath.Join(prefix, name), 0o700)
	}
	gateEnv := read(t, filepath.Join(prefix, "secrets/gate.env"))
	workerEnv := read(t, filepath.Join(prefix, "secrets/worker.env"))
	if !strings.Contains(gateEnv, "FORGE_OWNER_TOKEN=") || !strings.Contains(gateEnv, "FORGE_WORKER_TOKEN=") || strings.Contains(workerEnv, "FORGE_OWNER_TOKEN=") {
		t.Fatalf("bad secret split: gate=%q worker=%q", gateEnv, workerEnv)
	}
	workerToken := strings.TrimSpace(strings.TrimPrefix(workerEnv, "FORGE_WORKER_TOKEN="))
	if workerToken == "" || !strings.Contains(gateEnv, "FORGE_WORKER_TOKEN="+workerToken) {
		t.Fatal("worker token does not correspond")
	}
	ownerToken := ""
	for _, line := range strings.Split(gateEnv, "\n") {
		if strings.HasPrefix(line, "FORGE_OWNER_TOKEN=") {
			ownerToken = strings.TrimPrefix(line, "FORGE_OWNER_TOKEN=")
		}
	}
	t.Setenv("FORGE_OWNER_TOKEN", ownerToken)
	t.Setenv("FORGE_WORKER_TOKEN", workerToken)
	if _, err := gate.LoadConfig(filepath.Join(prefix, "etc/gate.json")); err != nil {
		t.Fatalf("generated Gate config: %v", err)
	}
	if _, err := worker.LoadConfig(filepath.Join(prefix, "etc/worker.json")); err != nil {
		t.Fatalf("generated Worker config: %v", err)
	}
	for _, public := range []string{"etc/gate.json", "etc/worker.json", "systemd/agent-forge-gate.service", "systemd/agent-forge-worker.service", "install-receipt.json"} {
		if strings.Contains(read(t, filepath.Join(prefix, public)), workerToken) {
			t.Fatalf("token leaked to %s", public)
		}
	}
	if account.calls != 1 {
		t.Fatalf("account calls = %d", account.calls)
	}
	if len(services.calls) != 0 {
		t.Fatalf("service calls = %#v", services.calls)
	}
	gateLink, err := os.Readlink(filepath.Join(root, "etc/systemd/system/agent-forge-gate.service"))
	wantLink := filepath.Join(root, "opt/agent-forge/systemd/agent-forge-gate.service")
	if err != nil || gateLink != wantLink {
		t.Fatalf("gate link = %q, %v", gateLink, err)
	}
}

func TestInstallRejectsUntrustedManifestBeforeDestinationMutation(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(assets, "SHA256SUMS"), []byte("attacker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", RunAsRoot: true})
	if err == nil || pathExists(filepath.Join(root, "opt")) {
		t.Fatalf("error=%v destination mutated=%v", err, pathExists(filepath.Join(root, "opt")))
	}
}

func pathExists(name string) bool { _, err := os.Lstat(name); return err == nil }

func TestInstallRejectsMalformedManifestWithoutPanicOrMutation(t *testing.T) {
	assets := t.TempDir()
	body := []byte("bad\n")
	if err := os.WriteFile(filepath.Join(assets, "SHA256SUMS"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	root := t.TempDir()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("panic: %v", recovered)
		}
	}()
	if err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: hex.EncodeToString(sum[:]), Root: root, Arch: "amd64", RunAsRoot: true}); err == nil {
		t.Fatal("accepted malformed manifest")
	}
	if pathExists(filepath.Join(root, "opt")) {
		t.Fatal("mutated destination")
	}
}

func TestVerifyAssetsCleansEarlierTemporariesOnLaterFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	assets, anchor := releaseFixture(t, nil)
	gate := filepath.Join(assets, "agent-forge-gate_"+testVersion+"_linux_amd64.tar.gz")
	if err := os.WriteFile(gate, []byte("corrupted after manifest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyAssets(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Arch: "amd64"}); err == nil {
		t.Fatal("accepted corrupted later archive")
	}
	leftovers, err := filepath.Glob(filepath.Join(tmp, "agent-forge-verified-*.tar.gz"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("verified temporary leak: %v err=%v", leftovers, err)
	}
}

func TestInstallRejectsConcatenatedArchiveStreamBeforeMutation(t *testing.T) {
	assets, anchor := releaseFixture(t, func(files map[string][]byte) {
		name := "agent-forge-cli_" + testVersion + "_linux_amd64.tar.gz"
		files[name] = append(files[name], archive(t, "ignored", "cli")...)
	})
	root := t.TempDir()
	err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: &fakeAccount{}, Ownership: newFakeOwnership()})
	if err == nil {
		t.Fatal("accepted concatenated archive stream")
	}
	if pathExists(filepath.Join(root, "opt")) {
		t.Fatal("mutated destination")
	}
}

func TestInstallRejectsWrongBinaryIdentityBeforeMutation(t *testing.T) {
	assets, anchor := releaseFixture(t, func(files map[string][]byte) {
		name := "agent-forge-cli_" + testVersion + "_linux_amd64.tar.gz"
		files[name] = archiveForIdentity(t, strings.TrimSuffix(name, ".tar.gz"), "cli", testVersion, testCommit, "v9.9.9", testCommit)
	})
	root := t.TempDir()
	account := &fakeAccount{}
	err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: newFakeOwnership()})
	if err == nil {
		t.Fatal("accepted wrong binary identity")
	}
	if account.calls != 0 || pathExists(filepath.Join(root, "opt")) {
		t.Fatal("mutated host before binary identity validation")
	}
}

func TestBinaryIdentityBoundsOutputAndKillsForkedProcessGroup(t *testing.T) {
	for name, body := range map[string]string{
		"output flood":   "#!/bin/sh\nwhile :; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done\n",
		"forked timeout": "#!/bin/sh\nsleep 30 & echo $! > \"$0.pid\"; wait\n",
	} {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "identity")
			if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if err := verifyBinaryIdentity(binary, "impossible\n", 150*time.Millisecond); err == nil {
				t.Fatal("accepted hostile identity process")
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("identity verifier did not return promptly")
			}
			if name == "forked timeout" {
				pidText, err := os.ReadFile(binary + ".pid")
				if err != nil {
					t.Fatal(err)
				}
				var pid int
				if _, err := fmt.Sscanf(strings.TrimSpace(string(pidText)), "%d", &pid); err != nil {
					t.Fatal(err)
				}
				time.Sleep(50 * time.Millisecond)
				if err := syscall.Kill(pid, 0); err == nil {
					t.Fatalf("forked child %d survived", pid)
				}
			}
		})
	}
}

func TestSameVersionRerunRejectsTamperedImmutableFileWithoutRewrite(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	root := t.TempDir()
	o := Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: &fakeAccount{}, Ownership: newFakeOwnership(), Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
	if err := Install(o); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "opt/agent-forge/bin/forge")
	if err := os.WriteFile(binary, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(o); err == nil {
		t.Fatal("accepted tampered same-version installation")
	}
	after, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || read(t, binary) != "tampered" {
		t.Fatal("rewrote tampered operator state")
	}
}

func TestSameVersionRerunRejectsTamperedRuntimeDirectoryWithoutRepair(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	root := t.TempDir()
	o := Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: &fakeAccount{}, Ownership: newFakeOwnership(), Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
	if err := Install(o); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "opt/agent-forge/var/gate/state")
	if err := os.Chmod(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(o); err == nil {
		t.Fatal("accepted tampered runtime directory")
	}
	assertMode(t, state, 0o755)
}

func TestSameVersionRerunRevalidatesDedicatedAccountIdentity(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	root := t.TempDir()
	first := &fakeAccount{}
	o := Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: first, Ownership: newFakeOwnership(), Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
	if err := Install(o); err != nil {
		t.Fatal(err)
	}
	mismatch := &fakeAccount{uid: first.uid + 1, gid: first.gid + 1}
	o.Account = mismatch
	if err := Install(o); err == nil {
		t.Fatal("accepted mismatched dedicated account on rerun")
	}
	if mismatch.calls != 0 || mismatch.validations != 1 {
		t.Fatalf("dedicated account mutations/checks = %d/%d, want 0/1", mismatch.calls, mismatch.validations)
	}
}

func TestSameVersionRerunIsValidationOnlyAndRequiresExistingLinks(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	root := t.TempDir()
	account := &fakeAccount{}
	services := &fakeServices{}
	o := Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Services: services, Ownership: newFakeOwnership(), Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
	if err := Install(o); err != nil {
		t.Fatal(err)
	}
	account.calls, account.validations = 0, 0
	services.calls = nil
	link := filepath.Join(root, "etc/systemd/system/agent-forge-worker.service")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := Install(o); err == nil {
		t.Fatal("rerun repaired missing unit link")
	}
	if pathExists(link) || account.calls != 0 || account.validations != 1 || len(services.calls) != 0 {
		t.Fatalf("rerun mutated state: link=%v ensure=%d validate=%d services=%v", pathExists(link), account.calls, account.validations, services.calls)
	}
}

func TestSameVersionRerunStrictlyValidatesReceipt(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	for _, tc := range []struct {
		name   string
		mutate func(string)
	}{
		{"mode", func(path string) { _ = os.Chmod(path, 0o600) }},
		{"unknown field", func(path string) {
			body, _ := os.ReadFile(path)
			body = bytes.Replace(body, []byte("{\n"), []byte("{\n  \"unknown\": true,\n"), 1)
			rewriteReceiptForTest(t, path, body)
		}},
		{"duplicate field", func(path string) {
			body, _ := os.ReadFile(path)
			body = bytes.Replace(body, []byte("{\n"), []byte("{\n  \"version\": \""+testVersion+"\",\n"), 1)
			rewriteReceiptForTest(t, path, body)
		}},
		{"token hash", func(path string) {
			body, _ := os.ReadFile(path)
			var value map[string]any
			_ = json.Unmarshal(body, &value)
			value["owner_token_sha256"] = strings.Repeat("0", 64)
			body, _ = json.MarshalIndent(value, "", "  ")
			rewriteReceiptForTest(t, path, append(body, '\n'))
		}},
		{"extra file key", func(path string) {
			body, _ := os.ReadFile(path)
			var value map[string]any
			_ = json.Unmarshal(body, &value)
			value["files"].(map[string]any)["extra"] = strings.Repeat("0", 64)
			body, _ = json.MarshalIndent(value, "", "  ")
			rewriteReceiptForTest(t, path, append(body, '\n'))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			account := &fakeAccount{}
			ownership := newFakeOwnership()
			o := Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
			if err := Install(o); err != nil {
				t.Fatal(err)
			}
			receiptPath := rooted(root, prefix+"/install-receipt.json")
			tc.mutate(receiptPath)
			if err := Install(o); err == nil {
				t.Fatal("accepted tampered receipt")
			}
		})
	}
}

func rewriteReceiptForTest(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
}

func TestHostileArchiveMatrixRejectsBeforePublication(t *testing.T) {
	root := "agent-forge-cli_" + testVersion + "_linux_amd64"
	base := tar.Header{Name: root + "/forge", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
	cases := map[string]func(*tar.Header){
		"absolute":   func(h *tar.Header) { h.Name = "/forge" },
		"traversal":  func(h *tar.Header) { h.Name = root + "/../forge" },
		"symlink":    func(h *tar.Header) { h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, "/tmp/x", 0 },
		"hardlink":   func(h *tar.Header) { h.Typeflag, h.Linkname, h.Size = tar.TypeLink, root+"/VERSION", 0 },
		"fifo":       func(h *tar.Header) { h.Typeflag, h.Size = tar.TypeFifo, 0 },
		"device":     func(h *tar.Header) { h.Typeflag, h.Size = tar.TypeChar, 0 },
		"pax":        func(h *tar.Header) { h.Format, h.PAXRecords = tar.FormatPAX, map[string]string{"comment": "x"} },
		"gnu":        func(h *tar.Header) { h.Format = tar.FormatGNU },
		"wrong mode": func(h *tar.Header) { h.Mode = 0o777 },
		"oversize":   func(h *tar.Header) { h.Size = (128 << 20) + 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := base
			mutate(&h)
			body := hostileTarGzip(t, root, []tar.Header{h})
			file := filepath.Join(t.TempDir(), "hostile.tar.gz")
			if err := os.WriteFile(file, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := extractArchive(verifiedArchive{role: "cli", name: root + ".tar.gz", file: file}, t.TempDir(), testVersion, testCommit); err == nil {
				t.Fatal("accepted hostile archive")
			}
		})
	}
	for _, name := range []string{"missing member", "duplicate member", "duplicate root"} {
		t.Run(name, func(t *testing.T) {
			headers := []tar.Header{{Name: root + "/", Mode: 0o755, Typeflag: tar.TypeDir, Format: tar.FormatUSTAR}}
			if name == "duplicate root" {
				headers = append(headers, headers[0])
			} else if name == "duplicate member" {
				headers = append(headers, base, base)
			}
			body := hostileTarGzip(t, root, headers)
			file := filepath.Join(t.TempDir(), "hostile.tar.gz")
			_ = os.WriteFile(file, body, 0o600)
			if err := extractArchive(verifiedArchive{role: "cli", name: root + ".tar.gz", file: file}, t.TempDir(), testVersion, testCommit); err == nil {
				t.Fatal("accepted incomplete or duplicate archive")
			}
		})
	}
}

func hostileTarGzip(t *testing.T, _ string, headers []tar.Header) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, header := range headers {
		h := header
		_ = tw.WriteHeader(&h)
		if h.Size > 0 && h.Size <= 1024 {
			_, _ = tw.Write(bytes.Repeat([]byte{'x'}, int(h.Size)))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}

func releaseFixture(t *testing.T, mutate func(map[string][]byte)) (string, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{}
	for _, roleArch := range []struct{ role, os, arch string }{
		{"cli", "linux", "amd64"}, {"cli", "linux", "arm64"},
		{"gate", "linux", "amd64"}, {"gate", "linux", "arm64"},
		{"worker", "linux", "amd64"}, {"worker", "linux", "arm64"},
	} {
		name := fmt.Sprintf("agent-forge-%s_%s_%s_%s.tar.gz", roleArch.role, testVersion, roleArch.os, roleArch.arch)
		files[name] = archive(t, strings.TrimSuffix(name, ".tar.gz"), roleArch.role)
	}
	if mutate != nil {
		mutate(files)
	}
	var manifest strings.Builder
	for _, name := range sortedKeys(files) {
		sum := sha256.Sum256(files[name])
		fmt.Fprintf(&manifest, "%x  %s\n", sum, name)
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte(manifest.String())
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return dir, hex.EncodeToString(sum[:])
}

func archive(t *testing.T, root, role string) []byte {
	return archiveForIdentity(t, root, role, testVersion, testCommit, testVersion, testCommit)
}

func archiveForIdentity(t *testing.T, root, role, version, commit, binaryVersion, binaryCommit string) []byte {
	t.Helper()
	var out strings.Builder
	gz := gzip.NewWriter(&stringWriter{&out})
	tw := tar.NewWriter(gz)
	members := []struct {
		name, body string
		mode       int64
	}{{root + "/", "", 0o755}, {root + "/VERSION", "version=" + version + "\ncommit=" + commit + "\n", 0o644}}
	binary := func(name string) struct {
		name, body string
		mode       int64
	} {
		return struct {
			name, body string
			mode       int64
		}{root + "/" + name, "#!/bin/sh\nprintf '%s\\n' '" + name + " " + binaryVersion + " " + binaryCommit + "'\n", 0o755}
	}
	switch role {
	case "cli":
		members = append(members, binary("forge"))
	case "gate":
		members = append(members, binary("forge-gate"))
	case "worker":
		for _, n := range []string{"forge-codex-plugin", "forge-ref-plugin", "forge-worker"} {
			members = append(members, binary(n))
		}
	}
	for _, m := range members {
		typ := byte(tar.TypeReg)
		if strings.HasSuffix(m.name, "/") {
			typ = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: m.mode, Size: int64(len(m.body)), Typeflag: typ, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(out.String())
}

type stringWriter struct{ s *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.s.Write(p) }
func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
func read(t *testing.T, p string) string {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func assertMode(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	s, e := os.Stat(p)
	if e != nil {
		t.Fatal(e)
	}
	if s.Mode().Perm() != want {
		t.Fatalf("%s mode=%o want=%o", p, s.Mode().Perm(), want)
	}
}

func assertOwnership(t *testing.T, manager OwnershipManager, p string, wantUID, wantGID int) {
	t.Helper()
	uid, gid, err := manager.Owner(p)
	if err != nil || uid != wantUID || gid != wantGID {
		t.Fatalf("%s owner=%d:%d want=%d:%d error=%v", p, uid, gid, wantUID, wantGID, err)
	}
}
