// Package runpod implements ports.ComputeProvider on RunPod's REST API
// (https://rest.runpod.io/v1). RunPod types never leave this package: pods are
// mapped to compute.Instance and RunPod states to compute.InstanceStatus.
//
// Design notes:
//   - Offerings (GPU type, count, price) are declared by the operator in
//     configuration rather than discovered, so the platform never deploys on
//     hardware or at a price the operator did not approve.
//   - RunPod has no idempotency keys. The adapter names every pod
//     "<prefix><idempotency key>" and looks the name up before creating, so a
//     retried Provision returns the existing pod.
//   - RunPod has no labels. Labels are carried as PAI_LABEL_* environment
//     variables and read back from the pod.
//   - Pods are reached through RunPod's HTTPS proxy
//     (https://<pod>-<port>.proxy.runpod.net); the runtime adapter supplies the
//     credential that protects the port.
package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/ports"
)

// DefaultBaseURL is RunPod's REST API.
const DefaultBaseURL = "https://rest.runpod.io/v1"

const (
	labelEnvPrefix  = "PAI_LABEL_"
	idempotencyEnv  = "PAI_IDEMPOTENCY_KEY"
	defaultPrefix   = "pai-"
	maxPodNameRunes = 191
)

// Offering is one operator-approved hardware configuration.
type Offering struct {
	ID           string
	GPUTypeID    string // RunPod GPU type, e.g. "NVIDIA A40"
	GPUCount     int
	VRAMPerGPUGB int
	CPUCores     int
	MemoryGB     int
	// HourlyCostMicros is the expected price in USD micros; 0 means unknown.
	HourlyCostMicros int64
}

// Config configures the adapter.
type Config struct {
	APIKey          string
	BaseURL         string
	CloudType       string // SECURE or COMMUNITY
	NamePrefix      string
	ContainerDiskGB int
	VolumeGB        int
	Offerings       []Offering
	HTTPClient      *http.Client
	// Retries is the number of extra attempts for retryable failures.
	Retries int
	// Backoff is the initial delay between retries; it doubles each time.
	Backoff time.Duration
}

// Provider is the RunPod compute adapter.
type Provider struct {
	cfg       Config
	offerings map[compute.CapabilityID]Offering
}

var _ ports.ComputeProvider = (*Provider)(nil)

// New validates the configuration and returns a provider.
func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("runpod: api key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.CloudType == "" {
		cfg.CloudType = "SECURE"
	}
	if cfg.NamePrefix == "" {
		cfg.NamePrefix = defaultPrefix
	}
	if cfg.ContainerDiskGB <= 0 {
		cfg.ContainerDiskGB = 50
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 500 * time.Millisecond
	}
	if len(cfg.Offerings) == 0 {
		return nil, errors.New("runpod: at least one offering is required")
	}
	p := &Provider{cfg: cfg, offerings: map[compute.CapabilityID]Offering{}}
	for _, o := range cfg.Offerings {
		if o.ID == "" || o.GPUTypeID == "" || o.GPUCount < 1 || o.VRAMPerGPUGB < 1 {
			return nil, fmt.Errorf("runpod: offering %q needs id, gpu_type_id, gpu_count >= 1 and vram_per_gpu_gb >= 1", o.ID)
		}
		if _, dup := p.offerings[compute.CapabilityID(o.ID)]; dup {
			return nil, fmt.Errorf("runpod: duplicate offering %q", o.ID)
		}
		p.offerings[compute.CapabilityID(o.ID)] = o
	}
	return p, nil
}

