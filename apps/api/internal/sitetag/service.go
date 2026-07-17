package sitetag

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// maxTagNameLen matches site.normalizeTags' `max=64` per-tag validation.
const maxTagNameLen = 64

// maxBulkApplySites bounds the portfolio bulk-apply batch (locked contract).
const maxBulkApplySites = 200

// colorHex mirrors internal/client's colorHex regex (case-insensitive
// 6-digit hex, e.g. "#1a2b3c" or "#1A2B3C").
var colorHex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Service holds tag-registry business logic.
type Service struct {
	repo Repo
}

// NewService builds a tag Service.
func NewService(repo Repo) *Service {
	return &Service{repo: repo}
}

// List returns the tenant's tag registry, sorted case-insensitively by name.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]Tag, error) {
	if tenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	return s.repo.List(ctx, tenantID)
}

// Create validates and creates a new tag.
func (s *Service) Create(ctx context.Context, in CreateInput) (Tag, error) {
	if in.TenantID == uuid.Nil {
		return Tag{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	name, err := validateTagName(in.Name)
	if err != nil {
		return Tag{}, err
	}
	color, err := validateColor(in.Color)
	if err != nil {
		return Tag{}, err
	}
	return s.repo.Create(ctx, CreateInput{TenantID: in.TenantID, Name: name, Color: color})
}

// Update validates and applies a partial update (color-only, rename, or
// rename-with-merge) to a tag.
func (s *Service) Update(ctx context.Context, in UpdateInput) (UpdateResult, error) {
	if in.TenantID == uuid.Nil {
		return UpdateResult{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if in.Name != nil {
		name, err := validateTagName(*in.Name)
		if err != nil {
			return UpdateResult{}, err
		}
		in.Name = &name
	}
	if in.Color != nil {
		color, err := validateColor(*in.Color)
		if err != nil {
			return UpdateResult{}, err
		}
		in.Color = &color
	}
	return s.repo.Update(ctx, in)
}

// Delete removes a tag fleet-wide (registry row + every site + every
// unexpired/unredeemed pairing code carrying it).
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.Forbidden("tenant_required", "a tenant context is required")
	}
	return s.repo.Delete(ctx, tenantID, id)
}

// ValidateDelta normalizes and validates a bulk-apply add/remove pair,
// independent of which sites are authorized. It is called BEFORE the
// handler's per-site authorization filtering so a request with no valid
// delta (e.g. both lists empty after trimming) is rejected outright, rather
// than silently succeeding with zero effect when every site_id also happens
// to be unauthorized.
func (s *Service) ValidateDelta(add, remove []string) (normAdd, normRemove []string, err error) {
	normAdd, err = validateTagNameList(add)
	if err != nil {
		return nil, nil, err
	}
	normRemove, err = validateTagNameList(remove)
	if err != nil {
		return nil, nil, err
	}
	if len(normAdd) == 0 && len(normRemove) == 0 {
		return nil, nil, domain.Validation("tag_delta_required", "at least one of add or remove must be non-empty")
	}
	return normAdd, normRemove, nil
}

// BulkApply applies an already-validated add/remove tag delta to a batch of
// sites. siteIDs must already be filtered to sites the caller is authorized
// to touch (see handler.go's CanAccessSite gate) — an empty siteIDs is a
// legitimate no-op (every requested site was unauthorized/invalid; the
// handler has already recorded ok:false results for those), not an error.
func (s *Service) BulkApply(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, add, remove []string) (map[uuid.UUID]bool, error) {
	if tenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if len(siteIDs) == 0 {
		return map[uuid.UUID]bool{}, nil
	}
	return s.repo.BulkApply(ctx, tenantID, siteIDs, add, remove)
}

// ---------------------------------------------------------------------------
// validation helpers
// ---------------------------------------------------------------------------

func validateTagName(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", domain.Validation("invalid_tag", "tag name must not be blank")
	}
	if utf8.RuneCountInString(trimmed) > maxTagNameLen {
		return "", domain.Validation("invalid_tag", "tag name must be 64 characters or fewer")
	}
	return trimmed, nil
}

// validateTagNameList trims and validates each name, silently dropping empty
// entries (a caller sending [""] is treated as "nothing there", not an
// error) — mirrors site.normalizeTags' tolerance for blank entries.
func validateTagNameList(names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, raw := range names {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if utf8.RuneCountInString(trimmed) > maxTagNameLen {
			return nil, domain.Validation("invalid_tag", "tag name must be 64 characters or fewer")
		}
		out = append(out, trimmed)
	}
	return out, nil
}

func validateColor(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if !colorHex.MatchString(raw) {
		return "", domain.Validation("invalid_color", "color must be a 6-digit hex code (e.g. #1a2b3c)")
	}
	return strings.ToLower(raw), nil
}
