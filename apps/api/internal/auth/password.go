// Package auth implements human and machine authentication for WPMgr: argon2id
// password hashing, the session manager (SCS), the email+password and OIDC
// login flows, and the request principal middleware that replaces the old
// X-Tenant-ID stub.
package auth

import (
	"github.com/alexedwards/argon2id"
)

// passwordParams are the argon2id parameters. They meet/exceed the OWASP
// minimums for the 19 MiB profile: memory >= 19 MiB, iterations >= 2,
// parallelism == 1 (ADR: alexedwards/argon2id).
var passwordParams = &argon2id.Params{
	Memory:      19 * 1024, // 19 MiB, in KiB
	Iterations:  2,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}

// HashPassword returns an argon2id PHC-format hash of the plaintext password.
func HashPassword(plain string) (string, error) {
	return argon2id.CreateHash(plain, passwordParams)
}

// VerifyPassword reports whether plain matches the stored argon2id hash. It
// returns false (no error) on a simple mismatch; an error only on a malformed
// stored hash.
func VerifyPassword(plain, hash string) (bool, error) {
	return argon2id.ComparePasswordAndHash(plain, hash)
}
