package main

import (
	"agent-forge/internal/worker"
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	gate := flag.String("gate", "ws://127.0.0.1:8080", "Gate WebSocket base URL")
	id := flag.String("id", "worker-1", "worker ID")
	token := flag.String("token", os.Getenv("FORGE_WORKER_TOKEN"), "bearer token (default FORGE_WORKER_TOKEN)")
	plugin := flag.String("plugin", "./bin/forge-ref-plugin", "plugin executable")
	heartbeatInterval := flag.Duration("heartbeat-interval", 10*time.Second, "heartbeat interval (must be shorter than Gate lease TTL)")
	roots := filepath.SplitList(os.Getenv("FORGE_REPO_ROOTS"))
	flag.Func("repo-root", "allowed repository root (repeatable; defaults FORGE_REPO_ROOTS)", func(root string) error {
		roots = append(roots, root)
		return nil
	})
	flag.Parse()
	if *token == "" {
		log.Fatal("non-empty worker token is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	options := worker.WorkerOptions{HeartbeatInterval: *heartbeatInterval}
	if err := options.Validate(); err != nil {
		log.Fatal(err)
	}
	if err := worker.RunWithOptions(ctx, *gate, *id, *token, *plugin, roots, options); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
