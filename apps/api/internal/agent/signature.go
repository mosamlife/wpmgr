// Package agent implements the agent side of the WPMgr control plane: the
// Ed25519 signed-request authentication scheme for agent->CP calls (with
// timestamp + nonce anti-replay), the agent metadata/heartbeat endpoints, and
// the periodic connection-health job (River worker).
package agent

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
)

// Signed-request header names. The agent (PHP) MUST mirror these exactly.
const (
	// HeaderAgentKey carries the agent's Ed25519 public key, base64 (std,
	// padded) encoded. It identifies the enrolled site.
	HeaderAgentKey = "X-WPMgr-Agent-Key"
	// HeaderTimestamp carries the request Unix time in seconds (decimal string).
	HeaderTimestamp = "X-WPMgr-Timestamp"
	// HeaderNonce carries a unique per-request token (the jti) for anti-replay.
	HeaderNonce = "X-WPMgr-Nonce"
	// HeaderSignature carries the base64 (std, padded) Ed25519 signature over the
	// canonical message (see CanonicalMessage).
	HeaderSignature = "X-WPMgr-Signature"
)

// CanonicalMessage builds the exact byte string the agent signs and the control
// plane re-derives and verifies. The format is fixed and newline-delimited:
//
//	METHOD\n
//	PATH\n
//	TIMESTAMP\n
//	NONCE\n
//	hex(sha256(body))
//
// METHOD is the upper-case HTTP method; PATH is the request path (no query,
// no host); TIMESTAMP is the decimal Unix-seconds string sent in the timestamp
// header; NONCE is the value sent in the nonce header; the body hash is the
// lower-case hex SHA-256 of the raw request body (the SHA-256 of the empty
// string for an empty body). Binding method+path+body prevents a captured
// signature from being replayed against a different route or with a tampered
// payload; the timestamp+nonce bound the replay window.
func CanonicalMessage(method, path, timestamp, nonce string, body []byte) []byte {
	bodyHash := sha256.Sum256(body)
	var b strings.Builder
	b.WriteString(strings.ToUpper(method))
	b.WriteByte('\n')
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString(timestamp)
	b.WriteByte('\n')
	b.WriteString(nonce)
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(bodyHash[:]))
	return []byte(b.String())
}

// VerifySignature reports whether sig is a valid Ed25519 signature by pub over
// the canonical message. pubKeyB64 and sigB64 are base64 std (padded).
func VerifySignature(pubKeyB64, sigB64, method, path, timestamp, nonce string, body []byte) bool {
	pub, err := DecodePublicKey(pubKeyB64)
	if err != nil {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, CanonicalMessage(method, path, timestamp, nonce, body), sig)
}

// DecodePublicKey parses a base64-std Ed25519 public key and validates its size.
func DecodePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, errInvalidKeySize
	}
	return ed25519.PublicKey(raw), nil
}

// ParseUnixSeconds parses a decimal Unix-seconds string.
func ParseUnixSeconds(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

type sentinel string

func (e sentinel) Error() string { return string(e) }

const errInvalidKeySize sentinel = "ed25519: invalid public key size"
