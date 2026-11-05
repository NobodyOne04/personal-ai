package inferenceapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	deploymentapp "personal-ai/core/application/deployment"
	modelapp "personal-ai/core/application/model"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
	"personal-ai/internal/testing/fakes"
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type env struct {
	svc     *Service
	deps    *deploymentapp.Service
	runtime *fakes.Runtime
	usage   *fakes.Usage
	models  *modelapp.Service
}

func newEnv(t *testing.T, policy Policy) *env {
	t.Helper()
	temp := 0.3
	m := model.Model{ID: "m1", Name: "M1", Artifact: "org/m1", Requirements: model.Requirements{MinVRAMGB: 24, MinGPUCount: 1},
		Features: []model.Feature{model.FeatureChat}, Status: model.StatusActive,
		Generation: model.Generation{Temperature: &temp, MaxTokens: 100}}
	models := fakes.NewModels(m)
	prefs := fakes.NewPrefs()
	comp := fakes.NewCompute()
	rt := fakes.NewRuntime()
	use := &fakes.Usage{}
	act := deployment.NewActivity()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ids := &fakes.IDs{}

	modelSvc := modelapp.NewService(models, prefs, comp, realClock{})
	depSvc := deploymentapp.NewService(deploymentapp.Deps{Deployments: fakes.NewDeployments(), Models: models, Compute: comp,
		Runtime: rt, Usage: use, Clock: realClock{}, IDs: ids, Activity: act, Logger: logger},
		deploymentapp.Config{MaxActive: 2, PollInterval: time.Millisecond})
	t.Cleanup(depSvc.Close)
	svc, err := NewService(Deps{Models: modelSvc, Deployments: depSvc, Runtime: rt, Usage: use, Activity: act,
		Clock: realClock{}, IDs: ids, Logger: logger}, Config{Policy: policy, WaitTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, deps: depSvc, runtime: rt, usage: use, models: modelSvc}
}

func (e *env) deployReady(t *testing.T) deployment.Deployment {
	t.Helper()
	d, err := e.deps.Create(context.Background(), deploymentapp.CreateRequest{ModelID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d, err = e.deps.WaitReady(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func hello() inference.Request {
	return inference.Request{ModelID: "m1", Messages: []inference.Message{{Role: inference.RoleUser, Content: "hi"}}}
}

func TestChatRoutesAppliesDefaultsAndRecordsUsage(t *testing.T) {
	e := newEnv(t, PolicyReject)
	d := e.deployReady(t)
	res, err := e.svc.Chat(context.Background(), "owner", hello())
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Message.Content != "ok" || res.DeploymentID != d.ID {
		t.Fatalf("result = %+v", res)
	}
	sent := e.runtime.Requests[0]
	if sent.Params.Temperature == nil || *sent.Params.Temperature != 0.3 || *sent.Params.MaxTokens != 100 {
		t.Fatalf("model defaults not applied: %+v", sent.Params)
	}
	recs := e.usage.All()
	last := recs[len(recs)-1]
	if last.Kind != usage.KindInference || last.Outcome != usage.OutcomeCompleted || last.Tokens.Total() != 5 {
		t.Fatalf("usage = %+v", last)
	}
	if e.deps.ActiveRequests(d.ID) != 0 {
		t.Fatal("activity must be released")
	}
}

func TestDefaultModel(t *testing.T) {
	e := newEnv(t, PolicyReject)
	e.deployReady(t)
	req := hello()
	req.ModelID = ""
	if _, err := e.svc.Chat(context.Background(), "owner", req); !errors.Is(err, modelapp.ErrNoDefault) {
		t.Fatalf("want ErrNoDefault, got %v", err)
	}
	if err := e.models.SetDefault(context.Background(), "owner", "m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Chat(context.Background(), "owner", req); err != nil {
		t.Fatal(err)
	}
}

func TestPolicies(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		e := newEnv(t, PolicyReject)
		if _, err := e.svc.Chat(context.Background(), "owner", hello()); !errors.Is(err, ErrModelNotDeployed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("return pending", func(t *testing.T) {
		e := newEnv(t, PolicyStartAndReturnPending)
		_, err := e.svc.Chat(context.Background(), "owner", hello())
		var pe *PendingError
		if !errors.As(err, &pe) || !errors.Is(err, ErrDeploymentPending) {
			t.Fatalf("got %v", err)
		}
		// A second request must not start a second deployment.
		_, err2 := e.svc.Chat(context.Background(), "owner", hello())
		var pe2 *PendingError
		if errors.As(err2, &pe2) && pe2.DeploymentID != pe.DeploymentID {
			t.Fatal("started a duplicate deployment")
		}
	})
	t.Run("start and wait", func(t *testing.T) {
		e := newEnv(t, PolicyStartAndWait)
		if _, err := e.svc.Chat(context.Background(), "owner", hello()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestValidationAndInactiveModel(t *testing.T) {
	e := newEnv(t, PolicyReject)
	if _, err := e.svc.Chat(context.Background(), "owner", inference.Request{ModelID: "m1"}); !errors.Is(err, inference.ErrInvalidRequest) {
		t.Fatalf("got %v", err)
	}
	_, _ = e.models.Deactivate(context.Background(), "m1")
	if _, err := e.svc.Chat(context.Background(), "owner", hello()); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestChatStream(t *testing.T) {
	e := newEnv(t, PolicyReject)
	e.deployReady(t)
	st, err := e.svc.ChatStream(context.Background(), "owner", hello())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for c := range st.Chunks {
		b.WriteString(c.Delta)
	}
	if b.String() != "ok" {
		t.Fatalf("text = %q", b.String())
	}
	waitUsage(t, e.usage, usage.OutcomeCompleted)
}

func TestChatStreamInterruptedAndFailed(t *testing.T) {
	e := newEnv(t, PolicyReject)
	d := e.deployReady(t)

	gate := make(chan struct{})
	e.runtime.StreamGate = gate
	ctx, cancel := context.WithCancel(context.Background())
	st, err := e.svc.ChatStream(ctx, "owner", hello())
	if err != nil {
		t.Fatal(err)
	}
	if e.deps.ActiveRequests(d.ID) != 1 {
		t.Fatal("stream must count as in-flight")
	}
	cancel()
	for range st.Chunks {
	}
	waitUsage(t, e.usage, usage.OutcomeInterrupted)
	if e.deps.ActiveRequests(d.ID) != 0 {
		t.Fatal("activity leaked after interruption")
	}

	e.runtime.StreamGate = nil
	e.runtime.StreamErr = errors.New("gpu fell over")
	st, err = e.svc.ChatStream(context.Background(), "owner", hello())
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	for c := range st.Chunks {
		if c.Err != nil {
			gotErr = c.Err
		}
	}
	if gotErr == nil {
		t.Fatal("terminal error chunk expected")
	}
	waitUsage(t, e.usage, usage.OutcomeFailed)
}

func TestListAvailableModels(t *testing.T) {
	e := newEnv(t, PolicyReject)
	ms, _ := e.svc.ListAvailableModels(context.Background())
	if len(ms) != 1 || ms[0].Ready {
		t.Fatalf("models = %+v", ms)
	}
	e.deployReady(t)
	ms, _ = e.svc.ListAvailableModels(context.Background())
	if !ms[0].Ready {
		t.Fatal("model should be ready")
	}
}

func waitUsage(t *testing.T, u *fakes.Usage, want usage.Outcome) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range u.All() {
			if r.Kind == usage.KindInference && r.Outcome == want {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no %s usage record; have %+v", want, u.All())
}
