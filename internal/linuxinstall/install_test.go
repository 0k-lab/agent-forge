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
	"agent-forge/internal/store"
	"agent-forge/internal/worker"
)

const (
	testVersion = "v1.2.3"
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
)

func TestStoreSchemaMarkerMatchesGate(t *testing.T) {
	if storeSchemaVersion != store.SchemaVersion() {
		t.Fatalf("installer schema=%d gate schema=%d", storeSchemaVersion, store.SchemaVersion())
	}
}

func TestCompareVersionsSupportsUnboundedCanonicalComponents(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v2.0.0", "v1.999.999", 1},
		{"v1.3.0", "v1.2.999", 1},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.2.3", "v1.2.4", -1},
		{"v999999999999999999999999999999.0.0", "v999999999999999999999999999998.999.999", 1},
	}
	for _, test := range tests {
		if got := compareVersions(test.a, test.b); got != test.want {
			t.Fatalf("compareVersions(%q,%q)=%d want %d", test.a, test.b, got, test.want)
		}
	}
}

func TestUpgradeLockIsExclusiveAndMetadataBound(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := acquireUpgradeLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := acquireUpgradeLock(root); err == nil {
		t.Fatal("concurrent upgrade lock unexpectedly succeeded")
	}
	info, err := os.Lstat(filepath.Join(root, "opt/.agent-forge.install.lock"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock metadata=%v err=%v", info, err)
	}
}

func TestUpgradeLockRejectsUnsafeObjects(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			opt := filepath.Join(root, "opt")
			if err := os.MkdirAll(opt, 0o755); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(opt, ".agent-forge.install.lock")
			switch kind {
			case "mode":
				if err := os.WriteFile(lock, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("target", lock); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.WriteFile(lock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(lock, filepath.Join(opt, "other")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := acquireUpgradeLock(root); err == nil {
				t.Fatalf("accepted unsafe %s lock", kind)
			}
		})
	}
}

func TestUpgradeRequiresExactTargetCLIIdentity(t *testing.T) {
	o := Options{
		Version: "v1.2.4", Commit: "4444444444444444444444444444444444444444",
		InstallerVersion: "v1.2.3", InstallerCommit: testCommit,
		AssetDir: "/offline", SHA256SUMSSHA256: strings.Repeat("a", 64), Upgrade: true,
	}
	if err := validate(&o); err == nil {
		t.Fatal("accepted upgrade from non-target forge CLI")
	}
}

func TestInstallFailureStageIsCoarseAndStable(t *testing.T) {
	tests := []struct {
		name string
		o    Options
		want string
	}{
		{name: "validate", o: Options{}, want: "validate"},
		{name: "assets", o: Options{Version: testVersion, Commit: testCommit, AssetDir: filepath.Join(t.TempDir(), "missing"), SHA256SUMSSHA256: strings.Repeat("a", 64)}, want: "assets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stage, ok := FailureStage(Install(tt.o))
			if !ok || stage != tt.want {
				t.Fatalf("FailureStage = %q, %v; want %q, true", stage, ok, tt.want)
			}
		})
	}
}

func TestInstallFailureStageWrapperPreservesEveryAllowedStageAndCause(t *testing.T) {
	for _, stage := range []string{"validate", "assets", "archives", "existing", "host", "account", "staging", "publication", "activation"} {
		t.Run(stage, func(t *testing.T) {
			cause := errors.New("internal detail")
			err := error(&installStageError{stage: stage, err: cause})
			got, ok := FailureStage(err)
			if !ok || got != stage || !errors.Is(err, cause) || err.Error() != cause.Error() {
				t.Fatalf("stage=%q ok=%v errors.Is=%v error=%q", got, ok, errors.Is(err, cause), err.Error())
			}
		})
	}
	if stage, ok := FailureStage(errors.New("plain")); ok || stage != "" {
		t.Fatalf("plain error classified as %q, %v", stage, ok)
	}
}

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
	calls                [][]string
	gateReleases         []string
	ready                int
	failAt               string
	failOnce             bool
	workerReadyCalls     int
	blockWorkerReadyCall int
	workerReadyEntered   chan struct{}
	workerReadyRelease   chan struct{}
}

