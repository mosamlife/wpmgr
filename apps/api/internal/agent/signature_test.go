package agent

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

// signFor builds the headers an agent would send, signing the canonical message.
func signFor(t *testing.T, priv ed25519.PrivateKey, method, path, ts, nonce string, body []byte) string {
	t.Helper()
	msg := CanonicalMessage(method, path, ts, nonce, body)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

func TestVerifySignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	body := []byte(`{"wp_version":"6.5"}`)
	ts, nonce := "1700000000", "nonce-abcdef12"
	sig := signFor(t, priv, "POST", "/agent/v1/metadata", ts, nonce, body)

	tests := []struct {
		name                          string
		pub, sig, method, path, ts, n string
		body                          []byte
		want                          bool
	}{
		{"valid", pubB64, sig, "POST", "/agent/v1/metadata", ts, nonce, body, true},
		{"tampered body", pubB64, sig, "POST", "/agent/v1/metadata", ts, nonce, []byte("evil"), false},
		{"wrong path", pubB64, sig, "POST", "/agent/v1/heartbeat", ts, nonce, body, false},
		{"wrong method", pubB64, sig, "GET", "/agent/v1/metadata", ts, nonce, body, false},
		{"wrong nonce", pubB64, sig, "POST", "/agent/v1/metadata", ts, "other", body, false},
		{"wrong ts", pubB64, sig, "POST", "/agent/v1/metadata", "1700000001", nonce, body, false},
		{"garbage sig", pubB64, "not-base64!!", "POST", "/agent/v1/metadata", ts, nonce, body, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VerifySignature(tt.pub, tt.sig, tt.method, tt.path, tt.ts, tt.n, tt.body)
			if got != tt.want {
				t.Fatalf("VerifySignature = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVerifySignatureWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	body := []byte("{}")
	ts, nonce := "1700000000", "nonce-12345678"
	sig := signFor(t, priv, "POST", "/agent/v1/heartbeat", ts, nonce, body)
	// Verifying against a DIFFERENT public key must fail.
	if VerifySignature(base64.StdEncoding.EncodeToString(otherPub), sig, "POST", "/agent/v1/heartbeat", ts, nonce, body) {
		t.Fatal("signature verified under the wrong public key")
	}
}

func TestDecodePublicKeyRejectsBadSize(t *testing.T) {
	if _, err := DecodePublicKey(base64.StdEncoding.EncodeToString([]byte("too-short"))); err == nil {
		t.Fatal("expected error for undersized key")
	}
	if _, err := DecodePublicKey("!!!not base64"); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}
