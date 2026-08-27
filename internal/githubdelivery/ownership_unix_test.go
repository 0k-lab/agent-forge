//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package githubdelivery

import (
	"os"
	"syscall"
	"testing"
)

type fileInfoWithOwner struct {
	os.FileInfo
	uid uint32
}

func (info fileInfoWithOwner) Sys() any { return &syscall.Stat_t{Uid: info.uid} }

func TestExecutableOwnedAcceptsRootOrCurrentUser(t *testing.T) {
	unrelatedUID := uint32(os.Geteuid() + 1)
	if unrelatedUID == 0 {
		unrelatedUID++
	}
	for _, test := range []struct {
		name string
		uid  uint32
		want bool
	}{
		{"root", 0, true},
		{"current user", uint32(os.Geteuid()), true},
		{"unrelated user", unrelatedUID, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := executableOwned(fileInfoWithOwner{uid: test.uid}); got != test.want {
				t.Fatalf("executableOwned(uid %d) = %t, want %t", test.uid, got, test.want)
			}
		})
	}
}
