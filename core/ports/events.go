package ports

import (
	"context"
	"time"

	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/model"
)

// EventType names an application event.
type EventType string

const (
	EventDeploymentRequested EventType = "deployment.requested"
	EventDeploymentReady     EventType = "deployment.ready"
	EventDeploymentFailed    EventType = "deployment.failed"
	EventDeploymentStopped   EventType = "deployment.stopped"
	EventInferenceCompleted  EventType = "inference.completed"
	EventUsageRecorded       EventType = "usage.recorded"
)

// Event is something that happened. Detail must never contain prompts,
// generated text or credentials.
type Event struct {
	Type         EventType
	At           time.Time
	DeploymentID deployment.ID
	ModelID      model.ID
	Detail       map[string]string
}

// EventPublisher delivers events to interested parties. Publishing is
// best-effort from the application's point of view: it must not block a use
// case on slow consumers.
type EventPublisher interface {
	Publish(ctx context.Context, e Event) error
}
