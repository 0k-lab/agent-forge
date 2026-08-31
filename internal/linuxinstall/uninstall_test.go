//go:build linux

package linuxinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type tamperingServices struct {
	*fakeServices
	link string
}

func (f *tamperingServices) Run(argv ...string) error {
	if strings.Join(argv, " ") == "disable agent-forge-gate.service" {
		if err := os.Remove(f.link); err != nil {
			return err
		}
		if err := os.Symlink("/tmp/not-agent-forge", f.link); err != nil {
			return err
		}
	}
	return f.fakeServices.Run(argv...)
}

type disableLinkRemovingServices struct {
	*fakeServices
	root           string
	linkForDisable map[string]string
}

func (f *disableLinkRemovingServices) Run(argv ...string) error {
	if err := f.fakeServices.Run(argv...); err != nil {
		return err
	}
	if len(argv) == 2 && argv[0] == "disable" {
		unit := argv[1]
		if f.linkForDisable != nil {
			unit = f.linkForDisable[unit]
		}
		return os.Remove(rooted(f.root, "/etc/systemd/system/"+unit))
	}
	return nil
}

func TestUninstallRejectsMissingLinkWithoutSuccessfulDisableForSameUnit(t *testing.T) {
	upgrade, installPath, _ := preparedUpgrade(t, &fakeServices{})
	if err := Install(upgrade); err != nil {
		t.Fatal(err)
	}
	fake := &fakeServices{states: map[string]ServiceState{
		"agent-forge-gate.service":   {Active: true},
		"agent-forge-worker.service": {Enabled: true, Active: true},
	}}
	services := &disableLinkRemovingServices{
		fakeServices: fake,
		root:         upgrade.Root,
		linkForDisable: map[string]string{
			"agent-forge-worker.service": "agent-forge-gate.service",
		},
	}
	if err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services}); err == nil {
		t.Fatal("accepted link disappearance after another unit's disable")
	}
	for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		link := rooted(upgrade.Root, "/etc/systemd/system/"+name)
		want := rooted(upgrade.Root, prefix+"/systemd/"+name)
		if got, readErr := os.Readlink(link); readErr != nil || got != want {
			t.Fatalf("link %s=%q err=%v", name, got, readErr)
		}
	}
	if !pathExists(filepath.Join(installPath, "install-receipt.json")) {
		t.Fatal("release was not recovered")
	}
}

func TestUninstallAcceptsDirectLinksRemovedBySuccessfulDisable(t *testing.T) {
	upgrade, installPath, _ := preparedUpgrade(t, &fakeServices{})
	if err := Install(upgrade); err != nil {
		t.Fatal(err)
	}
	services := &disableLinkRemovingServices{fakeServices: &fakeServices{}, root: upgrade.Root}
	if err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		if pathExists(rooted(upgrade.Root, "/etc/systemd/system/"+name)) {
			t.Fatalf("canonical unit link remains: %s", name)
		}
	}
	for _, name := range upgradeObjects {
		if pathExists(filepath.Join(installPath, name)) {
			t.Fatalf("release object remains: %s", name)
		}
	}
}

func TestUninstallFailureRestoresLinksSlotsBytesAndStateAfterDisableRemovedLinks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failAt     string
		failRename bool
	}{
		{name: "one link", failAt: "stop agent-forge-gate.service"},
		{name: "both links", failRename: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upgrade, installPath, _ := preparedUpgrade(t, &fakeServices{})
			if err := Install(upgrade); err != nil {
				t.Fatal(err)
			}
			previous := filepath.Join(filepath.Dir(installPath), previousSlotName)
			currentBefore, previousBefore := map[string][]byte{}, map[string][]byte{}
			for _, name := range upgradeObjects {
				currentBefore[name] = []byte(read(t, filepath.Join(installPath, name)))
				previousBefore[name] = []byte(read(t, filepath.Join(previous, name)))
			}
			fake := &fakeServices{failAt: tc.failAt, failOnce: true}
			services := &disableLinkRemovingServices{fakeServices: fake, root: upgrade.Root}
			previousRename := renameUninstall
			t.Cleanup(func() { renameUninstall = previousRename })
			if tc.failRename {
				renameUninstall = func(string, string) error { return errors.New("injected rename failure") }
			}

			err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services})
			if err == nil {
				t.Fatal("injected failure succeeded")
			}
			assertInstalledBytes(t, installPath, currentBefore)
			assertInstalledBytes(t, previous, previousBefore)
			for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
				link := rooted(upgrade.Root, "/etc/systemd/system/"+name)
				want := rooted(upgrade.Root, prefix+"/systemd/"+name)
				if got, readErr := os.Readlink(link); readErr != nil || got != want {
					t.Fatalf("link %s=%q err=%v", name, got, readErr)
				}
				if got := fake.serviceState(name); got != (ServiceState{Enabled: true, Active: true}) {
					t.Fatalf("%s state=%+v", name, got)
				}
			}
		})
	}
}

