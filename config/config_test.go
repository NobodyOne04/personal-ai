package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"personal-ai/core/domain/model"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodModel = `{"id":"m1","name":"M1","artifact":"org/m1","requirements":{"min_vram_gb":24}}`

func TestLoadDefaultsWithoutFile(t *testing.T) {
	cfg, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":8080" || cfg.Scheduling.MaxActive != 1 || cfg.Routing.NotDeployedPolicy != PolicyReject {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.Scheduling.IdleTimeout.Std() != 30*time.Minute {
		t.Fatalf("idle timeout = %v", cfg.Scheduling.IdleTimeout.Std())
	}
}

func TestLoadFileOverridesDefaultsButKeepsTheRest(t *testing.T) {
	p := writeFile(t, `{"server":{"addr":":9999"},"scheduling":{"idle_timeout":"5m","max_active":3}}`)
	cfg, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9999" || cfg.Scheduling.IdleTimeout.Std() != 5*time.Minute || cfg.Scheduling.MaxActive != 3 {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if cfg.Server.ReadTimeout.Std() != 30*time.Second || cfg.Scheduling.Interval.Std() != time.Minute {
		t.Fatalf("defaults for omitted fields were lost: %+v", cfg)
	}
}

func TestLoadReadsYAML(t *testing.T) {
	cfg, err := Load(writeFile(t, `
# comments are allowed
server:
  addr: ":9090" # trailing comment
scheduling:
  idle_timeout: 5m
models:
  - id: m1
    name: M1
    artifact: org/m1
    requirements: {min_vram_gb: 24}
    runtime: {max_model_len: 4096}
`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9090" || cfg.Scheduling.IdleTimeout.Std() != 5*time.Minute || len(cfg.Models) != 1 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, ok := cfg.Models[0].Runtime["max_model_len"].(int); !ok {
		t.Fatalf("runtime numbers decode as int: %T", cfg.Models[0].Runtime["max_model_len"])
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	tests := map[string]string{
		"unknown key":      `{"serverr":{"addr":":1"}}`,
		"unknown nested":   `{"server":{"adr":":1"}}`,
		"bad duration":     `{"scheduling":{"idle_timeout":"soon"}}`,
		"numeric duration": `{"scheduling":{"idle_timeout":300}}`,
		"trailing data":    `{} {}`,
		"scalar section":   `server: yaml`,
		"two documents":    "server: {addr: \":1\"}\n---\nserver: {addr: \":2\"}\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, content), env(nil)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), env(nil)); err == nil {
		t.Fatal("missing file must be an error")
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg, err := Load("", env(map[string]string{
		EnvServerAddr: ":7000", EnvLogLevel: "debug", EnvLogFormat: "text",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":7000" || cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Fatalf("env not applied: %+v", cfg)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := map[string]func(*Config){
		"empty addr":        func(c *Config) { c.Server.Addr = "" },
		"zero timeout":      func(c *Config) { c.Server.ReadTimeout = 0 },
		"zero body limit":   func(c *Config) { c.Server.MaxBodyBytes = 0 },
		"bad log level":     func(c *Config) { c.Log.Level = "loud" },
		"bad log format":    func(c *Config) { c.Log.Format = "xml" },
		"zero max active":   func(c *Config) { c.Scheduling.MaxActive = 0 },
		"zero idle timeout": func(c *Config) { c.Scheduling.IdleTimeout = 0 },
		"bad policy":        func(c *Config) { c.Routing.NotDeployedPolicy = "yolo" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c := Defaults()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestModelCatalog(t *testing.T) {
	p := writeFile(t, fmt.Sprintf(`{"default_model":"m1","models":[%s]}`, goodModel))
	cfg, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	models, err := cfg.DomainModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("got %v, %v", models, err)
	}
	m := models[0]
	if m.Status != model.StatusActive || !m.Supports(model.FeatureChat) || m.Requirements.MinGPUCount != 1 {
		t.Fatalf("defaults not applied to model: %+v", m)
	}
}

func TestModelCatalogRejects(t *testing.T) {
	tests := map[string]string{
		"invalid model":         `{"models":[{"id":"m1"}]}`,
		"duplicate ids":         fmt.Sprintf(`{"models":[%s,%s]}`, goodModel, goodModel),
		"unknown default model": fmt.Sprintf(`{"default_model":"nope","models":[%s]}`, goodModel),
		"default without any":   `{"default_model":"m1"}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, content), env(nil)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestModelRuntimeStaysOpaque(t *testing.T) {
	p := writeFile(t, `{"models":[{"id":"m1","name":"M1","artifact":"a","requirements":{"min_vram_gb":8},
		"runtime":{"max_model_len":8192,"dtype":"auto"},"generation":{"temperature":0.7,"top_p":0.9,"max_tokens":512}}]}`)
	cfg, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	models, _ := cfg.DomainModels()
	if models[0].Runtime["dtype"] != "auto" || models[0].Generation.MaxTokens != 512 {
		t.Fatalf("runtime/generation not carried through: %+v", models[0])
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := Load("config.example.yaml", env(nil))
	if err != nil {
		t.Fatalf("config.example.yaml must stay loadable: %v", err)
	}
	if _, err := cfg.DomainModels(); err != nil {
		t.Fatal(err)
	}
}

func goodSecrets() map[string]string {
	return map[string]string{
		EnvDatabaseURL:     "postgres://user:pw@db/app",
		EnvManagementToken: strings.Repeat("m", MinTokenLength),
		EnvInferenceToken:  strings.Repeat("i", MinTokenLength),
		EnvRunPodAPIKey:    "rpa_key",
		EnvRuntimeKey:      strings.Repeat("r", MinTokenLength),
	}
}

const offeringsJSON = `{"compute":{"runpod":{"offerings":[{"id":"a40","gpu_type_id":"NVIDIA A40","gpu_count":1,"vram_per_gpu_gb":48,"hourly_usd":0.4}]}}}`

func TestValidateForServer(t *testing.T) {
	withOfferings := writeFile(t, offeringsJSON)
	if c, _ := Load("", env(goodSecrets())); c.ValidateForServer() == nil {
		t.Fatal("server must require approved offerings")
	}
	cfg, err := Load(withOfferings, env(goodSecrets()))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateForServer(); err != nil {
		t.Fatalf("good secrets rejected: %v", err)
	}

	mutations := map[string]func(map[string]string){
		"missing db":        func(m map[string]string) { delete(m, EnvDatabaseURL) },
		"short management":  func(m map[string]string) { m[EnvManagementToken] = "short" },
		"missing inference": func(m map[string]string) { delete(m, EnvInferenceToken) },
		"identical tokens":  func(m map[string]string) { m[EnvInferenceToken] = m[EnvManagementToken] },
		"missing runpod":    func(m map[string]string) { delete(m, EnvRunPodAPIKey) },
		"short runtime key": func(m map[string]string) { m[EnvRuntimeKey] = "short" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			secrets := goodSecrets()
			mutate(secrets)
			c, err := Load(withOfferings, env(secrets))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.ValidateForServer(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidateForTelegram(t *testing.T) {
	c := Defaults()
	if err := c.ValidateForTelegram(); err == nil {
		t.Fatal("must require token and allowlist")
	}
	c.Secrets.TelegramToken = "tok"
	if err := c.ValidateForTelegram(); err == nil {
		t.Fatal("an empty allowlist must be rejected: owning the bot link is not authorisation")
	}
	c.Telegram.AllowedUserIDs = []int64{0}
	if err := c.ValidateForTelegram(); err == nil {
		t.Fatal("invalid user id must be rejected")
	}
	c.Telegram.AllowedUserIDs = []int64{12345}
	if err := c.ValidateForTelegram(); err == nil {
		t.Fatal("the bot needs an inference token to reach the API")
	}
	c.Secrets.InferenceToken = "inf"
	if err := c.ValidateForTelegram(); err != nil {
		t.Fatal(err)
	}
}

func TestSecretsAreNeverFormatted(t *testing.T) {
	secrets := goodSecrets()
	secrets[EnvRunPodAPIKey] = "rpa_SUPERSECRETKEY"
	cfg, err := Load("", env(secrets))
	if err != nil {
		t.Fatal(err)
	}

	var logged strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("cfg", "secrets", cfg.Secrets, "config", cfg)

	for _, rendered := range []string{
		fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg),
		fmt.Sprintf("%v", cfg.Secrets), fmt.Sprintf("%#v", cfg.Secrets), logged.String(),
	} {
		for _, secret := range []string{"SUPERSECRETKEY", "user:pw", strings.Repeat("m", MinTokenLength)} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("secret %q leaked in %q", secret, rendered)
			}
		}
	}
}

func TestSecretsCannotBeLoadedFromFile(t *testing.T) {
	p := writeFile(t, `{"Secrets":{"RunPodAPIKey":"from-file"}}`)
	if _, err := Load(p, env(nil)); err == nil {
		t.Fatal("secrets in the config file must be rejected, not silently accepted")
	}
}

// The configs shipped with the Kubernetes manifests must stay loadable and,
// with secrets supplied, acceptable to the server.
func TestKubernetesConfigsAreValid(t *testing.T) {
	for _, path := range []string{
		"../deployments/kubernetes/base/config.yaml",
		"../deployments/kubernetes/overlays/production/config.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			secrets := goodSecrets()
			secrets[EnvTelegramToken] = "tg"
			cfg, err := Load(path, env(secrets))
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.ValidateForServer(); err != nil {
				t.Fatal(err)
			}
		})
	}
	cfg, _ := Load("../deployments/kubernetes/overlays/production/config.yaml", env(map[string]string{
		EnvTelegramToken: "tg", EnvInferenceToken: "inf"}))
	if err := cfg.ValidateForTelegram(); err != nil {
		t.Fatalf("production config must be usable by the bot: %v", err)
	}
}
