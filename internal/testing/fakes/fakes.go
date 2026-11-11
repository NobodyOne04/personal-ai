// Package fakes provides in-memory implementations of every port for tests.
// They are concurrency-safe and honour the port contracts (optimistic
// locking, idempotent provisioning and termination, ErrNotFound).
package fakes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
	"personal-ai/core/ports"
)

// ---- clock / ids ----

// Clock is a settable clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// IDs generates sequential IDs.
type IDs struct{ n atomic.Int64 }

// NewID returns the next ID.
func (g *IDs) NewID() string { return fmt.Sprintf("id%d", g.n.Add(1)) }

// ---- repositories ----

// Models is an in-memory ports.ModelRepository.
type Models struct {
	mu   sync.Mutex
	data map[model.ID]model.Model
}

// NewModels returns a repository holding ms.
func NewModels(ms ...model.Model) *Models {
	r := &Models{data: map[model.ID]model.Model{}}
	for _, m := range ms {
		r.data[m.ID] = m
	}
	return r
}

func (r *Models) Save(_ context.Context, m model.Model) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[m.ID] = m
	return nil
}

func (r *Models) Get(_ context.Context, id model.ID) (model.Model, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.data[id]
	if !ok {
		return model.Model{}, ports.ErrNotFound
	}
	return m, nil
}

func (r *Models) List(context.Context) ([]model.Model, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]model.Model, 0, len(r.data))
	for _, m := range r.data {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Deployments is an in-memory ports.DeploymentRepository.
type Deployments struct {
	mu   sync.Mutex
	data map[deployment.ID]deployment.Deployment
}

// NewDeployments returns an empty repository.
func NewDeployments() *Deployments {
	return &Deployments{data: map[deployment.ID]deployment.Deployment{}}
}

func (r *Deployments) Save(_ context.Context, d deployment.Deployment) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, exists := r.data[d.ID]
	switch {
	case d.Version == 0 && exists:
		return deployment.Deployment{}, ports.ErrVersionConflict
	case d.Version != 0 && !exists:
		return deployment.Deployment{}, ports.ErrNotFound
	case exists && cur.Version != d.Version:
		return deployment.Deployment{}, ports.ErrVersionConflict
	}
	if exists && cur.LastActivityAt.After(d.LastActivityAt) {
		d.LastActivityAt = cur.LastActivityAt
	}
	d.Version++
	r.data[d.ID] = d
	return d, nil
}

func (r *Deployments) Get(_ context.Context, id deployment.ID) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.data[id]
	if !ok {
		return deployment.Deployment{}, ports.ErrNotFound
	}
	return d, nil
}

func (r *Deployments) FindByIdempotencyKey(_ context.Context, key string) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.data {
		if d.IdempotencyKey == key {
			return d, nil
		}
	}
	return deployment.Deployment{}, ports.ErrNotFound
}

func (r *Deployments) filter(keep func(deployment.Deployment) bool) []deployment.Deployment {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []deployment.Deployment{}
	for _, d := range r.data {
		if keep(d) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Deployments) List(context.Context) ([]deployment.Deployment, error) {
	return r.filter(func(deployment.Deployment) bool { return true }), nil
}

func (r *Deployments) ListByStatus(_ context.Context, statuses ...deployment.Status) ([]deployment.Deployment, error) {
	return r.filter(func(d deployment.Deployment) bool {
		for _, s := range statuses {
			if d.Status == s {
				return true
			}
		}
		return false
	}), nil
}

func (r *Deployments) ListByModel(_ context.Context, id model.ID) ([]deployment.Deployment, error) {
	return r.filter(func(d deployment.Deployment) bool { return d.ModelID == id }), nil
}

func (r *Deployments) RecordActivity(_ context.Context, id deployment.ID, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.data[id]
	if !ok {
		return ports.ErrNotFound
	}
	if at.After(d.LastActivityAt) {
		d.LastActivityAt = at
		r.data[id] = d
	}
	return nil
}

func (r *Deployments) Delete(_ context.Context, id deployment.ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.data[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.data, id)
	return nil
}

// Put stores d as-is (for test setup), bumping the version.
func (r *Deployments) Put(d deployment.Deployment) deployment.Deployment {
	r.mu.Lock()
	defer r.mu.Unlock()
	d.Version = r.data[d.ID].Version + 1
	r.data[d.ID] = d
	return d
}

// Usage is an in-memory ports.UsageRepository.
type Usage struct {
	mu      sync.Mutex
	Records []usage.Record
}

func (r *Usage) Append(_ context.Context, rec usage.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Records = append(r.Records, rec)
	return nil
}

func (r *Usage) Summarize(_ context.Context, tr usage.TimeRange) (usage.Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return usage.Summarize(r.Records, tr), nil
}

// All returns a copy of the records.
func (r *Usage) All() []usage.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]usage.Record(nil), r.Records...)
}

