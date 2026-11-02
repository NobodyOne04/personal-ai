// Package logging builds the application's structured logger.
//
// It adds a defence-in-depth redaction layer: attributes whose key names a
// credential are masked even if a caller passes one by mistake. Prompts and
// generated text must not be logged at all; that is a caller rule this
// package cannot enforce.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Standard attribute keys, so logs are consistent across the codebase.
const (
	KeyRequestID     = "request_id"
	KeyDeploymentID  = "deployment_id"
	KeyModelID       = "model_id"
	KeyOperation     = "op"
	KeyErrorCategory = "error_category"
	KeyDuration      = "duration"
)

const redacted = "[REDACTED]"

var sensitiveKeys = map[string]bool{
	"token": true, "secret": true, "password": true, "passwd": true,
	"api_key": true, "apikey": true, "authorization": true, "credential": true,
	"credentials": true, "database_url": true, "dsn": true, "cookie": true,
}

var sensitiveSuffixes = []string{"_token", "_secret", "_password", "_api_key", "_apikey", "_credential"}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	if sensitiveKeys[k] {
		return true
	}
	for _, s := range sensitiveSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

// New returns a logger writing to w. level is debug, info, warn or error;
// format is json or text.
func New(level, format string, w io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}

	opts := &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if isSensitive(a.Key) {
				return slog.String(a.Key, redacted)
			}
			return a
		},
	}
	switch strings.ToLower(format) {
	case "json", "":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
}
