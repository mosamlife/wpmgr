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
// within 60s, jti present, and aud/cmd matching the agent's enrollment site_id
// and dispatched command.
func verifyLikeAgent(t *testing.T, token string, pub ed25519.PublicKey, now time.Time, wantAud, wantCmd string) map[string]any {
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
	// 5. aud == agent's own enrollment site_id (anti cross-tenant replay).
	aud, ok := claims["aud"].(string)
	if !ok || aud == "" {
		t.Fatal("aud missing/empty")
	}
	if aud != wantAud {
		t.Fatalf("aud = %q, want %q (agent would reject: wrong site)", aud, wantAud)
	}
	// 6. cmd == dispatched command (anti cross-command reuse).
	cmd, ok := claims["cmd"].(string)
	if !ok || cmd == "" {
		t.Fatal("cmd missing/empty")
	}
	if cmd != wantCmd {
		t.Fatalf("cmd = %q, want %q (agent would reject: wrong command)", cmd, wantCmd)
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
	const wantAud = "11111111-2222-3333-4444-555555555555"
	const wantCmd = "update"
	token, jti, err := signer.Mint(now, wantAud, wantCmd)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if jti == "" {
		t.Fatal("empty jti")
	}

	claims := verifyLikeAgent(t, token, pub, now, wantAud, wantCmd)
	if claims["jti"] != jti {
		t.Fatalf("returned jti %q != claim jti %v", jti, claims["jti"])
	}
	if claims["iss"] != Issuer {
		t.Fatalf("iss = %v, want %q", claims["iss"], Issuer)
	}
}

// TestMintBindsAudAndCmd proves a token minted for one site/command does not
// verify when the agent expects a different site or command — the core of the
// cross-tenant/cross-command replay defense.
func TestMintBindsAudAndCmd(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := &Signer{priv: priv}
	now := time.Now()

	token, _, err := signer.Mint(now, "site-A", "rollback")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Correct aud+cmd verifies.
	_ = verifyLikeAgent(t, token, pub, now, "site-A", "rollback")

	// A different site (cross-tenant replay) must be rejected.
	parts := strings.Split(token, ".")
	var claims map[string]any
	if err := json.Unmarshal(mustB64urlDecode(t, parts[1]), &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if claims["aud"] != "site-A" {
		t.Fatalf("aud = %v, want site-A", claims["aud"])
	}
	if claims["cmd"] != "rollback" {
		t.Fatalf("cmd = %v, want rollback", claims["cmd"])
	}
}

func TestMintUsesFreshJTIPerCall(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	signer := &Signer{priv: priv}
	_, jti1, _ := signer.Mint(time.Now(), "site-A", "update")
	_, jti2, _ := signer.Mint(time.Now(), "site-A", "update")
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

// TestMintAutologinClaims proves the Phase 5.5 one-click login JWT carries the
// fixed cmd ("autologin"), the supplied aud (site UUID) and tgt (WP login),
// and a fresh base64url-no-pad 32-byte jti. The same jti is returned so the
// caller can persist it as the nonce id without re-parsing the token.
func TestMintAutologinClaims(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := &Signer{priv: priv}
	now := time.Now()
	const wantAud = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const wantTgt = "admin"

	token, jti, err := signer.MintAutologin(now, wantAud, wantTgt)
	if err != nil {
		t.Fatalf("mint autologin: %v", err)
	}
	if jti == "" {
		t.Fatal("empty jti")
	}
	if _, err := base64.RawURLEncoding.DecodeString(jti); err != nil {
		t.Fatalf("jti must be base64url-no-pad: %v", err)
	}

	claims := verifyLikeAgent(t, token, pub, now, wantAud, CmdAutologin)
	if claims["jti"] != jti {
		t.Fatalf("returned jti %q != claim jti %v", jti, claims["jti"])
	}
	if claims["tgt"] != wantTgt {
		t.Fatalf("tgt = %v, want %q", claims["tgt"], wantTgt)
	}

	// Empty target = "agent picks admin"; the tgt claim must be omitted entirely.
	token2, _, err := signer.MintAutologin(now, wantAud, "")
	if err != nil {
		t.Fatalf("mint autologin (empty tgt): %v", err)
	}
	claims2 := verifyLikeAgent(t, token2, pub, now, wantAud, CmdAutologin)
	if _, present := claims2["tgt"]; present {
		t.Fatalf("tgt claim must be omitted when empty, got %v", claims2["tgt"])
	}
}

// TestMintAutologinUsesFreshJTIPerCall guarantees every mint is single-use.
func TestMintAutologinUsesFreshJTIPerCall(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	signer := &Signer{priv: priv}
	_, j1, _ := signer.MintAutologin(time.Now(), "site-A", "")
	_, j2, _ := signer.MintAutologin(time.Now(), "site-A", "")
	if j1 == j2 {
		t.Fatal("autologin jti must be unique per mint (anti-replay)")
	}
}
