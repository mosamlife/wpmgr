package auth

import (
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
)

// User is a human principal. PasswordHash is empty for OIDC-only users; the
// hash is never serialized to the API.
type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	OIDCSubject  string
	OIDCIssuer   string
	Name         string
	Status       string // 'active' | 'pending' | 'disabled' (ADR-045 Phase 3)
	IsSuperadmin bool   // instance-level; written only by boot seeder
	// EmailVerifiedAt is when THIS install saw the human open a link sent to
	// that address. Distinct from an identity provider asserting the address is
	// verified, and the social linking rules need both, separately: see
	// SignInWithSocial. Nil means we have never confirmed it ourselves.
	EmailVerifiedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time

	// m73: two-factor authentication (ADR-056).
	// TwoFactorEnabled reports whether the user has an active second factor.
	// TOTPSecretEncrypted is the age-X25519 ciphertext of the base32 TOTP
	// shared secret; nil when no TOTP is enrolled. TOTPConfirmedAt is when
	// TOTP enrollment was confirmed (nil if never enrolled or unenrolled).
	// These fields are populated by repo.GetUserByID / GetUserByEmail (sqlc
	// SELECT * from the users table includes all columns).
	TwoFactorEnabled     bool
	TOTPSecretEncrypted  []byte
	TOTPConfirmedAt      *time.Time
}

// EmailVerified reports whether THIS install has confirmed the address, as
// opposed to an identity provider asserting it. Social account linking turns on
// this distinction, so it is a method rather than an inline nil check at each
// call site.
func (u User) EmailVerified() bool { return u.EmailVerifiedAt != nil }

// Membership binds a user to a tenant with a role.
type Membership struct {
	UserID    uuid.UUID
	TenantID  uuid.UUID
	Role      authz.Role
	CreatedAt time.Time
	UpdatedAt time.Time
}
