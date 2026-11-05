// Package apiclient is a typed client for the platform's REST API, shared by
// the CLI and the Telegram bot so that neither needs database or provider
// credentials (ARCHITECTURE.md §6a, A3). It depends only on the standard
// library and internal/apitypes.
package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"personal-ai/internal/apitypes"
)

// Error is a non-2xx API response.
type Error struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is set when the server suggests retrying (e.g. a model is
	// starting).
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s, HTTP %d)", e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

// IsCode reports whether err is an API error with the given code.
func IsCode(err error, code string) bool {
	var ae *Error
	return errors.As(err, &ae) && ae.Code == code
}

// Client talks to one platform server.
type Client struct {
	base            string
	managementToken string
	inferenceToken  string
	http            *http.Client
	timeout         time.Duration
}

// Config configures a client. Either token may be empty if the caller never
// uses that scope.
type Config struct {
	BaseURL         string
	ManagementToken string
	InferenceToken  string
	// Timeout bounds non-streaming calls. Streams are bounded by ctx only.
	Timeout    time.Duration
	HTTPClient *http.Client
}

// New returns a client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid API URL %q", cfg.BaseURL)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	return &Client{base: strings.TrimRight(cfg.BaseURL, "/"), managementToken: cfg.ManagementToken,
		inferenceToken: cfg.InferenceToken, http: cfg.HTTPClient, timeout: cfg.Timeout}, nil
}

type scope int

const (
	none scope = iota
	management
	inferenceScope
)

func (c *Client) token(s scope) (string, error) {
	switch s {
	case management:
		if c.managementToken == "" {
			return "", errors.New("management token is not configured (PERSONAL_AI_MANAGEMENT_TOKEN)")
		}
		return c.managementToken, nil
	case inferenceScope:
		if c.inferenceToken == "" {
			return "", errors.New("inference token is not configured (PERSONAL_AI_INFERENCE_TOKEN)")
		}
		return c.inferenceToken, nil
	}
	return "", nil
}

