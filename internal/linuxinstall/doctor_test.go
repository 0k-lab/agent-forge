//go:build linux

package linuxinstall

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-forge/internal/store"
)

func TestDoctorValidatesInstalledStateWithoutMutation(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	if err := Install(Options{
		Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor,
		Root: root, Arch: "amd64", Account: account, Ownership: ownership,
		Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32)),
	}); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(rooted(root, prefix), "var/gate/state/forge.db")
	s, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ownership.owners["var/gate/state/forge.db"] = [2]int{account.uid, account.gid}
	services := &fakeServices{}
	before := doctorSnapshot(t, root)
	report := Doctor(DoctorOptions{Root: root, Services: services, Ownership: ownership, Account: account})
	after := doctorSnapshot(t, root)

	want := "receipt cli-identity trusted-paths immutable-paths mutable-paths gate-enabled gate-active worker-enabled worker-active gate-readiness worker-readiness store-schema"
	var passed []string
	for _, check := range report.Checks {
		if check.OK {
			passed = append(passed, check.ID)
		}
	}
	if !report.Healthy() || strings.Join(passed, " ") != want {
		t.Fatalf("report=%#v", report)
	}
	if before != after {
		t.Fatal("doctor changed the installation filesystem")
	}
	var calls []string
	for _, call := range services.calls {
		calls = append(calls, strings.Join(call, " "))
	}
	if got := strings.Join(calls, "|"); got != "is-enabled agent-forge-gate.service|is-active agent-forge-gate.service|is-enabled agent-forge-worker.service|is-active agent-forge-worker.service|gate-ready|worker-ready" {
		t.Fatalf("service calls=%s", got)
	}
}

func TestDoctorServiceFailureUsesOnlyCompleteObservationalProbes(t *testing.T) {
	services := &fakeServices{failAt: "is-enabled agent-forge-gate.service"}
	root := t.TempDir()
	before := doctorSnapshot(t, root)
	report := Doctor(DoctorOptions{Root: root, Services: services, Ownership: newFakeOwnership(), Account: &fakeAccount{}})
	after := doctorSnapshot(t, root)
	var calls []string
	for _, call := range services.calls {
		calls = append(calls, strings.Join(call, " "))
	}
	want := "is-enabled agent-forge-gate.service|is-active agent-forge-gate.service|is-enabled agent-forge-worker.service|is-active agent-forge-worker.service"
	if checkOK(report, "gate-enabled") || !checkOK(report, "gate-active") || strings.Join(calls, "|") != want || before != after {
		t.Fatalf("report=%#v calls=%v changed=%v", report, calls, before != after)
	}
}

func TestDoctorRejectsHardlinkedSystemdLinkWithoutMutation(t *testing.T) {
	assets, anchor := releaseFixture(t, nil)
	root := t.TempDir()
	account := &fakeAccount{}
	ownership := newFakeOwnership()
	if err := Install(Options{
		Version: testVersion, Commit: testCommit, AssetDir: assets, SHA256SUMSSHA256: anchor,
		Root: root, Arch: "amd64", Account: account, Ownership: ownership,
		Random: strings.NewReader(strings.Repeat("c", 32) + strings.Repeat("d", 32)),
	}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "etc/systemd/system/agent-forge-gate.service")
	if err := os.Link(link, filepath.Join(root, "hardlinked-unit")); err != nil {
		t.Fatal(err)
	}
	before := doctorSnapshot(t, root)
	report := Doctor(DoctorOptions{Root: root, Services: &fakeServices{}, Ownership: ownership, Account: account})
	after := doctorSnapshot(t, root)
	if checkOK(report, "trusted-paths") || before != after {
		t.Fatalf("trusted-paths accepted hardlink or mutated filesystem: %#v", report)
	}
}

func checkOK(report DoctorReport, id string) bool {
	for _, check := range report.Checks {
		if check.ID == id {
			return check.OK
		}
	}
	return false
}

func doctorSnapshot(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %s %d %d\n", strings.TrimPrefix(name, root), info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			h.Write(body)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
