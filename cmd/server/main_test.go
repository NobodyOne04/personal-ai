package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"personal-ai/adapters/outbound/persistence/postgres"
)

func goodEnv(addr, dbURL, configPath string) func(string) string {
	m := map[string]string{
		"PERSONAL_AI_SERVER_ADDR":        addr,
		"PERSONAL_AI_DATABASE_URL":       dbURL,
		"PERSONAL_AI_MANAGEMENT_TOKEN":   strings.Repeat("m", 24),
		"PERSONAL_AI_INFERENCE_TOKEN":    strings.Repeat("i", 24),
		"PERSONAL_AI_RUNPOD_API_KEY":     "rpa_TESTKEY",
		"PERSONAL_AI_RUNTIME_KEY_SECRET": strings.Repeat("r", 24),
		"PERSONAL_AI_LOG_FORMAT":         "text",
		"PERSONAL_AI_CONFIG":             configPath,
	}
	return func(k string) string { return m[k] }
}

func TestRunRefusesToStartWithoutSecrets(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), nil, func(string) string { return "" }, &stderr)
	if err == nil || !strings.Contains(err.Error(), "PERSONAL_AI_MANAGEMENT_TOKEN") {
		t.Fatalf("want a missing-secret error, got %v", err)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(context.Background(), []string{"-bogus"}, goodEnv(":0", "postgres://x", ""), &stderr); err == nil {
		t.Fatal("expected flag error")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// TestRunServesAndStopsOnContextCancel boots the whole server against a real
// PostgreSQL (PERSONAL_AI_TEST_DATABASE_URL) and a stub RunPod API.
func TestRunServesAndStopsOnContextCancel(t *testing.T) {
	dbURL := isolatedDatabase(t)
	runpodStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/pods" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer runpodStub.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{
		"compute": {"runpod": {"base_url": %q, "offerings": [{"id":"a40","gpu_type_id":"NVIDIA A40","gpu_count":1,"vram_per_gpu_gb":48}]}},
		"default_model": "m1",
		"models": [{"id":"m1","name":"M1","artifact":"org/m1","requirements":{"min_vram_gb":24}}]
	}`, runpodStub.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, goodEnv(addr, dbURL, cfgPath), &stderr) }()

	var resp *http.Response
	var err error
	for i := 0; i < 100; i++ {
		resp, err = http.Get(fmt.Sprintf("http://%s/healthz", addr))
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never became reachable: %v\nlogs: %s", err, stderr.String())
	}
	resp.Body.Close()

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://%s/api/v1/preferences/default-model", addr), nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("m", 24))
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("default model from config not applied: %v %v\nlogs: %s", resp, err, stderr.String())
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not shut down")
	}
	for _, secret := range []string{strings.Repeat("m", 24), "TESTKEY", strings.Repeat("r", 24)} {
		if strings.Contains(stderr.String(), secret) {
			t.Fatalf("secret leaked into logs: %s", stderr.String())
		}
	}
}

// isolatedDatabase returns a URL whose search_path is a fresh schema, so the
// test cannot interfere with other packages' tests on the same database.
func isolatedDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("PERSONAL_AI_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("PERSONAL_AI_TEST_DATABASE_URL not set")
	}
	pool, err := postgres.Open(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	schema := fmt.Sprintf("pai_server_test_%d", time.Now().UnixNano())
	if _, err := pool.Exec(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "search_path=" + schema
}
