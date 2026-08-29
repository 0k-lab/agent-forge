package buildinfo

import (
	"fmt"
	"io"
)

var (
	Version = "dev"
	Commit  = "unknown"
)

func Line(component string) string {
	return component + " " + Version + " " + Commit
}

func WriteIfRequested(args []string, output io.Writer, component string) (bool, error) {
	if len(args) != 1 || args[0] != "--version" {
		return false, nil
	}
	_, err := fmt.Fprintln(output, Line(component))
	return true, err
}
