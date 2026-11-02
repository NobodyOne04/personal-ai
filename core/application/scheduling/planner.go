// Package scheduling holds the provider-independent cost-control decisions:
// which deployments are idle, which failed ones need cleanup, and whether
// another deployment may start. It makes decisions only; a caller (a periodic
// worker in cmd/) executes them through application use cases, so nothing
// here touches a compute provider.
package scheduling

import (
	"errors"
	"sort"
	"time"

	"personal-ai/core/domain/deployment"
)

// Policy is the configured scheduling policy.
type Policy struct {
	// IdleTimeout is how long a deployment must be inactive before it is
	// stopped automatically.
	IdleTimeout time.Duration
	// MaxActive caps simultaneously live deployments.
	MaxActive int
}

// Validate checks the policy.
func (p Policy) Validate() error {
	var errs []error
	if p.IdleTimeout <= 0 {
		errs = append(errs, errors.New("idle timeout must be positive"))
	}
	if p.MaxActive < 1 {
		errs = append(errs, errors.New("max active deployments must be at least 1"))
	}
	return errors.Join(errs...)
}

// Admits reports whether another deployment may start given the number that
// are currently live.
func (p Policy) Admits(live int) bool { return live < p.MaxActive }

// CountLive counts deployments that occupy a slot under MaxActive.
func CountLive(deps []deployment.Deployment) int {
	n := 0
	for _, d := range deps {
		if d.Status.IsLive() {
			n++
		}
	}
	return n
}

// ActiveRequests reports the in-flight request count for a deployment.
// deployment.Activity.Active satisfies it.
type ActiveRequests func(deployment.ID) int

// Plan is the set of actions the scheduler decided on.
type Plan struct {
	// StopIdle lists deployments to stop, least recently active first.
	StopIdle []deployment.ID
	// CleanupFailed lists failed deployments that still hold compute and must
	// have it released.
	CleanupFailed []deployment.ID
}

// IsEmpty reports whether there is nothing to do.
func (p Plan) IsEmpty() bool { return len(p.StopIdle) == 0 && len(p.CleanupFailed) == 0 }

// PlanActions decides what to stop or clean up. Pinned deployments and
// deployments with in-flight requests are never selected for stopping.
func PlanActions(deps []deployment.Deployment, now time.Time, p Policy, active ActiveRequests) Plan {
	var idle, failed []deployment.Deployment
	for _, d := range deps {
		switch {
		case d.Status == deployment.Failed && d.InstanceID != nil:
			failed = append(failed, d)
		case d.IsIdle(now, p.IdleTimeout, active(d.ID)):
			idle = append(idle, d)
		}
	}

	sort.Slice(idle, func(i, j int) bool {
		if !idle[i].LastActivityAt.Equal(idle[j].LastActivityAt) {
			return idle[i].LastActivityAt.Before(idle[j].LastActivityAt)
		}
		return idle[i].ID < idle[j].ID
	})
	sort.Slice(failed, func(i, j int) bool { return failed[i].ID < failed[j].ID })

	var plan Plan
	for _, d := range idle {
		plan.StopIdle = append(plan.StopIdle, d.ID)
	}
	for _, d := range failed {
		plan.CleanupFailed = append(plan.CleanupFailed, d.ID)
	}
	return plan
}
