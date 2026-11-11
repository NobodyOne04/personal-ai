package deploymentapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
	"personal-ai/core/ports"
)

// Errors returned by the deployment use cases.
var (
	// ErrLimitReached means starting another deployment would exceed MaxActive.
	ErrLimitReached = errors.New("active deployment limit reached")
	// ErrModelInactive means the model is deactivated and cannot be deployed.
	ErrModelInactive = errors.New("model is deactivated")
	// ErrNotDeletable means the deployment still runs or still holds compute.
	ErrNotDeletable = errors.New("deployment must be stopped before deletion")
	// ErrCleanupFailed means compute could not be released; the deployment is
	// kept Failed with its instance so cleanup is retried later.
	ErrCleanupFailed = errors.New("failed to release compute")
	// ErrNotServing means the deployment is not Ready or Unhealthy.
	ErrNotServing = errors.New("deployment is not serving")
	// ErrDeploymentFailed means a deployment that was being waited on failed or
	// was stopped.
	ErrDeploymentFailed = errors.New("deployment did not become ready")
)

// errSkip is returned by an update mutation whose precondition no longer
// holds; the caller treats it as "someone else changed the deployment".
var errSkip = errors.New("deployment changed concurrently")

// Config tunes the orchestration.
type Config struct {
	// MaxActive caps live deployments (see deployment.Status.IsLive).
	MaxActive int
	// ProvisionTimeout bounds waiting for an instance to run and expose an
	// endpoint.
	ProvisionTimeout time.Duration
	// ReadyTimeout bounds waiting for the runtime to load the model.
	ReadyTimeout time.Duration
	// PollInterval is the delay between status polls.
	PollInterval time.Duration
	// CleanupTimeout bounds compute release when the triggering context is
	// already cancelled.
	CleanupTimeout time.Duration
	// StorageGB is requested with every instance; 0 lets the provider decide.
	StorageGB int
}

func (c *Config) applyDefaults() {
	if c.MaxActive < 1 {
		c.MaxActive = 1
	}
	if c.ProvisionTimeout <= 0 {
		c.ProvisionTimeout = 15 * time.Minute
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = 30 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 10 * time.Second
	}
	if c.CleanupTimeout <= 0 {
		c.CleanupTimeout = 2 * time.Minute
	}
}

// Deps are the ports the service needs.
type Deps struct {
	Deployments ports.DeploymentRepository
	Models      ports.ModelRepository
	Compute     ports.ComputeProvider
	Runtime     ports.InferenceRuntime
	Usage       ports.UsageRepository
	Events      ports.EventPublisher
	Clock       ports.Clock
	IDs         ports.IDGenerator
	Activity    *deployment.Activity
	Logger      *slog.Logger
}

// Service implements the deployment lifecycle use cases.
type Service struct {
	Deps
	cfg Config

	admitMu sync.Mutex // serialises the MaxActive check with the insert

	launchMu sync.Mutex
	launches map[deployment.ID]context.CancelFunc

	bg       context.Context
	bgCancel context.CancelFunc
	wg       sync.WaitGroup
}

// NewService wires the use cases. Call Close on shutdown.
func NewService(d Deps, cfg Config) *Service {
	cfg.applyDefaults()
	if d.Activity == nil {
		d.Activity = deployment.NewActivity()
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	bg, cancel := context.WithCancel(context.Background())
	return &Service{Deps: d, cfg: cfg, launches: map[deployment.ID]context.CancelFunc{}, bg: bg, bgCancel: cancel}
}

// Close cancels background launches and waits for them to return. Interrupted
// launches are completed or cleaned up by Reconcile on the next start.
func (s *Service) Close() {
	s.bgCancel()
	s.wg.Wait()
}

// CreateRequest describes a new deployment.
type CreateRequest struct {
	ModelID model.ID
	Pinned  bool
	// RuntimeConfig overrides the model's runtime settings.
	RuntimeConfig map[string]any
}

// Create validates the request, records a Pending deployment and starts
// launching it in the background. It returns as soon as the record exists.
func (s *Service) Create(ctx context.Context, req CreateRequest) (deployment.Deployment, error) {
	m, err := s.Models.Get(ctx, req.ModelID)
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("get model %q: %w", req.ModelID, err)
	}
	if !m.IsActive() {
		return deployment.Deployment{}, fmt.Errorf("%w: %q", ErrModelInactive, m.ID)
	}
	id := deployment.ID(s.IDs.NewID())
	key := newRuntimeKey()
	// Validate everything that can be validated before spending money.
	if _, err := s.Runtime.Prepare(ctx, ports.PrepareSpec{DeploymentID: string(id), RuntimeKey: key, Model: m, Overrides: req.RuntimeConfig}); err != nil {
		return deployment.Deployment{}, fmt.Errorf("%w: %w", deployment.ErrInvalid, err)
	}
	caps, err := s.Compute.Capabilities(ctx)
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("list compute capabilities: %w", err)
	}
	if _, err := m.SelectCapability(caps); err != nil {
		return deployment.Deployment{}, err
	}

	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	if err := s.admit(ctx); err != nil {
		return deployment.Deployment{}, err
	}
	d, err := deployment.New(id, m.ID, s.IDs.NewID(), s.Clock.Now())
	if err != nil {
		return deployment.Deployment{}, err
	}
	d.Pinned = req.Pinned
	d.RuntimeConfig = req.RuntimeConfig
	d.RuntimeKey = key
	d, err = s.Deployments.Save(ctx, d)
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("save deployment: %w", err)
	}
	s.publish(ctx, ports.EventDeploymentRequested, d, nil)
	s.startLaunch(d.ID)
	return d, nil
}

