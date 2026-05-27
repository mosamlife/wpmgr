package agentcmd

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// verifyLikeAgent re-implements the WordPress agent's Connector::verify checks
// (apps/agent/includes/class-connector.php) in Go, so this test proves the
// minted token would pass agent verification byte-for-byte: signature over
// "header.payload" with the CP public key, alg == "EdDSA", exp present/future
// within 60s, jti present.
func verifyLikeAgent(t *testing.T, token string, pub ed25519.PublicKey, now time.Time) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token must have 3 parts, got %d", len(parts))
	}
	signingInput := parts[0] + "." + parts[1]
	sig := mustB64urlDecode(t, parts[2])
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature size = %d, want %d", len(sig), ed25519.SignatureSize)
	}
	// 1. Signature FIRST.
	if !ed25519.Verify(pub, []byte(signingInput), sig) {
		t.Fatal("signature verification failed (agent would reject)")
	}
	// 2. Header alg.
	var header map[string]any
	if err := json.Unmarshal(mustB64urlDecode(t, parts[0]), &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header["alg"] != "EdDSA" {
		t.Fatalf("alg = %v, want EdDSA", header["alg"])
	}
	// 3. Claims: exp window.
	var claims map[string]any
	if err := json.Unmarshal(mustB64urlDecode(t, parts[1]), &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	expF, ok := claims["exp"].(float64)
	if !ok {
		t.Fatal("exp missing/not numeric")
	}
	exp := int64(expF)
	if exp <= now.Unix() {
		t.Fatalf("exp %d <= now %d (agent: expired)", exp, now.Unix())
	}
	if exp > now.Unix()+60 {
		t.Fatalf("exp %d > now+60 %d (agent: exp too far in future)", exp, now.Unix()+60)
	}
	// 4. jti present.
	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		t.Fatal("jti missing/empty")
	}
	return claims
}

// mustB64urlDecode mirrors the agent's base64UrlDecode: re-pad then decode.
func mustB64urlDecode(t *testing.T, s string) []byte {
	t.Helper()
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64url decode %q: %v", s, err)
	}
	return b
}

func TestSignerMintVerifiesLikeAgent(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := &Signer{priv: priv}

	now := time.Now()
	token, jti, err := signer.Mint(now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if jti == "" {
		t.Fatal("empty jti")
	}

	claims := verifyLikeAgent(t, token, pub, now)
	if claims["jti"] != jti {
		t.Fatalf("returned jti %q != claim jti %v", jti, claims["jti"])
	}
}

func TestMintUsesFreshJTIPerCall(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	signer := &Signer{priv: priv}
	_, jti1, _ := signer.Mint(time.Now())
	_, jti2, _ := signer.Mint(time.Now())
	if jti1 == jti2 {
		t.Fatal("jti must be unique per mint (anti-replay)")
	}
}

func TestDecodePrivateKeyValidates(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	b64 := base64.StdEncoding.EncodeToString(priv)
	got, err := DecodePrivateKey(b64)
	if err != nil {
		t.Fatalf("decode valid key: %v", err)
	}
	if len(got) != ed25519.PrivateKeySize {
		t.Fatalf("decoded key size = %d", len(got))
	}
	if _, err := DecodePrivateKey("not-base64!!!"); err == nil {
		t.Fatal("expected error for non-base64")
	}
	if _, err := DecodePrivateKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("expected error for wrong-size key")
	}
}

func TestJWTTTLWithinAgentWindow(t *testing.T) {
	// The agent rejects exp more than 60s in the future; our TTL must be <= 60s.
	if JWTTTL > 60*time.Second {
		t.Fatalf("JWTTTL %v exceeds agent MAX_FUTURE_EXP of 60s", JWTTTL)
	}
}
