package vllm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/core/ports"
)

const secret = "0123456789abcdef0123456789abcdef"

func testModel() model.Model {
	return model.Model{ID: "m1", Name: "M", Artifact: "org/m1",
		Requirements: model.Requirements{MinVRAMGB: 24, MinGPUCount: 2},
		Features:     []model.Feature{model.FeatureChat}, Status: model.StatusActive,
		Runtime: map[string]any{"max_model_len": float64(8192), "dtype": "auto", "extra_args": []any{"--seed", "1"}}}
}

func newRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(Config{KeySecret: secret, HFToken: "hf"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPrepareBuildsLaunchSpec(t *testing.T) {
	r := newRuntime(t)
	ls, err := r.Prepare(context.Background(), ports.PrepareSpec{DeploymentID: "d1", Model: testModel(),
		Overrides: map[string]any{"gpu_memory_utilization": 0.85}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(ls.Args, " ")
	for _, want := range []string{"--model org/m1", "--served-model-name m1", "--max-model-len 8192",
		"--dtype auto", "--tensor-parallel-size 2", "--gpu-memory-utilization 0.85", "--seed 1"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if ls.Image != DefaultImage || !slices.Equal(ls.Ports, []int{8000}) {
		t.Fatalf("spec = %+v", ls)
	}
	if ls.Env["VLLM_API_KEY"] != r.apiKey("d1") || ls.Env["HF_TOKEN"] != "hf" || strings.Contains(args, r.apiKey("d1")) {
		t.Fatal("key must be in env only")
	}
	if r.apiKey("d1") == r.apiKey("d2") {
		t.Fatal("keys must be per deployment")
	}

	bad := testModel()
	bad.Runtime = map[string]any{"bogus": 1, "port": "x"}
	if _, err := r.Prepare(context.Background(), ports.PrepareSpec{DeploymentID: "d1", Model: bad}); err == nil {
		t.Fatal("want error for unknown/invalid settings")
	}
}

// fakeVLLM serves the subset of vLLM's API the adapter uses.
func fakeVLLM(t *testing.T, key string, loaded bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+key {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("GET /v1/models", auth(func(w http.ResponseWriter, _ *http.Request) {
		data := []map[string]string{}
		if loaded {
			data = append(data, map[string]string{"id": "m1"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	mux.HandleFunc("POST /v1/chat/completions", auth(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "m1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"bad model"}`))
			return
		}
		if !req.Stream {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, ev := range []string{
			`{"choices":[{"delta":{"role":"assistant"}}]}`,
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
			fl.Flush()
		}
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func target(t *testing.T, r *Runtime, url string) ports.RuntimeTarget {
	ref, err := r.Initialize(context.Background(), ports.InitSpec{DeploymentID: "d1", Model: testModel(),
		Instance: compute.Instance{Endpoint: &compute.Endpoint{URL: url}}})
	if err != nil {
		t.Fatal(err)
	}
	return ports.RuntimeTarget{Endpoint: compute.Endpoint{URL: url}, Ref: ref}
}

func TestHealth(t *testing.T) {
	r := newRuntime(t)
	srv := fakeVLLM(t, r.apiKey("d1"), true)
	h, err := r.Health(context.Background(), target(t, r, srv.URL))
	if err != nil || !h.Ready || !h.ModelLoaded {
		t.Fatalf("health = %+v, %v", h, err)
	}

	notLoaded := fakeVLLM(t, r.apiKey("d1"), false)
	if h, _ := r.Health(context.Background(), target(t, r, notLoaded.URL)); h.ModelLoaded {
		t.Fatal("model must not be reported loaded")
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()
	if h, err := r.Health(context.Background(), target(t, r, down.URL)); err != nil || h.Ready {
		t.Fatalf("starting server must be not-ready without error: %+v %v", h, err)
	}
}

func TestChat(t *testing.T) {
	r := newRuntime(t)
	srv := fakeVLLM(t, r.apiKey("d1"), true)
	resp, err := r.Chat(context.Background(), target(t, r, srv.URL), inference.Request{ModelID: "m1",
		Messages: []inference.Message{{Role: inference.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hello" || resp.Usage.Total() != 4 || resp.FinishReason != inference.FinishStop {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChatStream(t *testing.T) {
	r := newRuntime(t)
	srv := fakeVLLM(t, r.apiKey("d1"), true)
	ch, err := r.ChatStream(context.Background(), target(t, r, srv.URL), inference.Request{ModelID: "m1",
		Messages: []inference.Message{{Role: inference.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var usage *inference.TokenUsage
	var finish inference.FinishReason
	for c := range ch {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		text.WriteString(c.Delta)
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
	if text.String() != "Hello" || usage == nil || usage.Output != 2 || finish != inference.FinishStop {
		t.Fatalf("text=%q usage=%v finish=%q", text.String(), usage, finish)
	}
}

func TestChatStreamCancellationClosesChannel(t *testing.T) {
	r := newRuntime(t)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-req.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := r.ChatStream(ctx, target(t, r, srv.URL), inference.Request{ModelID: "m1",
		Messages: []inference.Message{{Role: inference.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("channel not closed after cancellation")
	}
}

func TestChatErrorsAreMapped(t *testing.T) {
	r := newRuntime(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := r.Chat(context.Background(), target(t, r, srv.URL), inference.Request{ModelID: "m1",
		Messages: []inference.Message{{Role: inference.RoleUser, Content: "hi"}}})
	if !errors.Is(err, ports.ErrRuntimeUnavailable) {
		t.Fatalf("want ErrRuntimeUnavailable, got %v", err)
	}
}
