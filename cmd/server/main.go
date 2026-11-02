// Command server is the composition root for the platform's HTTP server. It
// owns wiring only: loading configuration, building adapters and application
// services, injecting dependencies, and starting and stopping the process.
// No business logic lives here.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"personal-ai/adapters/inbound/rest"
	"personal-ai/config"
	"personal-ai/core/application/scheduling"
	"personal-ai/internal/logging"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", getenv(config.EnvConfigPath), "path to the JSON configuration file")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath, getenv)

	if err != nil {
		return err
	}

	if err := cfg.ValidateForServer(); err != nil {
		return fmt.Errorf("invalid server configuration: %w", err)
	}

	logger, err := logging.New(cfg.Log.Level, cfg.Log.Format, stderr)

	if err != nil {
		return err
	}

	models, err := cfg.DomainModels()

	if err != nil {
		return err
	}

	policy := scheduling.Policy{IdleTimeout: cfg.Scheduling.IdleTimeout.Std(), MaxActive: cfg.Scheduling.MaxActive}

	if err := policy.Validate(); err != nil {
		return fmt.Errorf("invalid scheduling policy: %w", err)
	}

	// Later phases construct adapters (PostgreSQL, RunPod, vLLM, event bus) and
	// application services here and inject them into the inbound adapters.
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           rest.NewHandler(logger),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes:    1 << 16,
		// No WriteTimeout: it would terminate long streamed responses.
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	logger.Info("server starting",
		"addr", cfg.Server.Addr,
		"models", len(models),
		"idle_timeout", policy.IdleTimeout.String(),
		"max_active", policy.MaxActive)

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	logger.Info("shutdown requested")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Std())
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info("server stopped")
	return nil
}
