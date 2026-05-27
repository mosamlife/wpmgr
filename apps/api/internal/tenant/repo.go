package tenant

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant persistence interface.
type Repo interface {
	Create(ctx context.Context, in CreateInput) (Tenant, error)
	// GetForUser returns a tenant by id only when userID has a membership in it,
	// otherwise domain.NotFound. Reads are scoped via the memberships_self_read
	// policy (InUserTx), never the unscoped tenants table.
	GetForUser(ctx context.Context, id, userID uuid.UUID) (Tenant, error)
	// ListForUser returns only the tenants userID is a member of.
	ListForUser(ctx context.Context, userID uuid.UUID, in ListInput) ([]Tenant, error)
	// GetByID loads a tenant by id without membership scoping. It is used only
	// for an API-key principal, which is already bound to exactly one tenant by
	// the auth middleware (so it can only ever be called for that one tenant).
	GetByID(ctx context.Context, id uuid.UUID) (Tenant, error)
}

// pgRepo is a Postgres-backed Repo over the pgx pool. Tenant rows themselves are
// not RLS-scoped, so reads MUST be membership-scoped in the application layer:
// list/get join the caller's memberships under the memberships_self_read policy
// (app.user_id GUC, set by InUserTx) so a caller can only ever see tenants they
// belong to.
type pgRepo struct {
	pool *db.Pool
	q    *sqlc.Queries
}

// NewRepo builds a Repo over the pgx pool. The pool is required for the
// per-user (InUserTx) scoping used by the read paths.
func NewRepo(pool *db.Pool) Repo {
	return &pgRepo{pool: pool, q: sqlc.New(pool.Pool)}
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

func (r *pgRepo) GetForUser(ctx context.Context, id, userID uuid.UUID) (Tenant, error) {
	var out Tenant
	err := r.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetTenantForUser(ctx, sqlc.GetTenantForUserParams{ID: id, UserID: userID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Non-member or unknown tenant: do not disclose existence.
				return domain.NotFound("tenant_not_found", "tenant not found")
			}
			return domain.Internal("tenant_get_failed", "failed to load tenant").WithCause(err)
		}
		out = Tenant{ID: row.ID, Name: row.Name, Slug: row.Slug, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
		return nil
	})
	return out, err
}

func (r *pgRepo) ListForUser(ctx context.Context, userID uuid.UUID, in ListInput) ([]Tenant, error) {
	var out []Tenant
	err := r.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListTenantsForUser(ctx, sqlc.ListTenantsForUserParams{
			UserID: userID,
			Limit:  in.Limit,
			Offset: in.Offset,
		})
		if err != nil {
			return domain.Internal("tenant_list_failed", "failed to list tenants").WithCause(err)
		}
		out = make([]Tenant, 0, len(rows))
		for _, row := range rows {
			out = append(out, Tenant{ID: row.ID, Name: row.Name, Slug: row.Slug, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetByID(ctx context.Context, id uuid.UUID) (Tenant, error) {
	row, err := r.q.GetTenant(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, domain.NotFound("tenant_not_found", "tenant not found")
		}
		return Tenant{}, domain.Internal("tenant_get_failed", "failed to load tenant").WithCause(err)
	}
	return toModel(row), nil
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
