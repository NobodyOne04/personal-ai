package postgres

import (
	"bytes"
	"testing"
)

func TestKeyCipher(t *testing.T) {
	if _, err := newKeyCipher("short"); err == nil {
		t.Fatal("short secret must be refused")
	}
	c, err := newKeyCipher("0123456789abcdef0123456789abcdef") // gitleaks:allow (fake test secret)
	if err != nil {
		t.Fatal(err)
	}
	ct := c.seal("d1", "the-runtime-key")
	if bytes.Contains(ct, []byte("the-runtime-key")) {
		t.Fatal("ciphertext contains the key")
	}
	if bytes.Equal(ct, c.seal("d1", "the-runtime-key")) {
		t.Fatal("each seal must use a fresh nonce")
	}
	if got, err := c.open("d1", ct); err != nil || got != "the-runtime-key" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := c.open("d2", ct); err == nil {
		t.Fatal("ciphertext must be bound to its deployment")
	}
	other, _ := newKeyCipher("another-secret-another-secret-xx") // gitleaks:allow (fake test secret)
	if _, err := other.open("d1", ct); err == nil {
		t.Fatal("a different server secret must not decrypt")
	}
	ct[len(ct)-1] ^= 1
	if _, err := c.open("d1", ct); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}