func (s *Service) admit(ctx context.Context) error {
	live, err := s.Deployments.ListByStatus(ctx, liveStatuses()...)
	if err != nil {
		return fmt.Errorf("count live deployments: %w", err)
	}
	if len(live) >= s.cfg.MaxActive {
		return fmt.Errorf("%w: %d of %d in use", ErrLimitReached, len(live), s.cfg.MaxActive)
	}
	return nil
}

func liveStatuses() []deployment.Status {
	var out []deployment.Status
	for _, st := range deployment.AllStatuses {
		if st.IsLive() {
			out = append(out, st)
		}
	}
	return out
}

// ---- background launch ----

func (s *Service) startLaunch(id deployment.ID) {
	ctx, cancel := context.WithCancel(s.bg)
	s.launchMu.Lock()
	if prev, ok := s.launches[id]; ok {
		prev()
	}
	s.launches[id] = cancel
	s.launchMu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.launchMu.Lock()
			// Only remove our own entry; a restart may have replaced it.
			if s.launches[id] != nil && ctx.Err() == nil {
				delete(s.launches, id)
			}
			s.launchMu.Unlock()
			cancel()
		}()
		if err := s.Launch(ctx, id); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errSkip) {
			s.Logger.Warn("deployment launch failed", "deployment_id", id, "error", err)
		}
	}()
}

func (s *Service) cancelLaunch(id deployment.ID) {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	if cancel, ok := s.launches[id]; ok {
		cancel()
		delete(s.launches, id)
	}
}

