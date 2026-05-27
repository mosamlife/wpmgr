package tenant

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant persistence interface.
type Repo interface {
	Create(ctx context.Context, in CreateInput) (Tenant, error)
	Get(ctx context.Context, id uuid.UUID) (Tenant, error)
	List(ctx context.Context, in ListInput) ([]Tenant, error)
}

// pgRepo is a Postgres-backed Repo over the sqlc Queries. Tenants are not
// tenant-scoped, so no RLS transaction wrapping is needed here.
type pgRepo struct {
	q *sqlc.Queries
}

// NewRepo builds a Repo from a pgx DBTX (pool).
func NewRepo(db sqlc.DBTX) Repo {
	return &pgRepo{q: sqlc.New(db)}
}

func (r *pgRepo) Create(ctx context.Context, in CreateInput) (Tenant, error) {
	row, err := r.q.CreateTenant(ctx, sqlc.CreateTenantParams{Name: in.Name, Slug: in.Slug})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Tenant{}, domain.Conflict("tenant_slug_exists", "a tenant with this slug already exists").WithCause(err)
		}
		return Tenant{}, domain.Internal("tenant_create_failed", "failed to create tenant").WithCause(err)
	}
	return toModel(row), nil
}

func (r *pgRepo) Get(ctx context.Context, id uuid.UUID) (Tenant, error) {
	row, err := r.q.GetTenant(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, domain.NotFound("tenant_not_found", "tenant not found")
		}
		return Tenant{}, domain.Internal("tenant_get_failed", "failed to load tenant").WithCause(err)
	}
	return toModel(row), nil
}

func (r *pgRepo) List(ctx context.Context, in ListInput) ([]Tenant, error) {
	rows, err := r.q.ListTenants(ctx, sqlc.ListTenantsParams{Limit: in.Limit, Offset: in.Offset})
	if err != nil {
		return nil, domain.Internal("tenant_list_failed", "failed to list tenants").WithCause(err)
	}
	out := make([]Tenant, 0, len(rows))
	for _, row := range rows {
		out = append(out, toModel(row))
	}
	return out, nil
}

func toModel(t sqlc.Tenant) Tenant {
	return Tenant{
		ID:        t.ID,
		Name:      t.Name,
		Slug:      t.Slug,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}
