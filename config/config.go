// Package config defines, loads and validates application configuration.
//
// Non-secret settings come from a JSON file (optional) with environment
// overrides for a few operational knobs. Secrets are read from the environment
// only and can never be loaded from, or written to, a file.
//
// The file format is JSON because the build environment of this phase could
// not fetch third-party modules; every struct also carries yaml tags so that
// switching to YAML later is a loader-only change.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"personal-ai/core/domain/model"
)

// Environment variable names.
const (
	EnvConfigPath      = "PERSONAL_AI_CONFIG"
	EnvServerAddr      = "PERSONAL_AI_SERVER_ADDR"
	EnvLogLevel        = "PERSONAL_AI_LOG_LEVEL"
	EnvLogFormat       = "PERSONAL_AI_LOG_FORMAT"
	EnvDatabaseURL     = "PERSONAL_AI_DATABASE_URL"
	EnvManagementToken = "PERSONAL_AI_MANAGEMENT_TOKEN"
	EnvInferenceToken  = "PERSONAL_AI_INFERENCE_TOKEN"
	EnvRunPodAPIKey    = "PERSONAL_AI_RUNPOD_API_KEY"
	EnvTelegramToken   = "PERSONAL_AI_TELEGRAM_TOKEN"
)

// MinTokenLength is the shortest accepted API token.
const MinTokenLength = 24

// Duration is a time.Duration that reads from strings such as "30m".
type Duration time.Duration

// Std converts to time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", b, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalText renders a Go duration string.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// Config is the full application configuration.
type Config struct {
	Server       ServerConfig     `json:"server" yaml:"server"`
	Log          LogConfig        `json:"log" yaml:"log"`
	Scheduling   SchedulingConfig `json:"scheduling" yaml:"scheduling"`
	Routing      RoutingConfig    `json:"routing" yaml:"routing"`
	Telegram     TelegramConfig   `json:"telegram" yaml:"telegram"`
	DefaultModel string           `json:"default_model" yaml:"default_model"`
	Models       []ModelConfig    `json:"models" yaml:"models"`

	// Secrets come from the environment only.
	Secrets Secrets `json:"-" yaml:"-"`
}

// ServerConfig configures the HTTP server. There is deliberately no write
// timeout: it would cut off long streamed responses.
type ServerConfig struct {
	Addr              string   `json:"addr" yaml:"addr"`
	ReadHeaderTimeout Duration `json:"read_header_timeout" yaml:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout" yaml:"read_timeout"`
	IdleTimeout       Duration `json:"idle_timeout" yaml:"idle_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout" yaml:"shutdown_timeout"`
	MaxBodyBytes      int64    `json:"max_body_bytes" yaml:"max_body_bytes"`
}

// LogConfig configures logging.
type LogConfig struct {
	Level  string `json:"level" yaml:"level"`   // debug, info, warn, error
	Format string `json:"format" yaml:"format"` // json, text
}

// SchedulingConfig configures cost control.
type SchedulingConfig struct {
	Interval    Duration `json:"interval" yaml:"interval"`
	IdleTimeout Duration `json:"idle_timeout" yaml:"idle_timeout"`
	MaxActive   int      `json:"max_active" yaml:"max_active"`
}

// Not-deployed policies: what to do when a request names a model that has no
// ready deployment.
const (
	PolicyReject             = "reject"
	PolicyStartAndWait       = "start_and_wait"
	PolicyStartAndReturnPend = "start_and_return_pending"
)

// RoutingConfig configures model routing.
type RoutingConfig struct {
	NotDeployedPolicy string `json:"not_deployed_policy" yaml:"not_deployed_policy"`
}

// TelegramConfig configures the Telegram bot. The allowlist is mandatory.
type TelegramConfig struct {
	AllowedUserIDs []int64 `json:"allowed_user_ids" yaml:"allowed_user_ids"`
}

// Secrets holds credentials. Every formatting path is redacted so an
// accidental log or print cannot leak them.
type Secrets struct {
	DatabaseURL     string
	ManagementToken string
	InferenceToken  string
	RunPodAPIKey    string
	TelegramToken   string
}

const redacted = "[REDACTED]"

// String redacts all secrets.
func (Secrets) String() string { return redacted }

// GoString redacts all secrets for %#v.
func (Secrets) GoString() string { return redacted }

// LogValue redacts all secrets for slog.
func (Secrets) LogValue() slog.Value { return slog.StringValue(redacted) }

