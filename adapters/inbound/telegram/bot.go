// Package telegram is the Telegram inbound adapter. Telegram update objects,
// commands and message formatting stay inside this package. It reaches the
// platform only through the REST API (internal/apiclient) and holds no
// business logic.
//
// Access is restricted by an explicit allowlist of Telegram user IDs; updates
// from anyone else are ignored without a reply, so the bot does not reveal
// itself. Conversation history lives in memory per chat and is never logged.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"personal-ai/internal/apiclient"
	"personal-ai/internal/apitypes"
)

// MaxMessageLen is Telegram's limit for one text message.
const MaxMessageLen = 4096

// Platform is the subset of the REST client the bot uses.
type Platform interface {
	InferenceModels(ctx context.Context) ([]apitypes.OpenAIModel, error)
	Chat(ctx context.Context, req apitypes.ChatRequest) (apitypes.ChatResponse, error)
	ListDeployments(ctx context.Context, status string) ([]apitypes.Deployment, error)
	CreateDeployment(ctx context.Context, req apitypes.CreateDeployment) (apitypes.Deployment, error)
	DeploymentAction(ctx context.Context, id, action string) (apitypes.Deployment, error)
	GetDefaultModel(ctx context.Context) (string, error)
}

var _ Platform = (*apiclient.Client)(nil)

// Config configures the bot.
type Config struct {
	API            *BotAPI
	Platform       Platform
	AllowedUserIDs []int64
	// ManagementEnabled allows /deploy, /stop and /status. It requires the
	// platform client to hold a management token.
	ManagementEnabled bool
	// MaxHistory caps remembered messages per chat (default 20).
	MaxHistory int
	// RequestTimeout bounds one chat request (default 10m).
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

type session struct {
	mu      sync.Mutex
	model   string
	history []apitypes.ChatMessage
}

// Bot is the Telegram adapter.
type Bot struct {
	cfg      Config
	allowed  map[int64]bool
	sessMu   sync.Mutex
	sessions map[int64]*session
	wg       sync.WaitGroup
}

// New validates the configuration and returns a bot.
func New(cfg Config) (*Bot, error) {
	if cfg.API == nil || cfg.Platform == nil {
		return nil, errors.New("telegram: API and platform are required")
	}
	if len(cfg.AllowedUserIDs) == 0 {
		return nil, errors.New("telegram: the allowlist must not be empty")
	}
	if cfg.MaxHistory <= 0 {
		cfg.MaxHistory = 20
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	b := &Bot{cfg: cfg, allowed: map[int64]bool{}, sessions: map[int64]*session{}}
	for _, id := range cfg.AllowedUserIDs {
		b.allowed[id] = true
	}
	return b, nil
}

// Run long-polls for updates until ctx ends, then waits for in-flight
// handlers to finish.
func (b *Bot) Run(ctx context.Context) error {
	defer b.wg.Wait()
	var offset int64
	backoff := time.Second
	for {
		updates, err := b.cfg.API.GetUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			b.cfg.Logger.Warn("telegram getUpdates failed", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			offset = max(offset, u.UpdateID+1)
			b.dispatch(ctx, u)
		}
	}
}

// dispatch handles one update on its own goroutine; messages within a chat
// are serialised by the session lock so history stays ordered.
func (b *Bot) dispatch(ctx context.Context, u Update) {
	m := u.Message
	if m == nil || m.From == nil || m.Text == "" {
		return
	}
	if !b.allowed[m.From.ID] {
		b.cfg.Logger.Warn("ignored message from user not on the allowlist", "telegram_user_id", m.From.ID)
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.handle(ctx, m.Chat.ID, strings.TrimSpace(m.Text))
	}()
}

func (b *Bot) session(chatID int64) *session {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	s, ok := b.sessions[chatID]
	if !ok {
		s = &session{}
		b.sessions[chatID] = s
	}
	return s
}

const helpText = `Commands:
/models - list models
/model [id] - show or choose the model for this chat
/reset - forget this conversation
/status - list deployments
/deploy <model> - start a deployment
/stop <deployment id> - stop a deployment
/help - this message

Anything else is sent to the model.`

func (b *Bot) handle(ctx context.Context, chatID int64, text string) {
	s := b.session(chatID)
	s.mu.Lock()
	defer s.mu.Unlock()

	cmd, arg := text, ""
	if strings.HasPrefix(text, "/") {
		cmd, arg, _ = strings.Cut(text, " ")
		cmd, _, _ = strings.Cut(cmd, "@") // "/models@MyBot" in groups
		arg = strings.TrimSpace(arg)
	}
	ctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout)
	defer cancel()

	var reply string
	switch cmd {
	case "/start", "/help":
		reply = helpText
	case "/reset":
		s.history = nil
		reply = "Conversation cleared."
	case "/model":
		reply = b.model(ctx, s, arg)
	case "/models":
		reply = b.models(ctx)
	case "/status", "/deploy", "/stop":
		if !b.cfg.ManagementEnabled {
			reply = "Management commands are disabled for this bot."
			break
		}
		reply = b.manage(ctx, cmd, arg)
	default:
		if strings.HasPrefix(cmd, "/") {
			reply = "Unknown command. " + helpText
			break
		}
		_ = b.cfg.API.SendChatAction(ctx, chatID, "typing")
		reply = b.chat(ctx, s, text)
	}
	b.send(ctx, chatID, reply)
}

func (b *Bot) send(ctx context.Context, chatID int64, text string) {
	for _, part := range splitMessage(text, MaxMessageLen) {
		if err := b.cfg.API.SendMessage(context.WithoutCancel(ctx), chatID, part); err != nil {
			b.cfg.Logger.Warn("telegram sendMessage failed", "chat_id", chatID, "error", err)
			return
		}
	}
}

func (b *Bot) model(ctx context.Context, s *session, arg string) string {
	if arg == "" {
		if s.model != "" {
			return "Model for this chat: " + s.model
		}
		def, err := b.cfg.Platform.GetDefaultModel(ctx)
		if err != nil {
			return "Using the default model."
		}
		return "Using the default model: " + def
	}
	ms, err := b.cfg.Platform.InferenceModels(ctx)
	if err != nil {
		return describeError(err)
	}
	for _, m := range ms {
		if m.ID == arg {
			s.model = arg
			return "Model for this chat set to " + arg + "."
		}
	}
	return "Unknown model " + arg + ". Use /models to list them."
}

func (b *Bot) models(ctx context.Context) string {
	ms, err := b.cfg.Platform.InferenceModels(ctx)
	if err != nil {
		return describeError(err)
	}
	if len(ms) == 0 {
		return "No models are available."
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].ID < ms[j].ID })
	var sb strings.Builder
	for _, m := range ms {
		state := "not deployed"
		switch {
		case m.Ready:
			state = "ready"
		case m.Starting:
			state = "starting"
		}
		fmt.Fprintf(&sb, "• %s (%s)\n", m.ID, state)
	}
	return sb.String()
}

