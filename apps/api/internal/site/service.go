package site

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Service holds site business logic. All operations require a tenant ID, which
// the handler derives from request context (tenant middleware).
type Service struct {
	repo      Repo
	validator *domain.Validator
	clock     domain.Clock
}

// NewService builds a site Service.
func NewService(repo Repo, v *domain.Validator, clock domain.Clock) *Service {
	return &Service{repo: repo, validator: v, clock: clock}
}

// Create validates and persists a new site under the given tenant.
func (s *Service) Create(ctx context.Context, in CreateInput) (Site, error) {
	if in.TenantID == uuid.Nil {
		return Site{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if err := s.validator.Struct(in); err != nil {
		return Site{}, err
	}
	return s.repo.Create(ctx, in)
}

// Get returns a tenant-scoped site by ID.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Site, error) {
	if tenantID == uuid.Nil {
		return Site{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	return s.repo.Get(ctx, tenantID, id)
}

// List returns a page of the tenant's sites.
func (s *Service) List(ctx context.Context, in ListInput) ([]Site, error) {
	if in.TenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	in.Limit, in.Offset = normalizePage(in.Limit, in.Offset)
	return s.repo.List(ctx, in)
}

// Delete removes a tenant-scoped site.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.Forbidden("tenant_required", "a tenant context is required")
	}
	return s.repo.Delete(ctx, tenantID, id)
}

func normalizePage(limit, offset int32) (int32, int32) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
