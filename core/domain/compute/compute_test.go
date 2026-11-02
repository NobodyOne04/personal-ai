package compute

import (
	"errors"
	"testing"
	"time"
)

func TestMoneyForDuration(t *testing.T) {
	hourly := Money{Currency: "USD", Micros: 1_800_000} // 1.80/h
	tests := []struct {
		name string
		d    time.Duration
		want int64
	}{
		{"zero", 0, 0},
		{"negative", -time.Hour, 0},
		{"one hour", time.Hour, 1_800_000},
		{"half hour", 30 * time.Minute, 900_000},
		{"one second", time.Second, 500},
		{"sub-second truncates", 999 * time.Millisecond, 0},
		{"long run does not overflow", 24 * 365 * time.Hour, 1_800_000 * 24 * 365},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hourly.ForDuration(tt.d)
			if got.Micros != tt.want || got.Currency != "USD" {
				t.Fatalf("ForDuration(%v) = %+v, want %d micros USD", tt.d, got, tt.want)
			}
		})
	}
}

func TestCapabilityTotalVRAM(t *testing.T) {
	c := Capability{GPUCount: 4, VRAMPerGPUGB: 24}
	if got := c.TotalVRAMGB(); got != 96 {
		t.Fatalf("TotalVRAMGB = %d, want 96", got)
	}
}

func TestEndpointStringOmitsHeaders(t *testing.T) {
	e := Endpoint{URL: "https://example.test", Headers: map[string]string{"Authorization": "Bearer secret"}}
	if got := e.String(); got != "https://example.test" {
		t.Fatalf("String = %q", got)
	}
}

func TestProvisionSpecValidate(t *testing.T) {
	valid := ProvisionSpec{CapabilityID: "c1", IdempotencyKey: "k", Launch: LaunchSpec{Image: "img"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ProvisionSpec){
		"no capability": func(s *ProvisionSpec) { s.CapabilityID = "" },
		"no key":        func(s *ProvisionSpec) { s.IdempotencyKey = "" },
		"no image":      func(s *ProvisionSpec) { s.Launch.Image = "" },
		"negative disk": func(s *ProvisionSpec) { s.StorageGB = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			mutate(&s)
			if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("want ErrInvalidSpec, got %v", err)
			}
		})
	}
}

func TestInstanceStatusIsTerminal(t *testing.T) {
	terminal := map[InstanceStatus]bool{InstanceTerminated: true, InstanceFailed: true}
	for _, s := range []InstanceStatus{InstanceRequested, InstanceProvisioning, InstanceRunning,
		InstanceTerminating, InstanceTerminated, InstanceFailed, InstanceUnknown} {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, terminal[s])
		}
	}
}
