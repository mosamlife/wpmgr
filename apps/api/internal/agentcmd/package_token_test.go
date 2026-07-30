package agentcmd

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func newPackageTestSigner(t *testing.T) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	s, err := NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// TestPackageTokenClaimWireContract pins the LITERAL claim names and the
// literal cmd value the token carries. Two halves of GH #302 are built in
// parallel and a rename on either side must fail here rather than at runtime.
func TestPackageTokenClaimWireContract(t *testing.T) {
	if CmdUpdatePackage != "update_package" {
		t.Fatalf("CmdUpdatePackage = %q, want %q", CmdUpdatePackage, "update_package")
	}

	s := newPackageTestSigner(t)
	now := time.Unix(1_800_000_000, 0)
	token, jti, err := s.MintPackageToken(now, "11111111-2222-3333-4444-555555555555", "0.61.102", time.Minute)
	if err != nil {
		t.Fatalf("MintPackageToken: %v", err)
	}
	if jti == "" {
		t.Fatal("MintPackageToken returned an empty jti")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(claimsJSON, &raw); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}

	// Every claim name is pinned as a literal string.
	for _, name := range []string{"jti", "exp", "iat", "iss", "aud", "cmd", "ver"} {
		if _, ok := raw[name]; !ok {
			t.Errorf("claim %q is missing from the minted package token", name)
		}
	}
	if raw["cmd"] != "update_package" {
		t.Errorf(`cmd = %v, want "update_package"`, raw["cmd"])
	}
	if raw["aud"] != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("aud = %v, want the site id", raw["aud"])
	}
	if raw["ver"] != "0.61.102" {
		t.Errorf(`ver = %v, want "0.61.102"`, raw["ver"])
	}
	if raw["iss"] != Issuer {
		t.Errorf("iss = %v, want %q", raw["iss"], Issuer)
	}
	// tgt belongs to autologin only and must never appear here.
	if _, ok := raw["tgt"]; ok {
		t.Error("package token carries a tgt claim; it must not")
	}
}

