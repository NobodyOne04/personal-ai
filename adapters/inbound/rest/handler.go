// Package rest is the HTTP inbound adapter. It translates HTTP requests into
// application use-case calls and use-case results into HTTP responses; it
// contains no business decisions and never talks to a provider, runtime or
// database directly.
//
// Routes:
//
//	GET  /healthz                       liveness (no auth)
//	/api/v1/...                         management API (management token)
//	GET  /v1/models                     OpenAI-compatible (inference token)
//	POST /v1/chat/completions           OpenAI-compatible, JSON or SSE
//
// Runtime endpoints and credentials are never exposed in responses. Request
// and response bodies are never logged.
package rest

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	deploymentapp "personal-ai/core/application/deployment"
	inferenceapp "personal-ai/core/application/inference"
	modelapp "personal-ai/core/application/model"
	"personal-ai/internal/apitypes"
)

// DefaultSubject is the preferences subject for the single-owner deployment.
const DefaultSubject = "owner"

// Config wires the handler to the application.
type Config struct {
	Logger      *slog.Logger
	Models      *modelapp.Service
	Deployments *deploymentapp.Service
	Inference   *inferenceapp.Service

	ManagementToken string
	InferenceToken  string
	MaxBodyBytes    int64
	// Subject is whose preferences apply; defaults to DefaultSubject.
	Subject string
}

type server struct {
	Config
	mgmtHash, infHash [32]byte
}

// NewHandler returns the HTTP handler. With no services configured it serves
// only /healthz.
func NewHandler(cfg Config) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if cfg.Subject == "" {
		cfg.Subject = DefaultSubject
	}
	s := &server{Config: cfg, mgmtHash: sha256.Sum256([]byte(cfg.ManagementToken)), infHash: sha256.Sum256([]byte(cfg.InferenceToken))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	if cfg.Models != nil && cfg.Deployments != nil && cfg.Inference != nil {
		mgmt := func(pattern string, h http.HandlerFunc) {
			mux.Handle(pattern, s.auth(s.mgmtHash, cfg.ManagementToken, h))
		}
		mgmt("GET /api/v1/models", s.listModels)
		mgmt("POST /api/v1/models", s.registerModel)
		mgmt("GET /api/v1/models/{id}", s.getModel)
		mgmt("PUT /api/v1/models/{id}", s.updateModel)
		mgmt("POST /api/v1/models/{id}/deactivate", s.deactivateModel)
		mgmt("POST /api/v1/models/{id}/activate", s.activateModel)
		mgmt("GET /api/v1/models/{id}/validate", s.validateModel)
		mgmt("GET /api/v1/preferences/default-model", s.getDefaultModel)
		mgmt("PUT /api/v1/preferences/default-model", s.setDefaultModel)
		mgmt("GET /api/v1/deployments", s.listDeployments)
		mgmt("POST /api/v1/deployments", s.createDeployment)
		mgmt("GET /api/v1/deployments/{id}", s.getDeployment)
		mgmt("DELETE /api/v1/deployments/{id}", s.deleteDeployment)
		mgmt("POST /api/v1/deployments/{id}/stop", s.stopDeployment)
		mgmt("POST /api/v1/deployments/{id}/restart", s.restartDeployment)
		mgmt("POST /api/v1/deployments/{id}/pin", s.pinDeployment)
		mgmt("POST /api/v1/deployments/{id}/unpin", s.unpinDeployment)
		mgmt("POST /api/v1/deployments/{id}/health", s.checkDeploymentHealth)
		mgmt("GET /api/v1/usage", s.usageSummary)

		inf := func(pattern string, h http.HandlerFunc) {
			mux.Handle(pattern, s.auth(s.infHash, cfg.InferenceToken, h))
		}
		inf("GET /v1/models", s.openAIModels)
		inf("POST /v1/chat/completions", s.chatCompletions)
	}

	return s.recoverer(s.logRequests(mux))
}

// ---- middleware ----

// auth checks a bearer token by comparing SHA-256 digests in constant time,
// so neither the token's content nor its length leaks through timing. An
// empty configured token rejects everything.
func (s *server) auth(want [32]byte, configured string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(strings.TrimSpace(tok)))
		if !ok || configured == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="personal-ai"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Flush supports streaming through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		s.Logger.Info("http request", "request_id", id, "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start).String())
	})
}

func (s *server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Logger.Error("handler panic", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---- encoding ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	typ := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		typ = "authentication_error"
	case status >= 500:
		typ = "server_error"
	}
	writeJSON(w, status, apitypes.Error{Error: apitypes.ErrorDetail{Message: msg, Type: typ, Code: code}})
}

// decode reads a size-limited JSON body. strict rejects unknown fields.
func (s *server) decode(w http.ResponseWriter, r *http.Request, v any, strict bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errorsAs(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
