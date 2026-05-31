// Package agentcmd implements the control-plane side of the CP->agent command
// protocol: minting the Ed25519-signed bearer JWT the WordPress agent verifies
// (see apps/agent/includes/class-connector.php), and POSTing signed `update`
// and `rollback` commands to an enrolled site's agent over the SSRF-hardened
// HTTP client (ADR-009).
//
// The JWT is a compact JWS the agent verifies BEFORE trusting any claim:
//
//	base64url(header) . base64url(payload) . base64url(Ed25519(signingInput))
//
//	header  = {"alg":"EdDSA","typ":"JWT"}     (agent checks alg == "EdDSA")
//	payload = {"jti","exp","iat","iss","aud","cmd"}  (agent checks exp window,
//	          jti, aud == own enrollment site_id, cmd == dispatched command)
//	sig     = Ed25519 detached signature over the ASCII "header.payload" string,
//	          using the control-plane signing PRIVATE key, base64url(no pad).
//
// The agent (Connector::verify) requires: exp present and numeric, exp > now,
// exp <= now+60s, jti present/non-empty (anti-replay), aud == its own
// enrollment site_id, cmd == the dispatched command path segment, and a valid
// signature over the signing input with the stored CP public key. We therefore
// set exp to now+JWTTTL (<= 60s), a fresh random jti per command, aud to the
// target site's canonical UUID string, and cmd to the literal command name.
// Binding aud+cmd defeats cross-tenant/cross-command replay of a captured token
// under the single global CP signing keypair (see contract.go for the exact,
// authoritative claim set the agent mirrors).
package agentcmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// JWTTTL is the lifetime of a minted command JWT. It MUST be <= 60s because the
// agent rejects any token whose exp is more than 60s in the future
// (Connector::MAX_FUTURE_EXP). We use 45s to leave clock-skew headroom.
const JWTTTL = 45 * time.Second

// Issuer is the iss claim placed on minted tokens (informational; the current
// agent does not verify it, but it documents provenance).
const Issuer = "wpmgr-control-plane"

// DecodePrivateKey parses a base64-std (padded) 64-byte raw Ed25519 private key
// (seed||public, the Go ed25519.PrivateKey layout) and validates its size.
func DecodePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("decode signing key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key must be %d bytes, got %d", ed25519.PrivateKeySize, len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}

// Signer mints CP->agent command JWTs with the control-plane private key.
type Signer struct {
	priv ed25519.PrivateKey
}

// NewSigner builds a Signer from a base64-std raw Ed25519 private key.
func NewSigner(privB64 string) (*Signer, error) {
	priv, err := DecodePrivateKey(privB64)
	if err != nil {
		return nil, err
	}
	return &Signer{priv: priv}, nil
}

// jwtHeader is the fixed JOSE header. typ is informational; the agent only
// checks alg == "EdDSA".
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// jwtClaims is the claim set the agent requires plus provenance fields. aud
// binds the token to a single target site (the agent's stable enrollment
// site_id) and cmd binds it to a single command, so a captured token cannot be
// replayed against a different tenant's site or repurposed for a different
// command within the exp window.
//
// Tgt is set ONLY by the "autologin" command (ADR-031): it carries the WP
// username the agent should establish a session as. An empty Tgt means "agent
// picks the first administrator". omitempty keeps the claim absent for all
// other commands so the existing M3/M4 contract is byte-identical.
type jwtClaims struct {
	JTI string `json:"jti"`
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	Iss string `json:"iss"`
	Aud string `json:"aud"`
	Cmd string `json:"cmd"`
	Tgt string `json:"tgt,omitempty"`
}

