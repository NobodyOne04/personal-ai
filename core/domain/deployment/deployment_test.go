package deployment

import (
	"errors"
	"sync"
	"testing"
	"time"

	"personal-ai/core/domain/compute"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// expected is written out independently of the production table so that an
// accidental edit of the table fails this test.
var expected = map[Status][]Status{
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

func TestTransitionTableIsExhaustivelyCorrect(t *testing.T) {
	for _, from := range AllStatuses {
		allowed := map[Status]bool{}
		for _, to := range expected[from] {
			allowed[to] = true
		}
		for _, to := range AllStatuses {
			if got := from.CanTransitionTo(to); got != allowed[to] {
				t.Errorf("%s -> %s: CanTransitionTo = %v, want %v", from, to, got, allowed[to])
			}
		}
	}
	if len(transitions) != len(AllStatuses) {
		t.Fatalf("transition table has %d states, AllStatuses has %d", len(transitions), len(AllStatuses))
	}
}

func TestStatusValid(t *testing.T) {
	for _, s := range AllStatuses {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if Status("bogus").Valid() {
		t.Error("bogus status reported valid")
	}
}

func TestIsLive(t *testing.T) {
	live := map[Status]bool{Pending: true, Provisioning: true, Initializing: true, Starting: true, Ready: true, Unhealthy: true}
	for _, s := range AllStatuses {
		if s.IsLive() != live[s] {
			t.Errorf("%s.IsLive() = %v", s, s.IsLive())
		}
	}
}

func newDep(t *testing.T) Deployment {
	t.Helper()
	d, err := New("d1", "m1", "key-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNew(t *testing.T) {
	d := newDep(t)
	if d.Status != Pending || d.Health != HealthUnknown || d.IdempotencyKey != "key-1" {
		t.Fatalf("unexpected new deployment: %+v", d)
	}
	if !d.CreatedAt.Equal(t0) || !d.LastActivityAt.Equal(t0) {
		t.Fatal("timestamps not initialised")
	}
	if _, err := New("", "m", "k", t0); !errors.Is(err, ErrInvalid) {
		t.Errorf("no id: want ErrInvalid, got %v", err)
	}
	if _, err := New("d", "", "k", t0); !errors.Is(err, ErrInvalid) {
		t.Errorf("no model: want ErrInvalid, got %v", err)
	}
	if _, err := New("d", "m", "", t0); !errors.Is(err, ErrInvalid) {
		t.Errorf("no key: want ErrInvalid, got %v", err)
	}
}

func TestHappyPathLifecycle(t *testing.T) {
	d := newDep(t)
	inst := compute.InstanceID("i-1")

	steps := []struct {
		to    Status
		setup func()
	}{
		{Provisioning, nil},
		{Initializing, func() { _ = d.AttachInstance(inst, t0) }},
		{Starting, nil},
		{Ready, func() { d.Endpoint = &compute.Endpoint{URL: "http://x"} }},
	}
	for i, s := range steps {
		if s.setup != nil {
			s.setup()
		}
		now := t0.Add(time.Duration(i+1) * time.Minute)
		if err := d.Transition(s.to, now); err != nil {
			t.Fatalf("-> %s: %v", s.to, err)
		}
	}
	if d.Status != Ready || d.Health != HealthHealthy || d.ReadyAt == nil {
		t.Fatalf("not ready: %+v", d)
	}
	if !d.AcceptsRequests() {
		t.Fatal("ready deployment must accept requests")
	}

	// shut down: release compute before Stopped is allowed.
	if err := d.Transition(Stopping, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := d.Transition(Stopped, t0.Add(11*time.Minute)); !errors.Is(err, ErrInstanceAttached) {
		t.Fatalf("Stopped with instance attached: want ErrInstanceAttached, got %v", err)
	}
	d.DetachInstance(t0.Add(11 * time.Minute))
	if err := d.Transition(Stopped, t0.Add(12*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d.AcceptsRequests() {
		t.Fatal("stopped deployment must not accept requests")
	}
}

func TestInvalidTransitionLeavesStateUntouched(t *testing.T) {
	d := newDep(t)
	before := d
	if err := d.Transition(Ready, t0.Add(time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	if d.Status != before.Status || !d.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("failed transition mutated the deployment")
	}
}

func TestInitializingRequiresInstance(t *testing.T) {
	d := newDep(t)
	if err := d.Transition(Provisioning, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.Transition(Initializing, t0); !errors.Is(err, ErrNoInstance) {
		t.Fatalf("want ErrNoInstance, got %v", err)
	}
}

func TestReadyRequiresEndpoint(t *testing.T) {
	d := newDep(t)
	_ = d.Transition(Provisioning, t0)
	_ = d.AttachInstance("i-1", t0)
	_ = d.Transition(Initializing, t0)
	_ = d.Transition(Starting, t0)
	if err := d.Transition(Ready, t0); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("want ErrNoEndpoint, got %v", err)
	}
}

func TestFailRecordsStageAndKeepsInstance(t *testing.T) {
	d := newDep(t)
	_ = d.Transition(Provisioning, t0)
	_ = d.AttachInstance("i-1", t0)
	_ = d.Transition(Initializing, t0)

	now := t0.Add(time.Minute)
	if err := d.Fail("runtime did not start", now); err != nil {
		t.Fatal(err)
	}
	if d.Status != Failed || d.Failure == nil {
		t.Fatalf("not failed: %+v", d)
	}
	if d.Failure.Stage != Initializing || d.Failure.Reason != "runtime did not start" || !d.Failure.At.Equal(now) {
		t.Fatalf("bad failure record: %+v", d.Failure)
	}
	if d.InstanceID == nil {
		t.Fatal("a failed deployment must keep its instance until it is released (no orphans)")
	}
	// cannot retry while still holding compute
	if err := d.Transition(Pending, now); !errors.Is(err, ErrInstanceAttached) {
		t.Fatalf("want ErrInstanceAttached, got %v", err)
	}
	// cleanup path: Stopping -> release -> Stopped
	if err := d.Transition(Stopping, now); err != nil {
		t.Fatal(err)
	}
}

func TestFailFromInvalidStateErrors(t *testing.T) {
	d := newDep(t)
	_ = d.Transition(Provisioning, t0)
	_ = d.AttachInstance("i-1", t0)
	_ = d.Transition(Initializing, t0)
	_ = d.Transition(Starting, t0)
	d.Endpoint = &compute.Endpoint{URL: "http://x"}
	_ = d.Transition(Ready, t0)
	if err := d.Fail("boom", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Ready cannot fail directly; want ErrInvalidTransition, got %v", err)
	}
}

func TestRetryFromFailedClearsState(t *testing.T) {
	d := newDep(t)
	_ = d.Transition(Provisioning, t0)
	if err := d.Fail("no capacity", t0); err != nil {
		t.Fatal(err)
	}
	if err := d.Transition(Pending, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d.Failure != nil || d.Health != HealthUnknown || d.Endpoint != nil || d.ReadyAt != nil {
		t.Fatalf("retry did not reset: %+v", d)
	}
}

func TestUnhealthyRecovery(t *testing.T) {
	d := newDep(t)
	_ = d.Transition(Provisioning, t0)
	_ = d.AttachInstance("i-1", t0)
	_ = d.Transition(Initializing, t0)
	_ = d.Transition(Starting, t0)
	d.Endpoint = &compute.Endpoint{URL: "http://x"}
	_ = d.Transition(Ready, t0)

	if err := d.Transition(Unhealthy, t0); err != nil {
		t.Fatal(err)
	}
	if d.Health != HealthUnhealthy || d.AcceptsRequests() {
		t.Fatalf("unhealthy deployment must not accept requests: %+v", d)
	}
	if err := d.Transition(Ready, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d.Health != HealthHealthy {
		t.Fatal("health not restored")
	}
}

func TestAttachInstance(t *testing.T) {
	d := newDep(t)
	if err := d.AttachInstance("", t0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty id: want ErrInvalid, got %v", err)
	}
	if err := d.AttachInstance("i-1", t0); err != nil {
		t.Fatal(err)
	}
	if err := d.AttachInstance("i-1", t0); err != nil {
		t.Fatalf("re-attaching the same instance must be idempotent: %v", err)
	}
	if err := d.AttachInstance("i-2", t0); !errors.Is(err, ErrInstanceAttached) {
		t.Fatalf("second instance: want ErrInstanceAttached, got %v", err)
	}
}

func TestRecordActivityNeverMovesBackwards(t *testing.T) {
	d := newDep(t)
	d.RecordActivity(t0.Add(time.Hour))
	d.RecordActivity(t0.Add(time.Minute))
	if !d.LastActivityAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("LastActivityAt = %v", d.LastActivityAt)
	}
}

func readyDep(t *testing.T) Deployment {
	t.Helper()
	d := newDep(t)
	_ = d.Transition(Provisioning, t0)
	_ = d.AttachInstance("i-1", t0)
	_ = d.Transition(Initializing, t0)
	_ = d.Transition(Starting, t0)
	d.Endpoint = &compute.Endpoint{URL: "http://x"}
	if err := d.Transition(Ready, t0); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestIsIdle(t *testing.T) {
	timeout := 30 * time.Minute
	d := readyDep(t)

	tests := []struct {
		name   string
		mutate func(*Deployment)
		now    time.Time
		active int
		want   bool
	}{
		{"quiet long enough", func(*Deployment) {}, t0.Add(31 * time.Minute), 0, true},
		{"exactly at timeout", func(*Deployment) {}, t0.Add(timeout), 0, true},
		{"just under timeout", func(*Deployment) {}, t0.Add(timeout - time.Second), 0, false},
		{"in-flight request", func(*Deployment) {}, t0.Add(time.Hour), 1, false},
		{"pinned", func(d *Deployment) { d.Pinned = true }, t0.Add(time.Hour), 0, false},
		{"unhealthy and quiet", func(d *Deployment) { d.Status = Unhealthy }, t0.Add(time.Hour), 0, true},
		{"starting is not idle", func(d *Deployment) { d.Status = Starting }, t0.Add(time.Hour), 0, false},
		{"stopped is not idle", func(d *Deployment) { d.Status = Stopped }, t0.Add(time.Hour), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dd := d
			tt.mutate(&dd)
			if got := dd.IsIdle(tt.now, timeout, tt.active); got != tt.want {
				t.Fatalf("IsIdle = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPinUnpin(t *testing.T) {
	d := newDep(t)
	d.Pin(t0)
	if !d.Pinned {
		t.Fatal("not pinned")
	}
	d.Unpin(t0)
	if d.Pinned {
		t.Fatal("still pinned")
	}
}

func TestActivityCountsAndEndIsIdempotent(t *testing.T) {
	a := NewActivity()
	end1 := a.Begin("d1")
	end2 := a.Begin("d1")
	other := a.Begin("d2")
	if a.Active("d1") != 2 || a.Active("d2") != 1 {
		t.Fatalf("counts: d1=%d d2=%d", a.Active("d1"), a.Active("d2"))
	}
	end1()
	end1() // second call must not double-decrement
	if a.Active("d1") != 1 {
		t.Fatalf("after idempotent end: %d, want 1", a.Active("d1"))
	}
	end2()
	other()
	if a.Active("d1") != 0 || a.Active("d2") != 0 {
		t.Fatal("counts should return to zero")
	}
}

func TestActivityConcurrent(t *testing.T) {
	a := NewActivity()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			end := a.Begin("d")
			_ = a.Active("d")
			end()
		}()
	}
	wg.Wait()
	if got := a.Active("d"); got != 0 {
		t.Fatalf("leaked %d in-flight requests", got)
	}
}
