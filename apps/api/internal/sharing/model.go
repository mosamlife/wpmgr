// Package sharing implements per-site collaborator grants (site_shares table).
// A site share gives an outside user (no tenant membership) scoped access to
// exactly one site. The allowlist is enforced at the DB layer via a RESTRICTIVE
// RLS policy; this package is the control-plane CRUD layer.
package sharing

import (
	"time"

	"github.com/google/uuid"
)

// Share is the domain model for a site_shares row.
type Share struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	SiteID    uuid.UUID
	UserID    uuid.UUID
	Role      string
	GrantedBy *uuid.UUID
	ExpiresAt *time.Time
	CreatedAt time.Time
}
