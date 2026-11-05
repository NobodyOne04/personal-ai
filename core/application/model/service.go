package modelapp

import (
	"context"
	"errors"
	"fmt"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/model"
	"personal-ai/core/ports"
)

// Errors returned by the model use cases. Validation failures wrap
// model.ErrInvalid; missing entities wrap ports.ErrNotFound.
var (
	// ErrAlreadyExists means a model with the same ID is already registered.
	ErrAlreadyExists = errors.New("model already exists")
	// ErrModelInactive means the operation needs an active model.
	ErrModelInactive = errors.New("model is deactivated")
	// ErrNoDefault means no usable default model is configured.
	ErrNoDefault = errors.New("no default model selected")
)

// Service implements the model-management use cases. It depends only on ports.
type Service struct {
	models  ports.ModelRepository
	prefs   ports.PreferenceRepository
	compute ports.ComputeProvider
	clock   ports.Clock
}

// NewService wires the use cases to their ports.
func NewService(models ports.ModelRepository, prefs ports.PreferenceRepository, provider ports.ComputeProvider, clock ports.Clock) *Service {
	return &Service{models: models, prefs: prefs, compute: provider, clock: clock}
}

// Register adds a new model to the catalog.
func (s *Service) Register(ctx context.Context, m model.Model) (model.Model, error) {
	if err := m.Validate(); err != nil {
		return model.Model{}, err
	}

	_, err := s.models.Get(ctx, m.ID)

	switch {
	case err == nil:
		return model.Model{}, fmt.Errorf("%w: %q", ErrAlreadyExists, m.ID)
	case !errors.Is(err, ports.ErrNotFound):
		return model.Model{}, fmt.Errorf("check existing model: %w", err)
	}

	if err := s.models.Save(ctx, m); err != nil {
		return model.Model{}, fmt.Errorf("save model: %w", err)
	}

	return m, nil
}

// Update replaces the definition of an existing model. The ID cannot change.
func (s *Service) Update(ctx context.Context, m model.Model) (model.Model, error) {
	if err := m.Validate(); err != nil {
		return model.Model{}, err
	}

	if _, err := s.models.Get(ctx, m.ID); err != nil {
		return model.Model{}, fmt.Errorf("get model %q: %w", m.ID, err)
	}

	if err := s.models.Save(ctx, m); err != nil {
		return model.Model{}, fmt.Errorf("save model: %w", err)
	}

	return m, nil
}

// Deactivate marks a model unavailable. Existing deployments are untouched;
// stopping them is a deployment use case. Deactivating twice is a no-op.
func (s *Service) Deactivate(ctx context.Context, id model.ID) (model.Model, error) {
	return s.setStatus(ctx, id, model.StatusDeactivated)
}

// Activate makes a deactivated model available again.
func (s *Service) Activate(ctx context.Context, id model.ID) (model.Model, error) {
	return s.setStatus(ctx, id, model.StatusActive)
}

func (s *Service) setStatus(ctx context.Context, id model.ID, status model.Status) (model.Model, error) {
	m, err := s.models.Get(ctx, id)

	if err != nil {
		return model.Model{}, fmt.Errorf("get model %q: %w", id, err)
	}

	if m.Status == status {
		return m, nil
	}

	m.Status = status

	if err := s.models.Save(ctx, m); err != nil {
		return model.Model{}, fmt.Errorf("save model: %w", err)
	}

	return m, nil
}

// Get returns one model.
func (s *Service) Get(ctx context.Context, id model.ID) (model.Model, error) {
	m, err := s.models.Get(ctx, id)
	if err != nil {
		return model.Model{}, fmt.Errorf("get model %q: %w", id, err)
	}
	return m, nil
}

// List returns the catalog. Deactivated models are included only when
// includeInactive is true.
func (s *Service) List(ctx context.Context, includeInactive bool) ([]model.Model, error) {
	all, err := s.models.List(ctx)

	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	if includeInactive {
		return all, nil
	}

	active := make([]model.Model, 0, len(all))

	for _, m := range all {
		if m.IsActive() {
			active = append(active, m)
		}
	}

	return active, nil
}

