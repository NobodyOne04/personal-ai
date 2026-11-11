// Package inferenceapp holds the inference use cases: resolve the requested
// (or default) model, route it to a ready deployment according to the
// not-deployed policy, execute it through the runtime port while tracking
// in-flight requests, and record usage whether the request completes, fails
// or is interrupted. Prompts and generated text are never stored or logged.
package inferenceapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
	"personal-ai/core/ports"
)

// Policy says what to do when the requested model has no ready deployment.
type Policy string

const (
	PolicyReject                = Policy("reject")
	PolicyStartAndWait          = Policy("start_and_wait")
	PolicyStartAndReturnPending = Policy("start_and_return_pending")
)

// Errors returned by the inference use cases.
var (
	// ErrModelNotDeployed means no deployment serves the model and the policy
	// forbids starting one.
	ErrModelNotDeployed = errors.New("model is not deployed")
	// ErrModelUnavailable means the model is deactivated.
	ErrModelUnavailable = errors.New("model is not available")
	// ErrUnsupported means the model does not support chat.
	ErrUnsupported = errors.New("model does not support chat")
	// ErrDeploymentPending is matched by *PendingError.
	ErrDeploymentPending = errors.New("model deployment is starting")
)

// PendingError reports that a deployment was started (or is starting) for the
// model and the caller should retry later.
type PendingError struct {
	DeploymentID deployment.ID
	Status       deployment.Status
}

func (e *PendingError) Error() string {
	return fmt.Sprintf("model deployment %s is %s; retry later", e.DeploymentID, e.Status)
}

// Is makes errors.Is(err, ErrDeploymentPending) true.
func (e *PendingError) Is(target error) bool { return target == ErrDeploymentPending }

// Models is the subset of the model use cases inference needs.
type Models interface {
	Get(ctx context.Context, id model.ID) (model.Model, error)
	GetDefault(ctx context.Context, subjectID string) (model.Model, error)
	List(ctx context.Context, includeInactive bool) ([]model.Model, error)
}

// Deployments is the subset of the deployment use cases inference needs.
type Deployments interface {
	FindReady(ctx context.Context, id model.ID) (deployment.Deployment, bool, error)
	FindStarting(ctx context.Context, id model.ID) (deployment.Deployment, bool, error)
	EnsureStarted(ctx context.Context, id model.ID) (deployment.Deployment, error)
	WaitReady(ctx context.Context, id deployment.ID) (deployment.Deployment, error)
	RecordActivity(ctx context.Context, id deployment.ID) error
}

// Config tunes routing.
type Config struct {
	Policy Policy
	// WaitTimeout bounds waiting for a deployment under PolicyStartAndWait.
	WaitTimeout time.Duration
}

// Service implements the inference use cases.
type Service struct {
	models   Models
	deps     Deployments
	runtime  ports.InferenceRuntime
	usage    ports.UsageRepository
	events   ports.EventPublisher
	activity *deployment.Activity
	clock    ports.Clock
	ids      ports.IDGenerator
	logger   *slog.Logger
	cfg      Config
}

// Deps are the collaborators of the service.
type Deps struct {
	Models      Models
	Deployments Deployments
	Runtime     ports.InferenceRuntime
	Usage       ports.UsageRepository
	Events      ports.EventPublisher
	Activity    *deployment.Activity
	Clock       ports.Clock
	IDs         ports.IDGenerator
	Logger      *slog.Logger
}

