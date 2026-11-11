// Command server is the composition root for the platform's HTTP server. It
// owns wiring only: loading configuration, building adapters and application
// services, injecting dependencies, and starting and stopping the process.
// No business logic lives here.
//
// Only this process holds database and provider credentials, and only it runs
// the scheduler and the startup reconciler (ARCHITECTURE.md §6a, A3/A4).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"personal-ai/adapters/inbound/rest"
	"personal-ai/adapters/outbound/compute/runpod"
	"personal-ai/adapters/outbound/inference/vllm"
	"personal-ai/adapters/outbound/messaging"
	"personal-ai/adapters/outbound/persistence/postgres"
	"personal-ai/config"
	deploymentapp "personal-ai/core/application/deployment"
	inferenceapp "personal-ai/core/application/inference"
	modelapp "personal-ai/core/application/model"
	"personal-ai/core/application/scheduling"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/model"
	"personal-ai/core/ports"
	"personal-ai/internal/logging"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type randomIDs struct{}

func (randomIDs) NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

var (
	_ ports.Clock       = systemClock{}
	_ ports.IDGenerator = randomIDs{}
)

func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", getenv(config.EnvConfigPath), "path to the JSON configuration file")
	syncModels := fs.Bool("sync-models", false, "overwrite catalog entries in the database with those in the config file")
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
	catalog, err := cfg.DomainModels()
	if err != nil {
		return err
	}
	policy := scheduling.Policy{IdleTimeout: cfg.Scheduling.IdleTimeout.Std(), MaxActive: cfg.Scheduling.MaxActive}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("invalid scheduling policy: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- outbound adapters ----
	pool, err := postgres.Open(ctx, cfg.Secrets.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, err := postgres.Migrate(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if len(applied) > 0 {
		logger.Info("database migrated", "versions", applied)
	}
	modelRepo := postgres.NewModelRepository(pool, logger)
	depRepo, err := postgres.NewDeploymentRepository(pool, cfg.Secrets.RuntimeKeySecret)
	if err != nil {
		return err
	}
	usageRepo := postgres.NewUsageRepository(pool)
	prefRepo := postgres.NewPreferenceRepository(pool)

	rp := cfg.Compute.RunPod
	offerings := make([]runpod.Offering, len(rp.Offerings))
	for i, o := range rp.Offerings {
		offerings[i] = runpod.Offering{ID: o.ID, GPUTypeID: o.GPUTypeID, GPUCount: o.GPUCount, VRAMPerGPUGB: o.VRAMPerGPUGB,
			CPUCores: o.CPUCores, MemoryGB: o.MemoryGB, HourlyCostMicros: int64(math.Round(o.HourlyUSD * 1_000_000))}
	}
	provider, err := runpod.New(runpod.Config{
		APIKey: cfg.Secrets.RunPodAPIKey, BaseURL: rp.BaseURL, CloudType: rp.CloudType, NamePrefix: rp.NamePrefix,
		ContainerDiskGB: rp.ContainerDiskGB, VolumeGB: rp.VolumeGB, Offerings: offerings, Retries: 3,
	})
	if err != nil {
		return err
	}
	runtime, err := vllm.New(vllm.Config{HFToken: cfg.Secrets.HFToken,
		DefaultImage: cfg.Runtime.Image, AllowedImages: cfg.Runtime.AllowedImages,
		AllowTrustRemoteCode: cfg.Runtime.AllowTrustRemoteCode, AllowExtraArgs: cfg.Runtime.AllowExtraArgs})
	if err != nil {
		return err
	}
	bus := messaging.New(messaging.WithLogger(logger))
	defer bus.Close()
	if _, err := bus.Subscribe(func(_ context.Context, e ports.Event) {
		logger.Debug("event", "type", e.Type, "deployment_id", e.DeploymentID, "model_id", e.ModelID)
	}); err != nil {
		return err
	}

	// ---- application services ----
	clock, ids := systemClock{}, randomIDs{}
	activity := deployment.NewActivity()

	models := modelapp.NewService(modelRepo, prefRepo, provider, clock)
	seed, err := models.Seed(ctx, catalog, *syncModels)
	if err != nil {
		return fmt.Errorf("seed model catalog: %w", err)
	}
	logger.Info("model catalog seeded", "created", len(seed.Created), "updated", len(seed.Updated), "kept", len(seed.Skipped))
	if cfg.DefaultModel != "" {
		if _, err := models.GetDefault(ctx, rest.DefaultSubject); errors.Is(err, modelapp.ErrNoDefault) {
			if err := models.SetDefault(ctx, rest.DefaultSubject, model.ID(cfg.DefaultModel)); err != nil {
				logger.Warn("could not set configured default model", "model_id", cfg.DefaultModel, "error", err)
			}
		}
	}

	deployments := deploymentapp.NewService(deploymentapp.Deps{
		Deployments: depRepo, Models: modelRepo, Compute: provider, Runtime: runtime, Usage: usageRepo,
		Events: bus, Clock: clock, IDs: ids, Activity: activity, Logger: logger,
	}, deploymentapp.Config{
		MaxActive:        cfg.Scheduling.MaxActive,
		ProvisionTimeout: cfg.Deployment.ProvisionTimeout.Std(),
		ReadyTimeout:     cfg.Deployment.ReadyTimeout.Std(),
		PollInterval:     cfg.Deployment.PollInterval.Std(),
		StorageGB:        cfg.Deployment.StorageGB,
	})
	defer deployments.Close()

	inference, err := inferenceapp.NewService(inferenceapp.Deps{
		Models: models, Deployments: deployments, Runtime: runtime, Usage: usageRepo, Events: bus,
		Activity: activity, Clock: clock, IDs: ids, Logger: logger,
	}, inferenceapp.Config{
		Policy:      inferenceapp.Policy(cfg.Routing.NotDeployedPolicy),
		WaitTimeout: cfg.Routing.WaitTimeout.Std(),
	})
	if err != nil {
		return err
	}
	scheduler, err := scheduling.NewScheduler(deployments, policy, clock, logger)
	if err != nil {
		return err
	}

	// Reconcile before accepting traffic. Failures are logged, not fatal: the
	// scheduler keeps retrying cleanup, and refusing to start would leave
	// compute running with nobody to stop it.
	rep, err := deployments.Reconcile(ctx)
	logger.Info("startup reconciliation finished", "relaunched", len(rep.Relaunched), "stopped", len(rep.Stopped),
		"health_checked", len(rep.HealthChecked), "orphans_terminated", len(rep.OrphansTerminated))
	if err != nil {
		logger.Error("startup reconciliation had errors", "error", err)
	}

	// ---- inbound adapter ----
	srv := &http.Server{
		Addr: cfg.Server.Addr,
		Handler: rest.NewHandler(rest.Config{
			Logger: logger, Models: models, Deployments: deployments, Inference: inference,
			ManagementToken: cfg.Secrets.ManagementToken, InferenceToken: cfg.Secrets.InferenceToken,
			MaxBodyBytes: cfg.Server.MaxBodyBytes,
		}),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes:    1 << 16,
		// No WriteTimeout: it would terminate long streamed responses.
		// Request contexts are not tied to the signal context, so Shutdown can
		// drain in-flight streams instead of cancelling them immediately.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	tickerDone := make(chan struct{})
	tickCtx, stopTicks := context.WithCancel(ctx)
	defer stopTicks()
	go func() {
		defer close(tickerDone)
		runScheduler(tickCtx, scheduler, cfg.Scheduling.Interval.Std(), logger)
	}()

	logger.Info("server starting", "addr", cfg.Server.Addr, "models", len(catalog),
		"idle_timeout", policy.IdleTimeout.String(), "max_active", policy.MaxActive,
		"not_deployed_policy", cfg.Routing.NotDeployedPolicy)

	var result error
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			result = fmt.Errorf("serve: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}

	stopTicks()
	<-tickerDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Std())
	defer cancel()
	// Shutdown stops accepting connections and waits for in-flight requests,
	// including streams, until the deadline.
	if err := srv.Shutdown(shutdownCtx); err != nil && result == nil {
		result = fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("server stopped")
	return result
}

// runScheduler calls Tick on every interval until ctx ends.
func runScheduler(ctx context.Context, s *scheduling.Scheduler, every time.Duration, logger *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			res, err := s.Tick(ctx)
			if err != nil && ctx.Err() == nil {
				logger.Warn("scheduler tick had errors", "error", err)
			}
			if len(res.Stopped)+len(res.CleanedUp) > 0 {
				logger.Info("scheduler tick", "stopped", len(res.Stopped), "cleaned_up", len(res.CleanedUp))
			}
		}
	}
}