func (c *Client) request(ctx context.Context, s scope, method, path string, in any) (*http.Response, error) {
	tok, err := c.token(s)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
	var env apitypes.Error
	if json.Unmarshal(b, &env) == nil && env.Error.Message != "" {
		e.Message, e.Code = env.Error.Message, env.Error.Code
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		var secs int
		if _, err := fmt.Sscan(ra, &secs); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

// call performs a bounded JSON request/response exchange. out may be nil.
func (c *Client) call(ctx context.Context, s scope, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.request(ctx, s, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func p(format string, args ...string) string {
	esc := make([]any, len(args))
	for i, a := range args {
		esc[i] = url.PathEscape(a)
	}
	return fmt.Sprintf(format, esc...)
}

// ---- health ----

// Health checks server liveness.
func (c *Client) Health(ctx context.Context) error {
	return c.call(ctx, none, http.MethodGet, "/healthz", nil, nil)
}

// ---- models ----

// ListModels lists the catalog; all includes deactivated models.
func (c *Client) ListModels(ctx context.Context, all bool) ([]apitypes.Model, error) {
	var out apitypes.ModelList
	path := "/api/v1/models"
	if all {
		path += "?all=true"
	}
	err := c.call(ctx, management, http.MethodGet, path, nil, &out)
	return out.Models, err
}

// GetModel returns one model.
func (c *Client) GetModel(ctx context.Context, id string) (apitypes.Model, error) {
	var out apitypes.Model
	err := c.call(ctx, management, http.MethodGet, p("/api/v1/models/%s", id), nil, &out)
	return out, err
}

// RegisterModel adds a model.
func (c *Client) RegisterModel(ctx context.Context, m apitypes.Model) (apitypes.Model, error) {
	var out apitypes.Model
	err := c.call(ctx, management, http.MethodPost, "/api/v1/models", m, &out)
	return out, err
}

// UpdateModel replaces a model definition.
func (c *Client) UpdateModel(ctx context.Context, m apitypes.Model) (apitypes.Model, error) {
	var out apitypes.Model
	err := c.call(ctx, management, http.MethodPut, p("/api/v1/models/%s", m.ID), m, &out)
	return out, err
}

// SetModelActive activates or deactivates a model.
func (c *Client) SetModelActive(ctx context.Context, id string, active bool) (apitypes.Model, error) {
	action := "deactivate"
	if active {
		action = "activate"
	}
	var out apitypes.Model
	err := c.call(ctx, management, http.MethodPost, p("/api/v1/models/%s/", id)+action, nil, &out)
	return out, err
}

// ValidateModel returns the capability the model would be deployed on.
func (c *Client) ValidateModel(ctx context.Context, id string) (apitypes.Capability, error) {
	var out apitypes.Capability
	err := c.call(ctx, management, http.MethodGet, p("/api/v1/models/%s/validate", id), nil, &out)
	return out, err
}

// GetDefaultModel returns the default model ID.
func (c *Client) GetDefaultModel(ctx context.Context) (string, error) {
	var out apitypes.DefaultModel
	err := c.call(ctx, management, http.MethodGet, "/api/v1/preferences/default-model", nil, &out)
	return out.ModelID, err
}

// SetDefaultModel sets the default model.
func (c *Client) SetDefaultModel(ctx context.Context, id string) error {
	return c.call(ctx, management, http.MethodPut, "/api/v1/preferences/default-model", apitypes.DefaultModel{ModelID: id}, nil)
}

// ---- deployments ----

// ListDeployments lists deployments, optionally filtered by status.
func (c *Client) ListDeployments(ctx context.Context, status string) ([]apitypes.Deployment, error) {
	var out apitypes.DeploymentList
	path := "/api/v1/deployments"
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}
	err := c.call(ctx, management, http.MethodGet, path, nil, &out)
	return out.Deployments, err
}

// GetDeployment returns one deployment.
func (c *Client) GetDeployment(ctx context.Context, id string) (apitypes.Deployment, error) {
	var out apitypes.Deployment
	err := c.call(ctx, management, http.MethodGet, p("/api/v1/deployments/%s", id), nil, &out)
	return out, err
}

// CreateDeployment starts a deployment; it returns while it is still starting.
func (c *Client) CreateDeployment(ctx context.Context, req apitypes.CreateDeployment) (apitypes.Deployment, error) {
	var out apitypes.Deployment
	err := c.call(ctx, management, http.MethodPost, "/api/v1/deployments", req, &out)
	return out, err
}

// DeploymentAction runs stop, restart, pin, unpin or health on a deployment.
func (c *Client) DeploymentAction(ctx context.Context, id, action string) (apitypes.Deployment, error) {
	switch action {
	case "stop", "restart", "pin", "unpin", "health":
	default:
		return apitypes.Deployment{}, fmt.Errorf("unknown deployment action %q", action)
	}
	var out apitypes.Deployment
	err := c.call(ctx, management, http.MethodPost, p("/api/v1/deployments/%s/", id)+action, nil, &out)
	return out, err
}

// DeleteDeployment removes a stopped deployment.
func (c *Client) DeleteDeployment(ctx context.Context, id string) error {
	return c.call(ctx, management, http.MethodDelete, p("/api/v1/deployments/%s", id), nil, nil)
}

// WaitDeployment polls until the deployment is ready or has failed/stopped.
func (c *Client) WaitDeployment(ctx context.Context, id string, every time.Duration, progress func(apitypes.Deployment)) (apitypes.Deployment, error) {
	last := ""
	for {
		d, err := c.GetDeployment(ctx, id)
		if err != nil {
			return d, err
		}
		if d.Status != last && progress != nil {
			progress(d)
		}
		last = d.Status
		switch d.Status {
		case "ready":
			return d, nil
		case "failed", "stopped", "stopping":
			reason := d.Status
			if d.Failure != nil {
				reason = d.Failure.Reason
			}
			return d, fmt.Errorf("deployment %s did not become ready: %s", id, reason)
		}
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case <-time.After(every):
		}
	}
}

// ---- usage ----

// Usage returns the usage summary for [from, to); zero times are unbounded.
func (c *Client) Usage(ctx context.Context, from, to time.Time) (apitypes.UsageSummary, error) {
	q := url.Values{}
	if !from.IsZero() {
		q.Set("from", from.UTC().Format(time.RFC3339))
	}
	if !to.IsZero() {
		q.Set("to", to.UTC().Format(time.RFC3339))
	}
	path := "/api/v1/usage"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out apitypes.UsageSummary
	err := c.call(ctx, management, http.MethodGet, path, nil, &out)
	return out, err
}

// ---- inference ----

// InferenceModels lists models available for chat.
func (c *Client) InferenceModels(ctx context.Context) ([]apitypes.OpenAIModel, error) {
	var out apitypes.OpenAIModelList
	err := c.call(ctx, inferenceScope, http.MethodGet, "/v1/models", nil, &out)
	return out.Data, err
}

// Chat runs a non-streamed completion.
func (c *Client) Chat(ctx context.Context, req apitypes.ChatRequest) (apitypes.ChatResponse, error) {
	req.Stream = false
	var out apitypes.ChatResponse
	err := c.call(ctx, inferenceScope, http.MethodPost, "/v1/chat/completions", req, &out)
	return out, err
}

// ChatStream runs a streamed completion, calling onDelta for each text
// fragment as it arrives. It returns the token usage if the server sent it.
func (c *Client) ChatStream(ctx context.Context, req apitypes.ChatRequest, onDelta func(string)) (*apitypes.Usage, error) {
	req.Stream = true
	req.StreamOptions = &apitypes.StreamOptions{IncludeUsage: true}
	resp, err := c.request(ctx, inferenceScope, http.MethodPost, "/v1/chat/completions", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var usage *apitypes.Usage
	rd := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := rd.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: "); ok {
			if data == "[DONE]" {
				return usage, nil
			}
			var chunk apitypes.ChatChunk
			if jerr := json.Unmarshal([]byte(data), &chunk); jerr != nil {
				return usage, fmt.Errorf("decode stream: %w", jerr)
			}
			if chunk.Error != nil {
				return usage, &Error{Status: http.StatusBadGateway, Code: chunk.Error.Code, Message: chunk.Error.Message}
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			for _, ch := range chunk.Choices {
				if ch.Delta.Content != "" {
					onDelta(ch.Delta.Content)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return usage, errors.New("stream ended unexpectedly")
			}
			return usage, err
		}
	}
}
