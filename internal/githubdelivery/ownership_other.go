//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package githubdelivery

import "os"

func owned(os.FileInfo) bool { return false }

func executableOwned(os.FileInfo) bool { return false }