// Launch drives a Pending deployment to Ready:
//
//  1. Provisioning: acquire compute with the deployment's idempotency key and
//     persist the instance ID immediately;
//  2. wait until the instance runs and exposes an endpoint;
//  3. Initializing: bind the runtime;
//  4. Starting: poll runtime health until the model is loaded;
//  5. Ready.
//
// On failure the deployment is marked Failed and any instance is released; if
// release fails the instance stays attached so the scheduler retries. If the
// deployment is stopped concurrently, Launch backs off and releases any
// instance it acquired that the stopper could not have known about.
// It is exported for tests and synchronous callers; Create runs it in the
// background.
func (s *Service) Launch(ctx context.Context, id deployment.ID) error {
	d, err := s.Deployments.Get(ctx, id)
	if err != nil {
		return err
	}
	if d.Status != deployment.Pending {
		return errSkip
	}
	m, err := s.Models.Get(ctx, d.ModelID)
	if err != nil {
		return s.fail(ctx, id, fmt.Errorf("get model: %w", err))
	}
	if d.RuntimeKey == "" {
		// Recorded before ADR 0009 keys existed: give it one before launching.
		if d, err = s.update(ctx, id, func(d *deployment.Deployment) error {
			if d.RuntimeKey == "" {
				d.RuntimeKey = newRuntimeKey()
			}
			return nil
		}); err != nil {
			return err
		}
	}
	launch, err := s.Runtime.Prepare(ctx, ports.PrepareSpec{DeploymentID: string(id), RuntimeKey: d.RuntimeKey, Model: m, Overrides: d.RuntimeConfig})
	if err != nil {
		return s.fail(ctx, id, fmt.Errorf("prepare runtime: %w", err))
	}
	caps, err := s.Compute.Capabilities(ctx)
	if err != nil {
		return s.fail(ctx, id, fmt.Errorf("list capabilities: %w", err))
	}
	capability, err := m.SelectCapability(caps)
	if err != nil {
		return s.fail(ctx, id, err)
	}

	// 1. Provisioning.
	d, err = s.update(ctx, id, func(d *deployment.Deployment) error {
		if d.Status != deployment.Pending {
			return errSkip
		}
		return d.Transition(deployment.Provisioning, s.Clock.Now())
	})
	if err != nil {
		return err
	}
	inst, err := s.Compute.Provision(ctx, compute.ProvisionSpec{
		CapabilityID:   capability.ID,
		StorageGB:      s.cfg.StorageGB,
		Launch:         launch,
		Labels:         map[string]string{"deployment_id": string(id), "model_id": string(m.ID)},
		IdempotencyKey: d.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			// Stopped or shutting down mid-call: the provider may have created
			// an instance we never learnt about.
			s.releaseUnattached(id)
			return ctx.Err()
		}
		return s.fail(ctx, id, fmt.Errorf("provision compute: %w", err))
	}
	if _, err := s.update(ctx, id, func(d *deployment.Deployment) error {
		if d.Status != deployment.Provisioning {
			return errSkip
		}
		return d.AttachInstance(inst.ID, s.Clock.Now())
	}); err != nil {
		// The deployment was stopped before we could record the instance, so
		// nobody else will release it.
		s.terminate(ctx, inst.ID)
		return err
	}
	s.Logger.Info("compute provisioned", "deployment_id", id, "instance_id", inst.ID)

	// 2. Wait for the instance to run.
	inst, err = s.waitForInstance(ctx, inst)
	if err != nil {
		return s.failUnlessCancelled(ctx, id, err)
	}

	// 3. Initializing.
	if _, err := s.update(ctx, id, func(d *deployment.Deployment) error {
		if d.Status != deployment.Provisioning {
			return errSkip
		}
		return d.Transition(deployment.Initializing, s.Clock.Now())
	}); err != nil {
		return err
	}
	ref, err := s.Runtime.Initialize(ctx, ports.InitSpec{DeploymentID: string(id), Model: m, Instance: inst, Overrides: d.RuntimeConfig})
	if err != nil {
		return s.failUnlessCancelled(ctx, id, fmt.Errorf("initialize runtime: %w", err))
	}
	endpoint := *inst.Endpoint
	if _, err := s.update(ctx, id, func(d *deployment.Deployment) error {
		if d.Status != deployment.Initializing {
			return errSkip
		}
		d.Endpoint = &endpoint
		d.RuntimeRef = ref
		return d.Transition(deployment.Starting, s.Clock.Now())
	}); err != nil {
		return err
	}

	// 4. Wait for the model to load.
	if err := s.waitForModel(ctx, ports.RuntimeTarget{Endpoint: endpoint, Ref: ref, Key: d.RuntimeKey}); err != nil {
		return s.failUnlessCancelled(ctx, id, err)
	}

	// 5. Ready.
	d, err = s.update(ctx, id, func(d *deployment.Deployment) error {
		if d.Status != deployment.Starting {
			return errSkip
		}
		now := s.Clock.Now()
		if err := d.Transition(deployment.Ready, now); err != nil {
			return err
		}
		d.RecordActivity(now) // the idle timer starts when serving starts
		return nil
	})
	if err != nil {
		return err
	}
	s.Logger.Info("deployment ready", "deployment_id", id, "model_id", d.ModelID)
	s.publish(ctx, ports.EventDeploymentReady, d, nil)
	return nil
}

func (s *Service) waitForInstance(ctx context.Context, inst compute.Instance) (compute.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ProvisionTimeout)
	defer cancel()
	for {
		switch {
		case inst.Status == compute.InstanceRunning && inst.Endpoint != nil:
			return inst, nil
		case inst.Status.IsTerminal():
			return inst, fmt.Errorf("instance %s became %s", inst.ID, inst.Status)
		}
		if err := sleep(ctx, s.cfg.PollInterval); err != nil {
			return inst, timeoutOr(err, "instance did not start in time")
		}
		next, err := s.Compute.GetInstance(ctx, inst.ID)
		if err != nil {
			if ctx.Err() != nil {
				return inst, timeoutOr(ctx.Err(), "instance did not start in time")
			}
			if errors.Is(err, ports.ErrNotFound) {
				return inst, fmt.Errorf("instance %s disappeared", inst.ID)
			}
			s.Logger.Warn("instance status check failed", "instance_id", inst.ID, "error", err)
			continue
		}
		inst = next
	}
}

