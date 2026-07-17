// Package sitetag implements the GH #230 "rich tags" tenant-level tag
// registry (m100). It owns tag existence, color, and canonical name;
// sites.tags (text[], owned by internal/site) remains the sole assignment
// store — there is no join table. Every path that writes tag names onto a
// site (site.Service.SetTags, pairing-code minting, and this package's
// BulkApply) upserts those names into the registry in the SAME transaction
// as the write (see internal/site/repo.go SetTags/CreatePairingCode/
// MintSiteBoundCode and this package's repo.BulkApply).
//
// Tag names are CASE-SENSITIVE, matching the existing
// site.normalizeTags + `= ANY(tags)` semantics; renaming a tag onto an
// existing name with merge:true is the remedy for case-insensitive
// duplicates, not a validation rule enforced at create time.
package sitetag

import (
	"time"

	"github.com/google/uuid"
)

// Tag is a tenant-level tag registry entry.
type Tag struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Name       string
	Color      string // "" = auto (client derives a deterministic color).
	UsageCount int64
	CreatedAt  time.Time
}

// CreateInput is the validated input for creating a new tag.
type CreateInput struct {
	TenantID uuid.UUID
	Name     string
	Color    string
}

// UpdateInput is the validated partial-update input for a tag. Name and
// Color are nil when the caller did not supply them ("leave unchanged").
// Merge only matters when Name is set and collides with an existing tag.
type UpdateInput struct {
	TenantID uuid.UUID
	ID       uuid.UUID
	Name     *string
	Color    *string
	Merge    bool
}

// UpdateResult is the outcome of Update — the resulting tag plus whether the
// rename collided with an existing tag and was merged into it (drives the
// handler's audit-action choice: tag.update vs tag.merge).
type UpdateResult struct {
	Tag        Tag
	Merged     bool
	OldName    string
	MergedInto string
}

// BulkApplyInput is the validated input for the portfolio bulk-apply route.
type BulkApplyInput struct {
	TenantID uuid.UUID
	SiteIDs  []uuid.UUID
	Add      []string
	Remove   []string
}

// BulkApplyItemResult is the per-site outcome of a bulk-apply call.
type BulkApplyItemResult struct {
	SiteID uuid.UUID
	OK     bool
	Detail string
}
