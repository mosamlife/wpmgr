package content

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	sqlc "github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the storage the service needs. Every method runs inside a tx helper;
// the repo never sets a GUC itself.
type Repo interface {
	// GetSiteTarget reads the facts a refresh needs, in the tenant's scope.
	GetSiteTarget(ctx context.Context, tenantID, siteID uuid.UUID) (SiteTarget, error)
	// ListEnabledIntegrations reads the global allowlist's enabled rows.
	ListEnabledIntegrations(ctx context.Context, tenantID uuid.UUID) ([]Integration, error)
	// ReplaceInventory upserts every row with one checked_at, then deletes the
	// site's older rows, in one tenant transaction.
	ReplaceInventory(ctx context.Context, tenantID, siteID uuid.UUID, checkedAt time.Time, rows []Row) error
	// ListInventory pages a site's inventory by post id, in the caller's scope.
	ListInventory(ctx context.Context, p domain.Principal, siteID uuid.UUID, afterPostID int64, owner *string, limit int32) ([]InventoryRow, error)
	// FleetReport aggregates every tenant's inventory (app.agent, FOR SELECT).
	FleetReport(ctx context.Context) ([]FleetVerdictShare, []FleetBuilderShare, error)
	// ListSweepSites enumerates the connected, unpaused sites across tenants.
	ListSweepSites(ctx context.Context) ([]SweepSite, error)
	// AdminUpsertIntegration calls the superadmin-only SQL writer.
	AdminUpsertIntegration(ctx context.Context, in AdminUpsertInput) (IntegrationRecord, error)
	// ListIntegrations reads every allowlist row (enabled or not).
	ListIntegrations(ctx context.Context) ([]IntegrationRecord, error)
}

type pgRepo struct{ pool *db.Pool }

// NewRepo builds the Postgres repo.
func NewRepo(pool *db.Pool) Repo { return &pgRepo{pool: pool} }

const siteTargetSQL = `
SELECT url, agent_version, connection_state, enrolled_at IS NOT NULL,
       monitoring_paused_at IS NOT NULL, components
  FROM sites
 WHERE tenant_id = $1 AND id = $2`

func (r *pgRepo) GetSiteTarget(ctx context.Context, tenantID, siteID uuid.UUID) (SiteTarget, error) {
	var t SiteTarget
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, siteTargetSQL, tenantID, siteID).
			Scan(&t.URL, &t.AgentVersion, &t.ConnectionState, &t.Enrolled, &t.Paused, &t.Components)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return SiteTarget{}, domain.NotFound("site_not_found", "site not found")
	}
	return t, err
}

func (r *pgRepo) ListEnabledIntegrations(ctx context.Context, tenantID uuid.UUID) ([]Integration, error) {
	var out []Integration
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListEnabledContentIntegrations(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, Integration{ID: row.IntegrationID, DisplayName: row.DisplayName, Descriptor: row.Descriptor})
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) ReplaceInventory(ctx context.Context, tenantID, siteID uuid.UUID, checkedAt time.Time, rows []Row) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if len(rows) > 0 {
			p := sqlc.UpsertSiteContentInventoryParams{
				TenantID: tenantID, SiteID: siteID, CheckedAt: checkedAt,
			}
			for _, row := range rows {
				p.PostIds = append(p.PostIds, row.PostID)
				p.PostTypes = append(p.PostTypes, row.PostType)
				p.PostStatuses = append(p.PostStatuses, row.PostStatus)
				p.Verdicts = append(p.Verdicts, row.Verdict)
				p.RouteNumbers = append(p.RouteNumbers, row.RouteNumber)
				p.RouteReasons = append(p.RouteReasons, row.RouteReason)
				p.OwnerIntegrationIds = append(p.OwnerIntegrationIds, row.OwnerID)
				p.OwnerDisplayNames = append(p.OwnerDisplayNames, row.OwnerName)
				p.OwnerVersions = append(p.OwnerVersions, row.OwnerVer)
				// List mode returns no fingerprints.
				p.Fingerprints = append(p.Fingerprints, "")
				p.Titles = append(p.Titles, row.Title)
			}
			if _, err := q.UpsertSiteContentInventory(ctx, p); err != nil {
				return err
			}
		}
		_, err := q.DeleteStaleSiteContentInventory(ctx, sqlc.DeleteStaleSiteContentInventoryParams{
			TenantID: tenantID, SiteID: siteID, CheckedAt: checkedAt,
		})
		return err
	})
}

