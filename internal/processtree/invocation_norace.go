//go:build !race

package processtree

func invocationWrapperEnvironment(env []string) []string { return env }