func (s *Service) waitForModel(ctx context.Context, target ports.RuntimeTarget) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ReadyTimeout)
	defer cancel()
	for {
		h, err := s.Runtime.Health(ctx, target)
		if err == nil && h.ModelLoaded {
			return nil
		}
		if err := sleep(ctx, s.cfg.PollInterval); err != nil {
			detail := h.Detail
			if detail == "" {
				detail = "no detail"
			}
			return timeoutOr(err, "model did not load in time ("+detail+")")
		}
	}
}

// timeoutOr turns a deadline into a descriptive error but keeps cancellation
// recognisable.
func timeoutOr(err error, msg string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New(msg)
	}
	return err
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Service) failUnlessCancelled(ctx context.Context, id deployment.ID, cause error) error {
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return s.fail(ctx, id, cause)
}

// fail marks the deployment Failed and tries to release its compute.
func (s *Service) fail(ctx context.Context, id deployment.ID, cause error) error {
	ctx = context.WithoutCancel(ctx)
	d, err := s.update(ctx, id, func(d *deployment.Deployment) error {
		switch d.Status {
		case deployment.Stopping, deployment.Stopped, deployment.Failed:
			return errSkip
		}
		return d.Fail(cause.Error(), s.Clock.Now())
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	s.Logger.Warn("deployment failed", "deployment_id", id, "stage", d.Failure.Stage, "reason", cause.Error())
	s.publish(ctx, ports.EventDeploymentFailed, d, map[string]string{"stage": string(d.Failure.Stage)})
	if d.InstanceID != nil {
		if _, err := s.releaseFailed(ctx, d); err != nil {
			return errors.Join(cause, err)
		}
	}
	return cause
}

// releaseFailed releases the compute of a Failed deployment and detaches it,
// keeping the deployment Failed so its failure stays visible.
func (s *Service) releaseFailed(ctx context.Context, d deployment.Deployment) (deployment.Deployment, error) {
	inst := s.instanceInfo(ctx, d)
	if err := s.releaseCompute(ctx, d); err != nil {
		return d, err
	}
	d, err := s.update(ctx, d.ID, func(cur *deployment.Deployment) error {
		if cur.Status != deployment.Failed {
			return errSkip
		}
		cur.DetachInstance(s.Clock.Now())
		return nil
	})
	if err == nil {
		s.recordUptime(ctx, d, inst)
	}
	return d, err
}

// ---- stop / restart / delete ----

// Stop stops a deployment and releases its compute. Stopping a stopped
// deployment is a no-op. A Failed deployment ends Stopped (its failure record
// is kept). If release fails the deployment becomes Failed with the instance
// still attached, and ErrCleanupFailed is returned.
func (s *Service) Stop(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	return s.stop(ctx, id, nil)
}

// StopIfIdle stops the deployment only if it is still idle at the moment of
// the transition; it exists so the scheduler cannot race an arriving request.
// It reports whether the deployment was stopped.
func (s *Service) StopIfIdle(ctx context.Context, id deployment.ID, idleTimeout time.Duration) (bool, error) {
	_, err := s.stop(ctx, id, func(d deployment.Deployment) bool {
		return d.IsIdle(s.Clock.Now(), idleTimeout, s.Activity.Active(id))
	})
	if errors.Is(err, errSkip) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) stop(ctx context.Context, id deployment.ID, guard func(deployment.Deployment) bool) (deployment.Deployment, error) {
	wasLaunching := false
	d, err := s.update(ctx, id, func(d *deployment.Deployment) error {
		if guard != nil && !guard(*d) {
			return errSkip
		}
		switch d.Status {
		case deployment.Stopped:
			return errNoop
		case deployment.Stopping:
			return nil // resume an interrupted stop
		case deployment.Pending, deployment.Provisioning, deployment.Initializing, deployment.Starting:
			wasLaunching = true
		}
		return d.Transition(deployment.Stopping, s.Clock.Now())
	})
	if errors.Is(err, errNoop) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	s.cancelLaunch(id)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.CleanupTimeout)
	defer cancel()

	inst := s.instanceInfo(ctx, d)
	if err := s.releaseCompute(ctx, d); err != nil {
		failed, ferr := s.update(ctx, id, func(cur *deployment.Deployment) error {
			if cur.Status != deployment.Stopping {
				return errSkip
			}
			return cur.Fail(err.Error(), s.Clock.Now())
		})
		if ferr != nil {
			return d, errors.Join(err, ferr)
		}
		s.publish(ctx, ports.EventDeploymentFailed, failed, map[string]string{"stage": string(deployment.Stopping)})
		return failed, err
	}
	if wasLaunching {
		s.releaseUnattached(id)
	}
	d, err = s.update(ctx, id, func(cur *deployment.Deployment) error {
		if cur.Status != deployment.Stopping {
			return errSkip
		}
		now := s.Clock.Now()
		cur.DetachInstance(now)
		return cur.Transition(deployment.Stopped, now)
	})
	if err != nil {
		return d, err
	}
	s.recordUptime(ctx, d, inst)
	s.Logger.Info("deployment stopped", "deployment_id", id)
	s.publish(ctx, ports.EventDeploymentStopped, d, nil)
	return d, nil
}

var errNoop = errors.New("no change")

// releaseCompute releases the runtime (best effort) and terminates the
// instance (must succeed).
func (s *Service) releaseCompute(ctx context.Context, d deployment.Deployment) error {
	if d.Endpoint != nil && d.RuntimeRef != "" {
		if err := s.Runtime.Release(ctx, ports.RuntimeTarget{Endpoint: *d.Endpoint, Ref: d.RuntimeRef, Key: d.RuntimeKey}); err != nil {
			s.Logger.Warn("runtime release failed", "deployment_id", d.ID, "error", err)
		}
	}
	if d.InstanceID == nil {
		return nil
	}
	if err := s.Compute.Terminate(ctx, *d.InstanceID); err != nil {
		return fmt.Errorf("%w: instance %s: %w", ErrCleanupFailed, *d.InstanceID, err)
	}
	return nil
}

// instanceInfo fetches the instance for uptime accounting; nil if unknown.
func (s *Service) instanceInfo(ctx context.Context, d deployment.Deployment) *compute.Instance {
	if d.InstanceID == nil {
		return nil
	}
	inst, err := s.Compute.GetInstance(ctx, *d.InstanceID)
	if err != nil {
		return nil
	}
	return &inst
}

// releaseUnattached terminates instances labelled with the deployment that
// the deployment does not (or no longer) reference. They exist when a launch
// is interrupted between the provider creating an instance and the instance
// ID being persisted.
func (s *Service) releaseUnattached(id deployment.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.CleanupTimeout)
	defer cancel()
	insts, err := s.Compute.ListInstances(ctx)
	if err != nil {
		s.Logger.Warn("orphan check failed", "deployment_id", id, "error", err)
		return
	}
	cur, err := s.Deployments.Get(ctx, id)
	for _, inst := range insts {
		if inst.Labels["deployment_id"] != string(id) || inst.Status == compute.InstanceTerminated {
			continue
		}
		if err == nil && cur.InstanceID != nil && *cur.InstanceID == inst.ID {
			continue
		}
		s.terminate(ctx, inst.ID)
	}
}

