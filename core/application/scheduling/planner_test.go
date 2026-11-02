package scheduling

import (
	"reflect"
	"testing"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func ready(id string, lastActive time.Time, pinned bool) deployment.Deployment {
	inst := compute.InstanceID("i-" + id)
	return deployment.Deployment{
		ID: deployment.ID(id), ModelID: "m", Status: deployment.Ready,
		InstanceID: &inst, Pinned: pinned, LastActivityAt: lastActive,
	}
}

func none(deployment.ID) int { return 0 }

func TestPolicyValidate(t *testing.T) {
	if err := (Policy{IdleTimeout: time.Minute, MaxActive: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Policy{
		"zero timeout": {IdleTimeout: 0, MaxActive: 1},
		"negative":     {IdleTimeout: -time.Second, MaxActive: 1},
		"no slots":     {IdleTimeout: time.Minute, MaxActive: 0},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAdmits(t *testing.T) {
	p := Policy{IdleTimeout: time.Minute, MaxActive: 2}
	if !p.Admits(0) || !p.Admits(1) || p.Admits(2) || p.Admits(3) {
		t.Fatal("Admits must allow exactly MaxActive live deployments")
	}
}

func TestCountLive(t *testing.T) {
	deps := []deployment.Deployment{
		{Status: deployment.Pending}, {Status: deployment.Ready}, {Status: deployment.Starting},
		{Status: deployment.Stopping}, {Status: deployment.Stopped}, {Status: deployment.Failed},
	}
	if got := CountLive(deps); got != 3 {
		t.Fatalf("CountLive = %d, want 3", got)
	}
}

func TestPlanStopsOnlyIdle(t *testing.T) {
	now := t0.Add(time.Hour)
	p := Policy{IdleTimeout: 30 * time.Minute, MaxActive: 3}
	deps := []deployment.Deployment{
		ready("idle-old", t0, false),
		ready("idle-newer", t0.Add(10*time.Minute), false),
		ready("recent", now.Add(-time.Minute), false),
		ready("pinned", t0, true),
		ready("busy", t0, false),
	}
	active := func(id deployment.ID) int {
		if id == "busy" {
			return 2
		}
		return 0
	}
	plan := PlanActions(deps, now, p, active)

	want := []deployment.ID{"idle-old", "idle-newer"}
	if !reflect.DeepEqual(plan.StopIdle, want) {
		t.Fatalf("StopIdle = %v, want %v (least recently active first; pinned/busy/recent excluded)", plan.StopIdle, want)
	}
	if len(plan.CleanupFailed) != 0 {
		t.Fatalf("unexpected cleanup: %v", plan.CleanupFailed)
	}
}

func TestPlanCleansFailedOnlyWhenHoldingCompute(t *testing.T) {
	inst := compute.InstanceID("i-1")
	deps := []deployment.Deployment{
		{ID: "b-holding", Status: deployment.Failed, InstanceID: &inst},
		{ID: "a-holding", Status: deployment.Failed, InstanceID: &inst},
		{ID: "released", Status: deployment.Failed},
	}
	plan := PlanActions(deps, t0, Policy{IdleTimeout: time.Minute, MaxActive: 1}, none)
	want := []deployment.ID{"a-holding", "b-holding"}
	if !reflect.DeepEqual(plan.CleanupFailed, want) {
		t.Fatalf("CleanupFailed = %v, want %v", plan.CleanupFailed, want)
	}
}

func TestPlanIsDeterministicOnTies(t *testing.T) {
	now := t0.Add(time.Hour)
	p := Policy{IdleTimeout: time.Minute, MaxActive: 5}
	deps := []deployment.Deployment{ready("z", t0, false), ready("a", t0, false), ready("m", t0, false)}
	plan := PlanActions(deps, now, p, none)
	want := []deployment.ID{"a", "m", "z"}
	if !reflect.DeepEqual(plan.StopIdle, want) {
		t.Fatalf("StopIdle = %v, want %v", plan.StopIdle, want)
	}
}

func TestPlanEmpty(t *testing.T) {
	plan := PlanActions(nil, t0, Policy{IdleTimeout: time.Minute, MaxActive: 1}, none)
	if !plan.IsEmpty() {
		t.Fatalf("expected empty plan, got %+v", plan)
	}
}

func TestPlanWorksWithActivityTracker(t *testing.T) {
	a := deployment.NewActivity()
	end := a.Begin("busy")
	defer end()

	deps := []deployment.Deployment{ready("busy", t0, false), ready("quiet", t0, false)}
	plan := PlanActions(deps, t0.Add(time.Hour), Policy{IdleTimeout: time.Minute, MaxActive: 2}, a.Active)
	if !reflect.DeepEqual(plan.StopIdle, []deployment.ID{"quiet"}) {
		t.Fatalf("StopIdle = %v, want only quiet", plan.StopIdle)
	}
}
