package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/ports"
)

// fakeAPI is a minimal in-memory RunPod REST API.
type fakeAPI struct {
	mu        sync.Mutex
	pods      map[string]pod
	next      int
	creates   int
	failNext  int // number of upcoming requests to answer with 503
	createErr string
	lastReq   createPodRequest
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer secret-key" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.failNext > 0 {
		f.failNext--
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/pods/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/pods":
		out := []pod{}
		for _, p := range f.pods {
			out = append(out, p)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && r.URL.Path == "/pods":
		if f.createErr != "" {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": f.createErr})
			return
		}
		var req createPodRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastReq = req
		f.creates++
		f.next++
		p := pod{ID: "pod" + string(rune('0'+f.next)), Name: req.Name, DesiredStatus: "RUNNING",
			CostPerHr: "0.39", GPUCount: req.GPUCount, Env: req.Env, Ports: req.Ports,
			CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		p.GPU = &struct {
			ID string `json:"id"`
		}{ID: req.GPUTypeIDs[0]}
		f.pods[p.ID] = p
		_ = json.NewEncoder(w).Encode(p)
	case r.Method == http.MethodGet:
		p, ok := f.pods[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(p)
	case r.Method == http.MethodDelete:
		if _, ok := f.pods[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.pods, id)
	}
}

func newTestProvider(t *testing.T) (*Provider, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{pods: map[string]pod{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	p, err := New(Config{
		APIKey: "secret-key", BaseURL: srv.URL, Retries: 2, Backoff: time.Millisecond,
		Offerings: []Offering{{ID: "a40", GPUTypeID: "NVIDIA A40", GPUCount: 1, VRAMPerGPUGB: 48, HourlyCostMicros: 400_000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, api
}

func spec(key string) compute.ProvisionSpec {
	return compute.ProvisionSpec{
		CapabilityID: "a40", IdempotencyKey: key,
		Launch: compute.LaunchSpec{Image: "vllm/vllm-openai:latest", Args: []string{"--model", "x"}, Env: map[string]string{"VLLM_API_KEY": "k"}, Ports: []int{8000}},
		Labels: map[string]string{"deployment_id": "dep-1"},
	}
}

func TestProvisionIsIdempotentAndMapsPod(t *testing.T) {
	p, api := newTestProvider(t)
	ctx := context.Background()

	inst, err := p.Provision(ctx, spec("key-1"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Provision(ctx, spec("key-1"))
	if err != nil {
		t.Fatal(err)
	}
	if api.creates != 1 || again.ID != inst.ID {
		t.Fatalf("creates = %d, ids %s/%s", api.creates, inst.ID, again.ID)
	}
	if inst.Status != compute.InstanceRunning || inst.Endpoint == nil || inst.Endpoint.URL != "https://"+string(inst.ID)+"-8000.proxy.runpod.net" {
		t.Fatalf("instance = %+v", inst)
	}
	if inst.Labels["deployment_id"] != "dep-1" || inst.Labels["idempotency_key"] != "key-1" {
		t.Fatalf("labels = %v", inst.Labels)
	}
	if inst.Capability.HourlyCost == nil || inst.Capability.HourlyCost.Micros != 390_000 {
		t.Fatalf("cost = %+v", inst.Capability.HourlyCost)
	}
	if api.lastReq.GPUTypeIDs[0] != "NVIDIA A40" || api.lastReq.Ports[0] != "8000/http" || api.lastReq.DockerStartCmd[0] != "--model" {
		t.Fatalf("request = %+v", api.lastReq)
	}

	list, err := p.ListInstances(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
}

func TestTerminateIsIdempotent(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx := context.Background()
	inst, _ := p.Provision(ctx, spec("k"))
	if err := p.Terminate(ctx, inst.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Terminate(ctx, inst.ID); err != nil {
		t.Fatalf("second terminate: %v", err)
	}
	if _, err := p.GetInstance(ctx, inst.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	p, api := newTestProvider(t)
	api.failNext = 2
	if _, err := p.ListInstances(context.Background()); err != nil {
		t.Fatalf("should succeed after retries: %v", err)
	}
}

func TestCapacityErrorIsMapped(t *testing.T) {
	p, api := newTestProvider(t)
	api.createErr = "There are no longer any instances available with the requested specifications."
	_, err := p.Provision(context.Background(), spec("k"))
	if !errors.Is(err, ports.ErrCapacityUnavailable) {
		t.Fatalf("want ErrCapacityUnavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatal("error leaks the api key")
	}
}

func TestCapabilitiesAndValidation(t *testing.T) {
	p, _ := newTestProvider(t)
	caps, _ := p.Capabilities(context.Background())
	if len(caps) != 1 || caps[0].TotalVRAMGB() != 48 || caps[0].HourlyCost.Micros != 400_000 {
		t.Fatalf("caps = %+v", caps)
	}
	bad := spec("k")
	bad.CapabilityID = "nope"
	if _, err := p.Provision(context.Background(), bad); !errors.Is(err, compute.ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
	if _, err := New(Config{APIKey: "x"}); err == nil {
		t.Fatal("want error without offerings")
	}
}

func TestStatusMapping(t *testing.T) {
	for in, want := range map[string]compute.InstanceStatus{
		"RUNNING": compute.InstanceRunning, "EXITED": compute.InstanceFailed,
		"TERMINATED": compute.InstanceTerminated, "CREATED": compute.InstanceProvisioning, "WEIRD": compute.InstanceUnknown,
	} {
		if got := mapStatus(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
