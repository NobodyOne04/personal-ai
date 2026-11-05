package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBase is Telegram's Bot API endpoint.
const DefaultAPIBase = "https://api.telegram.org"

// Update is the subset of a Telegram update the bot uses.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is a Telegram message.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

// User is a Telegram user.
type User struct {
	ID int64 `json:"id"`
}

// Chat is a Telegram chat.
type Chat struct {
	ID int64 `json:"id"`
}

// BotAPI is a minimal Telegram Bot API client. The bot token is part of every
// URL, so errors never include URLs.
type BotAPI struct {
	base   string
	token  string
	client *http.Client
}

// NewBotAPI returns a client. base may be empty for DefaultAPIBase.
func NewBotAPI(token, base string, client *http.Client) (*BotAPI, error) {
	if token == "" {
		return nil, errors.New("telegram: bot token is required")
	}
	if base == "" {
		base = DefaultAPIBase
	}
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	return &BotAPI{base: strings.TrimRight(base, "/"), token: token, client: client}, nil
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

func (a *BotAPI) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/bot"+a.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: build request failed", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL contains the token
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	var r apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d, undecodable response", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %d %s", method, r.ErrorCode, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// GetUpdates long-polls for updates after offset.
func (a *BotAPI) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	var out []Update
	err := a.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeoutSec, "allowed_updates": []string{"message"},
	}, &out)
	return out, err
}

// SendMessage sends plain text (no markup parsing, so model output can never
// break formatting or inject links).
func (a *BotAPI) SendMessage(ctx context.Context, chatID int64, text string) error {
	return a.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil)
}

// SendChatAction shows an activity indicator such as "typing".
func (a *BotAPI) SendChatAction(ctx context.Context, chatID int64, action string) error {
	return a.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, nil)
}
