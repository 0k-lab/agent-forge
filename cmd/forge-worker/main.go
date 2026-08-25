package main

import (
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