func (r *pgRepo) ListInventory(ctx context.Context, p domain.Principal, siteID uuid.UUID, afterPostID int64, owner *string, limit int32) ([]InventoryRow, error) {
	var out []InventoryRow
	// RunTenantTx sends a site-scoped principal through InScopedTenantTx, so
	// the RESTRICTIVE site_scope policy applies to it.
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSiteContentInventory(ctx, sqlc.ListSiteContentInventoryParams{
			TenantID: p.TenantID, SiteID: siteID, AfterPostID: afterPostID,
			OwnerIntegrationID: owner, RowLimit: limit,
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, InventoryRow{
				PostID: row.PostID, PostType: row.PostType, PostStatus: row.PostStatus,
				Verdict: row.Verdict, RouteNumber: row.RouteNumber, RouteReason: row.RouteReason,
				OwnerIntegrationID: row.OwnerIntegrationID, OwnerDisplayName: row.OwnerDisplayName,
				OwnerVersion: row.OwnerVersion, Title: row.Title, CheckedAt: row.CheckedAt,
			})
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) FleetReport(ctx context.Context) ([]FleetVerdictShare, []FleetBuilderShare, error) {
	var vs []FleetVerdictShare
	var bs []FleetBuilderShare
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		vrows, err := q.FleetContentShareByVerdict(ctx)
		if err != nil {
			return err
		}
		brows, err := q.FleetContentShareByBuilder(ctx)
		if err != nil {
			return err
		}
		for _, v := range vrows {
			vs = append(vs, FleetVerdictShare{Verdict: v.Verdict, RouteNumber: v.RouteNumber, Pages: v.Pages, Sites: v.Sites})
		}
		for _, b := range brows {
			bs = append(bs, FleetBuilderShare{IntegrationID: b.OwnerIntegrationID, Version: b.OwnerVersion, Pages: b.Pages, Sites: b.Sites})
		}
		return nil
	})
	return vs, bs, err
}

// sweepSitesSQL is the scheduled enumeration: connected, enrolled, unpaused.
// Monitoring pause governs the schedule, never an operator's Refresh.
const sweepSitesSQL = `
SELECT tenant_id, id FROM sites
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

// IntegrationRecord is one allowlist row as the admin API returns it.
type IntegrationRecord struct {
	IntegrationID          string
	DisplayName            string
	Enabled                bool
	Status                 string
	Descriptor             []byte
	Abilities              []byte
	MinVersion             *string
	MaxTestedVersion       *string
	MinWPVersion           *string
	IntegrationEntrySHA256 *string
	UpdatedAt              time.Time
}

// AdminUpsertInput is the superadmin's write. IntegrationEntrySHA256 is
// computed by the service, never accepted from the caller.
type AdminUpsertInput struct {
	ActorUserID            uuid.UUID
	IntegrationID          string
	DisplayName            string
	Enabled                bool
	Status                 string
	Descriptor             []byte
	Abilities              []byte
	MinVersion             *string
	MaxTestedVersion       *string
	MinWPVersion           *string
	IntegrationEntrySHA256 string
}

func (r *pgRepo) AdminUpsertIntegration(ctx context.Context, in AdminUpsertInput) (IntegrationRecord, error) {
	var out IntegrationRecord
	// The SQL function refuses unless actor_user_id names a superadmin; any
	// transaction reaches it. InUserTx keeps the actor on the tx for audit.
	err := r.pool.InUserTx(ctx, in.ActorUserID, func(tx pgx.Tx) error {
		sha := in.IntegrationEntrySHA256
		row, err := sqlc.New(tx).AdminUpsertContentIntegration(ctx, sqlc.AdminUpsertContentIntegrationParams{
			ActorUserID: in.ActorUserID, IntegrationID: in.IntegrationID, DisplayName: in.DisplayName,
			Enabled: in.Enabled, Status: in.Status, Descriptor: in.Descriptor, Abilities: in.Abilities,
			MinVersion: in.MinVersion, MaxTestedVersion: in.MaxTestedVersion, MinWpVersion: in.MinWPVersion,
			IntegrationEntrySha256: &sha,
		})
		if err != nil {
			return err
		}
		out = toRecord(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ListIntegrations(ctx context.Context) ([]IntegrationRecord, error) {
	var out []IntegrationRecord
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListContentIntegrations(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, toRecord(row))
		}
		return nil
	})
	return out, err
}

func toRecord(row sqlc.ContentIntegration) IntegrationRecord {
	return IntegrationRecord{
		IntegrationID: row.IntegrationID, DisplayName: row.DisplayName, Enabled: row.Enabled,
		Status: row.Status, Descriptor: row.Descriptor, Abilities: row.Abilities,
		MinVersion: row.MinVersion, MaxTestedVersion: row.MaxTestedVersion, MinWPVersion: row.MinWpVersion,
		IntegrationEntrySHA256: row.IntegrationEntrySha256, UpdatedAt: row.UpdatedAt,
	}
}