func TestUninstallRemovesOnlyReleaseObjectsAndLinks(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	assets, anchor := releaseFixture(t, nil)
	if err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}); err != nil {
		t.Fatal(err)
	}
	installPath := rooted(root, prefix)
	for name, body := range map[string]string{
		"var/gate/state/forge.db":       "sqlite-marker",
		"var/repositories/repository":   "repository",
		"var/worker/worktrees/worktree": "worktree",
		"var/worker/runtime/runtime":    "runtime",
	} {
		if err := os.WriteFile(filepath.Join(installPath, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mutable := []string{"etc", "secrets", "var"}
	type state struct {
		inode    uint64
		mode     os.FileMode
		uid, gid uint32
		body     []byte
	}
	before := map[string]state{}
	for _, name := range mutable {
		path := filepath.Join(installPath, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		before[name] = state{inode: stat.Ino, mode: info.Mode(), uid: stat.Uid, gid: stat.Gid}
	}
	for _, name := range []string{"etc/gate.json", "etc/worker.json", "secrets/gate.env", "secrets/worker.env", "var/gate/state/forge.db", "var/repositories/repository", "var/worker/worktrees/worktree", "var/worker/runtime/runtime"} {
		path := filepath.Join(installPath, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		before[name] = state{inode: stat.Ino, mode: info.Mode(), uid: stat.Uid, gid: stat.Gid, body: []byte(read(t, path))}
	}
	services := &fakeServices{}
	if err := Uninstall(UninstallOptions{Version: testVersion, Commit: testCommit, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Services: services}); err != nil {
		t.Fatal(err)
	}
	gotCalls := make([]string, len(services.calls))
	for i := range services.calls {
		gotCalls[i] = strings.Join(services.calls[i], " ")
	}
	wantCalls := []string{"stop agent-forge-worker.service", "disable agent-forge-worker.service", "stop agent-forge-gate.service", "disable agent-forge-gate.service", "daemon-reload"}
	if strings.Join(gotCalls, "|") != strings.Join(wantCalls, "|") {
		t.Fatalf("service order=%v", gotCalls)
	}
	for _, name := range upgradeObjects {
		if pathExists(filepath.Join(installPath, name)) {
			t.Fatalf("release object remains: %s", name)
		}
	}
	for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		if pathExists(rooted(root, "/etc/systemd/system/"+name)) {
			t.Fatalf("direct unit link remains: %s", name)
		}
	}
	for name, want := range before {
		path := filepath.Join(installPath, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("mutable path missing: %s", name)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Ino != want.inode || info.Mode() != want.mode || stat.Uid != want.uid || stat.Gid != want.gid {
			t.Fatalf("mutable metadata changed: %s", name)
		}
		if want.body != nil {
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want.body) {
				t.Fatalf("mutable bytes changed: %s", name)
			}
		}
	}
	if account.calls != 1 || account.validations != 1 || account.rollbacks != 0 {
		t.Fatalf("account calls=%d validations=%d rollbacks=%d", account.calls, account.validations, account.rollbacks)
	}
	entries, err := os.ReadDir(filepath.Dir(installPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".agent-forge.uninstall-") {
			t.Fatalf("uninstall material remains: %s", entry.Name())
		}
	}
}

func TestUninstallRechecksExactLinkBeforeUnlinkAndRecovers(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	assets, anchor := releaseFixture(t, nil)
	if err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}); err != nil {
		t.Fatal(err)
	}
	installPath := rooted(root, prefix)
	before := map[string][]byte{}
	for _, name := range upgradeObjects {
		before[name] = []byte(read(t, filepath.Join(installPath, name)))
	}
	gateLink := rooted(root, "/etc/systemd/system/agent-forge-gate.service")
	services := &tamperingServices{fakeServices: &fakeServices{}, link: gateLink}
	err := Uninstall(UninstallOptions{Version: testVersion, Commit: testCommit, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Services: services})
	if err == nil {
		t.Fatal("unlinked a changed direct unit link")
	}
	assertInstalledBytes(t, installPath, before)
	want := rooted(root, prefix+"/systemd/agent-forge-gate.service")
	if got, readErr := os.Readlink(gateLink); readErr != nil || got != want {
		t.Fatalf("gate link=%q err=%v", got, readErr)
	}
	if services.ready != 2 {
		t.Fatalf("recovery readiness proofs=%d", services.ready)
	}
}

func TestUninstallRemovesStrictPreviousSlot(t *testing.T) {
	upgrade, installPath, _ := preparedUpgrade(t, &fakeServices{})
	if err := Install(upgrade); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(filepath.Dir(installPath), previousSlotName)
	if !pathExists(previous) {
		t.Fatal("fixture has no previous slot")
	}
	services := &fakeServices{}
	if err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services}); err != nil {
		t.Fatal(err)
	}
	if pathExists(previous) {
		t.Fatal("previous slot remains")
	}
	for _, name := range []string{"etc", "secrets", "var"} {
		if !pathExists(filepath.Join(installPath, name)) {
			t.Fatalf("mutable directory removed: %s", name)
		}
	}
}

