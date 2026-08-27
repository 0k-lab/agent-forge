//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package githubdelivery

import (
	"os"
)

func openProtected(string) (*os.File, error)                     { return nil, errConfig }
func openProtectedExecutable(string) (*os.File, error)           { return nil, errConfig }
func readProtectedFile(string, os.FileMode, int) ([]byte, error) { return nil, errConfig }