// NewService wires the use cases. Activity must be the tracker the scheduler
// consults, so in-flight requests protect deployments from idle shutdown.
func NewService(d Deps, cfg Config) (*Service, error) {
	switch cfg.Policy {
	case PolicyReject, PolicyStartAndWait, PolicyStartAndReturnPending:
	case "":
		cfg.Policy = PolicyReject
	default:
		return nil, fmt.Errorf("unknown not-deployed policy %q", cfg.Policy)
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = 20 * time.Minute
	}
	if d.Activity == nil {
		return nil, errors.New("activity tracker is required")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{models: d.Models, deps: d.Deployments, runtime: d.Runtime, usage: d.Usage, events: d.Events,
		activity: d.Activity, clock: d.Clock, ids: d.IDs, logger: d.Logger, cfg: cfg}, nil
}

// resolve picks the model, validates the request and applies defaults.
func (s *Service) resolve(ctx context.Context, subject string, req inference.Request) (model.Model, inference.Request, error) {
	var m model.Model
	var err error
	if req.ModelID == "" {
		m, err = s.models.GetDefault(ctx, subject)
	} else {
		m, err = s.models.Get(ctx, req.ModelID)
	}
	if err != nil {
		return m, req, err
	}
	if !m.IsActive() {
		return m, req, fmt.Errorf("%w: %q", ErrModelUnavailable, m.ID)
	}
	if !m.Supports(model.FeatureChat) {
		return m, req, fmt.Errorf("%w: %q", ErrUnsupported, m.ID)
	}
	req.ModelID = m.ID
	if err := req.Validate(); err != nil {
		return m, req, err
	}
	req.Params = req.Params.WithDefaults(m.Generation)
	if req.ID == "" {
		req.ID = s.ids.NewID()
	}
	return m, req, nil
}

// route finds a ready deployment, applying the not-deployed policy.
func (s *Service) route(ctx context.Context, m model.Model) (deployment.Deployment, error) {
	d, ok, err := s.deps.FindReady(ctx, m.ID)
	if err != nil || ok {
		return d, err
	}
	switch s.cfg.Policy {
	case PolicyStartAndReturnPending:
		d, err := s.deps.EnsureStarted(ctx, m.ID)
		if err != nil {
			return d, err
		}
		if d.Status == deployment.Ready {
			return d, nil
		}
		return d, &PendingError{DeploymentID: d.ID, Status: d.Status}
	case PolicyStartAndWait:
		d, err := s.deps.EnsureStarted(ctx, m.ID)
		if err != nil {
			return d, err
		}
		wctx, cancel := context.WithTimeout(ctx, s.cfg.WaitTimeout)
		defer cancel()
		d, err = s.deps.WaitReady(wctx, d.ID)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return d, &PendingError{DeploymentID: d.ID, Status: d.Status}
		}
		return d, err
	default:
		if st, ok, _ := s.deps.FindStarting(ctx, m.ID); ok {
			return st, &PendingError{DeploymentID: st.ID, Status: st.Status}
		}
		return d, fmt.Errorf("%w: %q", ErrModelNotDeployed, m.ID)
	}
}

// begin marks the deployment busy and returns the function that ends the
// request.
func (s *Service) begin(ctx context.Context, d deployment.Deployment) func() {
	end := s.activity.Begin(d.ID)
	s.touch(ctx, d.ID)
	return func() {
		s.touch(ctx, d.ID)
		end()
	}
}

func (s *Service) touch(ctx context.Context, id deployment.ID) {
	if err := s.deps.RecordActivity(context.WithoutCancel(ctx), id); err != nil {
		s.logger.Warn("failed to record activity", "deployment_id", id, "error", err)
	}
}

// Result is a non-streamed chat result.
type Result struct {
	Response     inference.Response
	DeploymentID deployment.ID
}

// Chat runs a non-streamed request. subject identifies whose default model
// applies when the request names none.
func (s *Service) Chat(ctx context.Context, subject string, req inference.Request) (Result, error) {
	m, req, err := s.resolve(ctx, subject, req)
	if err != nil {
		return Result{}, err
	}
	d, err := s.route(ctx, m)
	if err != nil {
		return Result{}, err
	}
	end := s.begin(ctx, d)
	defer end()

	start := time.Now()
	resp, err := s.runtime.Chat(ctx, target(d), req)
	outcome := usage.OutcomeCompleted
	switch {
	case ctx.Err() != nil:
		outcome = usage.OutcomeInterrupted
	case err != nil:
		outcome = usage.OutcomeFailed
	}
	s.record(ctx, d, req, resp.Usage, outcome, time.Since(start))
	if err != nil {
		return Result{}, err
	}
	resp.ModelID = m.ID
	return Result{Response: resp, DeploymentID: d.ID}, nil
}

// Stream is a streamed chat result. Chunks must be drained until the channel
// closes, or the context passed to ChatStream cancelled.
type Stream struct {
	ModelID      model.ID
	DeploymentID deployment.ID
	Chunks       <-chan inference.Chunk
}

