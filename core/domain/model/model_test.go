package model

import (
	"errors"
	"testing"

	"personal-ai/core/domain/compute"
)

func validModel() Model {
	return Model{
		ID:           "llama-3-8b",
		Name:         "Llama 3 8B",
		Artifact:     "org/llama-3-8b",
		Requirements: Requirements{MinVRAMGB: 24, RecommendedVRAMGB: 32, MinGPUCount: 1},
		Features:     []Feature{FeatureChat},
		Status:       StatusActive,
	}
}

func TestValidateAcceptsValidModel(t *testing.T) {
	if err := validModel().Validate(); err != nil {
		t.Fatalf("valid model rejected: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	bad := 3.0
	zero := 0.0
	tests := map[string]func(*Model){
		"empty id":            func(m *Model) { m.ID = "" },
		"id with slash":       func(m *Model) { m.ID = "org/model" },
		"id with space":       func(m *Model) { m.ID = "a b" },
		"no name":             func(m *Model) { m.Name = "" },
		"no artifact":         func(m *Model) { m.Artifact = "" },
		"no vram":             func(m *Model) { m.Requirements.MinVRAMGB = 0 },
		"recommended < min":   func(m *Model) { m.Requirements.RecommendedVRAMGB = 8 },
		"negative gpus":       func(m *Model) { m.Requirements.MinGPUCount = -1 },
		"no features":         func(m *Model) { m.Features = nil },
		"unknown feature":     func(m *Model) { m.Features = []Feature{"telepathy"} },
		"temperature too big": func(m *Model) { m.Generation.Temperature = &bad },
		"top_p zero":          func(m *Model) { m.Generation.TopP = &zero },
		"negative max tokens": func(m *Model) { m.Generation.MaxTokens = -1 },
		"unknown status":      func(m *Model) { m.Status = "weird" },
		"negative params":     func(m *Model) { m.ParamCount = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m := validModel()
			mutate(&m)
			if err := m.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	m := validModel()
	m.Name, m.Artifact = "", ""
	err := m.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"name is required", "artifact is required"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRequirementsSatisfiedBy(t *testing.T) {
	r := Requirements{MinVRAMGB: 48, MinGPUCount: 1}
	tests := []struct {
		name string
		cap  compute.Capability
		want bool
	}{
		{"exact total", compute.Capability{GPUCount: 2, VRAMPerGPUGB: 24}, true},
		{"more", compute.Capability{GPUCount: 1, VRAMPerGPUGB: 80}, true},
		{"too little", compute.Capability{GPUCount: 1, VRAMPerGPUGB: 24}, false},
		{"no gpu", compute.Capability{GPUCount: 0, VRAMPerGPUGB: 80}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.SatisfiedBy(tt.cap); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}

	multi := Requirements{MinVRAMGB: 24, MinGPUCount: 2}
	if multi.SatisfiedBy(compute.Capability{GPUCount: 1, VRAMPerGPUGB: 80}) {
		t.Fatal("a single GPU must not satisfy a two-GPU requirement")
	}
}

func money(micros int64) *compute.Money { return &compute.Money{Currency: "USD", Micros: micros} }

func TestSelectCapability(t *testing.T) {
	m := validModel() // needs 24 GB
	caps := []compute.Capability{
		{ID: "small", GPUCount: 1, VRAMPerGPUGB: 16, HourlyCost: money(200_000)},
		{ID: "big-pricey", GPUCount: 1, VRAMPerGPUGB: 80, HourlyCost: money(2_000_000)},
		{ID: "mid-cheap", GPUCount: 1, VRAMPerGPUGB: 24, HourlyCost: money(400_000)},
		{ID: "mid-unknown-cost", GPUCount: 1, VRAMPerGPUGB: 24},
	}
	got, err := m.SelectCapability(caps)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "mid-cheap" {
		t.Fatalf("selected %q, want mid-cheap (cheapest satisfying)", got.ID)
	}
}

func TestSelectCapabilityTieBreaks(t *testing.T) {
	m := validModel()
	// equal cost: smaller VRAM wins, then ID.
	caps := []compute.Capability{
		{ID: "b", GPUCount: 1, VRAMPerGPUGB: 48, HourlyCost: money(500_000)},
		{ID: "a", GPUCount: 1, VRAMPerGPUGB: 24, HourlyCost: money(500_000)},
		{ID: "c", GPUCount: 1, VRAMPerGPUGB: 24, HourlyCost: money(500_000)},
	}
	got, err := m.SelectCapability(caps)
	if err != nil || got.ID != "a" {
		t.Fatalf("got %v, %v; want a", got.ID, err)
	}
}

func TestSelectCapabilityUnknownCostRanksLast(t *testing.T) {
	m := validModel()
	caps := []compute.Capability{
		{ID: "unknown", GPUCount: 1, VRAMPerGPUGB: 24},
		{ID: "priced", GPUCount: 1, VRAMPerGPUGB: 80, HourlyCost: money(3_000_000)},
	}
	got, err := m.SelectCapability(caps)
	if err != nil || got.ID != "priced" {
		t.Fatalf("got %v, %v; want priced", got.ID, err)
	}
}

func TestSelectCapabilityNoneSuitable(t *testing.T) {
	m := validModel()
	_, err := m.SelectCapability([]compute.Capability{{ID: "tiny", GPUCount: 1, VRAMPerGPUGB: 8}})
	if !errors.Is(err, ErrNoSuitableCompute) {
		t.Fatalf("want ErrNoSuitableCompute, got %v", err)
	}
	if _, err := m.SelectCapability(nil); !errors.Is(err, ErrNoSuitableCompute) {
		t.Fatalf("empty list: want ErrNoSuitableCompute, got %v", err)
	}
}

func TestSupportsAndIsActive(t *testing.T) {
	m := validModel()
	if !m.Supports(FeatureChat) || m.Supports(FeatureTools) {
		t.Fatal("Supports mismatch")
	}
	if !m.IsActive() {
		t.Fatal("expected active")
	}
	m.Status = StatusDeactivated
	if m.IsActive() {
		t.Fatal("expected inactive")
	}
}
