// Package cryptbox holds the control plane's shared secret-at-rest primitive:
// an age (X25519) identity used to encrypt customer/operator secrets before they
// touch Postgres. It was extracted from internal/sitedestination so multiple
// domains (site destinations, SMTP settings, per-site email, object-cache
// credentials, TOTP 2FA secrets, future alert-channel secrets) can share one
// encryption boundary without importing each other.
//
// The identity's private key never leaves the control plane and is never shipped
// to an agent. Every environment needs a STABLE identity across restarts —
// NewAgeIdentity("") deliberately does not provide one (see its doc comment);
// production code must resolve a stable key via an explicit
// WPMGR_SITE_DEST_AGE_SECRET or, absent that, DeriveAgeIdentity seeded from the
// (already-validated, restart-stable) session secret — see cmd/wpmgr's
// resolveAgeIdentity, the sole production wiring point for the shared identity.
package cryptbox

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"golang.org/x/crypto/hkdf"
)

// AgeIdentity wraps an age X25519 identity (for decryption) plus its recipient
// (for encryption). Encrypt at write time, Decrypt at read time.
type AgeIdentity struct {
	identity  *age.X25519Identity
	recipient *age.X25519Recipient
}

// NewAgeIdentity parses an age secret key (the AGE-SECRET-KEY-1... format). An
// empty key produces a fresh ephemeral identity — a NEW random key every call,
// which cannot decrypt anything encrypted by a previous call. This only exists
// for tests and other short-lived, non-persisting callers; production code
// must never pass an empty key to this function (use DeriveAgeIdentity, or an
// explicit key, so ciphertext survives a restart — see cmd/wpmgr's
// resolveAgeIdentity).
func NewAgeIdentity(secretKey string) (*AgeIdentity, error) {
	if strings.TrimSpace(secretKey) == "" {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return nil, fmt.Errorf("generate ephemeral age identity: %w", err)
		}
		return &AgeIdentity{identity: id, recipient: id.Recipient()}, nil
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(secretKey))
	if err != nil {
		return nil, fmt.Errorf("parse age identity: %w", err)
	}
	return &AgeIdentity{identity: id, recipient: id.Recipient()}, nil
}

// ageX25519ScalarSize is the length in bytes of an X25519 secret scalar
// (age's on-disk secret key encodes exactly this many bytes after the
// "AGE-SECRET-KEY-" bech32 human-readable part).
const ageX25519ScalarSize = 32

// DeriveAgeIdentity deterministically derives a STABLE age X25519 identity
// from seed material using HKDF-SHA256 (RFC 5869), with info as a fixed
// domain-separation label. The same (seed, info) pair always yields the same
// identity; a restart that re-derives from the same stable seed (e.g. the
// control plane's session secret) recovers the SAME key and can decrypt
// everything the previous process encrypted — unlike an ephemeral random
// identity, which cannot.
//
// The derivation is one-way: HKDF-SHA256 is a cryptographic extract-and-expand
// KDF, so the resulting 32-byte X25519 scalar does not allow recovering seed.
// info provides domain separation — deriving with a different info label from
// the same seed yields an unrelated, independent identity, so this function
// can safely be reused for other stable-identity needs without key reuse
// across purposes as long as each purpose has its own info label.
//
// seed should be secret, high-entropy material that is itself already
// validated to be non-empty and long enough (the control plane's session
// secret, validated by config.ValidateSessionSecret, qualifies). No salt is
// used (HKDF's salt is optional and primarily useful when the input keying
// material is not already high-entropy secret data); the info label is the
// only ceremony required here.
func DeriveAgeIdentity(seed []byte, info string) (*AgeIdentity, error) {
	if len(seed) == 0 {
		return nil, fmt.Errorf("derive age identity: seed is empty")
	}
	if strings.TrimSpace(info) == "" {
		return nil, fmt.Errorf("derive age identity: info label is empty")
	}
	h := hkdf.New(sha256.New, seed, nil, []byte(info))
	scalar := make([]byte, ageX25519ScalarSize)
	if _, err := io.ReadFull(h, scalar); err != nil {
		return nil, fmt.Errorf("derive age identity: hkdf expand: %w", err)
	}
	secretKey, err := encodeAgeSecretKey(scalar)
	if err != nil {
		return nil, fmt.Errorf("derive age identity: encode secret key: %w", err)
	}
	id, err := age.ParseX25519Identity(secretKey)
	if err != nil {
		return nil, fmt.Errorf("derive age identity: derived key failed to parse (this is a bug in the derivation, not caller input): %w", err)
	}
	return &AgeIdentity{identity: id, recipient: id.Recipient()}, nil
}

// Encrypt age-encrypts plaintext for the wrapped recipient.
func (a *AgeIdentity) Encrypt(plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, a.recipient)
	if err != nil {
		return nil, fmt.Errorf("age encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("age write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("age close: %w", err)
	}
	return buf.Bytes(), nil
}

// RecipientString returns the public age recipient ("age1...") for surfacing in
// a UI so operators can verify the encrypted-at-rest claim out of band, or "" if
// the identity is nil.
func (a *AgeIdentity) RecipientString() string {
	if a == nil || a.recipient == nil {
		return ""
	}
	return a.recipient.String()
}

// Decrypt age-decrypts ciphertext produced by Encrypt with the same identity.
func (a *AgeIdentity) Decrypt(ciphertext []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), a.identity)
	if err != nil {
		return nil, fmt.Errorf("age decrypt: %w", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("age read: %w", err)
	}
	return out, nil
}