func TestUninstallRejectsInvalidStateBeforeServiceMutation(t *testing.T) {
	for _, name := range []string{"missing-current", "cli-identity", "malformed-slot", "missing-link", "unsafe-link", "transaction-material"} {
		t.Run(name, func(t *testing.T) {
			services := &fakeServices{}
			upgrade, installPath, _ := preparedUpgrade(t, &fakeServices{})
			if err := Install(upgrade); err != nil {
				t.Fatal(err)
			}
			o := UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services}
			switch name {
			case "missing-current":
				if err := os.Remove(filepath.Join(installPath, "install-receipt.json")); err != nil {
					t.Fatal(err)
				}
			case "cli-identity":
				o.Commit = strings.Repeat("f", 40)
			case "malformed-slot":
				if err := os.Chmod(filepath.Join(filepath.Dir(installPath), previousSlotName, "install-receipt.json"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-link":
				if err := os.Remove(rooted(upgrade.Root, "/etc/systemd/system/agent-forge-gate.service")); err != nil {
					t.Fatal(err)
				}
			case "unsafe-link":
				link := rooted(upgrade.Root, "/etc/systemd/system/agent-forge-gate.service")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/tmp/wrong", link); err != nil {
					t.Fatal(err)
				}
			case "transaction-material":
				if err := os.WriteFile(filepath.Join(filepath.Dir(installPath), ".agent-forge.uninstall-recovery"), []byte("stale"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := Uninstall(o); err == nil {
				t.Fatal("accepted invalid uninstall state")
			}
			if len(services.calls) != 0 {
				t.Fatalf("services called before rejection: %v", services.calls)
			}
		})
	}
}

func TestUninstallRejectsUncertainOrImpossibleServiceStateBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		services *fakeServices
	}{
		{"probe failure", &fakeServices{stateFailures: map[int]error{1: errors.New("injected probe failure")}}},
		{"worker active without gate", &fakeServices{states: map[string]ServiceState{
			"agent-forge-gate.service":   {},
			"agent-forge-worker.service": {Active: true},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upgrade, installPath, _ := preparedUpgrade(t, &fakeServices{})
			if err := Install(upgrade); err != nil {
				t.Fatal(err)
			}
			err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: tc.services})
			if err == nil {
				t.Fatal("accepted uncertain service state")
			}
			if len(tc.services.calls) != 0 {
				t.Fatalf("service mutation before rejection: %v", tc.services.calls)
			}
			entries, readErr := os.ReadDir(filepath.Dir(installPath))
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".agent-forge.uninstall-") {
					t.Fatalf("staged before state rejection: %s", entry.Name())
				}
			}
		})
	}
}

