// Package compute defines provider-independent compute concepts: what a
// provider can offer (Capability), what we ask it for (ProvisionSpec) and what
// we get back (Instance).
//
// Nothing here may mention a specific vendor. Provider identifiers are carried
// only as opaque strings that the owning adapter alone interprets.
package compute

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// InstanceID is the platform-owned identifier of a compute instance.
type InstanceID string

// CapabilityID identifies one kind of compute offering (for example a GPU
// configuration) as reported by a provider adapter. It is opaque to the Core.
type CapabilityID string

// Money is an amount in micro-units of a currency (1_000_000 micros = 1 unit).
// Integer arithmetic avoids floating point drift in cost accounting.
type Money struct {
	Currency string
	Micros   int64
}

// ForDuration returns m, interpreted as an hourly rate, scaled to d.
// Non-positive durations yield zero. Resolution is one second.
func (m Money) ForDuration(d time.Duration) Money {
	if d <= 0 {
		return Money{Currency: m.Currency}
	}
	return Money{Currency: m.Currency, Micros: m.Micros * int64(d/time.Second) / 3600}
}

// Capability describes a compute offering that can be provisioned.
type Capability struct {
	ID           CapabilityID
	GPUModel     string
	GPUCount     int
	VRAMPerGPUGB int
	CPUCores     int
	MemoryGB     int
	StorageGB    int
	// HourlyCost is nil when the provider does not report a price. Unknown
	// cost is never treated as zero.
	HourlyCost *Money
	Region     string
}

// TotalVRAMGB is the VRAM summed across all GPUs.
func (c Capability) TotalVRAMGB() int { return c.GPUCount * c.VRAMPerGPUGB }

// Endpoint is an abstract reference to a network location that serves a
// workload. Headers may carry credentials and are deliberately excluded from
// String so an Endpoint can be logged safely.
type Endpoint struct {
	URL     string
	Headers map[string]string
}

func (e Endpoint) String() string { return e.URL }

// LaunchSpec describes the workload an instance must run on start. The
// inference runtime adapter produces it; the compute adapter executes it.
// Env may contain secrets and must never be logged.
type LaunchSpec struct {
	Image string
	Args  []string
	Env   map[string]string
	// Ports lists container ports that must be reachable via the instance
	// endpoint.
	Ports []int
}

// ProvisionSpec is the request to acquire compute.
type ProvisionSpec struct {
	CapabilityID CapabilityID
	StorageGB    int
	Launch       LaunchSpec
	Labels       map[string]string
	// IdempotencyKey makes Provision safe to retry: providers must return the
	// existing instance for a repeated key rather than create a second one.
	IdempotencyKey string
}

// ErrInvalidSpec is returned for malformed provision specs.
var ErrInvalidSpec = errors.New("invalid provision spec")

// Validate checks the structural validity of the spec.
func (s ProvisionSpec) Validate() error {
	var problems []string
	if s.CapabilityID == "" {
		problems = append(problems, "capability id is required")
	}
	if s.IdempotencyKey == "" {
		problems = append(problems, "idempotency key is required")
	}
	if s.StorageGB < 0 {
		problems = append(problems, "storage must not be negative")
	}
	if s.Launch.Image == "" {
		problems = append(problems, "launch image is required")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidSpec, strings.Join(problems, "; "))
	}
	return nil
}

// InstanceStatus is the platform's view of an instance's lifecycle.
// Provider adapters map their native states onto these.
type InstanceStatus string

const (
	InstanceRequested    InstanceStatus = "requested"
	InstanceProvisioning InstanceStatus = "provisioning"
	InstanceRunning      InstanceStatus = "running"
	InstanceTerminating  InstanceStatus = "terminating"
	InstanceTerminated   InstanceStatus = "terminated"
	InstanceFailed       InstanceStatus = "failed"
	InstanceUnknown      InstanceStatus = "unknown"
)

// IsTerminal reports whether the instance no longer consumes resources.
func (s InstanceStatus) IsTerminal() bool {
	return s == InstanceTerminated || s == InstanceFailed
}

// Instance is a provisioned (or requested) unit of compute.
type Instance struct {
	ID InstanceID
	// ProviderRef is an opaque, adapter-owned reference. The Core stores and
	// returns it but never parses or branches on it.
	ProviderRef string
	Capability  Capability
	Status      InstanceStatus
	Endpoint    *Endpoint
	// Labels echo ProvisionSpec.Labels so orphaned instances can be matched
	// back to deployments during reconciliation.
	Labels    map[string]string
	CreatedAt time.Time
}
