//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package githubdelivery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedOpenOwnerPolicySeam(t *testing.T) {
	if os.Geteuid() != 0 && owned(fileInfoWithOwner{uid: 0}) {
		t.Fatal("generic protected open ownership accepted root for a non-root euid")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "protected")
	if err := os.WriteFile(path, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if file, err := openProtectedWithOwner(path, func(os.FileInfo) bool { return false }); err == nil {
		file.Close()
		t.Fatal("opened file rejected by parent owner policy")
	}
	file, err := openProtectedWithOwner(path, func(os.FileInfo) bool { return true })
	if err != nil {
		t.Fatalf("open with accepting owner policy: %v", err)
	}
	file.Close()
}
