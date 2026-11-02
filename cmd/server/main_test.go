package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func goodEnv(addr string) func(string) string {
	m := map[string]string{
		"PERSONAL_AI_SERVER_ADDR":      addr,
		"PERSONAL_AI_DATABASE_URL":     "postgres://u:pw@localhost/db",
		"PERSONAL_AI_MANAGEMENT_TOKEN": strings.Repeat("m", 24),
		"PERSONAL_AI_INFERENCE_TOKEN":  strings.Repeat("i", 24),
		"PERSONAL_AI_LOG_FORMAT":       "text",
	}
	return func(k string) string { return m[k] }
}

func TestRunRefusesToStartWithoutSecrets(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), nil, func(string) string { return "" }, &stderr)
	if err == nil || !strings.Contains(err.Error(), "PERSONAL_AI_MANAGEMENT_TOKEN") {
		t.Fatalf("want a missing-secret error, got %v", err)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(context.Background(), []string{"-bogus"}, goodEnv(":0"), &stderr); err == nil {
		t.Fatal("expected flag error")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestRunServesHealthAndStopsOnContextCancel(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, goodEnv(addr), &stderr) }()

	url := fmt.Sprintf("http://%s/healthz", addr)
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never became reachable: %v\nlogs: %s", err, stderr.String())
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
	if strings.Contains(stderr.String(), "pw@") || strings.Contains(stderr.String(), strings.Repeat("m", 24)) {
		t.Fatalf("secrets leaked into logs: %s", stderr.String())
	}
}
