package modelapp

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/model"
	"personal-ai/core/ports"
)

// ---- fakes ----

type fakeModels struct {
	data    map[model.ID]model.Model
	saveErr error
}

func newFakeModels() *fakeModels { return &fakeModels{data: map[model.ID]model.Model{}} }

func (f *fakeModels) Save(_ context.Context, m model.Model) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.data[m.ID] = m
	return nil
}
func (f *fakeModels) Get(_ context.Context, id model.ID) (model.Model, error) {
	m, ok := f.data[id]
	if !ok {
		return model.Model{}, ports.ErrNotFound
	}
	return m, nil
}
func (f *fakeModels) List(context.Context) ([]model.Model, error) {
	out := make([]model.Model, 0, len(f.data))
	for _, m := range f.data {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type fakePrefs struct{ data map[string]model.Preferences }

func (f *fakePrefs) Get(_ context.Context, s string) (model.Preferences, error) {
	p, ok := f.data[s]
	if !ok {
		return model.Preferences{}, ports.ErrNotFound
	}
	return p, nil
}
func (f *fakePrefs) Set(_ context.Context, p model.Preferences) error {
	f.data[p.SubjectID] = p
	return nil
}

type fakeCompute struct {
	caps []compute.Capability
	err  error
}

func (f *fakeCompute) Capabilities(context.Context) ([]compute.Capability, error) {
	return f.caps, f.err
}
func (f *fakeCompute) Provision(context.Context, compute.ProvisionSpec) (compute.Instance, error) {
	panic("model use cases must never provision compute")
}
func (f *fakeCompute) GetInstance(context.Context, compute.InstanceID) (compute.Instance, error) {
	panic("unexpected call")
}
func (f *fakeCompute) ListInstances(context.Context) ([]compute.Instance, error) {
	panic("unexpected call")
}
func (f *fakeCompute) Terminate(context.Context, compute.InstanceID) error { panic("unexpected call") }

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func newSvc() (*Service, *fakeModels, *fakePrefs, *fakeCompute) {
	m, p, c := newFakeModels(), &fakePrefs{data: map[string]model.Preferences{}}, &fakeCompute{}
	return NewService(m, p, c, fixedClock{}), m, p, c
}

func mdl(id string) model.Model {
	return model.Model{
		ID: model.ID(id), Name: id, Artifact: "org/" + id,
		Requirements: model.Requirements{MinVRAMGB: 24, MinGPUCount: 1},
		Features:     []model.Feature{model.FeatureChat},
		Status:       model.StatusActive,
	}
}

var ctx = context.Background()

// ---- tests ----

func TestRegister(t *testing.T) {
	s, repo, _, _ := newSvc()
	if _, err := s.Register(ctx, mdl("a")); err != nil {
		t.Fatal(err)
	}
	if _, ok := repo.data["a"]; !ok {
		t.Fatal("model not saved")
	}
	if _, err := s.Register(ctx, mdl("a")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate: want ErrAlreadyExists, got %v", err)
	}
	bad := mdl("b")
	bad.Name = ""
	if _, err := s.Register(ctx, bad); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("invalid: want model.ErrInvalid, got %v", err)
	}
	if _, ok := repo.data["b"]; ok {
		t.Fatal("invalid model must not be saved")
	}
}

func TestRegisterSurfacesRepositoryErrors(t *testing.T) {
	s, repo, _, _ := newSvc()
	boom := errors.New("db down")
	repo.saveErr = boom
	if _, err := s.Register(ctx, mdl("a")); !errors.Is(err, boom) {
		t.Fatalf("want wrapped repo error, got %v", err)
	}
}

func TestUpdate(t *testing.T) {
	s, repo, _, _ := newSvc()
	if _, err := s.Update(ctx, mdl("missing")); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("update of unknown model: want ErrNotFound, got %v", err)
	}
	_, _ = s.Register(ctx, mdl("a"))
	changed := mdl("a")
	changed.Description = "new"
	if _, err := s.Update(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if repo.data["a"].Description != "new" {
		t.Fatal("update not persisted")
	}
	changed.Requirements.MinVRAMGB = 0
	if _, err := s.Update(ctx, changed); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if repo.data["a"].Requirements.MinVRAMGB != 24 {
		t.Fatal("invalid update must not be persisted")
	}
}

func TestDeactivateAndActivate(t *testing.T) {
	s, repo, _, _ := newSvc()
	if _, err := s.Deactivate(ctx, "nope"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	_, _ = s.Register(ctx, mdl("a"))

	m, err := s.Deactivate(ctx, "a")
	if err != nil || m.Status != model.StatusDeactivated || repo.data["a"].Status != model.StatusDeactivated {
		t.Fatalf("deactivate failed: %+v, %v", m, err)
	}
	// idempotent: no second write
	repo.saveErr = errors.New("must not write")
	if _, err := s.Deactivate(ctx, "a"); err != nil {
		t.Fatalf("second deactivate should be a no-op, got %v", err)
	}
	repo.saveErr = nil

	if m, err := s.Activate(ctx, "a"); err != nil || !m.IsActive() {
		t.Fatalf("activate failed: %+v, %v", m, err)
	}
}

func TestGetAndList(t *testing.T) {
	s, _, _, _ := newSvc()
	_, _ = s.Register(ctx, mdl("a"))
	_, _ = s.Register(ctx, mdl("b"))
	_, _ = s.Deactivate(ctx, "b")

	if _, err := s.Get(ctx, "zzz"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	active, _ := s.List(ctx, false)
	if len(active) != 1 || active[0].ID != "a" {
		t.Fatalf("active list = %+v", active)
	}
	all, _ := s.List(ctx, true)
	if len(all) != 2 {
		t.Fatalf("full list = %+v", all)
	}
}

func TestValidateRequirements(t *testing.T) {
	s, _, _, c := newSvc()
	_, _ = s.Register(ctx, mdl("a")) // needs 24 GB
	c.caps = []compute.Capability{
		{ID: "small", GPUCount: 1, VRAMPerGPUGB: 16},
		{ID: "ok", GPUCount: 1, VRAMPerGPUGB: 24},
	}
	got, err := s.ValidateRequirements(ctx, "a")
	if err != nil || got.ID != "ok" {
		t.Fatalf("got %v, %v; want ok", got.ID, err)
	}

	c.caps = []compute.Capability{{ID: "small", GPUCount: 1, VRAMPerGPUGB: 16}}
	if _, err := s.ValidateRequirements(ctx, "a"); !errors.Is(err, model.ErrNoSuitableCompute) {
		t.Fatalf("want ErrNoSuitableCompute, got %v", err)
	}

	boom := errors.New("provider down")
	c.err = boom
	if _, err := s.ValidateRequirements(ctx, "a"); !errors.Is(err, boom) {
		t.Fatalf("want provider error, got %v", err)
	}
	if _, err := s.ValidateRequirements(ctx, "nope"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown model: want ErrNotFound, got %v", err)
	}
}

func TestDefaultModel(t *testing.T) {
	s, _, _, _ := newSvc()
	if _, err := s.GetDefault(ctx, "owner"); !errors.Is(err, ErrNoDefault) {
		t.Fatalf("nothing set: want ErrNoDefault, got %v", err)
	}
	_, _ = s.Register(ctx, mdl("a"))
	_, _ = s.Register(ctx, mdl("off"))
	_, _ = s.Deactivate(ctx, "off")

	if err := s.SetDefault(ctx, "owner", "missing"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown model: want ErrNotFound, got %v", err)
	}
	if err := s.SetDefault(ctx, "owner", "off"); !errors.Is(err, ErrModelInactive) {
		t.Fatalf("inactive model: want ErrModelInactive, got %v", err)
	}
	if err := s.SetDefault(ctx, "", "a"); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("empty subject: want ErrInvalid, got %v", err)
	}

	if err := s.SetDefault(ctx, "owner", "a"); err != nil {
		t.Fatal(err)
	}
	if m, err := s.GetDefault(ctx, "owner"); err != nil || m.ID != "a" {
		t.Fatalf("got %v, %v", m.ID, err)
	}
	if _, err := s.GetDefault(ctx, "someone-else"); !errors.Is(err, ErrNoDefault) {
		t.Fatalf("defaults must be per subject, got %v", err)
	}

	// deactivating the chosen model means there is no usable default any more
	_, _ = s.Deactivate(ctx, "a")
	if _, err := s.GetDefault(ctx, "owner"); !errors.Is(err, ErrNoDefault) {
		t.Fatalf("deactivated default: want ErrNoDefault, got %v", err)
	}
}

func TestSeed(t *testing.T) {
	s, repo, _, _ := newSvc()
	existing := mdl("keep")
	existing.Description = "operator edited"
	repo.data["keep"] = existing

	incoming := mdl("keep")
	incoming.Description = "from config"
	res, err := s.Seed(ctx, []model.Model{incoming, mdl("new")}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Created, []model.ID{"new"}) || !reflect.DeepEqual(res.Skipped, []model.ID{"keep"}) || len(res.Updated) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if repo.data["keep"].Description != "operator edited" {
		t.Fatal("seed must not overwrite existing entries by default")
	}

	res, err = s.Seed(ctx, []model.Model{incoming}, true)
	if err != nil || !reflect.DeepEqual(res.Updated, []model.ID{"keep"}) {
		t.Fatalf("overwrite result = %+v, %v", res, err)
	}
	if repo.data["keep"].Description != "from config" {
		t.Fatal("overwrite did not apply")
	}
}

func TestSeedValidatesBeforeWritingAnything(t *testing.T) {
	s, repo, _, _ := newSvc()
	bad := mdl("bad")
	bad.Artifact = ""
	if _, err := s.Seed(ctx, []model.Model{mdl("good"), bad}, false); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if len(repo.data) != 0 {
		t.Fatalf("an invalid batch must write nothing, but saved %d", len(repo.data))
	}
}
