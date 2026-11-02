// Package ports declares the capabilities the application layer needs from the
// outside world. Every interface is expressed in domain types and from the
// Core's point of view; none mirrors a vendor API.
package ports

import (
	"context"
	"errors"

	"personal-ai/core/domain/compute"
)

// Errors that ports may return. Adapters wrap provider-specific errors in
// these so the application can react without knowing the provider.
var (
	// ErrNotFound means the requested entity or instance does not exist.
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict means an optimistic-lock check failed on save.
	ErrVersionConflict = errors.New("version conflict")
	// ErrCapacityUnavailable means the provider cannot currently supply the
	// requested compute. It may be retried later or with another capability.
	ErrCapacityUnavailable = errors.New("compute capacity unavailable")
)

// ComputeProvider acquires, inspects and releases compute resources.
type ComputeProvider interface {
	// Capabilities lists what can currently be provisioned.
	Capabilities(ctx context.Context) ([]compute.Capability, error)

	// Provision acquires compute. It is idempotent on spec.IdempotencyKey:
	// repeating a call returns the existing instance and never a second one.
	Provision(ctx context.Context, spec compute.ProvisionSpec) (compute.Instance, error)

	// GetInstance returns the current state of an instance, or ErrNotFound.
	GetInstance(ctx context.Context, id compute.InstanceID) (compute.Instance, error)

	// ListInstances returns every instance the provider holds for this
	// platform. It exists so startup reconciliation can find orphans.
	ListInstances(ctx context.Context) ([]compute.Instance, error)

	// Terminate releases an instance. It is idempotent: terminating an
	// already-terminated or unknown instance succeeds.
	Terminate(ctx context.Context, id compute.InstanceID) error
}
