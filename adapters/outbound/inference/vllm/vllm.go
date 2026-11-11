// Package vllm implements ports.InferenceRuntime for vLLM's OpenAI-compatible
// server. vLLM wire formats never leave this package.
//
// Bootstrapping: Prepare turns the model's opaque runtime settings into a
// compute.LaunchSpec (container image + vLLM arguments). The compute adapter
// launches it. Every deployment gets its own API key, derived with
// HMAC-SHA256 from a server secret and the deployment ID, so no runtime
// credential is ever stored and one leaked key exposes one deployment only.
package vllm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/core/ports"
)

// Defaults for the launched container.
const (
	DefaultImage = "vllm/vllm-openai:v0.33.0"
	DefaultPort  = 8000
)

// Config configures the runtime adapter.
type Config struct {
	// KeySecret derives per-deployment API keys. Required.
	KeySecret string
	// HFToken is passed to the container to download gated models. Optional.
	HFToken      string
	DefaultImage string
	// AllowedImages lists the container images a model may name; DefaultImage
	// is always allowed. Anything else is refused, so a management token cannot
	// run arbitrary code next to the runtime's credentials.
	AllowedImages []string
	// AllowTrustRemoteCode permits trust_remote_code and --trust-remote-code,
	// which run Python code from the model repository.
	AllowTrustRemoteCode bool
	HTTPClient           *http.Client
	// HealthTimeout bounds one health probe.
	HealthTimeout time.Duration
}

// Runtime is the vLLM adapter.
type Runtime struct {
	cfg    Config
	client *http.Client
}

var _ ports.InferenceRuntime = (*Runtime)(nil)

// New returns a runtime adapter.
func New(cfg Config) (*Runtime, error) {
	if len(cfg.KeySecret) < 24 {
		return nil, errors.New("vllm: key secret must be at least 24 characters")
	}
	if cfg.DefaultImage == "" {
		cfg.DefaultImage = DefaultImage
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = 10 * time.Second
	}
	client := cfg.HTTPClient
	if client == nil {
		// No overall timeout: streamed generations can be long. Request
		// contexts bound every call instead.
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   16,
		}}
	}
	return &Runtime{cfg: cfg, client: client}, nil
}

// apiKey derives the deployment's runtime credential.
func (r *Runtime) apiKey(deploymentID string) string {
	mac := hmac.New(sha256.New, []byte(r.cfg.KeySecret))
	mac.Write([]byte("vllm-api-key:" + deploymentID))
	return hex.EncodeToString(mac.Sum(nil))
}

// settings is the parsed form of the opaque runtime map.
type settings struct {
	Image                string
	Port                 int
	MaxModelLen          int
	DType                string
	GPUMemoryUtilization float64
	TensorParallelSize   int
	Quantization         string
	TrustRemoteCode      bool
	EnforceEager         bool
	ExtraArgs            []string
}

// perModelOnly are settings a deployment may not override: they choose what
// code runs in the container.
var perModelOnly = []string{"image", "trust_remote_code", "extra_args"}

