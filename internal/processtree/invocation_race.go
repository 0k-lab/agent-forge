//go:build race

package processtree

import (
	"os"
	"strings"
)

func invocationWrapperEnvironment(env []string) []string {
	if env == nil {
		env = os.Environ()
	} else {
		env = append([]string(nil), env...)
	}
	for i, entry := range env {
		if strings.HasPrefix(entry, "GORACE=") {
			env[i] = entry + " atexit_sleep_ms=0"
			return env
		}
	}
	return append(env, "GORACE=atexit_sleep_ms=0")
}
