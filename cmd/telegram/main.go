// Command telegram is the composition root for the Telegram bot. It runs as a
// separate process that reaches the platform only through the REST API, so it
// holds no database or provider credentials and never runs the scheduler
// (ARCHITECTURE.md §6a, A3/A4).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"personal-ai/adapters/inbound/telegram"
	"personal-ai/config"
	"personal-ai/internal/apiclient"
	"personal-ai/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv(config.EnvConfigPath), os.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.ValidateForTelegram(); err != nil {
		return fmt.Errorf("invalid telegram configuration: %w", err)
	}
	logger, err := logging.New(cfg.Log.Level, cfg.Log.Format, os.Stderr)
	if err != nil {
		return err
	}
	platform, err := apiclient.New(apiclient.Config{
		BaseURL:         cfg.Client.APIURL,
		ManagementToken: cfg.Secrets.ManagementToken,
		InferenceToken:  cfg.Secrets.InferenceToken,
		Timeout:         cfg.Client.Timeout.Std(),
	})
	if err != nil {
		return err
	}
	api, err := telegram.NewBotAPI(cfg.Secrets.TelegramToken, "", nil)
	if err != nil {
		return err
	}
	bot, err := telegram.New(telegram.Config{
		API: api, Platform: platform, AllowedUserIDs: cfg.Telegram.AllowedUserIDs,
		ManagementEnabled: cfg.Secrets.ManagementToken != "",
		RequestTimeout:    cfg.Client.Timeout.Std(), Logger: logger,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("telegram bot starting", "api_url", cfg.Client.APIURL, "allowed_users", len(cfg.Telegram.AllowedUserIDs),
		"management_commands", cfg.Secrets.ManagementToken != "")
	err = bot.Run(ctx)
	logger.Info("telegram bot stopped")
	return err
}
