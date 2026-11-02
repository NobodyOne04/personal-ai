// Package inference defines provider-independent concepts for executing
// prompts against a model: requests, responses and streamed chunks.
package inference

import (
	"errors"
	"fmt"

	"personal-ai/core/domain/model"
)

// Role is the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one conversational turn.
type Message struct {
	Role    Role
	Content string
}

// Params are generation parameters. Nil means "not set by the caller", so
// model defaults can be applied.
type Params struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	Stop        []string
}

// WithDefaults returns p with unset fields filled from the model's defaults.
func (p Params) WithDefaults(g model.Generation) Params {
	if p.Temperature == nil && g.Temperature != nil {
		v := *g.Temperature
		p.Temperature = &v
	}
	if p.TopP == nil && g.TopP != nil {
		v := *g.TopP
		p.TopP = &v
	}
	if p.MaxTokens == nil && g.MaxTokens > 0 {
		v := g.MaxTokens
		p.MaxTokens = &v
	}
	return p
}

// Request is a model-execution request. ModelID may be empty, meaning "use the
// default model"; resolving it is an application concern.
type Request struct {
	ID       string
	ModelID  model.ID
	Messages []Message
	Params   Params
	Stream   bool
}

// ErrInvalidRequest wraps request validation failures.
var ErrInvalidRequest = errors.New("invalid inference request")

// Validate checks structure and parameter ranges.
func (r Request) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if len(r.Messages) == 0 {
		add("at least one message is required")
	}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleSystem, RoleUser, RoleAssistant:
		default:
			add("message %d: unknown role %q", i, m.Role)
		}
		if m.Content == "" {
			add("message %d: content is required", i)
		}
	}
	if t := r.Params.Temperature; t != nil && (*t < 0 || *t > 2) {
		add("temperature must be within [0, 2]")
	}
	if p := r.Params.TopP; p != nil && (*p <= 0 || *p > 1) {
		add("top_p must be within (0, 1]")
	}
	if n := r.Params.MaxTokens; n != nil && *n <= 0 {
		add("max_tokens must be positive")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, errors.Join(errs...))
	}
	return nil
}

// FinishReason says why generation ended.
type FinishReason string

const (
	FinishStop   FinishReason = "stop"
	FinishLength FinishReason = "length"
)

// TokenUsage counts tokens consumed by a request.
type TokenUsage struct {
	Input  int
	Output int
}

// Total is input plus output tokens.
func (u TokenUsage) Total() int { return u.Input + u.Output }

// Response is a complete (non-streamed) result.
type Response struct {
	ModelID      model.ID
	Message      Message
	FinishReason FinishReason
	Usage        TokenUsage
}

// Chunk is one piece of a streamed result. A stream ends when the channel
// closes. If generation fails after streaming began, the final chunk carries
// Err. Usage, when known, is delivered on the last chunk.
type Chunk struct {
	Delta        string
	FinishReason FinishReason
	Usage        *TokenUsage
	Err          error
}