func (f *fakeServices) fails(name string) bool {
	if f.failAt != name {
		return false
	}
	if f.failOnce {
		f.failAt = ""
	}
	return true
}

func (f *fakeServices) Run(argv ...string) error {
	f.calls = append(f.calls, append([]string(nil), argv...))
	if f.fails(strings.Join(argv, " ")) {
		return errors.New("injected service failure")
	}
	return nil
}

func (f *fakeServices) GateReady(ownerToken, version, commit string) error {
	if ownerToken == "" {
		return fmt.Errorf("empty owner token")
	}
	f.calls = append(f.calls, []string{"gate-ready"})
	f.gateReleases = append(f.gateReleases, version+" "+commit)
	if f.fails("gate-ready") {
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
	f.workerReadyCalls++
	if f.fails("worker-ready") {
		return errors.New("injected Worker readiness failure")
	}
	if f.workerReadyCalls == f.blockWorkerReadyCall {
		close(f.workerReadyEntered)
		<-f.workerReadyRelease
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
			err := Install(o)
			if err == nil {
				t.Fatal("activation failure was not propagated")
			}
			if stage, ok := FailureStage(err); !ok || stage != "activation" {
				t.Fatalf("activation failure stage = %q, %v", stage, ok)
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
	if stage, ok := FailureStage(err); !ok || stage != "staging" {
		t.Fatalf("prepublication failure stage = %q, %v", stage, ok)
	}
	account2 := &fakeAccount{uid: 12346, gid: 12346}
	created2 := &createdFakeAccount{fakeAccount: account2, rollbackErr: errors.New("injected rollback failure")}
	err = Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: t.TempDir(), Arch: "amd64", Account: created2, Ownership: failingOwnership{newFakeOwnership()}, Random: strings.NewReader(strings.Repeat("c", 32) + strings.Repeat("d", 32))})
	if err == nil || !strings.Contains(err.Error(), "rollback failed") || created2.rollbacks != 1 {
		t.Fatalf("rollback failure not surfaced: err=%v rollbacks=%d", err, created2.rollbacks)
	}
	if stage, ok := FailureStage(err); !ok || stage != "staging" {
		t.Fatalf("rollback failure stage = %q, %v", stage, ok)
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

func preparedUpgrade(t *testing.T, services *fakeServices) (Options, string, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	oldAssets, oldAnchor := releaseFixture(t, nil)
	old := Options{Version: testVersion, Commit: testCommit, AssetDir: oldAssets, SHA256SUMSSHA256: oldAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
	if err := Install(old); err != nil {
		t.Fatal(err)
	}
	installPath := rooted(root, prefix)
	before := map[string][]byte{}
	for _, name := range append(append([]string(nil), immutableFiles()...), "install-receipt.json") {
		body, err := os.ReadFile(filepath.Join(installPath, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = body
	}
	newVersion := "v1.2.4"
	newCommit := "89abcdef0123456789abcdef0123456789abcdef"
	newAssets, newAnchor := releaseFixtureFor(t, newVersion, newCommit, nil)
	return Options{Version: newVersion, Commit: newCommit, InstallerVersion: newVersion, InstallerCommit: newCommit, AssetDir: newAssets, SHA256SUMSSHA256: newAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, EnableNow: true, Upgrade: true, Services: services}, installPath, before
}

func assertInstalledBytes(t *testing.T, installPath string, want map[string][]byte) {
	t.Helper()
	for name, body := range want {
		got, err := os.ReadFile(filepath.Join(installPath, name))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("installed object %s differs", name)
		}
	}
}

func TestLegacyV013ReceiptBootstrapsSchemaNeutralUpgrade(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	oldAssets, oldAnchor := releaseFixtureFor(t, legacyInstallerVersion, legacyInstallerCommit, nil)
	old := Options{Version: legacyInstallerVersion, Commit: legacyInstallerCommit, AssetDir: oldAssets, SHA256SUMSSHA256: oldAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
	if err := Install(old); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(rooted(root, prefix), "install-receipt.json")
	var legacy map[string]any
	if err := json.Unmarshal([]byte(read(t, receiptPath)), &legacy); err != nil {
		t.Fatal(err)
	}
	writeLegacy := func() {
		t.Helper()
		body, _ := json.MarshalIndent(legacy, "", "  ")
		body = append(body, '\n')
		if err := os.Chmod(receiptPath, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(receiptPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(receiptPath, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	newAssets, newAnchor := releaseFixtureFor(t, "v0.1.4", "4444444444444444444444444444444444444444", nil)
	upgrade := Options{Version: "v0.1.4", Commit: "4444444444444444444444444444444444444444", InstallerVersion: "v0.1.4", InstallerCommit: "4444444444444444444444444444444444444444", AssetDir: newAssets, SHA256SUMSSHA256: newAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, EnableNow: true, Upgrade: true, Services: &fakeServices{}}
	legacy["store_schema_version"] = float64(0)
	writeLegacy()
	if err := Install(upgrade); err == nil {
		t.Fatal("accepted explicit zero schema marker through legacy bridge")
	}
	delete(legacy, "store_schema_version")
	writeLegacy()
	if err := Install(upgrade); err != nil {
		t.Fatalf("legacy upgrade: %v", err)
	}
	var got receipt
	if err := json.Unmarshal([]byte(read(t, receiptPath)), &got); err != nil || got.StoreSchemaVersion != storeSchemaVersion {
		t.Fatalf("upgraded receipt=%#v err=%v", got, err)
	}
}

func TestUpgradePreservesConfigSecretsAndStateWhileReplacingExactBinaries(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	oldAssets, oldAnchor := releaseFixture(t, nil)
	old := Options{Version: testVersion, Commit: testCommit, AssetDir: oldAssets, SHA256SUMSSHA256: oldAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
	if err := Install(old); err != nil {
		t.Fatal(err)
	}
	installPath := rooted(root, prefix)
	statePath := filepath.Join(installPath, "var/gate/state/forge.db")
	if err := os.WriteFile(statePath, []byte("operator-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"var/gate/state/forge.db-wal": "wal", "var/gate/state/forge.db-shm": "shm", "var/gate/state/forge.db-journal": "journal",
		"var/repositories/repository": "repository", "var/worker/worktrees/worktree": "worktree", "var/worker/runtime/runtime": "runtime",
	} {
		if err := os.WriteFile(filepath.Join(installPath, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	preserved := map[string][]byte{}
	for _, name := range []string{"etc/gate.json", "etc/worker.json", "secrets/gate.env", "secrets/worker.env", "var/gate/state/forge.db", "var/gate/state/forge.db-wal", "var/gate/state/forge.db-shm", "var/gate/state/forge.db-journal", "var/repositories/repository", "var/worker/worktrees/worktree", "var/worker/runtime/runtime"} {
		body, err := os.ReadFile(filepath.Join(installPath, name))
		if err != nil {
			t.Fatal(err)
		}
		preserved[name] = body
	}

	newVersion := "v1.2.4"
	newCommit := "89abcdef0123456789abcdef0123456789abcdef"
	newAssets, newAnchor := releaseFixtureFor(t, newVersion, newCommit, nil)
	services := &fakeServices{}
	upgrade := Options{Version: newVersion, Commit: newCommit, InstallerVersion: newVersion, InstallerCommit: newCommit, AssetDir: newAssets, SHA256SUMSSHA256: newAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, EnableNow: true, Upgrade: true, Services: services}
	oldMask := syscall.Umask(0o077)
	upgradeErr := Install(upgrade)
	syscall.Umask(oldMask)
	if upgradeErr != nil {
		t.Fatalf("upgrade: %v", upgradeErr)
	}
	gotCalls := make([]string, len(services.calls))
	for i := range services.calls {
		gotCalls[i] = strings.Join(services.calls[i], " ")
	}
	wantCalls := []string{"stop agent-forge-worker.service", "stop agent-forge-gate.service", "daemon-reload", "enable --now agent-forge-gate.service", "gate-ready", "enable --now agent-forge-worker.service", "worker-ready"}
	if strings.Join(gotCalls, "|") != strings.Join(wantCalls, "|") {
		t.Fatalf("upgrade service order = %v", gotCalls)
	}
	for name, want := range preserved {
		got, err := os.ReadFile(filepath.Join(installPath, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("upgrade changed preserved path %s", name)
		}
	}
	for _, name := range []string{"bin/forge", "bin/forge-gate", "bin/forge-worker", "bin/forge-codex-plugin", "bin/forge-ref-plugin"} {
		body := read(t, filepath.Join(installPath, name))
		if !strings.Contains(body, newVersion) || !strings.Contains(body, newCommit) {
			t.Fatalf("binary %s was not upgraded", name)
		}
	}
	var gotReceipt receipt
	if err := json.Unmarshal([]byte(read(t, filepath.Join(installPath, "install-receipt.json"))), &gotReceipt); err != nil || gotReceipt.Version != newVersion || gotReceipt.Commit != newCommit {
		t.Fatalf("upgraded receipt = %#v, %v", gotReceipt, err)
	}
	validationServices := &fakeServices{}
	same := upgrade
	same.Upgrade = false
	same.Services = validationServices
	if err := Install(same); err != nil || len(validationServices.calls) != 0 {
		t.Fatalf("post-upgrade validation-only rerun: err=%v calls=%v", err, validationServices.calls)
	}
}

func TestUpgradeReadinessFailureRollsBackExactInstalledRelease(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	oldAssets, oldAnchor := releaseFixture(t, nil)
	old := Options{Version: testVersion, Commit: testCommit, AssetDir: oldAssets, SHA256SUMSSHA256: oldAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
	if err := Install(old); err != nil {
		t.Fatal(err)
	}
	installPath := rooted(root, prefix)
	statePath := filepath.Join(installPath, "var/gate/state/forge.db")
	if err := os.WriteFile(statePath, []byte("operator-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, name := range append(append([]string(nil), immutableFiles()...), "install-receipt.json", "var/gate/state/forge.db") {
		body, err := os.ReadFile(filepath.Join(installPath, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = body
	}

	newVersion := "v1.2.4"
	newCommit := "89abcdef0123456789abcdef0123456789abcdef"
	newAssets, newAnchor := releaseFixtureFor(t, newVersion, newCommit, nil)
	services := &fakeServices{failAt: "worker-ready", failOnce: true}
	upgrade := Options{Version: newVersion, Commit: newCommit, InstallerVersion: newVersion, InstallerCommit: newCommit, AssetDir: newAssets, SHA256SUMSSHA256: newAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, EnableNow: true, Upgrade: true, Services: services}
	err := Install(upgrade)
	stage, ok := FailureStage(err)
	if err == nil || !ok || stage != "activation" {
		t.Fatalf("upgrade error = %v, stage=%q, ok=%v", err, stage, ok)
	}
	for name, want := range before {
		got, readErr := os.ReadFile(filepath.Join(installPath, name))
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("rollback did not restore %s", name)
		}
	}
	if services.ready < 3 {
		t.Fatalf("old release readiness was not re-proved: ready=%d calls=%v", services.ready, services.calls)
	}
	wantReleases := []string{newVersion + " " + newCommit, testVersion + " " + testCommit}
	if strings.Join(services.gateReleases, "|") != strings.Join(wantReleases, "|") {
		t.Fatalf("Gate readiness releases=%v, want %v", services.gateReleases, wantReleases)
	}
}

func TestUpgradeLockIsHeldThroughRollbackRecovery(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	services := &fakeServices{
		failAt: "worker-ready", failOnce: true,
		blockWorkerReadyCall: 2, workerReadyEntered: entered, workerReadyRelease: release,
	}
	upgrade, _, _ := preparedUpgrade(t, services)
	firstDone := make(chan error, 1)
	go func() { firstDone <- Install(upgrade) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("rollback recovery did not reach old Worker readiness")
	}
	second := upgrade
	second.Services = &fakeServices{}
	if err := Install(second); err == nil || !strings.Contains(err.Error(), "another installer transaction is active") {
		close(release)
		t.Fatalf("concurrent upgrade error=%v", err)
	}
	close(release)
	select {
	case err := <-firstDone:
		if err == nil || !strings.Contains(err.Error(), "injected Worker readiness failure") {
			t.Fatalf("first upgrade error=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rollback recovery did not finish")
	}
	lock, err := acquireUpgradeLock(upgrade.Root)
	if err != nil {
		t.Fatalf("lock remained held after rollback cleanup: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradePublicationFailuresRestoreExactInstalledRelease(t *testing.T) {
	for _, name := range upgradeObjects {
		for _, phase := range []string{"current-to-backup", "candidate-to-current"} {
			t.Run(strings.ReplaceAll(name+"/"+phase, "/", "_"), func(t *testing.T) {
				services := &fakeServices{}
				upgrade, installPath, before := preparedUpgrade(t, services)
				previousRename := renameUpgrade
				t.Cleanup(func() { renameUpgrade = previousRename })
				failed := false
				current := filepath.Join(installPath, name)
				renameUpgrade = func(from, to string) error {
					match := phase == "current-to-backup" && from == current && strings.Contains(to, ".agent-forge.rollback-") || phase == "candidate-to-current" && strings.Contains(from, ".agent-forge.upgrade-") && to == current
					if match && !failed {
						failed = true
						return errors.New("injected publication rename failure")
					}
					return os.Rename(from, to)
				}
				err := Install(upgrade)
				stage, ok := FailureStage(err)
				if err == nil || !ok || stage != "publication" || !failed {
					t.Fatalf("error=%v stage=%q failed=%v", err, stage, failed)
				}
				assertInstalledBytes(t, installPath, before)
				if services.ready < 2 {
					t.Fatalf("old release was not re-proved: %v", services.calls)
				}
			})
		}
	}
}

func TestUpgradeServiceFailuresRestoreAndReproveOldRelease(t *testing.T) {
	for _, failAt := range []string{"stop agent-forge-worker.service", "stop agent-forge-gate.service", "daemon-reload", "enable --now agent-forge-gate.service", "gate-ready", "enable --now agent-forge-worker.service", "worker-ready"} {
		t.Run(strings.ReplaceAll(failAt, " ", "_"), func(t *testing.T) {
			services := &fakeServices{failAt: failAt, failOnce: true}
			upgrade, installPath, before := preparedUpgrade(t, services)
			err := Install(upgrade)
			stage, ok := FailureStage(err)
			if err == nil || !ok || stage != "activation" {
				t.Fatalf("error=%v stage=%q", err, stage)
			}
			assertInstalledBytes(t, installPath, before)
			if services.ready < 2 {
				t.Fatalf("old release was not re-proved: %v", services.calls)
			}
		})
	}
}

func TestUpgradeRollbackRenameFailureRetainsRecoveryMaterial(t *testing.T) {
	for _, phase := range []string{"current-to-stage", "backup-to-current"} {
		t.Run(phase, func(t *testing.T) {
			services := &fakeServices{failAt: "worker-ready", failOnce: true}
			upgrade, installPath, _ := preparedUpgrade(t, services)
			previousRename := renameUpgrade
			t.Cleanup(func() { renameUpgrade = previousRename })
			current := filepath.Join(installPath, "bin/forge")
			failed := false
			renameUpgrade = func(from, to string) error {
				match := phase == "current-to-stage" && from == current && strings.Contains(to, ".agent-forge.upgrade-") || phase == "backup-to-current" && strings.Contains(from, ".agent-forge.rollback-") && to == current
				if match && !failed {
					failed = true
					return errors.New("injected rollback rename failure")
				}
				return os.Rename(from, to)
			}
			if err := Install(upgrade); err == nil || !failed {
				t.Fatalf("rollback failure not surfaced: %v", err)
			}
			parent := filepath.Dir(installPath)
			stages, _ := filepath.Glob(filepath.Join(parent, ".agent-forge.upgrade-*"))
			backups, _ := filepath.Glob(filepath.Join(parent, ".agent-forge.rollback-*"))
			if len(stages) != 1 || len(backups) != 1 {
				t.Fatalf("recovery material missing: stages=%v backups=%v", stages, backups)
			}
		})
	}
}

func TestUpgradeRejectsDowngradeAndMissingReadinessWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name      string
		version   string
		commit    string
		enableNow bool
		upgrade   bool
		wantStage string
	}{
		{name: "missing explicit upgrade", version: "v1.2.4", commit: "1111111111111111111111111111111111111111", enableNow: true, wantStage: "existing"},
		{name: "downgrade", version: "v1.2.2", commit: "2222222222222222222222222222222222222222", enableNow: true, upgrade: true, wantStage: "existing"},
		{name: "missing readiness", version: "v1.2.4", commit: "3333333333333333333333333333333333333333", upgrade: true, wantStage: "activation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			account := &fakeAccount{}
			ownership := newFakeOwnership()
			assets, anchor := releaseFixture(t, nil)
			old := Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}
			if err := Install(old); err != nil {
				t.Fatal(err)
			}
			installPath := rooted(root, prefix)
			before := map[string][]byte{}
			for _, name := range append(append([]string(nil), immutableFiles()...), "install-receipt.json") {
				body, err := os.ReadFile(filepath.Join(installPath, name))
				if err != nil {
					t.Fatal(err)
				}
				before[name] = body
			}
			newAssets, newAnchor := releaseFixtureFor(t, test.version, test.commit, nil)
			err := Install(Options{Version: test.version, Commit: test.commit, InstallerVersion: test.version, InstallerCommit: test.commit, AssetDir: newAssets, SHA256SUMSSHA256: newAnchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, EnableNow: test.enableNow, Upgrade: test.upgrade, Services: &fakeServices{}})
			stage, ok := FailureStage(err)
			if err == nil || !ok || stage != test.wantStage {
				t.Fatalf("error=%v stage=%q ok=%v", err, stage, ok)
			}
			for name, want := range before {
				got, readErr := os.ReadFile(filepath.Join(installPath, name))
				if readErr != nil || !bytes.Equal(got, want) {
					t.Fatalf("rejected upgrade changed %s", name)
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
			err := Install(o)
			if err == nil {
				t.Fatal("accepted tampered receipt")
			}
			if stage, ok := FailureStage(err); !ok || stage != "existing" {
				t.Fatalf("tampered receipt stage = %q, %v", stage, ok)
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
	return releaseFixtureFor(t, testVersion, testCommit, mutate)
}

func releaseFixtureFor(t *testing.T, version, commit string, mutate func(map[string][]byte)) (string, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{}
	for _, roleArch := range []struct{ role, os, arch string }{
		{"cli", "linux", "amd64"}, {"cli", "linux", "arm64"},
		{"gate", "linux", "amd64"}, {"gate", "linux", "arm64"},
		{"worker", "linux", "amd64"}, {"worker", "linux", "arm64"},
	} {
		name := fmt.Sprintf("agent-forge-%s_%s_%s_%s.tar.gz", roleArch.role, version, roleArch.os, roleArch.arch)
		files[name] = archiveForIdentity(t, strings.TrimSuffix(name, ".tar.gz"), roleArch.role, version, commit, version, commit)
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
