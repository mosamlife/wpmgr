package agentcmd

// GH #302: the short-lived bearer token that authorises ONE agent package
// download from the control plane's own streaming route.
//
// Why this token exists at all. A mirrored agent package lives in the operator's
// OWN object storage, whose hostname differs per install and can therefore never
// ship as an agent default. The fix is for the control plane to SERVE the package
// itself, from a host the agent already trusts (the one it is enrolled with). But
// the agent sends NO Authorization header on the package download (it is a bare
// wp_remote_get on the manifest's package_url, with redirection => 0), so that
// streaming route cannot be behind the agent's signed-request middleware. An
// unauthenticated route that streams a multi-megabyte zip is a bandwidth
// amplification vector, so the URL itself has to carry the authorisation.
//
// This is NOT a new token format. It is the SAME compact EdDSA JWS every CP->agent
// command already uses (see jwt.go), minted by the SAME Signer with the SAME key,
// and it reuses the existing aud/cmd binding wholesale:
//
//   - cmd is pinned to CmdUpdatePackage, so no command token minted for `update`,
//     `rollback` or `autologin` can be redeemed here, and this token cannot be
//     replayed at an agent as any of those (the agent checks cmd itself).
//   - aud is the site the download is for, exactly as on every other token.
//   - ver pins the token to ONE release version, so a token stays valid only for
//     the build it was minted for and cannot be carried across a release.
//
// The one deliberate difference from Mint is the lifetime. Mint hard-codes
// JWTTTL (45s) because the AGENT rejects an exp more than 60s out. This token is
// verified by the CONTROL PLANE, never by the agent, so that clamp does not
// apply; instead its TTL matches the presigned object-storage URL it replaces,
// which keeps the manifest's existing expiry semantics unchanged.
//
// Replay: within its TTL this token is bearer, exactly like the presigned URL it
// replaces (anyone holding that URL could fetch it too). It is deliberately not
// single-use, because a download that is retried after a network drop must still
// work inside the window. It is not unlimited either: the download route counts
// redemptions per jti in process memory and refuses a token that has been
// redeemed more than a handful of times inside its window, so "not single-use"
// cannot become "an unbounded supply of package-sized responses". That counter is
// per instance and needs no shared state; see
// internal/agent/update_package_limits.go.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CmdUpdatePackage is the cmd claim value on an agent package download token.
// It is a CP-verified value: no agent ever sees a token carrying it, and the
// control plane refuses to serve the package for any other cmd.
const CmdUpdatePackage = "update_package"

// MaxPackageTokenTTL bounds how long a download token may live. It matches
// ADR-042's 5-minute cap on the presigned package URL this token replaces, so
// the exposure window of the two designs is identical. Enforced at BOTH mint
// (the requested ttl is clamped) and verify (a token whose own exp-iat span
// exceeds it is refused, so a token minted by some future looser caller still
// cannot buy a longer life).
const MaxPackageTokenTTL = 5 * time.Minute

// packageTokenSkew is the tolerance for a token whose iat is slightly ahead of
// the verifier's clock (replicas are not perfectly synchronised).
const packageTokenSkew = 60 * time.Second

// maxPackageTokenBytes bounds the token string before any decoding work is done.
// A real token is a few hundred bytes.
const maxPackageTokenBytes = 4096

// Package token verification failures. They are deliberately distinguishable to
// the CALLER for logging, while the HTTP surface collapses them all into one
// opaque 401: telling an unauthenticated caller which check failed is free
// oracle access to the token format.
var (
	// ErrPackageTokenMalformed: not three base64url segments, or the JOSE header
	// is not the expected EdDSA header.
	ErrPackageTokenMalformed = errors.New("agentcmd: package token is malformed")
	// ErrPackageTokenSignature: the signature does not verify under the control
	// plane's own public key. This is the forged/tampered case.
	ErrPackageTokenSignature = errors.New("agentcmd: package token signature is invalid")
	// ErrPackageTokenClaims: the signature is good but the claim set is not a
	// package token (wrong cmd, missing aud/ver/jti, impossible iat/exp span).
	ErrPackageTokenClaims = errors.New("agentcmd: package token claims are invalid")
	// ErrPackageTokenExpired: the signature and claims are good, the window has
	// passed.
	ErrPackageTokenExpired = errors.New("agentcmd: package token has expired")
)

