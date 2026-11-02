// Command telegram is the composition root for the Telegram bot (Phase 8).
// Until the adapter exists it only validates its configuration and then exits
// with a clear error, rather than pretending to run.
package main

import (
	"fmt"
	"os"

	"personal-ai/config"
)

func main() {
	cfg, err := config.Load(os.Getenv(config.EnvConfigPath), os.Getenv)
	if err == nil {
		err = cfg.ValidateForTelegram()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "error: the Telegram adapter is not implemented yet (planned for Phase 8)")
	os.Exit(2)
}
