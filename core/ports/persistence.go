package ports

import (
	"context"

	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
)

// ModelRepository stores the model catalog.
type ModelRepository interface {
	// Save inserts or replaces a model.
	Save(ctx context.Context, m model.Model) error
	// Get returns the model or ErrNotFound.
	Get(ctx context.Context, id model.ID) (model.Model, error)
	List(ctx context.Context) ([]model.Model, error)
}

// DeploymentRepository stores deployments with optimistic locking.
type DeploymentRepository interface {
	// Save inserts a deployment (Version 0) or updates it if the stored
	// version equals d.Version. It returns the saved deployment with its new
	// version, or ErrVersionConflict.
	Save(ctx context.Context, d deployment.Deployment) (deployment.Deployment, error)
	// Get returns the deployment or ErrNotFound.
	Get(ctx context.Context, id deployment.ID) (deployment.Deployment, error)
	// FindByIdempotencyKey returns the deployment created with the key, or
	// ErrNotFound.
	FindByIdempotencyKey(ctx context.Context, key string) (deployment.Deployment, error)
	List(ctx context.Context) ([]deployment.Deployment, error)
	ListByStatus(ctx context.Context, statuses ...deployment.Status) ([]deployment.Deployment, error)
	ListByModel(ctx context.Context, id model.ID) ([]deployment.Deployment, error)
}

// UsageRepository stores usage records.
type UsageRepository interface {
	Append(ctx context.Context, r usage.Record) error
	// Summarize aggregates records in the range.
	Summarize(ctx context.Context, tr usage.TimeRange) (usage.Summary, error)
}

// PreferenceRepository stores per-subject preferences.
type PreferenceRepository interface {
	// Get returns the preferences or ErrNotFound.
	Get(ctx context.Context, subjectID string) (model.Preferences, error)
	Set(ctx context.Context, p model.Preferences) error
}