func (b *Bot) manage(ctx context.Context, cmd, arg string) string {
	switch cmd {
	case "/status":
		ds, err := b.cfg.Platform.ListDeployments(ctx, "")
		if err != nil {
			return describeError(err)
		}
		var sb strings.Builder
		for _, d := range ds {
			if d.Status == "stopped" {
				continue
			}
			pin := ""
			if d.Pinned {
				pin = ", pinned"
			}
			fmt.Fprintf(&sb, "• %s: %s %s%s\n", d.ID, d.ModelID, d.Status, pin)
		}
		if sb.Len() == 0 {
			return "No active deployments."
		}
		return sb.String()
	case "/deploy":
		if arg == "" {
			return "Usage: /deploy <model>"
		}
		d, err := b.cfg.Platform.CreateDeployment(ctx, apitypes.CreateDeployment{ModelID: arg})
		if err != nil {
			return describeError(err)
		}
		return fmt.Sprintf("Deployment %s of %s is starting. This usually takes a few minutes.", d.ID, d.ModelID)
	case "/stop":
		if arg == "" {
			return "Usage: /stop <deployment id>"
		}
		d, err := b.cfg.Platform.DeploymentAction(ctx, arg, "stop")
		if err != nil {
			return describeError(err)
		}
		return fmt.Sprintf("Deployment %s is %s.", d.ID, d.Status)
	}
	return helpText
}

func (b *Bot) chat(ctx context.Context, s *session, text string) string {
	s.history = append(s.history, apitypes.ChatMessage{Role: "user", Content: apitypes.ChatContent(text)})
	resp, err := b.cfg.Platform.Chat(ctx, apitypes.ChatRequest{Model: s.model, Messages: s.history})
	if err != nil {
		s.history = s.history[:len(s.history)-1]
		return describeError(err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		s.history = s.history[:len(s.history)-1]
		return "The model returned an empty answer."
	}
	answer := string(resp.Choices[0].Message.Content)
	s.history = append(s.history, apitypes.ChatMessage{Role: "assistant", Content: apitypes.ChatContent(answer)})
	if over := len(s.history) - b.cfg.MaxHistory; over > 0 {
		s.history = append([]apitypes.ChatMessage(nil), s.history[over:]...)
	}
	return answer
}

// describeError turns API errors into short, user-facing explanations.
func describeError(err error) string {
	switch {
	case apiclient.IsCode(err, "model_not_deployed"):
		return "That model is not running. Start it with /deploy <model>."
	case apiclient.IsCode(err, "model_starting"):
		return "The model is starting up. Please try again in a minute."
	case apiclient.IsCode(err, "no_default_model"):
		return "No default model is set. Choose one with /model <id>."
	case apiclient.IsCode(err, "deployment_limit_reached"):
		return "The maximum number of running deployments is reached. Stop one first."
	case errors.Is(err, context.DeadlineExceeded):
		return "The request timed out."
	}
	var ae *apiclient.Error
	if errors.As(err, &ae) {
		return "Error: " + ae.Message
	}
	return "The platform is unreachable right now."
}

// splitMessage splits text into chunks of at most limit runes, preferring
// line breaks.
func splitMessage(text string, limit int) []string {
	if text == "" {
		return nil
	}
	var parts []string
	r := []rune(text)
	for len(r) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		parts = append(parts, string(r[:cut]))
		r = r[cut:]
	}
	return append(parts, string(r))
}
