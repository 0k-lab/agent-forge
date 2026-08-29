package main

import (
	"agent-forge/internal/buildinfo"
	"agent-forge/internal/worker"
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if requested, err := buildinfo.WriteIfRequested(os.Args[1:], os.Stdout, "forge-worker"); requested {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	configPath := flag.String("config", "", "Worker JSON config path")
	flag.Parse()
	if *configPath == "" || flag.NArg() != 0 {
		log.Fatal(errors.New("-config is required"))
	}
	config, err := worker.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := worker.RunConfigured(ctx, config); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
