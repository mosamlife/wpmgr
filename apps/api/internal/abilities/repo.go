package abilities

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// SiteTarget is what the refresh needs to reach one site.
type SiteTarget struct {
	URL             string
	AgentVersion    string
	ConnectionState string
	Enrolled        bool
	Paused          bool
}

// SweepSite is one site the daily sweep refreshes.
type SweepSite struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
}

// Repo is the ability engine's database access.
type Repo interface {
	GetSiteTarget(ctx context.Context, tenantID, siteID uuid.UUID) (SiteTarget, error)
	// CatalogueEntryByName returns the admitted, enabled entry with that name.
	CatalogueEntryByName(ctx context.Context, tenantID uuid.UUID, name string) (sqlc.AbilityCatalogue, error)
	// ReplaceInventory stores one refresh: upsert, delete stale, record the
	// run, in one tenant transaction with one checked_at.
	ReplaceInventory(ctx context.Context, tenantID, siteID uuid.UUID, checkedAt time.Time, snapshotID uuid.UUID, res InventoryResult) error
	ListSweepSites(ctx context.Context) ([]SweepSite, error)
}

type pgRepo struct{ pool *db.Pool }

// NewRepo builds the Postgres repo.
func NewRepo(pool *db.Pool) Repo { return &pgRepo{pool: pool} }

const siteTargetSQL = `
SELECT url, agent_version, connection_state, enrolled_at IS NOT NULL,
       monitoring_paused_at IS NOT NULL
  FROM sites
 WHERE tenant_id = $1 AND id = $2`

func (r *pgRepo) GetSiteTarget(ctx context.Context, tenantID, siteID uuid.UUID) (SiteTarget, error) {
	var t SiteTarget
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, siteTargetSQL, tenantID, siteID).
			Scan(&t.URL, &t.AgentVersion, &t.ConnectionState, &t.Enrolled, &t.Paused)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return SiteTarget{}, domain.NotFound("site_not_found", "site not found")
	}
	return t, err
}

func (r *pgRepo) CatalogueEntryByName(ctx context.Context, tenantID uuid.UUID, name string) (sqlc.AbilityCatalogue, error) {
	var out sqlc.AbilityCatalogue
	found := false
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListAdmittedAbilityCatalogue(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Name == name {
				out, found = row, true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return sqlc.AbilityCatalogue{}, err
	}
	if !found {
		return sqlc.AbilityCatalogue{}, domain.NotFound("ability_not_in_catalogue", "the ability is not admitted")
	}
	return out, nil
}

// ReplaceInventory runs in the site's tenant transaction. This is the path
// RunTenantTx takes for an org principal with no user (InTenantTx); the job
// has no user principal, so it calls that helper directly, as the content
// inventory job does. A per-site advisory lock orders concurrent refreshes,
// and an older refresh never replaces a newer one.
func (r *pgRepo) ReplaceInventory(ctx context.Context, tenantID, siteID uuid.UUID, checkedAt time.Time, snapshotID uuid.UUID, res InventoryResult) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ability_inventory'), hashtext($1))`, siteID.String()); err != nil {
			return err
		}
		if cur, err := q.GetSiteAbilityInventoryRun(ctx, sqlc.GetSiteAbilityInventoryRunParams{TenantID: tenantID, SiteID: siteID}); err == nil {
			if cur.CheckedAt.After(checkedAt) {
				return nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if len(res.Rows) > 0 {
			p := sqlc.UpsertSiteAbilityInventoryParams{TenantID: tenantID, SiteID: siteID, CheckedAt: checkedAt}
			for _, row := range res.Rows {
				p.Names = append(p.Names, row.Name)
				p.OwnerKinds = append(p.OwnerKinds, row.OwnerKind)
				p.OwnerDirs = append(p.OwnerDirs, row.OwnerDir)
				p.OwnerOks = append(p.OwnerOks, row.OwnerOK)
				p.OwnerVersions = append(p.OwnerVersions, row.OwnerVersion)
				p.SchemaStructSha256s = append(p.SchemaStructSha256s, row.SchemaStructSHA256)
				p.InputSchemas = append(p.InputSchemas, "")
				p.OutputSchemas = append(p.OutputSchemas, "")
				p.Annotations = append(p.Annotations, "")
				p.SiteLabels = append(p.SiteLabels, row.SiteLabel)
				p.SiteDescriptions = append(p.SiteDescriptions, row.SiteDescription)
			}
			if _, err := q.UpsertSiteAbilityInventory(ctx, p); err != nil {
				return err
			}
		}
		if _, err := q.DeleteStaleSiteAbilityInventory(ctx, sqlc.DeleteStaleSiteAbilityInventoryParams{
			TenantID: tenantID, SiteID: siteID, CheckedAt: checkedAt,
		}); err != nil {
			return err
		}
		return q.UpsertSiteAbilityInventoryRun(ctx, sqlc.UpsertSiteAbilityInventoryRunParams{
			TenantID: tenantID, SiteID: siteID, CheckedAt: checkedAt, SnapshotID: snapshotID,
			ApiPresent: res.APIPresent, AbilitiesStored: int32(len(res.Rows)), Truncated: res.Truncated,
		})
	})
}

const sweepSitesSQL = `
SELECT tenant_id, id
  FROM sites
 WHERE connection_state = 'connected'
   AND enrolled_at IS NOT NULL
   AND monitoring_paused_at IS NULL
 ORDER BY created_at DESC, id DESC`

func (r *pgRepo) ListSweepSites(ctx context.Context) ([]SweepSite, error) {
	var out []SweepSite
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sweepSitesSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s SweepSite
			if err := rows.Scan(&s.TenantID, &s.SiteID); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}
