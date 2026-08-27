package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-forge/internal/gate"
	"agent-forge/internal/store"
)

const shutdownTimeout = 10 * time.Second

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
}

func shutdown(server *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return server.Shutdown(ctx)
}

func main() {
	logger := newJSONLogger(os.Stderr)
	if err := run(os.Args[1:], logger); err != nil {
		writeStartupError(os.Stderr, err)
		os.Exit(1)
	}
}

func newJSONLogger(output io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, nil))
}

type startupErrorCode string

const (
	codeStartupFailed       startupErrorCode = "startup_failed"
	codeInvalidDatabase     startupErrorCode = "invalid_database_location"
	codeInsecureDatabase    startupErrorCode = "insecure_database"
	codeUnsupportedDatabase startupErrorCode = "unsupported_database"
	codeDatabaseOpenFailed  startupErrorCode = "database_open_failed"
	codeAlreadyOwned        startupErrorCode = "already_owned"
)

func writeStartupError(output io.Writer, err error) {
	code := codeStartupFailed
	for target, value := range map[error]startupErrorCode{
		store.ErrInvalidDatabaseLocation: codeInvalidDatabase,
		store.ErrInsecureDatabase:        codeInsecureDatabase,
		store.ErrUnsupportedDatabase:     codeUnsupportedDatabase,
		store.ErrDatabaseOpen:            codeDatabaseOpenFailed,
		store.ErrAlreadyOwned:            codeAlreadyOwned,
	} {
		if errors.Is(err, target) {
			code = value
		}
	}
	newJSONLogger(output).Error("gate lifecycle", "component", "gate", "event", "startup_failed", "failure_code", code)
}

func run(args []string, logger *slog.Logger) error {
	logger.Info("gate lifecycle", "component", "gate", "event", "startup")
	flags := flag.NewFlagSet("forge-gate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "Gate JSON config path")
	if flags.Parse(args) != nil || *configPath == "" || flags.NArg() != 0 {
		return errors.New("-config is required")
	}
	config, err := gate.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	options := gate.DefaultOptions()
	options.RecoveryInterval = config.RecoveryInterval
	options.LeasePollInterval = config.LeasePollInterval
	options.Logger = logger
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
	server := newHTTPServer(handler)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	logger.Info("gate lifecycle", "component", "gate", "event", "listening")
	select {
	case err := <-recoveryErr:
		_ = shutdown(server)
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
		err := shutdown(server)
		<-recoveryErr
		return err
	}
}
