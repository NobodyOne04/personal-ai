package ports

import (
	"context"
	"errors"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
)

// RuntimeTarget identifies one running runtime. Ref is opaque and returned by
// InferenceRuntime.Initialize; only the runtime adapter interprets it.
type RuntimeTarget struct {
	Endpoint compute.Endpoint
	Ref      string
}

// PrepareSpec is the input to InferenceRuntime.Prepare.
type PrepareSpec struct {
	// DeploymentID lets the runtime derive per-deployment settings such as a
	// credential protecting the runtime's port.
	DeploymentID string
	Model        model.Model
	// Overrides replace the model's runtime settings for this deployment.
	Overrides map[string]any
}

// InitSpec is the input to InferenceRuntime.Initialize.
type InitSpec struct {
	DeploymentID string
	Model        model.Model
	Instance     compute.Instance
	Overrides    map[string]any
}

// ErrRuntimeUnavailable means the runtime could not be reached or is not
// serving yet. Callers may retry later.
var ErrRuntimeUnavailable = errors.New("inference runtime unavailable")

// RuntimeHealth is a runtime's reported state.
type RuntimeHealth struct {
	// Ready means the runtime accepts inference requests.
	Ready bool
	// ModelLoaded means the model's weights are loaded and servable.
	ModelLoaded bool
	Detail      string
}

// InferenceRuntime runs models and executes inference.
//
// Lifecycle: Prepare produces the workload that compute must launch;
// Initialize is called once the instance exists; Health is polled until the
// model is loaded; then Chat/ChatStream serve requests; Release frees runtime
// resources before the instance is terminated.
type InferenceRuntime interface {
	// Prepare returns the workload description the compute provider should
	// launch for the model. It performs no network calls.
	Prepare(ctx context.Context, spec PrepareSpec) (compute.LaunchSpec, error)

	// Initialize binds the runtime to a provisioned instance and returns an
	// opaque reference for later calls. It does not wait for the model to
	// load; use Health for that.
	Initialize(ctx context.Context, spec InitSpec) (ref string, err error)

	Health(ctx context.Context, target RuntimeTarget) (RuntimeHealth, error)

	// Models lists the model names the runtime currently serves.
	Models(ctx context.Context, target RuntimeTarget) ([]string, error)

	Chat(ctx context.Context, target RuntimeTarget, req inference.Request) (inference.Response, error)

	// ChatStream starts a streamed generation. The returned channel is closed
	// when generation ends or ctx is cancelled; a failure after streaming
	// began is delivered as a final chunk with Err set. Implementations must
	// not buffer the whole response and must release the underlying
	// connection when ctx is cancelled.
	ChatStream(ctx context.Context, target RuntimeTarget, req inference.Request) (<-chan inference.Chunk, error)

	// Release frees runtime-side resources. It is idempotent.
	Release(ctx context.Context, target RuntimeTarget) error
}