func (s *Service) terminate(ctx context.Context, id compute.InstanceID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.CleanupTimeout)
	defer cancel()
	if err := s.Compute.Terminate(ctx, id); err != nil {
		s.Logger.Error("failed to terminate unattached instance; it will be retried at the next reconciliation",
			"instance_id", id, "error", err)
		return
	}
	s.Logger.Info("terminated unattached instance", "instance_id", id)
}

// recordUptime appends an uptime record for compute that was just released.
// Uptime runs from instance creation (or readiness if unknown) to now; cost
// is estimated from the instance's hourly rate and left unknown otherwise.
func (s *Service) recordUptime(ctx context.Context, d deployment.Deployment, inst *compute.Instance) {
	now := s.Clock.Now()
	var start time.Time
	var hourly *compute.Money
	if inst != nil {
		start = inst.CreatedAt
		hourly = inst.Capability.HourlyCost
	}
	if start.IsZero() && d.ReadyAt != nil {
		start = *d.ReadyAt
	}
	if start.IsZero() || !now.After(start) {
		return
	}
	rec := usage.UptimeRecord(s.IDs.NewID(), d.ID, d.ModelID, hourly, now.Sub(start), now)
	if err := s.Usage.Append(ctx, rec); err != nil {
		s.Logger.Warn("failed to record uptime", "deployment_id", d.ID, "error", err)
		return
	}
	s.publish(ctx, ports.EventUsageRecorded, d, map[string]string{"kind": string(usage.KindUptime)})
}

