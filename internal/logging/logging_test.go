package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewRejectsUnknownOptions(t *testing.T) {
	var b bytes.Buffer
	if _, err := New("loud", "json", &b); err == nil {
		t.Error("unknown level accepted")
	}
	if _, err := New("info", "xml", &b); err == nil {
		t.Error("unknown format accepted")
	}
}

func TestJSONOutputAndLevels(t *testing.T) {
	var b bytes.Buffer
	l, err := New("warn", "json", &b)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hidden")
	l.Warn("shown", KeyDeploymentID, "d1", KeyModelID, "m1", KeyOperation, "start")

	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one line (info filtered), got %q", b.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "shown" || rec["deployment_id"] != "d1" || rec["model_id"] != "m1" || rec["op"] != "start" {
		t.Fatalf("unexpected record: %v", rec)
	}
}

func TestTextFormat(t *testing.T) {
	var b bytes.Buffer
	l, err := New("info", "text", &b)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello", "k", "v")
	if !strings.Contains(b.String(), "msg=hello") || !strings.Contains(b.String(), "k=v") {
		t.Fatalf("unexpected text output: %q", b.String())
	}
}

func TestSensitiveKeysAreRedacted(t *testing.T) {
	var b bytes.Buffer
	l, _ := New("info", "json", &b)
	l.Info("x",
		"token", "t-secret", "api_key", "k-secret", "Authorization", "a-secret", "password", "p-secret",
		"telegram_token", "tg-secret", "runpod_api_key", "r-secret", "database_url", "postgres://u:pw@h/d",
		"input_tokens", 42)

	out := b.String()
	for _, leaked := range []string{"t-secret", "k-secret", "a-secret", "p-secret", "tg-secret", "r-secret", "u:pw"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("%q leaked: %s", leaked, out)
		}
	}
	if !strings.Contains(out, `"input_tokens":42`) {
		t.Fatalf("token *counts* must remain loggable: %s", out)
	}
}