func (r *Runtime) parseSettings(m model.Model, overrides map[string]any) (settings, error) {
	var errs []error
	for _, k := range perModelOnly {
		if _, ok := overrides[k]; ok {
			errs = append(errs, fmt.Errorf("runtime.%s cannot be overridden per deployment", k))
		}
	}
	merged := map[string]any{}
	for k, v := range m.Runtime {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	s := settings{Image: r.cfg.DefaultImage, Port: DefaultPort}
	if m.Requirements.MinGPUCount > 1 {
		s.TensorParallelSize = m.Requirements.MinGPUCount
	}
	for k, v := range merged {
		var err error
		switch k {
		case "image":
			s.Image, err = asString(v)
		case "port":
			s.Port, err = asInt(v)
		case "max_model_len", "context_length":
			var n int
			if n, err = asInt(v); err == nil && (s.MaxModelLen == 0 || k == "max_model_len") {
				s.MaxModelLen = n
			}
		case "dtype":
			s.DType, err = asString(v)
		case "gpu_memory_utilization":
			s.GPUMemoryUtilization, err = asFloat(v)
		case "tensor_parallel_size":
			s.TensorParallelSize, err = asInt(v)
		case "quantization":
			s.Quantization, err = asString(v)
		case "trust_remote_code":
			s.TrustRemoteCode, err = asBool(v)
		case "enforce_eager":
			s.EnforceEager, err = asBool(v)
		case "extra_args":
			s.ExtraArgs, err = asStrings(v)
		default:
			err = errors.New("unknown setting")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("runtime.%s: %w", k, err))
		}
	}
	if s.Port < 1 || s.Port > 65535 {
		errs = append(errs, errors.New("runtime.port must be within 1..65535"))
	}
	if s.GPUMemoryUtilization < 0 || s.GPUMemoryUtilization > 1 {
		errs = append(errs, errors.New("runtime.gpu_memory_utilization must be within (0, 1]"))
	}
	if s.Image != r.cfg.DefaultImage && !slices.Contains(r.cfg.AllowedImages, s.Image) {
		errs = append(errs, fmt.Errorf("runtime.image %q is not in runtime.allowed_images", s.Image))
	}
	if !r.cfg.AllowTrustRemoteCode && (s.TrustRemoteCode ||
		slices.ContainsFunc(s.ExtraArgs, func(a string) bool { return strings.HasPrefix(a, "--trust-remote-code") })) {
		errs = append(errs, errors.New("trust_remote_code needs runtime.allow_trust_remote_code"))
	}
	return s, errors.Join(errs...)
}

// Prepare builds the launch spec. It performs no network I/O.
func (r *Runtime) Prepare(_ context.Context, spec ports.PrepareSpec) (compute.LaunchSpec, error) {
	if spec.DeploymentID == "" {
		return compute.LaunchSpec{}, errors.New("vllm: deployment id is required")
	}
	s, err := r.parseSettings(spec.Model, spec.Overrides)
	if err != nil {
		return compute.LaunchSpec{}, fmt.Errorf("vllm: %w", err)
	}
	args := []string{
		"--model", spec.Model.Artifact,
		"--served-model-name", string(spec.Model.ID),
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(s.Port),
	}
	if s.MaxModelLen > 0 {
		args = append(args, "--max-model-len", strconv.Itoa(s.MaxModelLen))
	}
	if s.DType != "" {
		args = append(args, "--dtype", s.DType)
	}
	if s.GPUMemoryUtilization > 0 {
		args = append(args, "--gpu-memory-utilization", strconv.FormatFloat(s.GPUMemoryUtilization, 'f', -1, 64))
	}
	if s.TensorParallelSize > 1 {
		args = append(args, "--tensor-parallel-size", strconv.Itoa(s.TensorParallelSize))
	}
	if s.Quantization != "" {
		args = append(args, "--quantization", s.Quantization)
	}
	if s.TrustRemoteCode {
		args = append(args, "--trust-remote-code")
	}
	if s.EnforceEager {
		args = append(args, "--enforce-eager")
	}
	args = append(args, s.ExtraArgs...)

	// The key goes in the environment (vLLM reads VLLM_API_KEY), never in args,
	// which providers tend to display.
	env := map[string]string{"VLLM_API_KEY": r.apiKey(spec.DeploymentID)}
	if r.cfg.HFToken != "" {
		env["HF_TOKEN"] = r.cfg.HFToken
	}
	return compute.LaunchSpec{Image: s.Image, Args: args, Env: env, Ports: []int{s.Port}}, nil
}

// ref is the opaque runtime reference: enough to rebuild credentials and the
// served model name without storing secrets.
type ref struct {
	DeploymentID string `json:"d"`
	ServedModel  string `json:"m"`
}

func encodeRef(r ref) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func decodeRef(s string) (ref, error) {
	var r ref
	if err := json.Unmarshal([]byte(s), &r); err != nil || r.DeploymentID == "" {
		return ref{}, errors.New("vllm: invalid runtime reference")
	}
	return r, nil
}

// Initialize binds the runtime to the instance. vLLM starts with the
// container, so there is nothing to call; readiness is observed via Health.
func (r *Runtime) Initialize(_ context.Context, spec ports.InitSpec) (string, error) {
	if spec.DeploymentID == "" {
		return "", errors.New("vllm: deployment id is required")
	}
	if spec.Instance.Endpoint == nil || spec.Instance.Endpoint.URL == "" {
		return "", errors.New("vllm: instance has no endpoint")
	}
	return encodeRef(ref{DeploymentID: spec.DeploymentID, ServedModel: string(spec.Model.ID)}), nil
}

