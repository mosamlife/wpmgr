package site

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
)

// pairingCodeBytes is the entropy of a pairing code. 20 random bytes -> 32
// base32 chars (~160 bits), well beyond brute-force in the short TTL even
// without the single-use + attempt guards.
const pairingCodeBytes = 20

// pairingCodeTTL is how long a generated code stays valid (~15 min).
const pairingCodeTTL = 15 * time.Minute

// pairingCodeMaxAttempts caps failed validation attempts against a single code
// row before it is refused outright, defense-in-depth against guessing a code
// whose hash happened to collide (it cannot, but the limit also bounds abuse).
const pairingCodeMaxAttempts = 10

var pcEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// PairingCode is a stored pairing-code record (never includes the plaintext).
type PairingCode struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	CreatedBy  *uuid.UUID
	SiteName   string
	Tags       []string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	Attempts   int32
	CreatedAt  time.Time
}

// CreatedPairingCode bundles a freshly created code row with its one-time
// plaintext code, shown to the operator exactly once.
type CreatedPairingCode struct {
	Code      PairingCode
	Plaintext string // high-entropy, shown once, never stored
}

// CreatePairingCodeInput is the validated input for generating a code.
type CreatePairingCodeInput struct {
	TenantID  uuid.UUID `validate:"required"`
	CreatedBy uuid.UUID
	SiteName  string   `validate:"max=200"`
	Tags      []string `validate:"max=50,dive,min=1,max=64"`
}

// generatePairingCode returns a fresh high-entropy plaintext code (uppercase
// base32, no padding).
func generatePairingCode() (string, error) {
	buf := make([]byte, pairingCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return pcEnc.EncodeToString(buf), nil
}

// hashPairingCode returns the hex sha256 of a code's plaintext. Only the hash
// is stored; presented codes are hashed and looked up by hash.
func hashPairingCode(plaintext string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plaintext)))
	return hex.EncodeToString(sum[:])
}

// HashPairingCodeForTest exposes the code-hash function to the external
// integration tests (which need to drive the by-hash consume directly). It is
// the SAME function the production enroll path uses.
func HashPairingCodeForTest(plaintext string) string { return hashPairingCode(plaintext) }