// Restart stops the deployment if needed and launches it again on new
// compute.
func (s *Service) Restart(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	d, err := s.Deployments.Get(ctx, id)
	if err != nil {
		return d, err
	}
	if d.Status != deployment.Stopped && !(d.Status == deployment.Failed && d.InstanceID == nil) {
		if d, err = s.Stop(ctx, id); err != nil {
			return d, err
		}
	}
	m, err := s.Models.Get(ctx, d.ModelID)
	if err != nil {
		return d, fmt.Errorf("get model %q: %w", d.ModelID, err)
	}
	if !m.IsActive() {
		return d, fmt.Errorf("%w: %q", ErrModelInactive, m.ID)
	}

	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	if err := s.admit(ctx); err != nil {
		return d, err
	}
	d, err = s.update(ctx, id, func(d *deployment.Deployment) error {
		if err := d.Transition(deployment.Pending, s.Clock.Now()); err != nil {
			return err
		}
		// New compute needs a new key: the old one identifies released compute.
		d.IdempotencyKey = s.IDs.NewID()
		// And a new runtime credential (ADR 0009), so an old key opens nothing.
		d.RuntimeKey = newRuntimeKey()
		return nil
	})
	if err != nil {
		return d, err
	}
	s.publish(ctx, ports.EventDeploymentRequested, d, map[string]string{"restart": "true"})
	s.startLaunch(id)
	return d, nil
}

// Delete removes a deployment that no longer holds compute.
func (s *Service) Delete(ctx context.Context, id deployment.ID) error {
	d, err := s.Deployments.Get(ctx, id)
	if err != nil {
		return err
	}
	if d.InstanceID != nil || (d.Status != deployment.Stopped && d.Status != deployment.Failed) {
		return fmt.Errorf("%w: status %s", ErrNotDeletable, d.Status)
	}
	return s.Deployments.Delete(ctx, id)
}

// ---- queries and small commands ----

// Get returns one deployment.
func (s *Service) Get(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	return s.Deployments.Get(ctx, id)
}

// List returns all deployments.
func (s *Service) List(ctx context.Context) ([]deployment.Deployment, error) {
	return s.Deployments.List(ctx)
}

// ActiveRequests returns the in-flight request count of a deployment.
func (s *Service) ActiveRequests(id deployment.ID) int { return s.Activity.Active(id) }

// Pin protects the deployment from automatic shutdown.
func (s *Service) Pin(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	return s.update(ctx, id, func(d *deployment.Deployment) error { d.Pin(s.Clock.Now()); return nil })
}

// Unpin removes the protection.
func (s *Service) Unpin(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	return s.update(ctx, id, func(d *deployment.Deployment) error { d.Unpin(s.Clock.Now()); return nil })
}

// CheckHealth probes the runtime and moves a serving deployment between Ready
// and Unhealthy accordingly.
func (s *Service) CheckHealth(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	d, err := s.Deployments.Get(ctx, id)
	if err != nil {
		return d, err
	}
	if (d.Status != deployment.Ready && d.Status != deployment.Unhealthy) || d.Endpoint == nil {
		return d, fmt.Errorf("%w: status %s", ErrNotServing, d.Status)
	}
	h, err := s.Runtime.Health(ctx, ports.RuntimeTarget{Endpoint: *d.Endpoint, Ref: d.RuntimeRef, Key: d.RuntimeKey})
	healthy := err == nil && h.ModelLoaded
	d, err = s.update(ctx, id, func(cur *deployment.Deployment) error {
		switch {
		case cur.Status == deployment.Ready && !healthy:
			return cur.Transition(deployment.Unhealthy, s.Clock.Now())
		case cur.Status == deployment.Unhealthy && healthy:
			return cur.Transition(deployment.Ready, s.Clock.Now())
		case cur.Status == deployment.Ready || cur.Status == deployment.Unhealthy:
			return errNoop
		}
		return errSkip
	})
	if errors.Is(err, errNoop) {
		return d, nil
	}
	if errors.Is(err, errSkip) {
		return d, fmt.Errorf("%w: status %s", ErrNotServing, d.Status)
	}
	return d, err
}

