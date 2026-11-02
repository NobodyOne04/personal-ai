// Package model defines the platform's LLM catalog entries and the rules for
// deciding whether compute can host them.
package model

import (
	"errors"
	"fmt"
	"regexp"

	"personal-ai/core/domain/compute"
)

// ID identifies a model in the platform catalog. It is provider-independent.
type ID string

// Status is the availability of a catalog entry.
type Status string

const (
	StatusActive      Status = "active"
	StatusDeactivated Status = "deactivated"
)

// Feature is an inference capability a model supports.
type Feature string

const (
	FeatureChat       Feature = "chat"
	FeatureCompletion Feature = "completion"
	FeatureTools      Feature = "tools"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Requirements are the hardware needs declared by the operator.
// Hardware is judged on what is declared; the Core makes no attempt to
// estimate KV-cache or context-length memory.
type Requirements struct {
	// MinVRAMGB is the total VRAM across all GPUs.
	MinVRAMGB         int
	RecommendedVRAMGB int
	MinGPUCount       int
}

// SatisfiedBy reports whether the capability meets the minimum requirements.
func (r Requirements) SatisfiedBy(c compute.Capability) bool {
	minGPUs := r.MinGPUCount
	if minGPUs < 1 {
		minGPUs = 1
	}
	return c.GPUCount >= minGPUs && c.TotalVRAMGB() >= r.MinVRAMGB
}

// Generation holds default sampling parameters. Nil/zero means "unset".
type Generation struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   int
}

// Model is a catalog entry.
type Model struct {
	ID          ID
	Name        string
	Artifact    string // opaque artifact reference, e.g. a repository path
	Description string
	Family      string
	ParamCount  int64

	Requirements Requirements
	Features     []Feature
	Generation   Generation

	// Runtime is opaque to the Core. Only the inference runtime adapter
	// interprets it.
	Runtime map[string]any

	Status Status
}

// ErrInvalid wraps every validation failure of a Model.
var ErrInvalid = errors.New("invalid model")

// ErrNoSuitableCompute means no offered capability satisfies the model.
var ErrNoSuitableCompute = errors.New("no compute capability satisfies the model requirements")

// IsActive reports whether the model may be deployed and served.
func (m Model) IsActive() bool { return m.Status == StatusActive }

// Supports reports whether the model advertises the feature.
func (m Model) Supports(f Feature) bool {
	for _, have := range m.Features {
		if have == f {
			return true
		}
	}
	return false
}

// Validate checks the definition and returns all problems at once.
func (m Model) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !idPattern.MatchString(string(m.ID)) {
		add("id %q must match %s", m.ID, idPattern)
	}
	if m.Name == "" {
		add("name is required")
	}
	if m.Artifact == "" {
		add("artifact is required")
	}
	if m.ParamCount < 0 {
		add("param count must not be negative")
	}
	if m.Requirements.MinVRAMGB <= 0 {
		add("requirements.min_vram_gb must be positive")
	}
	if m.Requirements.MinGPUCount < 0 {
		add("requirements.min_gpu_count must not be negative")
	}
	if m.Requirements.RecommendedVRAMGB != 0 && m.Requirements.RecommendedVRAMGB < m.Requirements.MinVRAMGB {
		add("requirements.recommended_vram_gb must not be below min_vram_gb")
	}
	if len(m.Features) == 0 {
		add("at least one feature is required")
	}
	for _, f := range m.Features {
		switch f {
		case FeatureChat, FeatureCompletion, FeatureTools:
		default:
			add("unknown feature %q", f)
		}
	}
	if t := m.Generation.Temperature; t != nil && (*t < 0 || *t > 2) {
		add("generation.temperature must be within [0, 2]")
	}
	if p := m.Generation.TopP; p != nil && (*p <= 0 || *p > 1) {
		add("generation.top_p must be within (0, 1]")
	}
	if m.Generation.MaxTokens < 0 {
		add("generation.max_tokens must not be negative")
	}
	switch m.Status {
	case StatusActive, StatusDeactivated:
	default:
		add("unknown status %q", m.Status)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
	}
	return nil
}

// SelectCapability picks the capability to deploy on: among those satisfying
// the requirements, the cheapest by reported hourly cost (capabilities with
// unknown cost rank last), then the smallest VRAM, then by ID for
// determinism. Costs are compared as reported and assumed to share a currency.
func (m Model) SelectCapability(caps []compute.Capability) (compute.Capability, error) {
	var best *compute.Capability
	for i := range caps {
		if !m.Requirements.SatisfiedBy(caps[i]) {
			continue
		}
		if best == nil || cheaper(caps[i], *best) {
			c := caps[i]
			best = &c
		}
	}
	if best == nil {
		return compute.Capability{}, fmt.Errorf("%w: model %q needs %d GB VRAM across >= %d GPU(s)",
			ErrNoSuitableCompute, m.ID, m.Requirements.MinVRAMGB, max(1, m.Requirements.MinGPUCount))
	}
	return *best, nil
}

func cheaper(a, b compute.Capability) bool {
	switch {
	case a.HourlyCost != nil && b.HourlyCost == nil:
		return true
	case a.HourlyCost == nil && b.HourlyCost != nil:
		return false
	case a.HourlyCost != nil && b.HourlyCost != nil && a.HourlyCost.Micros != b.HourlyCost.Micros:
		return a.HourlyCost.Micros < b.HourlyCost.Micros
	}
	if a.TotalVRAMGB() != b.TotalVRAMGB() {
		return a.TotalVRAMGB() < b.TotalVRAMGB()
	}
	return a.ID < b.ID
}

// Preferences are per-subject settings. SubjectID is an opaque identifier
// ("owner" for the single-user deployment) so multi-user support remains
// possible without a model change.
type Preferences struct {
	SubjectID      string
	DefaultModelID ID
}