// Health reports whether the server answers and serves the model. A server
// that is still starting is reported as not ready rather than as an error.
func (r *Runtime) Health(ctx context.Context, target ports.RuntimeTarget) (ports.RuntimeHealth, error) {
	rf, err := decodeRef(target.Ref)
	if err != nil {
		return ports.RuntimeHealth{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.HealthTimeout)
	defer cancel()

	resp, err := r.send(ctx, http.MethodGet, target, rf, "/health", nil)
	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return ports.RuntimeHealth{}, ctx.Err()
		}
		return ports.RuntimeHealth{Detail: "unreachable: " + err.Error()}, nil
	}
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		return ports.RuntimeHealth{Detail: fmt.Sprintf("health status %d", resp.StatusCode)}, nil
	}
	names, err := r.models(ctx, target, rf)
	if err != nil {
		return ports.RuntimeHealth{Ready: true, Detail: "model list unavailable: " + err.Error()}, nil
	}
	for _, n := range names {
		if n == rf.ServedModel {
			return ports.RuntimeHealth{Ready: true, ModelLoaded: true}, nil
		}
	}
	return ports.RuntimeHealth{Ready: true, Detail: "model not listed"}, nil
}

// Models lists served model names.
func (r *Runtime) Models(ctx context.Context, target ports.RuntimeTarget) ([]string, error) {
	rf, err := decodeRef(target.Ref)
	if err != nil {
		return nil, err
	}
	return r.models(ctx, target, rf)
}

func (r *Runtime) models(ctx context.Context, target ports.RuntimeTarget, rf ref) ([]string, error) {
	resp, err := r.send(ctx, http.MethodGet, target, rf, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("vllm: decode models: %w", err)
	}
	names := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		names = append(names, d.ID)
	}
	return names, nil
}

// ---- chat ----

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	MaxTokens     *int          `json:"max_tokens,omitempty"`
	Stop          []string      `json:"stop,omitempty"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type usageWire struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func toWire(rf ref, req inference.Request, stream bool) chatRequest {
	msgs := make([]chatMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = chatMessage{Role: string(m.Role), Content: m.Content}
	}
	w := chatRequest{
		Model: rf.ServedModel, Messages: msgs, Temperature: req.Params.Temperature,
		TopP: req.Params.TopP, MaxTokens: req.Params.MaxTokens, Stop: req.Params.Stop, Stream: stream,
	}
	if stream {
		w.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	return w
}

func finishReason(s string) inference.FinishReason {
	if s == "length" {
		return inference.FinishLength
	}
	if s == "" {
		return ""
	}
	return inference.FinishStop
}

// Chat runs a non-streamed completion.
func (r *Runtime) Chat(ctx context.Context, target ports.RuntimeTarget, req inference.Request) (inference.Response, error) {
	rf, err := decodeRef(target.Ref)
	if err != nil {
		return inference.Response{}, err
	}
	resp, err := r.send(ctx, http.MethodPost, target, rf, "/v1/chat/completions", toWire(rf, req, false))
	if err != nil {
		return inference.Response{}, fmt.Errorf("%w: %v", ports.ErrRuntimeUnavailable, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return inference.Response{}, statusError(resp)
	}
	var out struct {
		Choices []struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage usageWire `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return inference.Response{}, fmt.Errorf("vllm: decode completion: %w", err)
	}
	if len(out.Choices) == 0 {
		return inference.Response{}, errors.New("vllm: completion has no choices")
	}
	c := out.Choices[0]
	return inference.Response{
		ModelID:      req.ModelID,
		Message:      inference.Message{Role: inference.RoleAssistant, Content: c.Message.Content},
		FinishReason: finishReason(c.FinishReason),
		Usage:        inference.TokenUsage{Input: out.Usage.PromptTokens, Output: out.Usage.CompletionTokens},
	}, nil
}