// Defaults returns the configuration used for anything the file omits.
func Defaults() Config {
	return Config{
		Server: ServerConfig{
			Addr:              ":8080",
			ReadHeaderTimeout: Duration(10 * time.Second),
			ReadTimeout:       Duration(30 * time.Second),
			IdleTimeout:       Duration(2 * time.Minute),
			ShutdownTimeout:   Duration(30 * time.Second),
			MaxBodyBytes:      1 << 20,
		},
		Log: LogConfig{Level: "info", Format: "json"},
		Scheduling: SchedulingConfig{
			Interval:    Duration(time.Minute),
			IdleTimeout: Duration(30 * time.Minute),
			MaxActive:   1,
		},
		Routing: RoutingConfig{NotDeployedPolicy: PolicyReject},
	}
}

// Load reads configuration. path may be empty to use defaults only. getenv is
// injected for testability (pass os.Getenv). The result is validated.
func Load(path string, getenv func(string) string) (*Config, error) {
	cfg := Defaults()
	if path != "" {
		if err := cfg.readFile(path); err != nil {
			return nil, err
		}
	}
	cfg.applyEnv(getenv)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return &cfg, nil
}

func (c *Config) readFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields() // a typo in a key must not be silently ignored
	if err := dec.Decode(c); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse config %s: unexpected data after the top-level object", path)
	}
	return nil
}

func (c *Config) applyEnv(getenv func(string) string) {
	if v := getenv(EnvServerAddr); v != "" {
		c.Server.Addr = v
	}
	if v := getenv(EnvLogLevel); v != "" {
		c.Log.Level = v
	}
	if v := getenv(EnvLogFormat); v != "" {
		c.Log.Format = v
	}
	c.Secrets = Secrets{
		DatabaseURL:     getenv(EnvDatabaseURL),
		ManagementToken: getenv(EnvManagementToken),
		InferenceToken:  getenv(EnvInferenceToken),
		RunPodAPIKey:    getenv(EnvRunPodAPIKey),
		TelegramToken:   getenv(EnvTelegramToken),
	}
}

// Validate checks everything that does not depend on which executable runs.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Server.Addr == "" {
		add("server.addr is required")
	}
	for name, d := range map[string]Duration{
		"server.read_header_timeout": c.Server.ReadHeaderTimeout,
		"server.read_timeout":        c.Server.ReadTimeout,
		"server.idle_timeout":        c.Server.IdleTimeout,
		"server.shutdown_timeout":    c.Server.ShutdownTimeout,
		"scheduling.interval":        c.Scheduling.Interval,
		"scheduling.idle_timeout":    c.Scheduling.IdleTimeout,
	} {
		if d <= 0 {
			add("%s must be positive", name)
		}
	}
	if c.Server.MaxBodyBytes <= 0 {
		add("server.max_body_bytes must be positive")
	}

	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		add("log.level %q must be one of debug, info, warn, error", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "json", "text":
	default:
		add("log.format %q must be json or text", c.Log.Format)
	}

	if c.Scheduling.MaxActive < 1 {
		add("scheduling.max_active must be at least 1")
	}
	switch c.Routing.NotDeployedPolicy {
	case PolicyReject, PolicyStartAndWait, PolicyStartAndReturnPend:
	default:
		add("routing.not_deployed_policy %q must be one of %s, %s, %s",
			c.Routing.NotDeployedPolicy, PolicyReject, PolicyStartAndWait, PolicyStartAndReturnPend)
	}

	if _, err := c.DomainModels(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ValidateForServer adds the requirements specific to cmd/server: secrets
// must be present and strong.
func (c Config) ValidateForServer() error {
	var errs []error
	if c.Secrets.DatabaseURL == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvDatabaseURL))
	}
	for env, tok := range map[string]string{
		EnvManagementToken: c.Secrets.ManagementToken,
		EnvInferenceToken:  c.Secrets.InferenceToken,
	} {
		if len(tok) < MinTokenLength {
			errs = append(errs, fmt.Errorf("%s must be set to at least %d characters", env, MinTokenLength))
		}
	}
	if c.Secrets.ManagementToken != "" && c.Secrets.ManagementToken == c.Secrets.InferenceToken {
		errs = append(errs, errors.New("management and inference tokens must differ"))
	}
	return errors.Join(errs...)
}

