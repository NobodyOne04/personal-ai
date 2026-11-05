package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-ai/internal/apiclient"
	"personal-ai/internal/apitypes"
)

// fakeTelegram serves getUpdates from a queue and records sent messages.
type fakeTelegram struct {
	mu      sync.Mutex
	queue   []Update
	sent    map[int64][]string
	badPath bool
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/botTOKEN/") {
		f.badPath = true
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var params map[string]any
	_ = json.NewDecoder(r.Body).Decode(&params)
	ok := func(v any) {
		b, _ := json.Marshal(v)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": json.RawMessage(b)})
	}
	switch strings.TrimPrefix(r.URL.Path, "/botTOKEN/") {
	case "getUpdates":
		q := f.queue
		f.queue = nil
		ok(q)
	case "sendMessage":
		id := int64(params["chat_id"].(float64))
		f.sent[id] = append(f.sent[id], params["text"].(string))
		ok(true)
	default:
		ok(true)
	}
}

func (f *fakeTelegram) messages(chat int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[chat]...)
}

// fakePlatform records chat requests and answers with the message count.
type fakePlatform struct {
	mu       sync.Mutex
	requests []apitypes.ChatRequest
	chatErr  error
}

func (p *fakePlatform) InferenceModels(context.Context) ([]apitypes.OpenAIModel, error) {
	return []apitypes.OpenAIModel{{ID: "m1", Ready: true}, {ID: "m2"}}, nil
}
func (p *fakePlatform) Chat(_ context.Context, req apitypes.ChatRequest) (apitypes.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.chatErr != nil {
		return apitypes.ChatResponse{}, p.chatErr
	}
	return apitypes.ChatResponse{Choices: []apitypes.ChatChoice{{Message: apitypes.ChatMessage{Role: "assistant",
		Content: apitypes.ChatContent("answer " + string(req.Messages[len(req.Messages)-1].Content))}}}}, nil
}
func (p *fakePlatform) ListDeployments(context.Context, string) ([]apitypes.Deployment, error) {
	return []apitypes.Deployment{{ID: "d1", ModelID: "m1", Status: "ready"}}, nil
}
func (p *fakePlatform) CreateDeployment(_ context.Context, r apitypes.CreateDeployment) (apitypes.Deployment, error) {
	return apitypes.Deployment{ID: "d2", ModelID: r.ModelID, Status: "pending"}, nil
}
func (p *fakePlatform) DeploymentAction(_ context.Context, id, _ string) (apitypes.Deployment, error) {
	return apitypes.Deployment{ID: id, Status: "stopped"}, nil
}
func (p *fakePlatform) GetDefaultModel(context.Context) (string, error) { return "m1", nil }

func msg(id, user, chat int64, text string) Update {
	return Update{UpdateID: id, Message: &Message{From: &User{ID: user}, Chat: Chat{ID: chat}, Text: text}}
}

func TestBot(t *testing.T) {
	tg := &fakeTelegram{sent: map[int64][]string{}}
	srv := httptest.NewServer(tg)
	defer srv.Close()
	api, _ := NewBotAPI("TOKEN", srv.URL, nil)
	plat := &fakePlatform{}
	bot, err := New(Config{API: api, Platform: plat, AllowedUserIDs: []int64{1}, ManagementEnabled: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- bot.Run(ctx) }()

	tg.mu.Lock()
	tg.queue = []Update{msg(1, 99, 99, "hello from a stranger"), msg(2, 1, 10, "/model m1")}
	tg.mu.Unlock()
	waitMessages(t, tg, 10, 1)
	tg.mu.Lock()
	tg.queue = []Update{msg(3, 1, 10, "first")}
	tg.mu.Unlock()
	waitMessages(t, tg, 10, 2)
	tg.mu.Lock()
	tg.queue = []Update{msg(4, 1, 10, "second"), msg(5, 1, 11, "/deploy m2")}
	tg.mu.Unlock()
	waitMessages(t, tg, 10, 3)
	waitMessages(t, tg, 11, 1)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bot did not stop")
	}

	if got := tg.messages(99); len(got) != 0 {
		t.Fatalf("stranger got a reply: %v", got)
	}
	got := tg.messages(10)
	if !strings.Contains(got[0], "m1") || got[1] != "answer first" || got[2] != "answer second" {
		t.Fatalf("chat 10 = %v", got)
	}
	last := plat.requests[len(plat.requests)-1]
	if last.Model != "m1" || len(last.Messages) != 3 {
		t.Fatalf("history not kept: %+v", last)
	}
	if !strings.Contains(tg.messages(11)[0], "d2") {
		t.Fatalf("deploy reply = %v", tg.messages(11))
	}
	if tg.badPath {
		t.Fatal("unexpected API path")
	}
}

func TestDescribeErrorAndSplit(t *testing.T) {
	err := &apiclient.Error{Status: 503, Code: "model_starting", Message: "x"}
	if !strings.Contains(describeError(err), "starting") {
		t.Fatal(describeError(err))
	}
	parts := splitMessage(strings.Repeat("a", 5000), MaxMessageLen)
	if len(parts) != 2 || len([]rune(parts[0])) != MaxMessageLen {
		t.Fatalf("split = %d parts", len(parts))
	}
	if splitMessage("", 10) != nil {
		t.Fatal("empty text must produce no messages")
	}
}

func TestNewRequiresAllowlist(t *testing.T) {
	api, _ := NewBotAPI("TOKEN", "", nil)
	if _, err := New(Config{API: api, Platform: &fakePlatform{}}); err == nil {
		t.Fatal("empty allowlist must be rejected")
	}
}

func waitMessages(t *testing.T, tg *fakeTelegram, chat int64, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(tg.messages(chat)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("chat %d: got %v, want %d messages", chat, tg.messages(chat), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
