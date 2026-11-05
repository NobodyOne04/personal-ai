package messaging

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"personal-ai/core/ports"
)

var ctx = context.Background()

func ev(t ports.EventType) ports.Event { return ports.Event{Type: t, DeploymentID: "d1"} }

// collector gathers events for assertions.
type collector struct {
	mu  sync.Mutex
	got []ports.Event
	ch  chan struct{}
}

func newCollector() *collector { return &collector{ch: make(chan struct{}, 1024)} }

func (c *collector) handle(_ context.Context, e ports.Event) {
	c.mu.Lock()
	c.got = append(c.got, e)
	c.mu.Unlock()
	c.ch <- struct{}{}
}

func (c *collector) types() []ports.EventType {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ports.EventType, len(c.got))
	for i, e := range c.got {
		out[i] = e.Type
	}
	return out
}

func (c *collector) waitFor(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-c.ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d of %d", i+1, n)
		}
	}
}

func TestDeliversToAllSubscribersInOrder(t *testing.T) {
	b := New()
	defer b.Close()
	c1, c2 := newCollector(), newCollector()
	if _, err := b.Subscribe(c1.handle); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Subscribe(c2.handle); err != nil {
		t.Fatal(err)
	}

	want := []ports.EventType{ports.EventDeploymentRequested, ports.EventDeploymentReady, ports.EventDeploymentStopped}
	for _, typ := range want {
		if err := b.Publish(ctx, ev(typ)); err != nil {
			t.Fatal(err)
		}
	}
	c1.waitFor(t, 3)
	c2.waitFor(t, 3)

	for i, c := range []*collector{c1, c2} {
		got := c.types()
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("subscriber %d: order = %v, want %v", i+1, got, want)
			}
		}
	}
}

func TestFiltersByType(t *testing.T) {
	b := New()
	defer b.Close()
	ready, all := newCollector(), newCollector()
	_, _ = b.Subscribe(ready.handle, ports.EventDeploymentReady)
	_, _ = b.Subscribe(all.handle)

	_ = b.Publish(ctx, ev(ports.EventDeploymentRequested))
	_ = b.Publish(ctx, ev(ports.EventDeploymentReady))
	_ = b.Publish(ctx, ev(ports.EventUsageRecorded))

	all.waitFor(t, 3)
	ready.waitFor(t, 1)
	b.Close() // drain before asserting that nothing else arrived

	if got := ready.types(); len(got) != 1 || got[0] != ports.EventDeploymentReady {
		t.Fatalf("filtered subscriber got %v", got)
	}
}

func TestPublishWithNoSubscribers(t *testing.T) {
	b := New()
	defer b.Close()
	if err := b.Publish(ctx, ev(ports.EventDeploymentReady)); err != nil {
		t.Fatalf("publishing with no subscribers must succeed, got %v", err)
	}
}

func TestPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	b := New(WithBuffer(1))
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var handled atomic.Int64
	_, _ = b.Subscribe(func(context.Context, ports.Event) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		handled.Add(1)
	})

	_ = b.Publish(ctx, ev(ports.EventDeploymentReady))
	<-started // handler is now stuck; queue (size 1) is empty again

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_ = b.Publish(ctx, ev(ports.EventDeploymentReady))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	// 1 queued, 99 dropped.
	if got := b.Dropped(); got != 99 {
		t.Fatalf("Dropped = %d, want 99", got)
	}
	close(release)
	b.Close()
	if got := handled.Load(); got != 2 {
		t.Fatalf("handled = %d, want 2 (the in-flight one plus the queued one)", got)
	}
}

func TestSlowSubscriberDoesNotAffectOthers(t *testing.T) {
	b := New(WithBuffer(1))
	release := make(chan struct{})
	_, _ = b.Subscribe(func(context.Context, ports.Event) { <-release })
	fast := newCollector()
	_, _ = b.Subscribe(fast.handle)

	for i := 0; i < 20; i++ {
		_ = b.Publish(ctx, ev(ports.EventDeploymentReady))
		fast.waitFor(t, 1) // keep the fast queue from filling
	}
	if len(fast.types()) != 20 {
		t.Fatalf("fast subscriber got %d events, want 20", len(fast.types()))
	}
	close(release)
	b.Close()
}