// TestPackageToken_NonPackageCommandsUnchanged proves adding the ver claim did
// not alter the bytes of any command token the AGENT verifies.
func TestPackageToken_NonPackageCommandsUnchanged(t *testing.T) {
	s := newPackageTestSigner(t)
	token, _, err := s.Mint(time.Unix(1_800_000_000, 0), "site-a", "update")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(claimsJSON, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["ver"]; ok {
		t.Error("an ordinary command token gained a ver claim; omitempty must keep it absent")
	}
	if _, ok := raw["tgt"]; ok {
		t.Error("an ordinary command token gained a tgt claim")
	}
}

func TestVerifyPackageToken_RoundTrip(t *testing.T) {
	s := newPackageTestSigner(t)
	now := time.Now()
	token, jti, err := s.MintPackageToken(now, "site-1", "1.2.3", 2*time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := s.VerifyPackageToken(now.Add(30*time.Second), token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.SiteID != "site-1" || got.Version != "1.2.3" || got.JTI != jti {
		t.Fatalf("claims round-tripped wrong: %+v (jti %s)", got, jti)
	}
}

func TestVerifyPackageToken_Expired(t *testing.T) {
	s := newPackageTestSigner(t)
	now := time.Now()
	token, _, err := s.MintPackageToken(now, "site-1", "1.2.3", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := s.VerifyPackageToken(now.Add(61*time.Second), token); err != ErrPackageTokenExpired {
		t.Fatalf("want ErrPackageTokenExpired, got %v", err)
	}
}

func TestVerifyPackageToken_TTLClampedAtMint(t *testing.T) {
	s := newPackageTestSigner(t)
	now := time.Now()
	// Ask for an hour; the mint must clamp to MaxPackageTokenTTL.
	token, _, err := s.MintPackageToken(now, "site-1", "1.2.3", time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := s.VerifyPackageToken(now.Add(MaxPackageTokenTTL+time.Second), token); err != ErrPackageTokenExpired {
		t.Fatalf("token outlived the cap: %v", err)
	}
}

func TestVerifyPackageToken_ForgedSignature(t *testing.T) {
	minter := newPackageTestSigner(t)
	other := newPackageTestSigner(t)
	now := time.Now()
	token, _, err := minter.MintPackageToken(now, "site-1", "1.2.3", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// A token signed by a DIFFERENT key must not verify.
	if _, err := other.VerifyPackageToken(now, token); err != ErrPackageTokenSignature {
		t.Fatalf("want ErrPackageTokenSignature for a foreign key, got %v", err)
	}
}

func TestVerifyPackageToken_TamperedClaims(t *testing.T) {
	s := newPackageTestSigner(t)
	now := time.Now()
	token, _, err := s.MintPackageToken(now, "site-1", "1.2.3", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(token, ".")
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	tampered := strings.Replace(string(claimsJSON), `"ver":"1.2.3"`, `"ver":"9.9.9"`, 1)
	if tampered == string(claimsJSON) {
		t.Fatal("test setup: ver claim not found in the payload")
	}
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(tampered)) + "." + parts[2]
	if _, err := s.VerifyPackageToken(now, forged); err != ErrPackageTokenSignature {
		t.Fatalf("want ErrPackageTokenSignature for tampered claims, got %v", err)
	}
}

func TestVerifyPackageToken_WrongCommandRefused(t *testing.T) {
	s := newPackageTestSigner(t)
	now := time.Now()

	// A perfectly valid `update` command token must not redeem as a package
	// token: same key, same format, different cmd.
	cmdToken, _, err := s.Mint(now, "site-1", "update")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := s.VerifyPackageToken(now, cmdToken); err != ErrPackageTokenClaims {
		t.Fatalf("want ErrPackageTokenClaims for an update command token, got %v", err)
	}

	// Same for an autologin token, which also carries a tgt claim.
	alToken, _, err := s.MintAutologin(now, "site-1", "admin")
	if err != nil {
		t.Fatalf("MintAutologin: %v", err)
	}
	if _, err := s.VerifyPackageToken(now, alToken); err != ErrPackageTokenClaims {
		t.Fatalf("want ErrPackageTokenClaims for an autologin token, got %v", err)
	}
}

func TestVerifyPackageToken_AlgNoneRefused(t *testing.T) {
	s := newPackageTestSigner(t)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(
		`{"jti":"x","exp":9999999999,"iat":1,"iss":"` + Issuer + `","aud":"site-1","cmd":"update_package","ver":"1.2.3"}`))
	if _, err := s.VerifyPackageToken(time.Now(), header+"."+claims+"."); err != ErrPackageTokenMalformed {
		t.Fatalf("want ErrPackageTokenMalformed for alg=none, got %v", err)
	}
}

func TestVerifyPackageToken_Junk(t *testing.T) {
	s := newPackageTestSigner(t)
	now := time.Now()
	for _, tok := range []string{"", "not-a-token", "a.b", "a.b.c.d", strings.Repeat("x", maxPackageTokenBytes+1)} {
		if _, err := s.VerifyPackageToken(now, tok); err == nil {
			t.Errorf("token %q verified; it must not", truncate(tok))
		}
	}
}

func TestMintPackageToken_RequiresSiteAndVersion(t *testing.T) {
	s := newPackageTestSigner(t)
	now := time.Now()
	if _, _, err := s.MintPackageToken(now, "", "1.2.3", time.Minute); err == nil {
		t.Error("minted a package token with no site id")
	}
	if _, _, err := s.MintPackageToken(now, "site-1", "  ", time.Minute); err == nil {
		t.Error("minted a package token with no version")
	}
}

func truncate(s string) string {
	if len(s) <= 24 {
		return s
	}
	return s[:24] + "..."
}
