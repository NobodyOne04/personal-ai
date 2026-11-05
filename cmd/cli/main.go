// Command cli builds the `personal-ai` command-line client. It is a
// composition root only: it reads the API location and tokens, builds the REST
// client and hands control to the CLI adapter. It never touches the database
// or the compute provider.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"personal-ai/adapters/inbound/cli"
	"personal-ai/config"
	"personal-ai/internal/apiclient"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(os.Getenv(config.EnvConfigPath), os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return cli.ExitError
	}
	client, err := apiclient.New(apiclient.Config{
		BaseURL:         cfg.Client.APIURL,
		ManagementToken: cfg.Secrets.ManagementToken,
		InferenceToken:  cfg.Secrets.InferenceToken,
		Timeout:         cfg.Client.Timeout.Std(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return cli.ExitError
	}
	// Ctrl-C cancels the current request (and stops a streamed answer).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	app := &cli.App{Client: client, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	return app.Run(ctx, os.Args[1:])
}