// Capabilities returns the configured offerings.
func (p *Provider) Capabilities(context.Context) ([]compute.Capability, error) {
	out := make([]compute.Capability, 0, len(p.offerings))
	for _, o := range p.offerings {
		out = append(out, offeringCapability(o))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func offeringCapability(o Offering) compute.Capability {
	c := compute.Capability{
		ID: compute.CapabilityID(o.ID), GPUModel: o.GPUTypeID, GPUCount: o.GPUCount,
		VRAMPerGPUGB: o.VRAMPerGPUGB, CPUCores: o.CPUCores, MemoryGB: o.MemoryGB,
	}
	if o.HourlyCostMicros > 0 {
		c.HourlyCost = &compute.Money{Currency: "USD", Micros: o.HourlyCostMicros}
	}
	return c
}

// ---- RunPod wire types (never exported) ----

type createPodRequest struct {
	Name              string            `json:"name"`
	ImageName         string            `json:"imageName"`
	GPUTypeIDs        []string          `json:"gpuTypeIds"`
	GPUCount          int               `json:"gpuCount"`
	CloudType         string            `json:"cloudType"`
	ContainerDiskInGb int               `json:"containerDiskInGb"`
	VolumeInGb        int               `json:"volumeInGb"`
	Ports             []string          `json:"ports"`
	Env               map[string]string `json:"env"`
	DockerStartCmd    []string          `json:"dockerStartCmd,omitempty"`
}

type pod struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	DesiredStatus string            `json:"desiredStatus"`
	CostPerHr     json.Number       `json:"costPerHr"`
	GPUCount      int               `json:"gpuCount"`
	Env           map[string]string `json:"env"`
	Ports         []string          `json:"ports"`
	CreatedAt     string            `json:"createdAt"`
	GPU           *struct {
		ID string `json:"id"`
	} `json:"gpu"`
}

// ---- operations ----

func (p *Provider) podName(key string) string {
	name := p.cfg.NamePrefix + key
	if r := []rune(name); len(r) > maxPodNameRunes {
		name = string(r[:maxPodNameRunes])
	}
	return name
}

// Provision creates a pod for the spec, or returns the pod already created for
// the same idempotency key.
func (p *Provider) Provision(ctx context.Context, spec compute.ProvisionSpec) (compute.Instance, error) {
	if err := spec.Validate(); err != nil {
		return compute.Instance{}, err
	}
	off, ok := p.offerings[spec.CapabilityID]
	if !ok {
		return compute.Instance{}, fmt.Errorf("%w: unknown capability %q", compute.ErrInvalidSpec, spec.CapabilityID)
	}
	name := p.podName(spec.IdempotencyKey)

	if existing, found, err := p.findByName(ctx, name); err != nil {
		return compute.Instance{}, err
	} else if found {
		return p.toInstance(existing), nil
	}

	env := map[string]string{idempotencyEnv: spec.IdempotencyKey}
	for k, v := range spec.Launch.Env {
		env[k] = v
	}
	for k, v := range spec.Labels {
		env[labelEnvPrefix+labelEnvKey(k)] = v
	}
	ports := make([]string, 0, len(spec.Launch.Ports))
	for _, port := range spec.Launch.Ports {
		ports = append(ports, fmt.Sprintf("%d/http", port))
	}
	storage := p.cfg.ContainerDiskGB
	if spec.StorageGB > 0 {
		storage = spec.StorageGB
	}
	req := createPodRequest{
		Name: name, ImageName: spec.Launch.Image, GPUTypeIDs: []string{off.GPUTypeID}, GPUCount: off.GPUCount,
		CloudType: p.cfg.CloudType, ContainerDiskInGb: storage, VolumeInGb: p.cfg.VolumeGB,
		Ports: ports, Env: env, DockerStartCmd: spec.Launch.Args,
	}

	var created pod
	// Creation is not retried blindly: after a failure we look the pod up by
	// name, because the request may have succeeded on RunPod's side.
	err := p.do(ctx, http.MethodPost, "/pods", req, &created, false)
	if err != nil {
		if existing, found, lookupErr := p.findByName(ctx, name); lookupErr == nil && found {
			return p.toInstance(existing), nil
		}
		return compute.Instance{}, fmt.Errorf("runpod: create pod: %w", err)
	}
	inst := p.toInstance(created)
	if inst.Capability.HourlyCost == nil {
		inst.Capability = mergeCost(inst.Capability, off)
	}
	return inst, nil
}

// GetInstance returns the pod's current state.
func (p *Provider) GetInstance(ctx context.Context, id compute.InstanceID) (compute.Instance, error) {
	var got pod
	if err := p.do(ctx, http.MethodGet, "/pods/"+url.PathEscape(string(id)), nil, &got, true); err != nil {
		return compute.Instance{}, fmt.Errorf("runpod: get pod %s: %w", id, err)
	}
	return p.toInstance(got), nil
}

// ListInstances returns pods created by this platform (by name prefix).
func (p *Provider) ListInstances(ctx context.Context) ([]compute.Instance, error) {
	pods, err := p.listPods(ctx)
	if err != nil {
		return nil, err
	}
	out := []compute.Instance{}
	for _, pd := range pods {
		if strings.HasPrefix(pd.Name, p.cfg.NamePrefix) {
			out = append(out, p.toInstance(pd))
		}
	}
	return out, nil
}

// Terminate deletes the pod. Unknown pods count as already terminated.
func (p *Provider) Terminate(ctx context.Context, id compute.InstanceID) error {
	err := p.do(ctx, http.MethodDelete, "/pods/"+url.PathEscape(string(id)), nil, nil, true)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return fmt.Errorf("runpod: terminate pod %s: %w", id, err)
	}
	return nil
}

