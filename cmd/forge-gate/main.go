package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-forge/internal/gate"
	"agent-forge/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	db := flag.String("db", "forge.db", "SQLite path")
	workerID := flag.String("worker-id", "worker-1", "authorized worker ID")
	workerToken := flag.String("worker-token", os.Getenv("FORGE_WORKER_TOKEN"), "worker bearer token (default FORGE_WORKER_TOKEN)")
	ownerToken := flag.String("owner-token", os.Getenv("FORGE_OWNER_TOKEN"), "owner bearer token (default FORGE_OWNER_TOKEN)")
	leaseTTL := flag.Duration("lease-ttl", 30*time.Second, "lease TTL")
	retryBase := flag.Duration("retry-base", time.Second, "base retry backoff")
	maxAttempts := flag.Int("max-attempts", 3, "maximum attempts per job")
	recoveryInterval := flag.Duration("recovery-interval", time.Second, "expired lease sweep interval")
	flag.Parse()
	if *workerToken == "" || *ownerToken == "" || *workerToken == *ownerToken {
		return errors.New("distinct non-empty worker and owner tokens are required")
	}
	options := gate.DefaultOptions()
	options.Policy = store.RecoveryPolicy{LeaseTTL: *leaseTTL, BaseRetryBackoff: *retryBase, MaxAttempts: *maxAttempts}
	options.RecoveryInterval = *recoveryInterval
	if err := options.Validate(); err != nil {
		return err
	}
	s, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer s.Close()
	handler, err := gate.NewHandlerWithOptions(s, map[string]string{*workerToken: *workerID}, *ownerToken, options)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	recoveryErr, err := gate.StartRecovery(ctx, s, options)
	if err != nil {
		return fmt.Errorf("recovery startup: %w", err)
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		stop()
		<-recoveryErr
		return err
	}
	server := &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	log.Printf("forge-gate listening on %s", *addr)
	select {
	case err := <-recoveryErr:
		_ = server.Shutdown(context.Background())
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("recovery stopped: %w", err)
	case err := <-serveErr:
		stop()
		<-recoveryErr
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		err := server.Shutdown(context.Background())
		<-recoveryErr
		return err
	}
}
