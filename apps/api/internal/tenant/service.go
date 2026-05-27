package tenant

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Service holds tenant business logic.
type Service struct {
	repo      Repo
	validator *domain.Validator
	clock     domain.Clock
}

// NewService builds a tenant Service.
func NewService(repo Repo, v *domain.Validator, clock domain.Clock) *Service {
	return &Service{repo: repo, validator: v, clock: clock}
}

// Create validates and persists a new tenant.
func (s *Service) Create(ctx context.Context, in CreateInput) (Tenant, error) {
	if err := s.validator.Struct(in); err != nil {
		return Tenant{}, err
	}
	return s.repo.Create(ctx, in)
}

// GetForPrincipal returns a tenant by ID, scoped to the principal's access: a
// user principal must have a membership in the tenant; an API-key principal may
// only read its own (single) tenant. Any other case yields domain.NotFound so a
// caller cannot probe for tenants they do not belong to.
func (s *Service) GetForPrincipal(ctx context.Context, p domain.Principal, id uuid.UUID) (Tenant, error) {
	switch p.Type {
	case domain.PrincipalAPIKey:
		if id != p.TenantID || p.TenantID == uuid.Nil {
			return Tenant{}, domain.NotFound("tenant_not_found", "tenant not found")
		}
		// The key is bound to exactly this tenant, so reading it is in-scope.
		return s.repo.GetByID(ctx, id)
	case domain.PrincipalUser:
		return s.repo.GetForUser(ctx, id, p.UserID)
	default:
		return Tenant{}, domain.NotFound("tenant_not_found", "tenant not found")
	}
}

// ListForPrincipal returns the page of tenants the principal may see: a user's
// own memberships, or the single tenant an API key is bound to.
func (s *Service) ListForPrincipal(ctx context.Context, p domain.Principal, in ListInput) ([]Tenant, error) {
	in.Limit, in.Offset = normalizePage(in.Limit, in.Offset)
	switch p.Type {
	case domain.PrincipalAPIKey:
		if p.TenantID == uuid.Nil {
			return []Tenant{}, nil
		}
		t, err := s.repo.GetByID(ctx, p.TenantID)
		if err != nil {
			if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
				return []Tenant{}, nil
			}
			return nil, err
		}
		return []Tenant{t}, nil
	case domain.PrincipalUser:
		return s.repo.ListForUser(ctx, p.UserID, in)
	default:
		return []Tenant{}, nil
	}
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