// FindReady returns a Ready deployment of the model, preferring the least
// recently used one, and false if there is none.
func (s *Service) FindReady(ctx context.Context, modelID model.ID) (deployment.Deployment, bool, error) {
	deps, err := s.Deployments.ListByModel(ctx, modelID)
	if err != nil {
		return deployment.Deployment{}, false, err
	}
	var ready []deployment.Deployment
	for _, d := range deps {
		if d.AcceptsRequests() && d.Endpoint != nil {
			ready = append(ready, d)
		}
	}
	if len(ready) == 0 {
		return deployment.Deployment{}, false, nil
	}
	sort.Slice(ready, func(i, j int) bool {
		if !ready[i].LastActivityAt.Equal(ready[j].LastActivityAt) {
			return ready[i].LastActivityAt.Before(ready[j].LastActivityAt)
		}
		return ready[i].ID < ready[j].ID
	})
	return ready[0], true, nil
}

// FindStarting returns a deployment of the model that is on its way to Ready.
func (s *Service) FindStarting(ctx context.Context, modelID model.ID) (deployment.Deployment, bool, error) {
	deps, err := s.Deployments.ListByModel(ctx, modelID)
	if err != nil {
		return deployment.Deployment{}, false, err
	}
	for _, d := range deps {
		switch d.Status {
		case deployment.Pending, deployment.Provisioning, deployment.Initializing, deployment.Starting:
			return d, true, nil
		}
	}
	return deployment.Deployment{}, false, nil
}

// EnsureStarted returns a Ready or starting deployment of the model, creating
// one if none exists.
func (s *Service) EnsureStarted(ctx context.Context, modelID model.ID) (deployment.Deployment, error) {
	if d, ok, err := s.FindReady(ctx, modelID); err != nil || ok {
		return d, err
	}
	if d, ok, err := s.FindStarting(ctx, modelID); err != nil || ok {
		return d, err
	}
	return s.Create(ctx, CreateRequest{ModelID: modelID})
}

// WaitReady blocks until the deployment is Ready, fails, or ctx ends.
func (s *Service) WaitReady(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	for {
		d, err := s.Deployments.Get(ctx, id)
		if err != nil {
			return d, err
		}
		switch d.Status {
		case deployment.Ready:
			return d, nil
		case deployment.Failed, deployment.Stopping, deployment.Stopped:
			reason := string(d.Status)
			if d.Failure != nil {
				reason = d.Failure.Reason
			}
			return d, fmt.Errorf("%w: %s", ErrDeploymentFailed, reason)
		}
		if err := sleep(ctx, min(s.cfg.PollInterval, 2*time.Second)); err != nil {
			return d, err
		}
	}
}

// RecordActivity marks the deployment as used now.
func (s *Service) RecordActivity(ctx context.Context, id deployment.ID) error {
	return s.Deployments.RecordActivity(ctx, id, s.Clock.Now())
}

// Target returns how to reach a serving deployment's runtime.
func Target(d deployment.Deployment) (ports.RuntimeTarget, error) {
	if d.Endpoint == nil {
		return ports.RuntimeTarget{}, deployment.ErrNoEndpoint
	}
	return ports.RuntimeTarget{Endpoint: *d.Endpoint, Ref: d.RuntimeRef, Key: d.RuntimeKey}, nil
}

// ---- reconciliation ----

// ReconcileReport summarises what Reconcile did.
type ReconcileReport struct {
	Relaunched        []deployment.ID
	Stopped           []deployment.ID
	HealthChecked     []deployment.ID
	OrphansTerminated []compute.InstanceID
	Errors            []error
}

