// Package deployment defines the Deployment aggregate and its lifecycle.
//
// Status changes happen only through Deployment.Transition (or helpers built
// on it), which enforces the transition table and the invariants that keep
// compute from being orphaned.
package deployment

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/model"
)

// ID identifies a deployment.
type ID string

// RuntimeKey is the random credential protecting one deployment's runtime
// (ADR 0009). Every formatting path is redacted so it cannot leak into logs.
type RuntimeKey string

const redactedKey = "[REDACTED]"

func (RuntimeKey) String() string       { return redactedKey }
func (RuntimeKey) GoString() string     { return redactedKey }
func (RuntimeKey) LogValue() slog.Value { return slog.StringValue(redactedKey) }

// Status is the lifecycle state of a deployment.
type Status string

const (
	Pending      Status = "pending"
	Provisioning Status = "provisioning"
	Initializing Status = "initializing"
	Starting     Status = "starting"
	Ready        Status = "ready"
	Unhealthy    Status = "unhealthy"
	Stopping     Status = "stopping"
	Stopped      Status = "stopped"
	Failed       Status = "failed"
)

// AllStatuses lists every status, in lifecycle order.
var AllStatuses = []Status{Pending, Provisioning, Initializing, Starting, Ready, Unhealthy, Stopping, Stopped, Failed}