func TestUninstallPreservesServiceStateByCallingOnlyNecessaryShutdownOperations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state ServiceState
		want  []string
	}{
		{"enabled active", ServiceState{Enabled: true, Active: true}, []string{"stop agent-forge-worker.service", "disable agent-forge-worker.service", "stop agent-forge-gate.service", "disable agent-forge-gate.service", "daemon-reload"}},
		{"disabled inactive", ServiceState{}, []string{"daemon-reload"}},
		{"disabled active", ServiceState{Active: true}, []string{"stop agent-forge-worker.service", "stop agent-forge-gate.service", "daemon-reload"}},
		{"enabled inactive", ServiceState{Enabled: true}, []string{"disable agent-forge-worker.service", "disable agent-forge-gate.service", "daemon-reload"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upgrade, _, _ := preparedUpgrade(t, &fakeServices{})
			if err := Install(upgrade); err != nil {
				t.Fatal(err)
			}
			services := &fakeServices{states: map[string]ServiceState{
				"agent-forge-gate.service": tc.state, "agent-forge-worker.service": tc.state,
			}}
			if err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services}); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, call := range services.calls {
				got = append(got, strings.Join(call, " "))
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("calls=%v want=%v", got, tc.want)
			}
			if services.ready != 0 {
				t.Fatalf("success readiness=%d", services.ready)
			}
		})
	}
}

func TestUninstallFailureRestoresExactPriorServiceState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state ServiceState
	}{
		{"enabled active", ServiceState{Enabled: true, Active: true}},
		{"disabled inactive", ServiceState{}},
		{"disabled active", ServiceState{Active: true}},
		{"enabled inactive", ServiceState{Enabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upgrade, _, _ := preparedUpgrade(t, &fakeServices{})
			if err := Install(upgrade); err != nil {
				t.Fatal(err)
			}
			services := &fakeServices{
				states: map[string]ServiceState{"agent-forge-gate.service": tc.state, "agent-forge-worker.service": tc.state},
				failAt: "daemon-reload", failOnce: true,
			}
			if err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services}); err == nil {
				t.Fatal("injected failure succeeded")
			}
			for _, unit := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
				if got := services.serviceState(unit); got != tc.state {
					t.Fatalf("%s state=%+v want=%+v", unit, got, tc.state)
				}
			}
			wantReady := 0
			if tc.state.Active {
				wantReady = 2
			}
			if services.ready != wantReady {
				t.Fatalf("readiness=%d want=%d", services.ready, wantReady)
			}
		})
	}
}

func TestUninstallPostReloadStateVerificationFailureRecovers(t *testing.T) {
	upgrade, _, _ := preparedUpgrade(t, &fakeServices{})
	if err := Install(upgrade); err != nil {
		t.Fatal(err)
	}
	services := &fakeServices{stateFailures: map[int]error{3: errors.New("injected post-reload probe failure")}}
	err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services})
	if err == nil {
		t.Fatal("committed without verified stopped services")
	}
	for _, unit := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		if got := services.serviceState(unit); got != (ServiceState{Enabled: true, Active: true}) {
			t.Fatalf("%s state=%+v", unit, got)
		}
	}
	if services.ready != 2 {
		t.Fatalf("recovery readiness=%d", services.ready)
	}
}

func TestUninstallHandledFailuresRestoreCurrentSlotLinksAndReadiness(t *testing.T) {
	type failure struct {
		name     string
		service  string
		renameAt int
		unlinkAt int
	}
	failures := []failure{
		{name: "stop-worker", service: "stop agent-forge-worker.service"},
		{name: "disable-worker", service: "disable agent-forge-worker.service"},
		{name: "stop-gate", service: "stop agent-forge-gate.service"},
		{name: "disable-gate", service: "disable agent-forge-gate.service"},
		{name: "daemon-reload", service: "daemon-reload"},
		{name: "unlink-gate", unlinkAt: 1},
		{name: "unlink-worker", unlinkAt: 2},
	}
	for i := 1; i <= len(upgradeObjects)+1; i++ {
		failures = append(failures, failure{name: fmt.Sprintf("rename-%d", i), renameAt: i})
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			services := &fakeServices{failAt: failure.service, failOnce: true}
			upgrade, installPath, _ := preparedUpgrade(t, &fakeServices{})
			if err := Install(upgrade); err != nil {
				t.Fatal(err)
			}
			previous := filepath.Join(filepath.Dir(installPath), previousSlotName)
			currentBefore, slotBefore := map[string][]byte{}, map[string][]byte{}
			for _, name := range upgradeObjects {
				currentBefore[name] = []byte(read(t, filepath.Join(installPath, name)))
				slotBefore[name] = []byte(read(t, filepath.Join(previous, name)))
			}

			previousRename, previousUnlink := renameUninstall, unlinkUninstall
			t.Cleanup(func() { renameUninstall, unlinkUninstall = previousRename, previousUnlink })
			renameCalls, unlinkCalls := 0, 0
			renameUninstall = func(a, b string) error {
				renameCalls++
				if renameCalls == failure.renameAt {
					return errors.New("injected rename failure")
				}
				return previousRename(a, b)
			}
			unlinkUninstall = func(name string) error {
				unlinkCalls++
				if unlinkCalls == failure.unlinkAt {
					return errors.New("injected unlink failure")
				}
				return previousUnlink(name)
			}

			err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services})
			if err == nil {
				t.Fatal("injected failure succeeded")
			}
			assertInstalledBytes(t, installPath, currentBefore)
			assertInstalledBytes(t, previous, slotBefore)
			for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
				link := rooted(upgrade.Root, "/etc/systemd/system/"+name)
				want := rooted(upgrade.Root, prefix+"/systemd/"+name)
				if got, readErr := os.Readlink(link); readErr != nil || got != want {
					t.Fatalf("link %s=%q err=%v", name, got, readErr)
				}
			}
			if services.ready != 2 {
				t.Fatalf("recovery readiness proofs=%d", services.ready)
			}
			entries, readErr := os.ReadDir(filepath.Dir(installPath))
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".agent-forge.uninstall-") {
					t.Fatalf("recovered transaction retained %s", entry.Name())
				}
			}
		})
	}
}

