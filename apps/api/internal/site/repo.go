package site

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

// Repo is the tenant-scoped site persistence interface.
type Repo interface {
	Create(ctx context.Context, in CreateInput) (Site, error)
	Get(ctx context.Context, tenantID, id uuid.UUID) (Site, error)
	List(ctx context.Context, in ListInput) ([]Site, error)
	Delete(ctx context.Context, tenantID, id uuid.UUID) error
}

// pgRepo runs every operation inside a tenant-scoped transaction (RLS). The
// app.tenant_id GUC is set for the transaction, so even if a query omitted its
// tenant_id filter the RLS policy would still prevent cross-tenant rows from
// being read or written.
type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo {
	return &pgRepo{pool: pool}
}

func (r *pgRepo) Create(ctx context.Context, in CreateInput) (Site, error) {
	status := in.Status
	if status == "" {
		status = "pending"
	}
	var out Site
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateSite(ctx, sqlc.CreateSiteParams{
			TenantID:   in.TenantID,
			Url:        in.URL,
			Name:       in.Name,
			Status:     status,
			WpVersion:  in.WPVersion,
			PhpVersion: in.PHPVersion,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.Conflict("site_url_exists", "a site with this URL already exists for this tenant").WithCause(err)
			}
			return domain.Internal("site_create_failed", "failed to create site").WithCause(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) Get(ctx context.Context, tenantID, id uuid.UUID) (Site, error) {
	var out Site
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetSite(ctx, sqlc.GetSiteParams{ID: id, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_get_failed", "failed to load site").WithCause(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) List(ctx context.Context, in ListInput) ([]Site, error) {
	var out []Site
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSites(ctx, sqlc.ListSitesParams{
			TenantID: in.TenantID,
			Limit:    in.Limit,
			Offset:   in.Offset,
		})
		if err != nil {
			return domain.Internal("site_list_failed", "failed to list sites").WithCause(err)
		}
		out = make([]Site, 0, len(rows))
		for _, row := range rows {
			out = append(out, toModel(row))
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).DeleteSite(ctx, sqlc.DeleteSiteParams{ID: id, TenantID: tenantID})
		if err != nil {
			return domain.Internal("site_delete_failed", "failed to delete site").WithCause(err)
		}
		if n == 0 {
			return domain.NotFound("site_not_found", "site not found")
		}
		return nil
	})
}

func toModel(s sqlc.Site) Site {
	return Site{
		ID:         s.ID,
		TenantID:   s.TenantID,
		URL:        s.Url,
		Name:       s.Name,
		Status:     s.Status,
		WPVersion:  s.WpVersion,
		PHPVersion: s.PhpVersion,
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
	}
}
