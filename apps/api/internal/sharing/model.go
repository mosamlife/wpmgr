// Package sharing implements per-site collaborator grants (site_shares table).
// A site share gives an outside user (no tenant membership) scoped access to
// exactly one site. The allowlist is enforced at the DB layer via a RESTRICTIVE
// RLS policy; this package is the control-plane CRUD layer.
package sharing

import (
	"time"

	"github.com/google/uuid"
)

// Share is the domain model for a site_shares row. Email/Name are resolved from
// the users table at list time so the UI shows a human identity, not a UUID.
type Share struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	SiteID    uuid.UUID
	UserID    uuid.UUID
	Role      string
	Email     string
	Name      string
	GrantedBy *uuid.UUID
	ExpiresAt *time.Time
	CreatedAt time.Time
	// Populated only by SharedWithMe (the "Shared with me" surface) so the UI can
	// show the site + owning org instead of a bare UUID.
	SiteURL  string
	SiteName string
	OrgName  string
}

// Invitation is the domain model for a site-scoped invitations row, used by the
// "link history" surface (pending + accepted + expired + revoked). The display
// status is DERIVED from the timestamps (see DeriveStatus), never stored.
type Invitation struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	SiteID     *uuid.UUID
	Email      string
	Role       string
	InvitedBy  *uuid.UUID
	ExpiresAt  time.Time
	Attempts   int
	AcceptedAt *time.Time
	RevokedAt  *time.Time
	RevokedBy  *uuid.UUID
	CreatedAt  time.Time
}

// DeriveStatus computes the lifecycle status from the row's timestamps.
// Precedence: revoked > accepted > expired > pending.
func (inv Invitation) DeriveStatus(now time.Time) string {
	switch {
	case inv.RevokedAt != nil:
		return "revoked"
	case inv.AcceptedAt != nil:
		return "accepted"
	case inv.ExpiresAt.Before(now):
		return "expired"
	default:
		return "pending"
	}
}