func TestUninstallRejectsCrossMountRenameTopologyBeforeServices(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	assets, anchor := releaseFixture(t, nil)
	if err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}); err != nil {
		t.Fatal(err)
	}
	installPath := rooted(root, prefix)
	before := map[string][]byte{}
	for _, name := range upgradeObjects {
		before[name] = []byte(read(t, filepath.Join(installPath, name)))
	}
	previousSameFilesystem := sameFilesystem
	t.Cleanup(func() { sameFilesystem = previousSameFilesystem })
	sameFilesystem = func(a, _ string) bool { return a != filepath.Join(installPath, "bin/forge") }
	services := &fakeServices{}
	if err := Uninstall(UninstallOptions{Version: testVersion, Commit: testCommit, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Services: services}); err == nil {
		t.Fatal("accepted cross-mount rename topology")
	}
	assertInstalledBytes(t, installPath, before)
	if len(services.calls) != 0 {
		t.Fatalf("services called before topology rejection: %v", services.calls)
	}
}

func TestUninstallRecoveryFailureRetainsPrivateEvidence(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	assets, anchor := releaseFixture(t, nil)
	if err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}); err != nil {
		t.Fatal(err)
	}
	previousRename := renameUninstall
	t.Cleanup(func() { renameUninstall = previousRename })
	calls := 0
	renameUninstall = func(a, b string) error {
		calls++
		if calls == 2 || calls == 3 {
			return errors.New("injected rename failure")
		}
		return previousRename(a, b)
	}
	services := &fakeServices{}
	err := Uninstall(UninstallOptions{Version: testVersion, Commit: testCommit, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Services: services})
	if err == nil || strings.Contains(err.Error(), "FORGE_") {
		t.Fatalf("recovery error=%v", err)
	}
	parent := filepath.Dir(rooted(root, prefix))
	marker := filepath.Join(parent, ".agent-forge.uninstall-recovery")
	info, statErr := os.Lstat(marker)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery marker info=%v err=%v", info, statErr)
	}
	foundPrivate := false
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".agent-forge.uninstall-") && entry.IsDir() {
			info, statErr := os.Lstat(filepath.Join(parent, entry.Name()))
			foundPrivate = statErr == nil && info.Mode().Perm() == 0o700
		}
	}
	if !foundPrivate {
		t.Fatal("private recovery quarantine missing")
	}
}

