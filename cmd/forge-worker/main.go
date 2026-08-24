package main

import (
	"agent-forge/internal/worker"
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	gate := flag.String("gate", "ws://127.0.0.1:8080", "Gate WebSocket base URL")
	id := flag.String("id", "worker-1", "worker ID")
	token := flag.String("token", "dev-token", "bearer token")
	plugin := flag.String("plugin", "./bin/forge-ref-plugin", "plugin executable")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := worker.Run(ctx, *gate, *id, *token, *plugin); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
