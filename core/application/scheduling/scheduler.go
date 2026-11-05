package scheduling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"personal-ai/core/domain/deployment"
	"personal-ai/core/ports"
)

// Executor carries out scheduling decisions. The deployment use cases
// satisfy it; the scheduler never talks to a provider itself.
type Executor interface {
	List(ctx context.Context) ([]deployment.Deployment, error)
	// StopIfIdle stops the deployment only if it is still idle when the stop
	// takes effect, so an arriving request is never cut off.
	StopIfIdle(ctx context.Context, id deployment.ID, idleTimeout time.Duration) (bool, error)
	Stop(ctx context.Context, id deployment.ID) (deployment.Deployment, error)
	CheckHealth(ctx context.Context, id deployment.ID) (deployment.Deployment, error)
	ActiveRequests(id deployment.ID) int
}

// Scheduler runs one cost-control pass per Tick. The periodic trigger lives in
// cmd/server.
type Scheduler struct {
	exec   Executor
	policy Policy
	clock  ports.Clock
	logger *slog.Logger
}

// NewScheduler validates the policy and returns a scheduler.
func NewScheduler(exec Executor, policy Policy, clock ports.Clock, logger *slog.Logger) (*Scheduler, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{exec: exec, policy: policy, clock: clock, logger: logger}, nil
}

// TickResult reports what a pass did.
type TickResult struct {
	HealthChecked int
	Stopped       []deployment.ID
	CleanedUp     []deployment.ID
}

// Tick health-checks serving deployments, stops idle ones and retries cleanup
// of failed ones that still hold compute. Errors on one deployment do not
// prevent work on the others.
func (s *Scheduler) Tick(ctx context.Context) (TickResult, error) {
	var res TickResult
	var errs []error

	deps, err := s.exec.List(ctx)
	if err != nil {
		return res, fmt.Errorf("list deployments: %w", err)
	}
	for _, d := range deps {
		if d.Status == deployment.Ready || d.Status == deployment.Unhealthy {
			if _, err := s.exec.CheckHealth(ctx, d.ID); err != nil {
				errs = append(errs, fmt.Errorf("health %s: %w", d.ID, err))
			}
			res.HealthChecked++
		}
	}
	// Re-list: health checks changed statuses.
	if deps, err = s.exec.List(ctx); err != nil {
		return res, fmt.Errorf("list deployments: %w", err)
	}

	plan := PlanActions(deps, s.clock.Now(), s.policy, s.exec.ActiveRequests)
	for _, id := range plan.StopIdle {
		stopped, err := s.exec.StopIfIdle(ctx, id, s.policy.IdleTimeout)
		if err != nil {
			errs = append(errs, fmt.Errorf("stop idle %s: %w", id, err))
			continue
		}
		if stopped {
			s.logger.Info("stopped idle deployment", "deployment_id", id)
			res.Stopped = append(res.Stopped, id)
		}
	}
	for _, id := range plan.CleanupFailed {
		if _, err := s.exec.Stop(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("cleanup %s: %w", id, err))
			continue
		}
		s.logger.Info("released compute of failed deployment", "deployment_id", id)
		res.CleanedUp = append(res.CleanedUp, id)
	}
	return res, errors.Join(errs...)
}