// Reconcile brings stored state and provider state back in line after a
// restart. It must run before traffic is accepted and while no launch is in
// progress:
//   - Pending deployments are launched again (they never touched compute);
//   - deployments interrupted mid-launch are marked Failed and their compute
//     released (the operator can restart them);
//   - interrupted stops and failed cleanups are retried;
//   - serving deployments whose instance vanished are stopped, the rest are
//     health-checked;
//   - platform instances that no deployment references are terminated.
func (s *Service) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	deps, err := s.Deployments.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("list deployments: %w", err)
	}
	insts, err := s.Compute.ListInstances(ctx)
	if err != nil {
		return rep, fmt.Errorf("list instances: %w", err)
	}
	byID := map[compute.InstanceID]compute.Instance{}
	for _, inst := range insts {
		byID[inst.ID] = inst
	}

	for _, d := range deps {
		switch d.Status {
		case deployment.Pending:
			s.startLaunch(d.ID)
			rep.Relaunched = append(rep.Relaunched, d.ID)
		case deployment.Provisioning, deployment.Initializing, deployment.Starting:
			if _, err := s.update(ctx, d.ID, func(cur *deployment.Deployment) error {
				return cur.Fail("launch interrupted by a server restart", s.Clock.Now())
			}); err != nil {
				rep.Errors = append(rep.Errors, fmt.Errorf("deployment %s: %w", d.ID, err))
				continue
			}
			s.stopInto(ctx, d.ID, &rep)
		case deployment.Stopping:
			s.stopInto(ctx, d.ID, &rep)
		case deployment.Failed:
			if d.InstanceID != nil {
				s.stopInto(ctx, d.ID, &rep)
			}
		case deployment.Ready, deployment.Unhealthy:
			inst, ok := byID[derefInstance(d.InstanceID)]
			if d.InstanceID == nil || !ok || inst.Status.IsTerminal() {
				s.stopInto(ctx, d.ID, &rep)
				continue
			}
			if _, err := s.CheckHealth(ctx, d.ID); err != nil {
				rep.Errors = append(rep.Errors, fmt.Errorf("health check %s: %w", d.ID, err))
			}
			rep.HealthChecked = append(rep.HealthChecked, d.ID)
		}
	}

	// Re-read both sides: the steps above released compute and changed
	// attachments.
	deps, err = s.Deployments.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("list deployments: %w", err)
	}
	if insts, err = s.Compute.ListInstances(ctx); err != nil {
		return rep, fmt.Errorf("list instances: %w", err)
	}
	attached := map[compute.InstanceID]bool{}
	for _, d := range deps {
		if d.InstanceID != nil {
			attached[*d.InstanceID] = true
		}
	}
	for _, inst := range insts {
		if attached[inst.ID] || inst.Status == compute.InstanceTerminated {
			continue
		}
		if err := s.Compute.Terminate(ctx, inst.ID); err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("terminate orphan %s: %w", inst.ID, err))
			continue
		}
		s.Logger.Info("terminated orphaned instance", "instance_id", inst.ID, "deployment_id", inst.Labels["deployment_id"])
		rep.OrphansTerminated = append(rep.OrphansTerminated, inst.ID)
	}
	return rep, errors.Join(rep.Errors...)
}

func (s *Service) stopInto(ctx context.Context, id deployment.ID, rep *ReconcileReport) {
	if _, err := s.Stop(ctx, id); err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("stop %s: %w", id, err))
		return
	}
	rep.Stopped = append(rep.Stopped, id)
}

func derefInstance(id *compute.InstanceID) compute.InstanceID {
	if id == nil {
		return ""
	}
	return *id
}

// ---- helpers ----

// update reloads the deployment, applies mutate and saves it, retrying on
// optimistic-lock conflicts. mutate may return errSkip or errNoop to abort
// without saving; the current deployment is returned with that error.
func (s *Service) update(ctx context.Context, id deployment.ID, mutate func(*deployment.Deployment) error) (deployment.Deployment, error) {
	const attempts = 5
	for i := 0; i < attempts; i++ {
		d, err := s.Deployments.Get(ctx, id)
		if err != nil {
			return d, err
		}
		orig := d
		if err := mutate(&d); err != nil {
			return orig, err
		}
		saved, err := s.Deployments.Save(ctx, d)
		if errors.Is(err, ports.ErrVersionConflict) {
			continue
		}
		if err != nil {
			return orig, fmt.Errorf("save deployment %s: %w", id, err)
		}
		return saved, nil
	}
	return deployment.Deployment{}, fmt.Errorf("save deployment %s: %w", id, ports.ErrVersionConflict)
}

func (s *Service) publish(ctx context.Context, t ports.EventType, d deployment.Deployment, detail map[string]string) {
	if s.Events == nil {
		return
	}
	err := s.Events.Publish(context.WithoutCancel(ctx), ports.Event{
		Type: t, At: s.Clock.Now(), DeploymentID: d.ID, ModelID: d.ModelID, Detail: detail,
	})
	if err != nil {
		s.Logger.Debug("event publish failed", "type", t, "error", err)
	}
}

// newRuntimeKey returns a random 256-bit runtime credential (ADR 0009).
func newRuntimeKey() deployment.RuntimeKey {
	var b [32]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return deployment.RuntimeKey(hex.EncodeToString(b[:]))
}