func (p *Provider) listPods(ctx context.Context) ([]pod, error) {
	var pods []pod
	if err := p.do(ctx, http.MethodGet, "/pods", nil, &pods, true); err != nil {
		return nil, fmt.Errorf("runpod: list pods: %w", err)
	}
	return pods, nil
}

func (p *Provider) findByName(ctx context.Context, name string) (pod, bool, error) {
	pods, err := p.listPods(ctx)
	if err != nil {
		return pod{}, false, err
	}
	for _, pd := range pods {
		if pd.Name == name && mapStatus(pd.DesiredStatus) != compute.InstanceTerminated {
			return pd, true, nil
		}
	}
	return pod{}, false, nil
}

// ---- mapping ----

func mapStatus(s string) compute.InstanceStatus {
	switch strings.ToUpper(s) {
	case "RUNNING":
		return compute.InstanceRunning
	case "CREATED", "RESTARTING", "":
		return compute.InstanceProvisioning
	case "EXITED", "DEAD":
		// The container stopped; the pod no longer serves and must be cleaned up.
		return compute.InstanceFailed
	case "TERMINATED":
		return compute.InstanceTerminated
	default:
		return compute.InstanceUnknown
	}
}

func (p *Provider) toInstance(pd pod) compute.Instance {
	inst := compute.Instance{
		ID:          compute.InstanceID(pd.ID),
		ProviderRef: pd.ID,
		Status:      mapStatus(pd.DesiredStatus),
		Labels:      map[string]string{},
	}
	if t, err := time.Parse(time.RFC3339, pd.CreatedAt); err == nil {
		inst.CreatedAt = t
	}
	for k, v := range pd.Env {
		if rest, ok := strings.CutPrefix(k, labelEnvPrefix); ok {
			inst.Labels[strings.ToLower(rest)] = v
		}
	}
	if key := pd.Env[idempotencyEnv]; key != "" {
		inst.Labels["idempotency_key"] = key
	} else if key, ok := strings.CutPrefix(pd.Name, p.cfg.NamePrefix); ok {
		inst.Labels["idempotency_key"] = key
	}

	inst.Capability = compute.Capability{GPUCount: pd.GPUCount}
	if pd.GPU != nil {
		inst.Capability.GPUModel = pd.GPU.ID
	}
	for _, o := range p.offerings {
		if o.GPUTypeID == inst.Capability.GPUModel && o.GPUCount == pd.GPUCount {
			inst.Capability = offeringCapability(o)
			break
		}
	}
	// Prefer the price RunPod reports for this pod over the configured one.
	if f, err := pd.CostPerHr.Float64(); err == nil && f > 0 {
		inst.Capability.HourlyCost = &compute.Money{Currency: "USD", Micros: int64(f*1_000_000 + 0.5)}
	}

	if inst.Status == compute.InstanceRunning && len(pd.Ports) > 0 {
		port, _, _ := strings.Cut(pd.Ports[0], "/")
		inst.Endpoint = &compute.Endpoint{URL: fmt.Sprintf("https://%s-%s.proxy.runpod.net", pd.ID, port)}
	}
	return inst
}

func mergeCost(c compute.Capability, o Offering) compute.Capability {
	if o.HourlyCostMicros > 0 {
		c.HourlyCost = &compute.Money{Currency: "USD", Micros: o.HourlyCostMicros}
	}
	if c.GPUModel == "" {
		c = offeringCapability(o)
	}
	return c
}

func labelEnvKey(k string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, k)
}

// ---- HTTP ----

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("runpod api status %d: %s", e.status, e.msg) }

func (e *apiError) Unwrap() error {
	switch {
	case e.status == http.StatusNotFound:
		return ports.ErrNotFound
	case isCapacityMessage(e.msg):
		return ports.ErrCapacityUnavailable
	}
	return nil
}

func isCapacityMessage(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no longer any instances available") ||
		strings.Contains(m, "not enough free gpus") ||
		strings.Contains(m, "no available") ||
		strings.Contains(m, "insufficient capacity")
}

func retryable(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status == http.StatusTooManyRequests || ae.status >= 500
	}
	// Transport errors are retryable unless the caller gave up.
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (p *Provider) do(ctx context.Context, method, path string, in, out any, idempotent bool) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	attempts := 1
	if idempotent {
		attempts += p.cfg.Retries
	}
	delay := p.cfg.Backoff
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		lastErr = p.once(ctx, method, path, body, out)
		if lastErr == nil || !retryable(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

func (p *Provider) once(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, scrubURLError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return &apiError{status: resp.StatusCode, msg: errorMessage(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func errorMessage(data []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) == nil {
		if e.Error != "" {
			return e.Error
		}
		if e.Message != "" {
			return e.Message
		}
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