// ChatStream runs a streamed completion, parsing server-sent events as they
// arrive. The response body is closed when the stream ends or ctx is
// cancelled.
func (r *Runtime) ChatStream(ctx context.Context, target ports.RuntimeTarget, req inference.Request) (<-chan inference.Chunk, error) {
	rf, err := decodeRef(target.Ref)
	if err != nil {
		return nil, err
	}
	resp, err := r.send(ctx, http.MethodPost, target, rf, "/v1/chat/completions", toWire(rf, req, true))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ports.ErrRuntimeUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer drain(resp)
		return nil, statusError(resp)
	}

	ch := make(chan inference.Chunk)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		send := func(c inference.Chunk) bool {
			select {
			case ch <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		rd := bufio.NewReaderSize(resp.Body, 64<<10)
		for {
			line, err := rd.ReadString('\n')
			if line = strings.TrimRight(line, "\r\n"); strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					return
				}
				var ev struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
						FinishReason *string `json:"finish_reason"`
					} `json:"choices"`
					Usage *usageWire `json:"usage"`
				}
				if jerr := json.Unmarshal([]byte(data), &ev); jerr != nil {
					send(inference.Chunk{Err: fmt.Errorf("vllm: decode stream event: %w", jerr)})
					return
				}
				var c inference.Chunk
				if len(ev.Choices) > 0 {
					c.Delta = ev.Choices[0].Delta.Content
					if fr := ev.Choices[0].FinishReason; fr != nil {
						c.FinishReason = finishReason(*fr)
					}
				}
				if ev.Usage != nil {
					c.Usage = &inference.TokenUsage{Input: ev.Usage.PromptTokens, Output: ev.Usage.CompletionTokens}
				}
				// Role-only and empty events carry nothing for the caller. Usage
				// arrives in its own final event and is passed on as a chunk.
				if c.Delta == "" && c.FinishReason == "" && c.Usage == nil {
					continue
				}
				if !send(c) {
					return
				}
			}
			if err != nil {
				if ctx.Err() == nil {
					if errors.Is(err, io.EOF) {
						send(inference.Chunk{Err: errors.New("vllm: stream ended without [DONE]")})
					} else {
						send(inference.Chunk{Err: fmt.Errorf("vllm: read stream: %w", err)})
					}
				}
				return
			}
		}
	}()
	return ch, nil
}

// Release is a no-op: vLLM's resources go away with the instance.
func (r *Runtime) Release(context.Context, ports.RuntimeTarget) error { return nil }

// ---- HTTP helpers ----

func (r *Runtime) send(ctx context.Context, method string, target ports.RuntimeTarget, rf ref, path string, body any) (*http.Response, error) {
	if target.Endpoint.URL == "" {
		return nil, errors.New("no endpoint")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(target.Endpoint.URL, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range target.Endpoint.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey(rf.DeploymentID))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, ue.Err // drop the URL from the message
		}
		return nil, err
	}
	return resp, nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func statusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	msg := strings.TrimSpace(string(b))
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Error.Message != "" {
			msg = e.Error.Message
		} else if e.Message != "" {
			msg = e.Message
		}
	}
	err := fmt.Errorf("vllm: status %d: %s", resp.StatusCode, msg)
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: %v", inference.ErrInvalidRequest, err)
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %v", ports.ErrRuntimeUnavailable, err)
	}
	return err
}

// ---- opaque setting conversion ----

func asString(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", errors.New("must be a string")
	}
	return s, nil
}

func asFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	}
	return 0, errors.New("must be a number")
}

func asInt(v any) (int, error) {
	f, err := asFloat(v)
	if err != nil {
		return 0, err
	}
	if f != float64(int(f)) {
		return 0, errors.New("must be an integer")
	}
	return int(f), nil
}

func asBool(v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, errors.New("must be a boolean")
	}
	return b, nil
}

func asStrings(v any) ([]string, error) {
	switch xs := v.(type) {
	case []string:
		return xs, nil
	case []any:
		out := make([]string, len(xs))
		for i, x := range xs {
			s, ok := x.(string)
			if !ok {
				return nil, errors.New("must be a list of strings")
			}
			out[i] = s
		}
		return out, nil
	}
	return nil, errors.New("must be a list of strings")
}