// Prefs is an in-memory ports.PreferenceRepository.
type Prefs struct {
	mu   sync.Mutex
	data map[string]model.Preferences
}

// NewPrefs returns an empty repository.
func NewPrefs() *Prefs { return &Prefs{data: map[string]model.Preferences{}} }

func (r *Prefs) Get(_ context.Context, s string) (model.Preferences, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.data[s]
	if !ok {
		return model.Preferences{}, ports.ErrNotFound
	}
	return p, nil
}

func (r *Prefs) Set(_ context.Context, p model.Preferences) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[p.SubjectID] = p
	return nil
}

// ---- events ----

// Events records published events.
type Events struct {
	mu     sync.Mutex
	Events []ports.Event
}

func (e *Events) Publish(_ context.Context, ev ports.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Events = append(e.Events, ev)
	return nil
}

// Types returns the published event types in order.
func (e *Events) Types() []ports.EventType {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ports.EventType, len(e.Events))
	for i, ev := range e.Events {
		out[i] = ev.Type
	}
	return out
}

// ---- compute ----

// Compute is a scriptable ports.ComputeProvider. Provisioned instances start
// Running with an endpoint unless StartStatus says otherwise.
type Compute struct {
	mu        sync.Mutex
	Caps      []compute.Capability
	Instances map[compute.InstanceID]compute.Instance
	byKey     map[string]compute.InstanceID
	n         int

	ProvisionErr error
	TerminateErr error
	// ProvisionDelay blocks Provision (honouring ctx) to simulate slow APIs.
	ProvisionDelay time.Duration
	// StartStatus is the status of new instances (default Running).
	StartStatus compute.InstanceStatus
	Provisions  int
	Terminated  []compute.InstanceID
	LastSpec    compute.ProvisionSpec
}

// NewCompute offers one 48 GB GPU at $0.50/h.
func NewCompute() *Compute {
	return &Compute{
		Caps: []compute.Capability{{ID: "gpu48", GPUModel: "A40", GPUCount: 1, VRAMPerGPUGB: 48,
			HourlyCost: &compute.Money{Currency: "USD", Micros: 500_000}}},
		Instances: map[compute.InstanceID]compute.Instance{},
		byKey:     map[string]compute.InstanceID{},
	}
}

func (c *Compute) Capabilities(context.Context) ([]compute.Capability, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]compute.Capability(nil), c.Caps...), nil
}

func (c *Compute) Provision(ctx context.Context, spec compute.ProvisionSpec) (compute.Instance, error) {
	if err := spec.Validate(); err != nil {
		return compute.Instance{}, err
	}
	c.mu.Lock()
	delay := c.ProvisionDelay
	c.mu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastSpec = spec
	if c.ProvisionErr != nil {
		return compute.Instance{}, c.ProvisionErr
	}
	if id, ok := c.byKey[spec.IdempotencyKey]; ok {
		return c.Instances[id], nil
	}
	c.n++
	c.Provisions++
	id := compute.InstanceID(fmt.Sprintf("inst%d", c.n))
	status := c.StartStatus
	if status == "" {
		status = compute.InstanceRunning
	}
	inst := compute.Instance{ID: id, ProviderRef: string(id), Status: status, Labels: spec.Labels,
		CreatedAt: time.Now()}
	for _, cp := range c.Caps {
		if cp.ID == spec.CapabilityID {
			inst.Capability = cp
		}
	}
	if status == compute.InstanceRunning {
		inst.Endpoint = &compute.Endpoint{URL: "http://" + string(id)}
	}
	c.Instances[id] = inst
	c.byKey[spec.IdempotencyKey] = id
	// Report "created" even if the caller gave up meanwhile, like a real API.
	if ctx.Err() != nil {
		return compute.Instance{}, ctx.Err()
	}
	return inst, nil
}

// SetRunning marks an instance running with an endpoint.
func (c *Compute) SetRunning(id compute.InstanceID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	inst := c.Instances[id]
	inst.Status = compute.InstanceRunning
	inst.Endpoint = &compute.Endpoint{URL: "http://" + string(id)}
	c.Instances[id] = inst
}

// Add registers an instance directly (e.g. an orphan).
func (c *Compute) Add(inst compute.Instance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Instances[inst.ID] = inst
}

func (c *Compute) GetInstance(_ context.Context, id compute.InstanceID) (compute.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	inst, ok := c.Instances[id]
	if !ok {
		return compute.Instance{}, ports.ErrNotFound
	}
	return inst, nil
}

func (c *Compute) ListInstances(context.Context) ([]compute.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []compute.Instance{}
	for _, inst := range c.Instances {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *Compute) Terminate(_ context.Context, id compute.InstanceID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.TerminateErr != nil {
		return c.TerminateErr
	}
	if _, ok := c.Instances[id]; ok {
		delete(c.Instances, id)
		c.Terminated = append(c.Terminated, id)
	}
	return nil
}

// Live returns the number of instances still held.
func (c *Compute) Live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Instances)
}

