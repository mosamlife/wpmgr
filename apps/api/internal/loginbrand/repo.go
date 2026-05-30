package loginbrand

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant-scoped persistence layer for site_login_brand rows. All
// mutating calls run inside InTenantTx so the RLS policy filters every row by
// the active tenant.
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// Get returns the stored login brand config for (tenantID, siteID).
// found=false (and no error) when no row exists yet; callers should default to
// the all-empty LoginBrand (all strings "").
func (r *Repo) Get(ctx context.Context, tenantID, siteID uuid.UUID) (LoginBrand, bool, error) {
	var out LoginBrand
	var found bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, site_id, logo_url, logo_link, message, updated_at
			 FROM site_login_brand
			 WHERE tenant_id = $1 AND site_id = $2`,
			tenantID, siteID,
		)
		if err := row.Scan(&out.TenantID, &out.SiteID, &out.LogoURL, &out.LogoLink, &out.Message, &out.UpdatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("login_brand_get_failed", "failed to get login brand config").WithCause(err)
		}
		found = true
		return nil
	})
	return out, found, err
}

// Upsert inserts or replaces the login brand config for (tenantID, siteID).
// updated_at is refreshed on every upsert. Returns the stored config.
func (r *Repo) Upsert(ctx context.Context, cfg LoginBrand) (LoginBrand, error) {
	var out LoginBrand
	err := r.pool.InTenantTx(ctx, cfg.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO site_login_brand
				(tenant_id, site_id, logo_url, logo_link, message, updated_at)
			 VALUES ($1, $2, $3, $4, $5, now())
			 ON CONFLICT (site_id) DO UPDATE
			   SET logo_url   = EXCLUDED.logo_url,
			       logo_link  = EXCLUDED.logo_link,
			       message    = EXCLUDED.message,
			       updated_at = now()
			 RETURNING tenant_id, site_id, logo_url, logo_link, message, updated_at`,
			cfg.TenantID, cfg.SiteID, cfg.LogoURL, cfg.LogoLink, cfg.Message,
		)
		if err := row.Scan(&out.TenantID, &out.SiteID, &out.LogoURL, &out.LogoLink, &out.Message, &out.UpdatedAt); err != nil {
			return domain.Internal("login_brand_upsert_failed", "failed to upsert login brand config").WithCause(err)
		}
		return nil
	})
	return out, err
}

// _ keeps the errors import honest.
var _ = errors.Is
