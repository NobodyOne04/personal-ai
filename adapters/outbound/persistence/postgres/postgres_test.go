package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
	"personal-ai/core/ports"
)

// These tests need a PostgreSQL database. They are skipped unless
// PERSONAL_AI_TEST_DATABASE_URL is set. Each test runs in its own schema,
// which is dropped afterwards.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("PERSONAL_AI_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("PERSONAL_AI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("pai_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	pool, err := Open(ctx, base+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	applied, err := Migrate(ctx, pool)
	if err != nil || len(applied) != 0 {
		t.Fatalf("second migrate applied %v, err %v", applied, err)
	}
	return pool
}

func sampleModel() model.Model {
	temp := 0.7
	return model.Model{
		ID: "m1", Name: "Model 1", Artifact: "org/m1", Family: "llama", ParamCount: 8_000_000_000,
		Requirements: model.Requirements{MinVRAMGB: 24, RecommendedVRAMGB: 48, MinGPUCount: 1},
		Features:     []model.Feature{model.FeatureChat},
		Generation:   model.Generation{Temperature: &temp, MaxTokens: 512},
		Runtime:      map[string]any{"max_model_len": float64(8192)},
		Status:       model.StatusActive,
	}
}

func TestRepositories(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	models := NewModelRepository(pool, nil)
	deps, err := NewDeploymentRepository(pool, "0123456789abcdef0123456789abcdef") // gitleaks:allow (fake test secret)
	if err != nil {
		t.Fatal(err)
	}
	use := NewUsageRepository(pool)
	prefs := NewPreferenceRepository(pool)

	// Models.
	m := sampleModel()
	if err := models.Save(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := models.Get(ctx, "m1")
	if err != nil || got.Name != "Model 1" || *got.Generation.Temperature != 0.7 || got.Runtime["max_model_len"] != float64(8192) {
		t.Fatalf("model roundtrip: %+v %v", got, err)
	}
	if _, err := models.Get(ctx, "nope"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// Preferences.
	if _, err := prefs.Get(ctx, "owner"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := prefs.Set(ctx, model.Preferences{SubjectID: "owner", DefaultModelID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := prefs.Get(ctx, "owner"); p.DefaultModelID != "m1" {
		t.Fatalf("prefs = %+v", p)
	}
	if err := prefs.Set(ctx, model.Preferences{SubjectID: "owner", DefaultModelID: "missing"}); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("want ErrNotFound for unknown model, got %v", err)
	}

	// Deployments with optimistic locking.
	now := time.Now().UTC().Truncate(time.Microsecond)
	d, _ := deployment.New("d1", "m1", "key-1", now)
	d.RuntimeKey = "runtime-key-0123456789abcdef0123456789"
	saved, err := deps.Save(ctx, d)
	if err != nil || saved.Version != 1 {
		t.Fatalf("insert: %+v %v", saved, err)
	}
	if _, err := deps.Save(ctx, d); !errors.Is(err, ports.ErrVersionConflict) {
		t.Fatalf("duplicate insert: want conflict, got %v", err)
	}
	_ = saved.Transition(deployment.Provisioning, now)
	_ = saved.AttachInstance("inst-1", now)
	saved.Endpoint = &compute.Endpoint{URL: "https://x"}
	saved.RuntimeConfig = map[string]any{"dtype": "auto"}
	updated, err := deps.Save(ctx, saved)
	if err != nil || updated.Version != 2 || *updated.InstanceID != "inst-1" || updated.Endpoint.URL != "https://x" {
		t.Fatalf("update: %+v %v", updated, err)
	}
	// ADR 0009: the key round-trips but is stored only encrypted.
	if updated.RuntimeKey != d.RuntimeKey {
		t.Fatal("runtime key must round-trip")
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT runtime_key_ciphertext FROM deployments WHERE id = 'd1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 || bytes.Contains(stored, []byte(d.RuntimeKey)) {
		t.Fatal("runtime key must be stored encrypted")
	}
	if _, err := deps.Save(ctx, saved); !errors.Is(err, ports.ErrVersionConflict) {
		t.Fatalf("stale update: want conflict, got %v", err)
	}
	if err := updated.Fail("boom", now); err != nil {
		t.Fatal(err)
	}
	failed, err := deps.Save(ctx, updated)
	if err != nil || failed.Failure == nil || failed.Failure.Reason != "boom" || failed.Failure.Stage != deployment.Provisioning {
		t.Fatalf("failure roundtrip: %+v %v", failed.Failure, err)
	}
	byKey, err := deps.FindByIdempotencyKey(ctx, "key-1")
	if err != nil || byKey.ID != "d1" {
		t.Fatalf("by key: %v", err)
	}
	if l, _ := deps.ListByStatus(ctx, deployment.Failed); len(l) != 1 {
		t.Fatalf("by status: %d", len(l))
	}
	if l, _ := deps.ListByModel(ctx, "m1"); len(l) != 1 {
		t.Fatalf("by model: %d", len(l))
	}

	later := now.Add(time.Hour)
	if err := deps.RecordActivity(ctx, "d1", later); err != nil {
		t.Fatal(err)
	}
	if err := deps.RecordActivity(ctx, "d1", now); err != nil {
		t.Fatal(err)
	}
	if g, _ := deps.Get(ctx, "d1"); !g.LastActivityAt.Equal(later) || g.Version != failed.Version {
		t.Fatalf("activity must only move forward and not bump version: %v v%d", g.LastActivityAt, g.Version)
	}

	// Usage.
	hourly := compute.Money{Currency: "USD", Micros: 3_600_000}
	if err := use.Append(ctx, usage.UptimeRecord("u1", "d1", "m1", &hourly, time.Hour, now)); err != nil {
		t.Fatal(err)
	}
	if err := use.Append(ctx, usage.UptimeRecord("u2", "d1", "m1", nil, time.Hour, now)); err != nil {
		t.Fatal(err)
	}
	if err := use.Append(ctx, usage.Record{ID: "u3", Kind: usage.KindInference, DeploymentID: "d1", ModelID: "m1",
		Outcome: usage.OutcomeCompleted, Tokens: inference.TokenUsage{Input: 10, Output: 5}, Duration: time.Second, At: now}); err != nil {
		t.Fatal(err)
	}
	sum, err := use.Summarize(ctx, usage.TimeRange{From: now.Add(-time.Minute), To: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 1 || sum.TotalTokens != 15 || sum.Uptime != 2*time.Hour ||
		sum.EstimatedMicros["USD"] != 3_600_000 || sum.UptimeRecordsWithUnknownCost != 1 {
		t.Fatalf("summary = %+v", sum)
	}

	if err := deps.Delete(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := deps.Delete(ctx, "d1"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// Usage survives deletion of its deployment.
	if sum, _ := use.Summarize(ctx, usage.TimeRange{}); sum.Requests != 1 {
		t.Fatalf("usage lost on delete: %+v", sum)
	}
}