// Mint produces a signed compact JWT valid for JWTTTL from now, bound to the
// target site (aud) and the named command (cmd). jti is a fresh random 128-bit
// value (hex) so each command is single-use under the agent's anti-replay
// window. aud MUST be the target site's canonical lowercase UUID string (the
// agent compares it to its own enrollment site_id); cmd MUST be the literal
// command name ("update"|"rollback") matching the dispatched path segment. The
// returned jti lets the caller correlate/log without re-parsing the token.
func (s *Signer) Mint(now time.Time, aud, cmd string) (token string, jti string, err error) {
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", "", fmt.Errorf("generate jti: %w", err)
	}
	jti = hex.EncodeToString(jtiBytes)
	token, err = s.mintWithJTI(now, aud, cmd, jti, "")
	return token, jti, err
}

// MintAutologin produces a signed compact JWT for the Phase 5.5 one-click login
// flow (ADR-031). The jti is a 256-bit random base64url-no-pad value (the
// caller stores it as the nonce id in PG + Redis and the agent presents it back
// on the consume callback). aud is the target site's enrollment UUID; tgt is
// the WP username the agent should log in as ("" = agent picks the first
// administrator). cmd is fixed to "autologin" so the agent verifier rejects
// any cross-command reuse of a captured token. Returned jti is the SAME value
// embedded in the claims, so the caller can persist it without re-parsing.
func (s *Signer) MintAutologin(now time.Time, aud, targetWPUser string) (token, jti string, err error) {
	jtiBytes := make([]byte, 32)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", "", fmt.Errorf("generate autologin jti: %w", err)
	}
	jti = base64.RawURLEncoding.EncodeToString(jtiBytes)
	token, err = s.mintWithJTI(now, aud, CmdAutologin, jti, targetWPUser)
	return token, jti, err
}

// mintWithJTI is the shared signing primitive: marshal header+claims, sign the
// canonical "header.payload" with the CP private key, return the compact JWT.
func (s *Signer) mintWithJTI(now time.Time, aud, cmd, jti, tgt string) (string, error) {
	header := jwtHeader{Alg: "EdDSA", Typ: "JWT"}
	claims := jwtClaims{
		JTI: jti,
		Exp: now.Add(JWTTTL).Unix(),
		Iat: now.Unix(),
		Iss: Issuer,
		Aud: aud,
		Cmd: cmd,
		Tgt: tgt,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal jwt claims: %w", err)
	}

	signingInput := b64url(headerJSON) + "." + b64url(claimsJSON)
	sig := ed25519.Sign(s.priv, []byte(signingInput))
	return signingInput + "." + b64url(sig), nil
}

// CmdAutologin is the literal cmd claim value for the Phase 5.5 one-click
// login JWT (ADR-031). The agent verifier MUST reject a token whose cmd is not
// exactly this string when serving the autologin REST route.
const CmdAutologin = "autologin"

// CmdUpdateManifest is the cmd claim value carried inside a CP-driven agent
// self-update manifest (ADR-042). The agent's UpdateChecker MUST reject any
// manifest whose cmd is not exactly this string.
const CmdUpdateManifest = "update_manifest"

// SignManifest returns a DETACHED Ed25519 signature (base64url, no padding) over
// the exact payload bytes — the CP-driven self-update manifest (ADR-042).
//
// Unlike Mint this is NOT a JWT and deliberately carries no 60s exp clamp: the
// agent caches a verified manifest for hours and verifies the detached signature
// over the canonical JSON the CP returns (base64url-encoded alongside this
// signature), so it must not be bound by Connector::MAX_FUTURE_EXP. Freshness +
// anti-replay are enforced by the manifest's OWN iat/exp/jti claims, which the
// agent re-checks in code (it cannot route this through verifyCommand). The
// agent verifies with sodium_crypto_sign_verify_detached against the stored CP
// public key — the same primitive Connector::verify already uses.
func (s *Signer) SignManifest(payload []byte) string {
	return b64url(ed25519.Sign(s.priv, payload))
}

// b64url base64url-encodes without padding (the JWS compact-serialization form
// the agent's base64UrlDecode tolerates — it re-pads on decode).
func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