// ValidateRequirements checks that the compute currently on offer can host the
// model and returns the capability that would be chosen. It fails with
// model.ErrNoSuitableCompute when nothing fits. It provisions nothing.
func (s *Service) ValidateRequirements(ctx context.Context, id model.ID) (compute.Capability, error) {
	m, err := s.Get(ctx, id)

	if err != nil {
		return compute.Capability{}, err
	}

	caps, err := s.compute.Capabilities(ctx)

	if err != nil {
		return compute.Capability{}, fmt.Errorf("list compute capabilities: %w", err)
	}

	return m.SelectCapability(caps)
}

// SetDefault selects the default model for a subject. The model must exist and
// be active.
func (s *Service) SetDefault(ctx context.Context, subjectID string, id model.ID) error {
	if subjectID == "" {
		return fmt.Errorf("%w: subject id is required", model.ErrInvalid)
	}

	m, err := s.Get(ctx, id)

	if err != nil {
		return err
	}

	if !m.IsActive() {
		return fmt.Errorf("%w: %q", ErrModelInactive, id)
	}

	if err := s.prefs.Set(ctx, model.Preferences{SubjectID: subjectID, DefaultModelID: id}); err != nil {
		return fmt.Errorf("save preferences: %w", err)
	}

	return nil
}

// GetDefault returns the subject's default model. If none is set, or the chosen
// model has since been deactivated or removed, it returns ErrNoDefault so the
// caller never routes to an unusable model.
func (s *Service) GetDefault(ctx context.Context, subjectID string) (model.Model, error) {
	p, err := s.prefs.Get(ctx, subjectID)
	if errors.Is(err, ports.ErrNotFound) || (err == nil && p.DefaultModelID == "") {
		return model.Model{}, ErrNoDefault
	}
	if err != nil {
		return model.Model{}, fmt.Errorf("get preferences: %w", err)
	}
	m, err := s.models.Get(ctx, p.DefaultModelID)
	if errors.Is(err, ports.ErrNotFound) {
		return model.Model{}, fmt.Errorf("%w: %q no longer exists", ErrNoDefault, p.DefaultModelID)
	}
	if err != nil {
		return model.Model{}, fmt.Errorf("get model %q: %w", p.DefaultModelID, err)
	}
	if !m.IsActive() {
		return model.Model{}, fmt.Errorf("%w: %q is deactivated", ErrNoDefault, p.DefaultModelID)
	}
	return m, nil
}

// SeedResult reports what Seed did.
type SeedResult struct {
	Created []model.ID
	Updated []model.ID
	Skipped []model.ID
}

// Seed loads catalog entries (normally from configuration) into the database,
// which is authoritative after first start. Entries already present are left
// alone unless overwrite is true. Entries are validated before anything is
// written, so an invalid file changes nothing.
func (s *Service) Seed(ctx context.Context, entries []model.Model, overwrite bool) (SeedResult, error) {
	for _, m := range entries {
		if err := m.Validate(); err != nil {
			return SeedResult{}, fmt.Errorf("model %q: %w", m.ID, err)
		}
	}
	var res SeedResult
	for _, m := range entries {
		_, err := s.models.Get(ctx, m.ID)
		exists := err == nil

		if err != nil && !errors.Is(err, ports.ErrNotFound) {
			return res, fmt.Errorf("check model %q: %w", m.ID, err)
		}

		switch {
		case exists && !overwrite:
			res.Skipped = append(res.Skipped, m.ID)
			continue
		case exists:
			res.Updated = append(res.Updated, m.ID)
		default:
			res.Created = append(res.Created, m.ID)
		}

		if err := s.models.Save(ctx, m); err != nil {
			return res, fmt.Errorf("save model %q: %w", m.ID, err)
		}
	}
	return res, nil
}