func TestUnsubscribe(t *testing.T) {
	b := New()
	defer b.Close()
	c := newCollector()
	unsub, _ := b.Subscribe(c.handle)

	_ = b.Publish(ctx, ev(ports.EventDeploymentReady))
	c.waitFor(t, 1)

	unsub()
	unsub() // idempotent
	_ = b.Publish(ctx, ev(ports.EventDeploymentReady))
	time.Sleep(20 * time.Millisecond)
	if n := len(c.types()); n != 1 {
		t.Fatalf("received %d events, want 1 (nothing after unsubscribe)", n)
	}
}

func TestCloseDrainsQueuedEventsAndRejectsNewOnes(t *testing.T) {
	b := New(WithBuffer(10))
	var handled atomic.Int64
	_, _ = b.Subscribe(func(context.Context, ports.Event) {
		time.Sleep(5 * time.Millisecond)
		handled.Add(1)
	})
	for i := 0; i < 5; i++ {
		_ = b.Publish(ctx, ev(ports.EventDeploymentReady))
	}
	b.Close()
	if got := handled.Load(); got != 5 {
		t.Fatalf("Close returned with %d of 5 events handled", got)
	}
	if err := b.Publish(ctx, ev(ports.EventDeploymentReady)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close: want ErrClosed, got %v", err)
	}
	if _, err := b.Subscribe(func(context.Context, ports.Event) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close: want ErrClosed, got %v", err)
	}
	b.Close() // idempotent
}

func TestUnsubscribeAfterCloseIsSafe(t *testing.T) {
	b := New()
	unsub, _ := b.Subscribe(newCollector().handle)
	b.Close()
	unsub() // must not panic on an already-closed channel
}

func TestSubscribeRequiresHandler(t *testing.T) {
	b := New()
	defer b.Close()
	if _, err := b.Subscribe(nil); err == nil {
		t.Fatal("expected error for nil handler")
	}
}

func TestPanickingHandlerIsContained(t *testing.T) {
	b := New()
	_, _ = b.Subscribe(func(context.Context, ports.Event) { panic("boom") })
	ok := newCollector()
	_, _ = b.Subscribe(ok.handle)

	_ = b.Publish(ctx, ev(ports.EventDeploymentReady))
	_ = b.Publish(ctx, ev(ports.EventDeploymentStopped))
	ok.waitFor(t, 2)
	b.Close()
}

func TestHandlerContextIsDetachedFromPublisher(t *testing.T) {
	b := New()
	defer b.Close()
	errCh := make(chan error, 1)
	_, _ = b.Subscribe(func(hctx context.Context, _ ports.Event) { errCh <- hctx.Err() })

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := b.Publish(cancelled, ev(ports.EventDeploymentReady)); err != nil {
		t.Fatalf("publish with a cancelled context should still succeed, got %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("handler context was cancelled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not delivered")
	}
}

func TestEachSubscriberGetsItsOwnDetailMap(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	wg.Add(2)
	mutate := func(_ context.Context, e ports.Event) {
		defer wg.Done()
		e.Detail["touched"] = "yes" // would be a data race or leak if the map were shared
	}
	_, _ = b.Subscribe(mutate)
	_, _ = b.Subscribe(mutate)

	orig := map[string]string{"k": "v"}
	_ = b.Publish(ctx, ports.Event{Type: ports.EventDeploymentReady, Detail: orig})
	wg.Wait()
	b.Close()
	if len(orig) != 1 {
		t.Fatalf("publisher's map was modified: %v", orig)
	}
}

func TestConcurrentPublishSubscribeUnsubscribeClose(t *testing.T) {
	b := New(WithBuffer(4))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = b.Publish(ctx, ev(ports.EventUsageRecorded))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				unsub, err := b.Subscribe(func(context.Context, ports.Event) {})
				if err != nil {
					return // bus closed
				}
				unsub()
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	b.Close()
	wg.Wait()
}
