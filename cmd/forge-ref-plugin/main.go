package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"agent-forge/internal/pluginprotocol"
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(in io.Reader, out io.Writer) error {
	return pluginprotocol.Serve(in, out, []pluginprotocol.Capability{pluginprotocol.Text}, func(_ context.Context, request pluginprotocol.Request) (pluginprotocol.Result, error) {
		return pluginprotocol.Result{Output: "FORGE: " + strings.ToUpper(request.Input)}, nil
	})
}