func TestUninstallValidationNeverExecutesInstalledBinaries(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	assets, anchor := releaseFixture(t, nil)
	if err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}); err != nil {
		t.Fatal(err)
	}
	installPath := rooted(root, prefix)
	sentinel := filepath.Join(root, "executed")
	binary := filepath.Join(installPath, "bin/forge")
	body := []byte("#!/bin/sh\ntouch '" + sentinel + "'\n")
	if err := os.WriteFile(binary, body, 0o755); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(installPath, "install-receipt.json")
	var current receipt
	if err := json.Unmarshal([]byte(read(t, receiptPath)), &current); err != nil {
		t.Fatal(err)
	}
	current.Files["bin/forge"] = hashBytes(body)
	receiptBody, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(receiptPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, append(receiptBody, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(receiptPath, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(UninstallOptions{Version: testVersion, Commit: testCommit, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Services: &fakeServices{}}); err != nil {
		t.Fatal(err)
	}
	if pathExists(sentinel) {
		t.Fatal("uninstall executed an installed binary")
	}
}

func TestUninstallHoldsInstallerLockThroughRecovery(t *testing.T) {
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	assets, anchor := releaseFixture(t, nil)
	if err := Install(Options{Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32))}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	services := &fakeServices{failAt: "stop agent-forge-worker.service", failOnce: true, blockWorkerReadyCall: 1, workerReadyEntered: entered, workerReadyRelease: release}
	done := make(chan error, 1)
	go func() {
		done <- Uninstall(UninstallOptions{Version: testVersion, Commit: testCommit, Root: root, Arch: "amd64", Account: account, Ownership: ownership, Services: services})
	}()
	<-entered
	if lock, err := acquireUpgradeLock(root); err == nil {
		_ = lock.Close()
		t.Fatal("concurrent lifecycle operation acquired lock during recovery")
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("injected uninstall failure succeeded")
	}
}

func TestUninstallPartialCleanupFailureLeavesCommittedUninstall(t *testing.T) {
	upgrade, installPath, _ := preparedUpgrade(t, &fakeServices{})
	if err := Install(upgrade); err != nil {
		t.Fatal(err)
	}
	previousPath := filepath.Join(filepath.Dir(installPath), previousSlotName)
	mutablePath := filepath.Join(installPath, "var/gate/state/forge.db")
	if err := os.WriteFile(mutablePath, []byte("mutable-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutableBefore, err := os.Lstat(mutablePath)
	if err != nil {
		t.Fatal(err)
	}
	previousRemove := removeUninstallCleanup
	t.Cleanup(func() { removeUninstallCleanup = previousRemove })
	cleanupCalls := 0
	removeUninstallCleanup = func(name string) error {
		cleanupCalls++
		if cleanupCalls == 2 {
			return errors.New("injected partial cleanup failure")
		}
		return previousRemove(name)
	}
	services := &fakeServices{}
	if err := Uninstall(UninstallOptions{Version: upgrade.Version, Commit: upgrade.Commit, Root: upgrade.Root, Arch: upgrade.Arch, Account: upgrade.Account, Ownership: upgrade.Ownership, Services: services}); err != nil {
		t.Fatalf("committed uninstall returned cleanup error: %v", err)
	}
	if cleanupCalls != 2 {
		t.Fatalf("cleanup continued after failure; calls=%d", cleanupCalls)
	}
	for _, name := range upgradeObjects {
		if pathExists(filepath.Join(installPath, name)) {
			t.Fatalf("canonical release object restored: %s", name)
		}
	}
	if pathExists(previousPath) {
		t.Fatal("canonical previous slot restored")
	}
	for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		if pathExists(rooted(upgrade.Root, "/etc/systemd/system/"+name)) {
			t.Fatalf("canonical unit link restored: %s", name)
		}
	}
	if services.ready != 0 {
		t.Fatalf("post-commit cleanup reactivated services; readiness proofs=%d", services.ready)
	}
	mutableAfter, err := os.Lstat(mutablePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, afterStat := mutableBefore.Sys().(*syscall.Stat_t), mutableAfter.Sys().(*syscall.Stat_t)
	if beforeStat.Ino != afterStat.Ino || beforeStat.Uid != afterStat.Uid || beforeStat.Gid != afterStat.Gid || mutableBefore.Mode() != mutableAfter.Mode() || read(t, mutablePath) != "mutable-marker" {
		t.Fatal("mutable state changed")
	}
	entries, err := os.ReadDir(filepath.Dir(installPath))
	if err != nil {
		t.Fatal(err)
	}
	var residue string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".agent-forge.uninstall-") && entry.IsDir() {
			residue = filepath.Join(filepath.Dir(installPath), entry.Name())
		}
	}
	if residue == "" || pathExists(filepath.Join(residue, "bin/forge")) || !pathExists(filepath.Join(residue, "bin/forge-gate")) {
		t.Fatal("partial immutable cleanup residue not preserved")
	}
	if rejectTransactionMaterial(filepath.Dir(installPath)) == nil {
		t.Fatal("cleanup residue did not block later lifecycle operations")
	}
}
