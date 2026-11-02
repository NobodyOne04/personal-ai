// Package rest is the HTTP inbound adapter. It translates HTTP requests into
// application use-case calls and use-case results into HTTP responses; it
// contains no business decisions and never talks to a provider, runtime or
// database directly.
//
// Phase 1 provides process liveness only. The management API (/api/v1) and the
// OpenAI-compatible inference API (/v1) arrive in Phase 6.
package rest

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// NewHandler returns the HTTP handler for the server.
func NewHandler(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
			logger.Debug("healthz write failed", "error", err)
		}
	})
	return mux
}
