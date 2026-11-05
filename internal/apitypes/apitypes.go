// Package apitypes defines the JSON wire contract of the REST API. The server
// (adapters/inbound/rest) and the API client used by the CLI and Telegram bot
// (internal/apiclient) share these types, so the two cannot drift apart. It
// imports only the standard library.
package apitypes

import (
	"encoding/json"
	"strings"
	"time"
)

// Error is the error envelope of every non-2xx response.
type Error struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes an error.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ---- management: models ----

// Requirements are a model's hardware requirements.
type Requirements struct {
	MinVRAMGB         int `json:"min_vram_gb"`
	RecommendedVRAMGB int `json:"recommended_vram_gb,omitempty"`
	MinGPUCount       int `json:"min_gpu_count,omitempty"`
}

// Generation holds default sampling parameters.
type Generation struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
}

// Model is a catalog entry.
type Model struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Artifact     string         `json:"artifact"`
	Description  string         `json:"description,omitempty"`
	Family       string         `json:"family,omitempty"`
	ParamCount   int64          `json:"param_count,omitempty"`
	Status       string         `json:"status,omitempty"`
	Features     []string       `json:"features,omitempty"`
	Requirements Requirements   `json:"requirements"`
	Generation   Generation     `json:"generation"`
	Runtime      map[string]any `json:"runtime,omitempty"`
}

// ModelList wraps a list of models.
type ModelList struct {
	Models []Model `json:"models"`
}

// Capability is a compute offering.
type Capability struct {
	ID               string `json:"id"`
	GPUModel         string `json:"gpu_model"`
	GPUCount         int    `json:"gpu_count"`
	VRAMPerGPUGB     int    `json:"vram_per_gpu_gb"`
	HourlyCostMicros *int64 `json:"hourly_cost_micros,omitempty"`
	Currency         string `json:"currency,omitempty"`
}

// DefaultModel is the default-model preference.
type DefaultModel struct {
	ModelID string `json:"model_id"`
}

// ---- management: deployments ----

// Failure explains why a deployment failed.
type Failure struct {
	Stage  string    `json:"stage"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Deployment is a deployment's public state. Runtime endpoints and
// credentials are deliberately absent.
type Deployment struct {
	ID             string         `json:"id"`
	ModelID        string         `json:"model_id"`
	Status         string         `json:"status"`
	Health         string         `json:"health"`
	Pinned         bool           `json:"pinned"`
	HasInstance    bool           `json:"has_instance"`
	InstanceID     string         `json:"instance_id,omitempty"`
	RuntimeConfig  map[string]any `json:"runtime_config,omitempty"`
	ActiveRequests int            `json:"active_requests"`
	Failure        *Failure       `json:"failure,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	ReadyAt        *time.Time     `json:"ready_at,omitempty"`
	LastActivityAt time.Time      `json:"last_activity_at"`
}

// DeploymentList wraps a list of deployments.
type DeploymentList struct {
	Deployments []Deployment `json:"deployments"`
}

// CreateDeployment is the body of POST /api/v1/deployments.
type CreateDeployment struct {
	ModelID       string         `json:"model_id"`
	Pinned        bool           `json:"pinned,omitempty"`
	RuntimeConfig map[string]any `json:"runtime_config,omitempty"`
}

// ---- management: usage ----

// UsageSummary aggregates recorded usage. Costs are per currency in micros;
// estimated and billed amounts are never mixed.
type UsageSummary struct {
	From                         *time.Time       `json:"from,omitempty"`
	To                           *time.Time       `json:"to,omitempty"`
	Requests                     int              `json:"requests"`
	InputTokens                  int64            `json:"input_tokens"`
	OutputTokens                 int64            `json:"output_tokens"`
	TotalTokens                  int64            `json:"total_tokens"`
	InferenceSeconds             float64          `json:"inference_seconds"`
	UptimeSeconds                float64          `json:"uptime_seconds"`
	EstimatedCostMicros          map[string]int64 `json:"estimated_cost_micros"`
	BilledCostMicros             map[string]int64 `json:"billed_cost_micros"`
	UptimeRecordsWithUnknownCost int              `json:"uptime_records_with_unknown_cost"`
}

// ---- OpenAI-compatible inference ----

// ChatMessage is an OpenAI chat message. Content may be a string or an array
// of content parts; only text parts are supported.
type ChatMessage struct {
	Role    string      `json:"role"`
	Content ChatContent `json:"content"`
}

// ChatContent accepts a string or [{"type":"text","text":...}] and always
// marshals as a string.
type ChatContent string

// UnmarshalJSON implements json.Unmarshaler.
func (c *ChatContent) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = ChatContent(s)
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	*c = ChatContent(sb.String())
	return nil
}

// StopSequences accepts a string or an array of strings.
type StopSequences []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StopSequences) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// StreamOptions are OpenAI stream options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatRequest is the body of POST /v1/chat/completions. An empty model means
// "use the default model".
type ChatRequest struct {
	Model         string         `json:"model,omitempty"`
	Messages      []ChatMessage  `json:"messages"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	Stop          StopSequences  `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// Usage is OpenAI token usage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatChoice is one non-streamed choice.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatResponse is a non-streamed completion.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

// Delta is a streamed message fragment.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// ChunkChoice is one streamed choice.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// ChatChunk is one server-sent event of a streamed completion.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
	// Error is set on a terminal event when generation failed mid-stream.
	Error *ErrorDetail `json:"error,omitempty"`
}

// OpenAIModel is an entry of GET /v1/models.
type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Ready is an extension: whether a deployment currently serves the model.
	Ready bool `json:"ready"`
	// Starting is an extension: whether a deployment is being started.
	Starting bool `json:"starting,omitempty"`
}

// OpenAIModelList is GET /v1/models.
type OpenAIModelList struct {
	Object string        `json:"object"`
	Data   []OpenAIModel `json:"data"`
}
