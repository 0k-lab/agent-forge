package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"agent-forge/internal/githubdelivery"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runContext(context.Background(), args, stdin, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("forge-github", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "GitHub delivery JSON config path")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid arguments")
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New("-config is required")
	}
	publication, err := githubdelivery.ReadPublication(stdin)
	if err != nil {
		return err
	}
	config, err := githubdelivery.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	result, err := githubdelivery.Deliver(ctx, config, publication, githubdelivery.Options{})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}
