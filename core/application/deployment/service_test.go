package deploymentapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
	"personal-ai/core/ports"
	"personal-ai/internal/testing/fakes"
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type env struct {
	svc     *Service
	deps    *fakes.Deployments
	models  *fakes.Models
	compute *fakes.Compute
	runtime *fakes.Runtime
	usage   *fakes.Usage
	events  *fakes.Events
}

func testModel() model.Model {
	return model.Model{ID: "m1", Name: "M1", Artifact: "org/m1", Requirements: model.Requirements{MinVRAMGB: 24, MinGPUCount: 1},
		Features: []model.Feature{model.FeatureChat}, Status: model.StatusActive}
}

func newEnv(t *testing.T, maxActive int) *env {
	t.Helper()
	e := &env{deps: fakes.NewDeployments(), models: fakes.NewModels(testModel()), compute: fakes.NewCompute(),
		runtime: fakes.NewRuntime(), usage: &fakes.Usage{}, events: &fakes.Events{}}
	e.svc = NewService(Deps{
		Deployments: e.deps, Models: e.models, Compute: e.compute, Runtime: e.runtime, Usage: e.usage,
		Events: e.events, Clock: realClock{}, IDs: &fakes.IDs{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, Config{MaxActive: maxActive, PollInterval: time.Millisecond, ProvisionTimeout: 200 * time.Millisecond,
		ReadyTimeout: 200 * time.Millisecond, CleanupTimeout: time.Second})
	t.Cleanup(e.svc.Close)
	return e
}

func (e *env) waitStatus(t *testing.T, id deployment.ID, want ...deployment.Status) deployment.Deployment {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		d, err := e.deps.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if d.Status == w {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment %s is %s, want one of %v", id, d.Status, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCreateLaunchesToReady(t *testing.T) {
	e := newEnv(t, 1)
	ctx := context.Background()
	d, err := e.svc.Create(ctx, CreateRequest{ModelID: "m1", Pinned: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != deployment.Pending {
		t.Fatalf("Create must return the Pending record, got %s", d.Status)
	}
	ready := e.waitStatus(t, d.ID, deployment.Ready)
	if ready.InstanceID == nil || ready.Endpoint == nil || ready.RuntimeRef == "" || !ready.Pinned {
		t.Fatalf("ready deployment incomplete: %+v", ready)
	}
	if e.compute.LastSpec.IdempotencyKey != d.IdempotencyKey || e.compute.LastSpec.Labels["deployment_id"] != string(d.ID) {
		t.Fatalf("provision spec = %+v", e.compute.LastSpec)
	}
	got, ok, err := e.svc.FindReady(ctx, "m1")
	if err != nil || !ok || got.ID != d.ID {
		t.Fatalf("FindReady = %v %v %v", got.ID, ok, err)
	}
}

func TestCreateValidatesBeforeSpending(t *testing.T) {
	e := newEnv(t, 1)
	ctx := context.Background()
	if _, err := e.svc.Create(ctx, CreateRequest{ModelID: "nope"}); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown model: %v", err)
	}
	if _, err := e.svc.Create(ctx, CreateRequest{ModelID: "m1", RuntimeConfig: map[string]any{"invalid": 1}}); !errors.Is(err, deployment.ErrInvalid) {
		t.Fatalf("invalid runtime config: %v", err)
	}
	big := testModel()
	big.ID, big.Requirements.MinVRAMGB = "big", 1000
	_ = e.models.Save(ctx, big)
	if _, err := e.svc.Create(ctx, CreateRequest{ModelID: "big"}); !errors.Is(err, model.ErrNoSuitableCompute) {
		t.Fatalf("no suitable compute: %v", err)
	}
	off := testModel()
	off.ID, off.Status = "off", model.StatusDeactivated
	_ = e.models.Save(ctx, off)
	if _, err := e.svc.Create(ctx, CreateRequest{ModelID: "off"}); !errors.Is(err, ErrModelInactive) {
		t.Fatalf("inactive: %v", err)
	}
	if e.compute.Provisions != 0 {
		t.Fatal("nothing may be provisioned for invalid requests")
	}
}

func TestMaxActiveIsEnforced(t *testing.T) {
	e := newEnv(t, 1)
	ctx := context.Background()
	if _, err := e.svc.Create(ctx, CreateRequest{ModelID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, CreateRequest{ModelID: "m1"}); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("want ErrLimitReached, got %v", err)
	}
}

func TestInitFailureReleasesCompute(t *testing.T) {
	e := newEnv(t, 1)
	e.runtime.InitErr = errors.New("init boom")
	d, err := e.svc.Create(context.Background(), CreateRequest{ModelID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	failed := e.waitStatus(t, d.ID, deployment.Failed)
	waitFor(t, func() bool { d, _ := e.deps.Get(context.Background(), d.ID); return d.InstanceID == nil })
	failed, _ = e.deps.Get(context.Background(), d.ID)
	if failed.Failure == nil || failed.Failure.Stage != deployment.Initializing || e.compute.Live() != 0 {
		t.Fatalf("failure=%+v live=%d", failed.Failure, e.compute.Live())
	}
}

func TestCleanupFailureRetainsInstance(t *testing.T) {
	e := newEnv(t, 1)
	e.runtime.SetLoaded(false) // model never loads -> ReadyTimeout
	e.compute.SetTerminateErr(errors.New("provider down"))
	d, _ := e.svc.Create(context.Background(), CreateRequest{ModelID: "m1"})
	failed := e.waitStatus(t, d.ID, deployment.Failed)
	if failed.InstanceID == nil || e.compute.Live() != 1 {
		t.Fatalf("instance must be retained when release fails: %+v", failed)
	}
	if failed.Failure.Stage != deployment.Starting {
		t.Fatalf("stage = %s", failed.Failure.Stage)
	}

	// Once the provider recovers, cleanup succeeds and the deployment ends Stopped.
	e.compute.SetTerminateErr(nil)
	stopped, err := e.svc.Stop(context.Background(), d.ID)
	if err != nil || stopped.Status != deployment.Stopped || stopped.InstanceID != nil || e.compute.Live() != 0 {
		t.Fatalf("stop after recovery: %+v %v", stopped, err)
	}
	if stopped.Failure == nil {
		t.Fatal("failure record should be kept")
	}
}

func TestStopReleasesComputeAndRecordsUptime(t *testing.T) {
	e := newEnv(t, 1)
	ctx := context.Background()
	d, _ := e.svc.Create(ctx, CreateRequest{ModelID: "m1"})
	e.waitStatus(t, d.ID, deployment.Ready)
	time.Sleep(5 * time.Millisecond)

	stopped, err := e.svc.Stop(ctx, d.ID)
	if err != nil || stopped.Status != deployment.Stopped || e.compute.Live() != 0 {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	recs := e.usage.All()
	if len(recs) != 1 || recs[0].Kind != usage.KindUptime || recs[0].Cost == nil || recs[0].Duration <= 0 {
		t.Fatalf("uptime records = %+v", recs)
	}
	if again, err := e.svc.Stop(ctx, d.ID); err != nil || again.Status != deployment.Stopped {
		t.Fatalf("second stop must be a no-op: %v", err)
	}
	if err := e.svc.Delete(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStopDuringProvisioningLeavesNoInstance(t *testing.T) {
	e := newEnv(t, 1)
	e.compute.ProvisionDelay = 50 * time.Millisecond
	ctx := context.Background()
	d, _ := e.svc.Create(ctx, CreateRequest{ModelID: "m1"})
	e.waitStatus(t, d.ID, deployment.Provisioning)

	stopped, err := e.svc.Stop(ctx, d.ID)
	if err != nil || stopped.Status != deployment.Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	// The fake provider creates the instance even though the caller gave up;
	// the service must find and release it.
	waitFor(t, func() bool { return e.compute.Live() == 0 })
	time.Sleep(20 * time.Millisecond)
	if d, _ := e.deps.Get(ctx, d.ID); d.Status != deployment.Stopped {
		t.Fatalf("launch must not resurrect a stopped deployment: %s", d.Status)
	}
}

func TestRestart(t *testing.T) {
	e := newEnv(t, 1)
	ctx := context.Background()
	d, _ := e.svc.Create(ctx, CreateRequest{ModelID: "m1"})
	first := e.waitStatus(t, d.ID, deployment.Ready)
	if _, err := e.svc.Restart(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	second := e.waitStatus(t, d.ID, deployment.Ready)
	if *second.InstanceID == *first.InstanceID || second.IdempotencyKey == first.IdempotencyKey {
		t.Fatal("restart must use new compute and a new idempotency key")
	}
	if e.compute.Live() != 1 {
		t.Fatalf("live instances = %d", e.compute.Live())
	}
}

func TestDeleteRequiresStopped(t *testing.T) {
	e := newEnv(t, 1)
	ctx := context.Background()
	d, _ := e.svc.Create(ctx, CreateRequest{ModelID: "m1"})
	e.waitStatus(t, d.ID, deployment.Ready)
	if err := e.svc.Delete(ctx, d.ID); !errors.Is(err, ErrNotDeletable) {
		t.Fatalf("want ErrNotDeletable, got %v", err)
	}
}

func TestCheckHealthTogglesUnhealthy(t *testing.T) {
	e := newEnv(t, 1)
	ctx := context.Background()
	d, _ := e.svc.Create(ctx, CreateRequest{ModelID: "m1"})
	e.waitStatus(t, d.ID, deployment.Ready)
	e.runtime.SetLoaded(false)
	if got, err := e.svc.CheckHealth(ctx, d.ID); err != nil || got.Status != deployment.Unhealthy {
		t.Fatalf("got %s %v", got.Status, err)
	}
	if _, ok, _ := e.svc.FindReady(ctx, "m1"); ok {
		t.Fatal("unhealthy deployments must not receive traffic")
	}
	e.runtime.SetLoaded(true)
	if got, err := e.svc.CheckHealth(ctx, d.ID); err != nil || got.Status != deployment.Ready {
		t.Fatalf("got %s %v", got.Status, err)
	}
	if got, err := e.svc.CheckHealth(ctx, d.ID); err != nil || got.Status != deployment.Ready {
		t.Fatalf("unchanged health: %s %v", got.Status, err)
	}
}

func TestStopIfIdleRespectsActivityAndPins(t *testing.T) {
	e := newEnv(t, 1)
	ctx := context.Background()
	d, _ := e.svc.Create(ctx, CreateRequest{ModelID: "m1"})
	e.waitStatus(t, d.ID, deployment.Ready)

	end := e.svc.Activity.Begin(d.ID)
	if stopped, err := e.svc.StopIfIdle(ctx, d.ID, 0); err != nil || stopped {
		t.Fatalf("busy deployment stopped: %v %v", stopped, err)
	}
	end()
	_, _ = e.svc.Pin(ctx, d.ID)
	if stopped, _ := e.svc.StopIfIdle(ctx, d.ID, 0); stopped {
		t.Fatal("pinned deployment stopped")
	}
	_, _ = e.svc.Unpin(ctx, d.ID)
	if stopped, err := e.svc.StopIfIdle(ctx, d.ID, 0); err != nil || !stopped {
		t.Fatalf("idle deployment not stopped: %v %v", stopped, err)
	}
}

func TestReconcile(t *testing.T) {
	e := newEnv(t, 5)
	ctx := context.Background()
	now := time.Now()

	// Interrupted mid-launch with an instance attached.
	inst1 := compute.Instance{ID: "i1", Status: compute.InstanceRunning, Labels: map[string]string{"deployment_id": "a"}}
	e.compute.Add(inst1)
	a, _ := deployment.New("a", "m1", "ka", now)
	_ = a.Transition(deployment.Provisioning, now)
	_ = a.AttachInstance("i1", now)
	e.deps.Put(a)

	// Ready, but its instance vanished.
	b, _ := deployment.New("b", "m1", "kb", now)
	_ = b.Transition(deployment.Provisioning, now)
	_ = b.AttachInstance("gone", now)
	_ = b.Transition(deployment.Initializing, now)
	_ = b.Transition(deployment.Starting, now)
	b.Endpoint = &compute.Endpoint{URL: "http://gone"}
	_ = b.Transition(deployment.Ready, now)
	e.deps.Put(b)

	// Pending: never touched compute, relaunch.
	c, _ := deployment.New("c", "m1", "kc", now)
	e.deps.Put(c)

	// Orphan instance nobody references.
	e.compute.Add(compute.Instance{ID: "orphan", Status: compute.InstanceRunning, Labels: map[string]string{"deployment_id": "zzz"}})

	rep, err := e.svc.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if da, _ := e.deps.Get(ctx, "a"); da.Status != deployment.Stopped || da.Failure == nil || da.InstanceID != nil {
		t.Fatalf("a = %+v", da)
	}
	if db, _ := e.deps.Get(ctx, "b"); db.Status != deployment.Stopped {
		t.Fatalf("b = %s", db.Status)
	}
	e.waitStatus(t, "c", deployment.Ready)
	if len(rep.OrphansTerminated) != 1 || rep.OrphansTerminated[0] != "orphan" {
		t.Fatalf("orphans = %v", rep.OrphansTerminated)
	}
	if _, err := e.compute.GetInstance(ctx, "i1"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("interrupted launch's instance must be terminated")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
