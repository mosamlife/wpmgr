package sharing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// generateSecureToken creates a 32-byte cryptographically random token.
// Returns the raw URL-safe hex token (64 chars) and its SHA-256 hex hash.
// The raw token is returned once to the caller (for the accept link / email);
// only the hash is stored.
func generateSecureToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("rand token: %w", err)
	}
	raw = hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(sum[:])
	return raw, hash, nil
}