var transitions = map[Status][]Status{
	Pending:      {Provisioning, Stopping, Failed},
	Provisioning: {Initializing, Stopping, Failed},
	Initializing: {Starting, Stopping, Failed},
	Starting:     {Ready, Stopping, Failed},
	Ready:        {Unhealthy, Stopping},
	Unhealthy:    {Ready, Stopping, Failed},
	Stopping:     {Stopped, Failed},
	Stopped:      {Pending},
	Failed:       {Stopping, Pending},
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransitionTo reports whether the transition table allows s -> to.
func (s Status) CanTransitionTo(to Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsLive reports whether the deployment counts toward the active-deployment
// limit: it is running, or on its way to running.
func (s Status) IsLive() bool {
	switch s {
	case Pending, Provisioning, Initializing, Starting, Ready, Unhealthy:
		return true
	}
	return false
}

// Health is the last observed runtime health.
type Health string

const (
	HealthUnknown   Health = "unknown"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

// Failure records why a deployment failed and at which stage.
type Failure struct {
	Stage  Status
	Reason string
	At     time.Time
}

// Errors returned by lifecycle operations.
var (
	ErrInvalid           = errors.New("invalid deployment")
	ErrInvalidTransition = errors.New("invalid deployment transition")
	ErrInstanceAttached  = errors.New("compute instance still attached")
	ErrNoInstance        = errors.New("no compute instance attached")
	ErrNoEndpoint        = errors.New("no endpoint available")
)

// Deployment is a requested or running instance of a model on compute.
type Deployment struct {
	ID      ID
	ModelID model.ID

	// InstanceID is nil until compute has been provisioned and again after it
	// has been released.
	InstanceID *compute.InstanceID
	Status     Status

	// RuntimeConfig overrides the model's runtime settings; opaque to the Core.
	RuntimeConfig map[string]any
	Endpoint      *compute.Endpoint
	// RuntimeRef is an opaque handle returned by the inference runtime.
	RuntimeRef string
	// RuntimeKey authenticates calls to the runtime. Generated per launch;
	// persistence stores it encrypted.
	RuntimeKey RuntimeKey

	Pinned  bool
	Health  Health
	Failure *Failure

	// IdempotencyKey is generated before any external call so that retries
	// cannot create duplicate compute.
	IdempotencyKey string
	// Version supports optimistic locking in persistence; the domain only
	// carries it.
	Version int64

	CreatedAt      time.Time
	ReadyAt        *time.Time
	LastActivityAt time.Time
	UpdatedAt      time.Time
}

// New creates a Pending deployment.
func New(id ID, modelID model.ID, idempotencyKey string, now time.Time) (Deployment, error) {
	switch {
	case id == "":
		return Deployment{}, fmt.Errorf("%w: id is required", ErrInvalid)
	case modelID == "":
		return Deployment{}, fmt.Errorf("%w: model id is required", ErrInvalid)
	case idempotencyKey == "":
		return Deployment{}, fmt.Errorf("%w: idempotency key is required", ErrInvalid)
	}
	return Deployment{
		ID:             id,
		ModelID:        modelID,
		Status:         Pending,
		Health:         HealthUnknown,
		IdempotencyKey: idempotencyKey,
		CreatedAt:      now,
		LastActivityAt: now,
		UpdatedAt:      now,
	}, nil
}

// Transition moves the deployment to the given status if the transition table
// and invariants allow it:
//
//   - entering Initializing requires an attached instance;
//   - entering Ready requires an endpoint;
//   - entering Pending or Stopped requires that no instance is attached, so a
//     deployment can never be recorded as stopped while still holding compute.
func (d *Deployment) Transition(to Status, now time.Time) error {
	if !d.Status.CanTransitionTo(to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, d.Status, to)
	}
	switch to {
	case Initializing:
		if d.InstanceID == nil {
			return fmt.Errorf("%w: cannot enter %s", ErrNoInstance, to)
		}
	case Ready:
		if d.Endpoint == nil {
			return fmt.Errorf("%w: cannot enter %s", ErrNoEndpoint, to)
		}
	case Pending, Stopped:
		if d.InstanceID != nil {
			return fmt.Errorf("%w: cannot enter %s while holding %s", ErrInstanceAttached, to, *d.InstanceID)
		}
	}

	d.Status = to
	d.UpdatedAt = now
	switch to {
	case Ready:
		t := now
		d.ReadyAt = &t
		d.Health = HealthHealthy
		d.Failure = nil
	case Unhealthy:
		d.Health = HealthUnhealthy
	case Pending:
		d.ReadyAt = nil
		d.Failure = nil
		d.Endpoint = nil
		d.RuntimeRef = ""
		d.Health = HealthUnknown
	case Stopped:
		d.Health = HealthUnknown
	}
	return nil
}

// Fail moves the deployment to Failed and records the stage it failed in.
func (d *Deployment) Fail(reason string, now time.Time) error {
	stage := d.Status
	if err := d.Transition(Failed, now); err != nil {
		return err
	}
	d.Failure = &Failure{Stage: stage, Reason: reason, At: now}
	d.Health = HealthUnknown
	return nil
}

// AttachInstance records the provisioned compute.
func (d *Deployment) AttachInstance(id compute.InstanceID, now time.Time) error {
	if id == "" {
		return fmt.Errorf("%w: instance id is required", ErrInvalid)
	}
	if d.InstanceID != nil && *d.InstanceID != id {
		return fmt.Errorf("%w: already holding %s", ErrInstanceAttached, *d.InstanceID)
	}
	d.InstanceID = &id
	d.UpdatedAt = now
	return nil
}

// DetachInstance records that the compute has been released. It is the only
// way to satisfy the invariant required to enter Stopped or Pending.
func (d *Deployment) DetachInstance(now time.Time) {
	d.InstanceID = nil
	d.Endpoint = nil
	d.RuntimeRef = ""
	d.UpdatedAt = now
}

// RecordActivity updates the last-activity timestamp.
func (d *Deployment) RecordActivity(now time.Time) {
	if now.After(d.LastActivityAt) {
		d.LastActivityAt = now
	}
}

// Pin protects the deployment from automatic shutdown.
func (d *Deployment) Pin(now time.Time) { d.Pinned = true; d.UpdatedAt = now }

// Unpin removes the protection.
func (d *Deployment) Unpin(now time.Time) { d.Pinned = false; d.UpdatedAt = now }

// AcceptsRequests reports whether inference may be routed to the deployment.
func (d Deployment) AcceptsRequests() bool { return d.Status == Ready }

// IsIdle reports whether the deployment is eligible for automatic shutdown:
// serving (Ready or Unhealthy), not pinned, with no in-flight requests, and
// quiet for at least idleTimeout. activeRequests is supplied by the caller
// because in-flight counts are process state, not persisted state.
func (d Deployment) IsIdle(now time.Time, idleTimeout time.Duration, activeRequests int) bool {
	if d.Status != Ready && d.Status != Unhealthy {
		return false
	}
	if d.Pinned || activeRequests > 0 {
		return false
	}
	return now.Sub(d.LastActivityAt) >= idleTimeout
}