// ValidateForTelegram adds the requirements specific to cmd/telegram. Possessing
// the bot link must never be enough: an explicit allowlist is mandatory.
func (c Config) ValidateForTelegram() error {
	var errs []error
	if c.Secrets.TelegramToken == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvTelegramToken))
	}
	if len(c.Telegram.AllowedUserIDs) == 0 {
		errs = append(errs, errors.New("telegram.allowed_user_ids must list at least one user"))
	}
	for _, id := range c.Telegram.AllowedUserIDs {
		if id <= 0 {
			errs = append(errs, fmt.Errorf("telegram.allowed_user_ids contains invalid id %d", id))
		}
	}
	return errors.Join(errs...)
}

// ModelConfig is the file representation of a catalog entry. Hardware
// requirements, runtime settings and generation defaults are kept in separate
// sections; runtime is opaque and interpreted by the runtime adapter.
type ModelConfig struct {
	ID           string             `json:"id" yaml:"id"`
	Name         string             `json:"name" yaml:"name"`
	Artifact     string             `json:"artifact" yaml:"artifact"`
	Description  string             `json:"description" yaml:"description"`
	Family       string             `json:"family" yaml:"family"`
	ParamCount   int64              `json:"param_count" yaml:"param_count"`
	Status       string             `json:"status" yaml:"status"`
	Features     []string           `json:"features" yaml:"features"`
	Requirements RequirementsConfig `json:"requirements" yaml:"requirements"`
	Runtime      map[string]any     `json:"runtime" yaml:"runtime"`
	Generation   GenerationConfig   `json:"generation" yaml:"generation"`
}

// RequirementsConfig is the file form of model.Requirements.
type RequirementsConfig struct {
	MinVRAMGB         int `json:"min_vram_gb" yaml:"min_vram_gb"`
	RecommendedVRAMGB int `json:"recommended_vram_gb" yaml:"recommended_vram_gb"`
	MinGPUCount       int `json:"min_gpu_count" yaml:"min_gpu_count"`
}

// GenerationConfig is the file form of model.Generation.
type GenerationConfig struct {
	Temperature *float64 `json:"temperature" yaml:"temperature"`
	TopP        *float64 `json:"top_p" yaml:"top_p"`
	MaxTokens   int      `json:"max_tokens" yaml:"max_tokens"`
}

// ToDomain converts to a domain model, applying defaults (status active,
// feature chat, one GPU). It does not validate; call Validate on the result.
func (mc ModelConfig) ToDomain() model.Model {
	status := model.Status(mc.Status)
	if status == "" {
		status = model.StatusActive
	}
	features := make([]model.Feature, 0, len(mc.Features))
	for _, f := range mc.Features {
		features = append(features, model.Feature(f))
	}
	if len(features) == 0 {
		features = []model.Feature{model.FeatureChat}
	}
	minGPUs := mc.Requirements.MinGPUCount
	if minGPUs == 0 {
		minGPUs = 1
	}
	return model.Model{
		ID:          model.ID(mc.ID),
		Name:        mc.Name,
		Artifact:    mc.Artifact,
		Description: mc.Description,
		Family:      mc.Family,
		ParamCount:  mc.ParamCount,
		Requirements: model.Requirements{
			MinVRAMGB:         mc.Requirements.MinVRAMGB,
			RecommendedVRAMGB: mc.Requirements.RecommendedVRAMGB,
			MinGPUCount:       minGPUs,
		},
		Features: features,
		Generation: model.Generation{
			Temperature: mc.Generation.Temperature,
			TopP:        mc.Generation.TopP,
			MaxTokens:   mc.Generation.MaxTokens,
		},
		Runtime: mc.Runtime,
		Status:  status,
	}
}

// DomainModels converts and validates the configured catalog. It rejects
// duplicate IDs and a default model that is not in the catalog.
func (c Config) DomainModels() ([]model.Model, error) {
	var errs []error
	models := make([]model.Model, 0, len(c.Models))
	seen := make(map[model.ID]bool, len(c.Models))
	for i, mc := range c.Models {
		m := mc.ToDomain()
		if err := m.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("models[%d] (%q): %w", i, mc.ID, err))
			continue
		}
		if seen[m.ID] {
			errs = append(errs, fmt.Errorf("models[%d]: duplicate id %q", i, m.ID))
			continue
		}
		seen[m.ID] = true
		models = append(models, m)
	}
	if c.DefaultModel != "" && !seen[model.ID(c.DefaultModel)] {
		errs = append(errs, fmt.Errorf("default_model %q is not in the model catalog", c.DefaultModel))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return models, nil
}