// SetTerminateErr sets the error Terminate returns.
func (c *Compute) SetTerminateErr(err error) {
	c.mu.Lock()
	c.TerminateErr = err
	c.mu.Unlock()
}

// ---- runtime ----

// Runtime is a scriptable ports.InferenceRuntime.
type Runtime struct {
	mu          sync.Mutex
	Loaded      bool
	PrepareErr  error
	InitErr     error
	ChatErr     error
	Reply       string
	Usage       inference.TokenUsage
	StreamParts []string
	// StreamErr is delivered as a terminal chunk after StreamParts.
	StreamErr error
	// StreamGate, if set, is awaited before each streamed chunk.
	StreamGate chan struct{}
	Requests   []inference.Request
}

// NewRuntime returns a runtime whose model is loaded and replies "ok".
func NewRuntime() *Runtime {
	return &Runtime{Loaded: true, Reply: "ok", Usage: inference.TokenUsage{Input: 3, Output: 2},
		StreamParts: []string{"o", "k"}}
}

// SetLoaded changes the reported health.
func (r *Runtime) SetLoaded(v bool) { r.mu.Lock(); r.Loaded = v; r.mu.Unlock() }

func (r *Runtime) Prepare(_ context.Context, spec ports.PrepareSpec) (compute.LaunchSpec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.PrepareErr != nil {
		return compute.LaunchSpec{}, r.PrepareErr
	}
	if _, bad := spec.Overrides["invalid"]; bad {
		return compute.LaunchSpec{}, errors.New("invalid override")
	}
	if spec.RuntimeKey == "" {
		return compute.LaunchSpec{}, errors.New("fake runtime: no runtime key")
	}
	return compute.LaunchSpec{Image: "img", Ports: []int{8000}}, nil
}

func (r *Runtime) Initialize(_ context.Context, spec ports.InitSpec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.InitErr != nil {
		return "", r.InitErr
	}
	return "ref-" + spec.DeploymentID, nil
}

func (r *Runtime) Health(_ context.Context, t ports.RuntimeTarget) (ports.RuntimeHealth, error) {
	if t.Key == "" {
		return ports.RuntimeHealth{}, errors.New("fake runtime: no runtime key")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return ports.RuntimeHealth{Ready: r.Loaded, ModelLoaded: r.Loaded}, nil
}

func (r *Runtime) Models(context.Context, ports.RuntimeTarget) ([]string, error) { return nil, nil }

func (r *Runtime) Chat(_ context.Context, t ports.RuntimeTarget, req inference.Request) (inference.Response, error) {
	if t.Key == "" {
		return inference.Response{}, errors.New("fake runtime: no runtime key")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Requests = append(r.Requests, req)
	if r.ChatErr != nil {
		return inference.Response{}, r.ChatErr
	}
	return inference.Response{ModelID: req.ModelID, Message: inference.Message{Role: inference.RoleAssistant, Content: r.Reply},
		FinishReason: inference.FinishStop, Usage: r.Usage}, nil
}

func (r *Runtime) ChatStream(ctx context.Context, t ports.RuntimeTarget, req inference.Request) (<-chan inference.Chunk, error) {
	if t.Key == "" {
		return nil, errors.New("fake runtime: no runtime key")
	}
	r.mu.Lock()
	r.Requests = append(r.Requests, req)
	if r.ChatErr != nil {
		defer r.mu.Unlock()
		return nil, r.ChatErr
	}
	parts, streamErr, gate, use := r.StreamParts, r.StreamErr, r.StreamGate, r.Usage
	r.mu.Unlock()

	ch := make(chan inference.Chunk)
	go func() {
		defer close(ch)
		for i, p := range parts {
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					return
				}
			}
			c := inference.Chunk{Delta: p}
			if i == len(parts)-1 && streamErr == nil {
				c.FinishReason = inference.FinishStop
				u := use
				c.Usage = &u
			}
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
		if streamErr != nil {
			select {
			case ch <- inference.Chunk{Err: streamErr}:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}

func (r *Runtime) Release(context.Context, ports.RuntimeTarget) error { return nil }

// Interface checks.
var (
	_ ports.ModelRepository      = (*Models)(nil)
	_ ports.DeploymentRepository = (*Deployments)(nil)
	_ ports.UsageRepository      = (*Usage)(nil)
	_ ports.PreferenceRepository = (*Prefs)(nil)
	_ ports.EventPublisher       = (*Events)(nil)
	_ ports.ComputeProvider      = (*Compute)(nil)
	_ ports.InferenceRuntime     = (*Runtime)(nil)
	_ ports.Clock                = (*Clock)(nil)
	_ ports.IDGenerator          = (*IDs)(nil)
)