// PackageToken is the verified content of a package download token. Every field
// is covered by the signature.
type PackageToken struct {
	// SiteID is the aud claim: the site this download was authorised for, as a
	// canonical UUID string. The caller MUST compare it to the site named in the
	// request.
	SiteID string
	// Version is the ver claim: the single agent release version this token
	// authorises. The caller MUST compare it to the version actually published.
	Version string
	// JTI is the token's unique id, for correlation in logs.
	JTI string
	// IssuedAt / ExpiresAt are the iat / exp claims.
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// MintPackageToken produces a signed download token for one site and one agent
// release version, valid for ttl (clamped to MaxPackageTokenTTL; a non-positive
// ttl takes the cap). siteID must be the target site's canonical UUID string and
// version the exact published version string. The returned jti lets the caller
// correlate the mint with the later download without re-parsing the token.
func (s *Signer) MintPackageToken(now time.Time, siteID, version string, ttl time.Duration) (token string, jti string, err error) {
	if strings.TrimSpace(siteID) == "" {
		return "", "", errors.New("agentcmd: package token needs a site id")
	}
	if strings.TrimSpace(version) == "" {
		return "", "", errors.New("agentcmd: package token needs a version")
	}
	if ttl <= 0 || ttl > MaxPackageTokenTTL {
		ttl = MaxPackageTokenTTL
	}

	jtiBytes := make([]byte, 16)
	if _, rerr := rand.Read(jtiBytes); rerr != nil {
		return "", "", fmt.Errorf("generate package token jti: %w", rerr)
	}
	jti = hex.EncodeToString(jtiBytes)

	token, err = s.mintClaims(now, ttl, siteID, CmdUpdatePackage, jti, "", version)
	return token, jti, err
}

// VerifyPackageToken checks a package download token against the control
// plane's own signing key and returns its verified claims.
//
// Order matters: the signature is verified BEFORE any claim is read, so no
// unsigned bytes ever influence a decision. Only after that are the claims
// checked, and cmd is checked first, so a valid token for a different purpose is
// rejected as a package token rather than being partially honoured.
func (s *Signer) VerifyPackageToken(now time.Time, token string) (PackageToken, error) {
	if token == "" || len(token) > maxPackageTokenBytes {
		return PackageToken{}, ErrPackageTokenMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return PackageToken{}, ErrPackageTokenMalformed
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return PackageToken{}, ErrPackageTokenMalformed
	}
	var header jwtHeader
	if jerr := json.Unmarshal(headerJSON, &header); jerr != nil {
		return PackageToken{}, ErrPackageTokenMalformed
	}
	// alg is pinned to the one algorithm this key can produce. Refusing anything
	// else here is what stops the classic "alg": "none" downgrade from ever
	// reaching the verification below.
	if header.Alg != "EdDSA" {
		return PackageToken{}, ErrPackageTokenMalformed
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return PackageToken{}, ErrPackageTokenMalformed
	}
	pub, ok := s.priv.Public().(ed25519.PublicKey)
	if !ok {
		return PackageToken{}, ErrPackageTokenSignature
	}
	if len(sig) != ed25519.SignatureSize {
		return PackageToken{}, ErrPackageTokenSignature
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return PackageToken{}, ErrPackageTokenSignature
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return PackageToken{}, ErrPackageTokenMalformed
	}
	var claims jwtClaims
	if jerr := json.Unmarshal(claimsJSON, &claims); jerr != nil {
		return PackageToken{}, ErrPackageTokenMalformed
	}

	// cmd binding: this is what makes a captured `update` / `rollback` /
	// `autologin` token useless here, and this token useless there.
	if claims.Cmd != CmdUpdatePackage {
		return PackageToken{}, ErrPackageTokenClaims
	}
	if claims.Iss != Issuer {
		return PackageToken{}, ErrPackageTokenClaims
	}
	if claims.Aud == "" || claims.Ver == "" || claims.JTI == "" {
		return PackageToken{}, ErrPackageTokenClaims
	}
	// A package token never carries tgt. Refusing a stray one keeps the claim set
	// exactly what this path declares it to be.
	if claims.Tgt != "" {
		return PackageToken{}, ErrPackageTokenClaims
	}
	if claims.Iat <= 0 || claims.Exp <= 0 || claims.Exp <= claims.Iat {
		return PackageToken{}, ErrPackageTokenClaims
	}
	if claims.Exp-claims.Iat > int64(MaxPackageTokenTTL/time.Second) {
		return PackageToken{}, ErrPackageTokenClaims
	}

	issuedAt := time.Unix(claims.Iat, 0)
	expiresAt := time.Unix(claims.Exp, 0)
	// Issued in the future beyond tolerable skew: not a token this verifier can
	// reason about.
	if issuedAt.After(now.Add(packageTokenSkew)) {
		return PackageToken{}, ErrPackageTokenClaims
	}
	if !now.Before(expiresAt) {
		return PackageToken{}, ErrPackageTokenExpired
	}

	return PackageToken{
		SiteID:    claims.Aud,
		Version:   claims.Ver,
		JTI:       claims.JTI,
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}, nil
}
