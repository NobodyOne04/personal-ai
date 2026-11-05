// Package e2etest drives the CLI through the REST client against the real
// HTTP handler and application services, with fakes standing in for the
// database, compute provider and runtime.
package e2etest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"personal-ai/adapters/inbound/cli"
	"personal-ai/adapters/inbound/rest"
	deploymentapp "personal-ai/core/application/deployment"
	inferenceapp "personal-ai/core/application/inference"
	modelapp "personal-ai/core/application/model"
	"personal-ai/core/domain/deployment"
	"personal-ai/internal/apiclient"
	"personal-ai/internal/testing/fakes"
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

const (
	mgmt = "management-token-0123456789"
	inf  = "inference-token-01234567890"
)

func TestCLIAgainstServer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	models, prefs, comp, rt := fakes.NewModels(), fakes.NewPrefs(), fakes.NewCompute(), fakes.NewRuntime()
	use, act, ids := &fakes.Usage{}, deployment.NewActivity(), &fakes.IDs{}
	modelSvc := modelapp.NewService(models, prefs, comp, realClock{})
	depSvc := deploymentapp.NewService(deploymentapp.Deps{Deployments: fakes.NewDeployments(), Models: models, Compute: comp,
		Runtime: rt, Usage: use, Clock: realClock{}, IDs: ids, Activity: act, Logger: logger},
		deploymentapp.Config{MaxActive: 1, PollInterval: time.Millisecond})
	defer depSvc.Close()
	infSvc, _ := inferenceapp.NewService(inferenceapp.Deps{Models: modelSvc, Deployments: depSvc, Runtime: rt, Usage: use,
		Activity: act, Clock: realClock{}, IDs: ids, Logger: logger}, inferenceapp.Config{Policy: inferenceapp.PolicyReject})
	srv := httptest.NewServer(rest.NewHandler(rest.Config{Logger: logger, Models: modelSvc, Deployments: depSvc,
		Inference: infSvc, ManagementToken: mgmt, InferenceToken: inf}))
	defer srv.Close()

	client, err := apiclient.New(apiclient.Config{BaseURL: srv.URL, ManagementToken: mgmt, InferenceToken: inf})
	if err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		app := &cli.App{Client: client, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut}
		code := app.Run(context.Background(), args)
		return out.String(), errOut.String(), code
	}
	mustRun := func(stdin string, args ...string) string {
		t.Helper()
		out, errOut, code := run(stdin, args...)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, errOut)
		}
		return out
	}

	mustRun(`{"id":"m1","name":"M1","artifact":"org/m1","requirements":{"min_vram_gb":24}}`, "models", "register", "-")
	if out := mustRun("", "models", "list"); !strings.Contains(out, "m1") {
		t.Fatalf("list: %s", out)
	}
	if out := mustRun("", "models", "validate", "m1"); !strings.Contains(out, "gpu48") {
		t.Fatalf("validate: %s", out)
	}
	mustRun("", "default", "m1")

	if _, errOut, code := run("", "chat", "hi"); code != 1 || !strings.Contains(errOut, "personal-ai deploy") {
		t.Fatalf("chat before deploy: %d %s", code, errOut)
	}
	depID := strings.TrimSpace(mustRun("", "deploy", "m1", "-wait"))
	if out := mustRun("", "chat", "hi"); strings.TrimSpace(out) != "ok" {
		t.Fatalf("stream chat: %q", out)
	}
	if out := mustRun("", "chat", "-no-stream", "hi"); strings.TrimSpace(out) != "ok" {
		t.Fatalf("chat: %q", out)
	}
	if out := mustRun("hello\n/reset\nagain\n/exit\n", "chat"); strings.Count(out, "ok") != 2 {
		t.Fatalf("interactive: %q", out)
	}
	if out := mustRun("", "deployments", "list"); !strings.Contains(out, depID) || !strings.Contains(out, "ready") {
		t.Fatalf("deployments: %s", out)
	}
	mustRun("", "deployments", "stop", depID)
	if out := mustRun("", "usage"); !strings.Contains(out, "requests:        4") || !strings.Contains(out, "USD") {
		t.Fatalf("usage: %s", out)
	}
	mustRun("", "deployments", "delete", depID)

	if _, _, code := run("", "bogus"); code != cli.ExitUsage {
		t.Fatalf("unknown command exit = %d", code)
	}
}
