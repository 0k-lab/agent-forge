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

	"agent-forge/internal/gate"
	"agent-forge/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	configPath := flag.String("config", "", "Gate JSON config path")
	flag.Parse()
	if *configPath == "" || flag.NArg() != 0 {
		return errors.New("-config is required")
	}
	config, err := gate.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	options := gate.DefaultOptions()
	options.RecoveryInterval = config.RecoveryInterval
	options.LeasePollInterval = config.LeasePollInterval
	s, err := store.Open(config.Database)
	if err != nil {
		return err
	}
	defer s.Close()
	handler, err := gate.NewConfiguredHandler(s, config, options)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	recoveryErr, err := gate.StartConfiguredRecovery(ctx, s, config.RecoveryInterval, options.Now)
	if err != nil {
		return fmt.Errorf("recovery startup: %w", err)
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		stop()
		<-recoveryErr
		return err
	}
	server := &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	log.Printf("forge-gate listening on %s", config.Listen)
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
