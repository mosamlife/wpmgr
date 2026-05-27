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

// Get returns a tenant by ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Tenant, error) {
	return s.repo.Get(ctx, id)
}

// List returns a page of tenants, normalizing pagination bounds.
func (s *Service) List(ctx context.Context, in ListInput) ([]Tenant, error) {
	in.Limit, in.Offset = normalizePage(in.Limit, in.Offset)
	return s.repo.List(ctx, in)
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