// ChatStream starts a streamed request. Usage is recorded when the stream
// ends: completed, failed (terminal error chunk) or interrupted (ctx
// cancelled, e.g. client disconnect).
func (s *Service) ChatStream(ctx context.Context, subject string, req inference.Request) (Stream, error) {
	m, req, err := s.resolve(ctx, subject, req)
	if err != nil {
		return Stream{}, err
	}
	req.Stream = true
	d, err := s.route(ctx, m)
	if err != nil {
		return Stream{}, err
	}
	end := s.begin(ctx, d)
	start := time.Now()
	src, err := s.runtime.ChatStream(ctx, target(d), req)
	if err != nil {
		s.record(ctx, d, req, inference.TokenUsage{}, usage.OutcomeFailed, time.Since(start))
		end()
		return Stream{}, err
	}

	out := make(chan inference.Chunk)
	go func() {
		defer close(out)
		defer end()
		var tokens inference.TokenUsage
		outcome := usage.OutcomeInterrupted
		defer func() { s.record(ctx, d, req, tokens, outcome, time.Since(start)) }()

		for c := range src {
			if c.Usage != nil {
				tokens = *c.Usage
			}
			if c.Err != nil {
				outcome = usage.OutcomeFailed
			}
			select {
			case out <- c:
			case <-ctx.Done():
				outcome = usage.OutcomeInterrupted
				// Drain so the runtime adapter's goroutine can exit.
				for range src {
				}
				return
			}
			if c.Err != nil {
				return
			}
		}
		if ctx.Err() == nil && outcome != usage.OutcomeFailed {
			outcome = usage.OutcomeCompleted
		}
	}()
	return Stream{ModelID: m.ID, DeploymentID: d.ID, Chunks: out}, nil
}

func (s *Service) record(ctx context.Context, d deployment.Deployment, req inference.Request, tokens inference.TokenUsage, outcome usage.Outcome, dur time.Duration) {
	rec := usage.Record{
		ID: s.ids.NewID(), Kind: usage.KindInference, DeploymentID: d.ID, ModelID: req.ModelID,
		Outcome: outcome, Tokens: tokens, Duration: dur, At: s.clock.Now(),
	}
	ctx = context.WithoutCancel(ctx)
	if err := s.usage.Append(ctx, rec); err != nil {
		s.logger.Warn("failed to record usage", "deployment_id", d.ID, "request_id", req.ID, "error", err)
	}
	if s.events != nil {
		_ = s.events.Publish(ctx, ports.Event{Type: ports.EventInferenceCompleted, At: rec.At, DeploymentID: d.ID,
			ModelID: req.ModelID, Detail: map[string]string{"outcome": string(outcome)}})
	}
	s.logger.Info("inference finished", "request_id", req.ID, "deployment_id", d.ID, "model_id", req.ModelID,
		"outcome", outcome, "input_tokens", tokens.Input, "output_tokens", tokens.Output, "duration", dur.String())
}

func target(d deployment.Deployment) ports.RuntimeTarget {
	t := ports.RuntimeTarget{Ref: d.RuntimeRef, Key: d.RuntimeKey}
	if d.Endpoint != nil {
		t.Endpoint = *d.Endpoint
	}
	return t
}

// AvailableModel is an active catalog entry with its serving state.
type AvailableModel struct {
	Model    model.Model
	Ready    bool
	Starting bool
}

// ListAvailableModels returns active chat models and whether each is served.
func (s *Service) ListAvailableModels(ctx context.Context) ([]AvailableModel, error) {
	ms, err := s.models.List(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]AvailableModel, 0, len(ms))
	for _, m := range ms {
		if !m.Supports(model.FeatureChat) {
			continue
		}
		am := AvailableModel{Model: m}
		if _, ok, err := s.deps.FindReady(ctx, m.ID); err == nil {
			am.Ready = ok
		}
		if !am.Ready {
			if _, ok, err := s.deps.FindStarting(ctx, m.ID); err == nil {
				am.Starting = ok
			}
		}
		out = append(out, am)
	}
	return out, nil
}

// UsageSummary aggregates recorded usage in the range.
func (s *Service) UsageSummary(ctx context.Context, tr usage.TimeRange) (usage.Summary, error) {
	return s.usage.Summarize(ctx, tr)
}
