package rest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	deploymentapp "personal-ai/core/application/deployment"
	inferenceapp "personal-ai/core/application/inference"
	modelapp "personal-ai/core/application/model"
	"personal-ai/core/domain/deployment"
	"personal-ai/internal/apitypes"
	"personal-ai/internal/testing/fakes"
)

const (
	mgmtToken = "management-token-0123456789"
	infToken  = "inference-token-0123456789x"
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func TestHealthz(t *testing.T) {
	h := NewHandler(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

type apiEnv struct {
	srv     *httptest.Server
	deps    *deploymentapp.Service
	runtime *fakes.Runtime
}

func newAPI(t *testing.T, policy inferenceapp.Policy) *apiEnv {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	models, prefs, comp, rt := fakes.NewModels(), fakes.NewPrefs(), fakes.NewCompute(), fakes.NewRuntime()
	use, act, ids := &fakes.Usage{}, deployment.NewActivity(), &fakes.IDs{}
	modelSvc := modelapp.NewService(models, prefs, comp, realClock{})
	depSvc := deploymentapp.NewService(deploymentapp.Deps{Deployments: fakes.NewDeployments(), Models: models, Compute: comp,
		Runtime: rt, Usage: use, Clock: realClock{}, IDs: ids, Activity: act, Logger: logger},
		deploymentapp.Config{MaxActive: 1, PollInterval: time.Millisecond})
	t.Cleanup(depSvc.Close)
	infSvc, err := inferenceapp.NewService(inferenceapp.Deps{Models: modelSvc, Deployments: depSvc, Runtime: rt, Usage: use,
		Activity: act, Clock: realClock{}, IDs: ids, Logger: logger}, inferenceapp.Config{Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(Config{Logger: logger, Models: modelSvc, Deployments: depSvc, Inference: infSvc,
		ManagementToken: mgmtToken, InferenceToken: infToken, MaxBodyBytes: 4096}))
	t.Cleanup(srv.Close)
	return &apiEnv{srv: srv, deps: depSvc, runtime: rt}
}

func (e *apiEnv) do(t *testing.T, method, path, token, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

const modelJSON = `{"id":"m1","name":"M1","artifact":"org/m1","requirements":{"min_vram_gb":24},"generation":{"max_tokens":64}}`

func TestAuthScopes(t *testing.T) {
	e := newAPI(t, inferenceapp.PolicyReject)
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/api/v1/models", "", 401},
		{"/api/v1/models", infToken, 401},
		{"/api/v1/models", mgmtToken, 200},
		{"/v1/models", mgmtToken, 401},
		{"/v1/models", infToken, 200},
	} {
		if resp, _ := e.do(t, "GET", tc.path, tc.token, ""); resp.StatusCode != tc.want {
			t.Errorf("%s with %q: %d, want %d", tc.path, tc.token, resp.StatusCode, tc.want)
		}
	}
}

func TestManagementFlowAndChat(t *testing.T) {
	e := newAPI(t, inferenceapp.PolicyReject)

	if resp, b := e.do(t, "POST", "/api/v1/models", mgmtToken, modelJSON); resp.StatusCode != 201 {
		t.Fatalf("register: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/models", mgmtToken, modelJSON); resp.StatusCode != 409 {
		t.Fatalf("duplicate register: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/models", mgmtToken, `{"id":"bad"}`); resp.StatusCode != 400 {
		t.Fatalf("invalid model: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/models", mgmtToken, `{"id":"x","bogus":1}`); resp.StatusCode != 400 {
		t.Fatalf("unknown field: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/models", mgmtToken, `{"name":"`+strings.Repeat("x", 5000)+`"}`); resp.StatusCode != 413 {
		t.Fatalf("oversized body: %d", resp.StatusCode)
	}
	if resp, b := e.do(t, "GET", "/api/v1/models/m1/validate", mgmtToken, ""); resp.StatusCode != 200 || !strings.Contains(string(b), "gpu48") {
		t.Fatalf("validate: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(t, "PUT", "/api/v1/preferences/default-model", mgmtToken, `{"model_id":"m1"}`); resp.StatusCode != 200 {
		t.Fatalf("set default: %d", resp.StatusCode)
	}

	chat := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	resp, b := e.do(t, "POST", "/v1/chat/completions", infToken, chat)
	if resp.StatusCode != 503 || !strings.Contains(string(b), "model_not_deployed") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("chat before deploy: %d %s", resp.StatusCode, b)
	}

	resp, b = e.do(t, "POST", "/api/v1/deployments", mgmtToken, `{"model_id":"m1"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("create deployment: %d %s", resp.StatusCode, b)
	}
	var dep apitypes.Deployment
	_ = json.Unmarshal(b, &dep)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := e.deps.WaitReady(ctx, deployment.ID(dep.ID)); err != nil {
		t.Fatal(err)
	}
	if resp, b := e.do(t, "GET", "/api/v1/deployments/"+dep.ID, mgmtToken, ""); strings.Contains(string(b), "http://") || resp.StatusCode != 200 {
		t.Fatalf("runtime endpoint leaked or bad status: %s", b)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/deployments", mgmtToken, `{"model_id":"m1"}`); resp.StatusCode != 429 {
		t.Fatalf("limit: %d", resp.StatusCode)
	}

	resp, b = e.do(t, "POST", "/v1/chat/completions", infToken, chat)
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, b)
	}
	var cr apitypes.ChatResponse
	if err := json.Unmarshal(b, &cr); err != nil || cr.Choices[0].Message.Content != "ok" || cr.Usage.TotalTokens != 5 || cr.Model != "m1" {
		t.Fatalf("chat response: %s", b)
	}

	if resp, _ := e.do(t, "DELETE", "/api/v1/deployments/"+dep.ID, mgmtToken, ""); resp.StatusCode != 409 {
		t.Fatalf("delete running: %d", resp.StatusCode)
	}
	if resp, b := e.do(t, "POST", "/api/v1/deployments/"+dep.ID+"/stop", mgmtToken, ""); resp.StatusCode != 200 || !strings.Contains(string(b), `"stopped"`) {
		t.Fatalf("stop: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/deployments/"+dep.ID, mgmtToken, ""); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, b := e.do(t, "GET", "/api/v1/usage", mgmtToken, ""); resp.StatusCode != 200 || !strings.Contains(string(b), `"requests":1`) {
		t.Fatalf("usage: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/deployments/nope", mgmtToken, ""); resp.StatusCode != 404 {
		t.Fatalf("missing: %d", resp.StatusCode)
	}
}

func TestStreaming(t *testing.T) {
	e := newAPI(t, inferenceapp.PolicyStartAndWait)
	e.do(t, "POST", "/api/v1/models", mgmtToken, modelJSON)

	body := `{"model":"m1","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+infToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var text strings.Builder
	var sawUsage, sawDone bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var c apitypes.ChatChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatal(err)
		}
		if c.Usage != nil {
			sawUsage = true
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if text.String() != "ok" || !sawUsage || !sawDone {
		t.Fatalf("text=%q usage=%v done=%v", text.String(), sawUsage, sawDone)
	}
}
