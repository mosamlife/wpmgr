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
	// The refresh's run record is written in the same transaction, with the
	// same checked_at.
	ReplaceInventory(ctx context.Context, tenantID, siteID uuid.UUID, checkedAt time.Time, rows []Row, truncated bool, deleteStale bool) error
	// GetRun reads the site's last refresh record; nil means never refreshed.
	GetRun(ctx context.Context, p domain.Principal, siteID uuid.UUID) (*Run, error)
	// ListInventory pages a site's inventory by post id, in the caller's scope.
	ListInventory(ctx context.Context, p domain.Principal, siteID uuid.UUID, afterPostID int64, owner *string, limit int32) ([]InventoryRow, error)
	// FleetReport reads the cross-tenant counts through the database's
	// count-only SECURITY DEFINER functions. No session can read another
	// tenant's inventory rows; the caller must already be gated to the
	// platform owner.
	FleetReport(ctx context.Context, actor uuid.UUID) ([]FleetVerdictShare, []FleetBuilderShare, error)
	// ListSweepSites enumerates the connected, unpaused sites across tenants.
	ListSweepSites(ctx context.Context) ([]SweepSite, error)
	// AdminUpsertIntegration calls the superadmin-only SQL writer.
	// The stored row is read, merged by finalize and written in ONE transaction
	// under a per-integration lock, so concurrent updates cannot re-write a
	// stale row. existing is nil for a new integration.
	AdminUpsertIntegration(ctx context.Context, in AdminUpsertInput, finalize func(existing *IntegrationRecord, in AdminUpsertInput) (AdminUpsertInput, error)) (IntegrationRecord, error)
	// ListIntegrations reads every allowlist row (enabled or not).
	ListIntegrations(ctx context.Context, actor uuid.UUID) ([]IntegrationRecord, error)
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
			it := Integration{ID: row.IntegrationID, DisplayName: row.DisplayName, Status: row.Status, Enabled: row.Enabled, Descriptor: row.Descriptor}
			if row.ThemeSlug != nil {
				it.ThemeSlug = *row.ThemeSlug
			}
			out = append(out, it)
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) ReplaceInventory(ctx context.Context, tenantID, siteID uuid.UUID, checkedAt time.Time, rows []Row, truncated bool, deleteStale bool) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		// One writer per site at a time, and never older data over newer: a
		// refresh that started before the stored one finished changes nothing.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('content_inventory'), hashtext($1))`, siteID.String()); err != nil {
			return err
		}
		if cur, err := q.GetSiteContentInventoryRun(ctx, sqlc.GetSiteContentInventoryRunParams{TenantID: tenantID, SiteID: siteID}); err == nil {
			if cur.CheckedAt.After(checkedAt) {
				return nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
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
		if deleteStale {
			if _, err := q.DeleteStaleSiteContentInventory(ctx, sqlc.DeleteStaleSiteContentInventoryParams{
				TenantID: tenantID, SiteID: siteID, CheckedAt: checkedAt,
			}); err != nil {
				return err
			}
		}
		return q.UpsertSiteContentInventoryRun(ctx, sqlc.UpsertSiteContentInventoryRunParams{
			TenantID: tenantID, SiteID: siteID, CheckedAt: checkedAt,
			PagesStored: int32(len(rows)), Truncated: truncated,
		})
	})
}

func (r *pgRepo) GetRun(ctx context.Context, p domain.Principal, siteID uuid.UUID) (*Run, error) {
	var out *Run
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetSiteContentInventoryRun(ctx, sqlc.GetSiteContentInventoryRunParams{TenantID: p.TenantID, SiteID: siteID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &Run{CheckedAt: row.CheckedAt, PagesStored: row.PagesStored, Truncated: row.Truncated}
		return nil
	})
	return out, err
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

func (r *pgRepo) FleetReport(ctx context.Context, actor uuid.UUID) ([]FleetVerdictShare, []FleetBuilderShare, error) {
	var vs []FleetVerdictShare
	var bs []FleetBuilderShare
	err := r.pool.InUserTx(ctx, actor, func(tx pgx.Tx) error {
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
			var ver *string
			if b.OwnerVersion != "" {
				v := b.OwnerVersion
				ver = &v
			}
			bs = append(bs, FleetBuilderShare{IntegrationID: b.OwnerIntegrationID, Version: ver, Pages: b.Pages, Sites: b.Sites})
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
	ThemeSlug              *string
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
	ThemeSlug              *string
	// Present names the optional fields the request carried; the rest keep
	// their stored values. Nil means every field was supplied.
	Present map[string]bool
}

func (r *pgRepo) AdminUpsertIntegration(ctx context.Context, in AdminUpsertInput, finalize func(existing *IntegrationRecord, in AdminUpsertInput) (AdminUpsertInput, error)) (IntegrationRecord, error) {
	var out IntegrationRecord
	// The SQL function refuses unless actor_user_id names a superadmin; any
	// transaction reaches it. InUserTx keeps the actor on the tx for audit.
	err := r.pool.InUserTx(ctx, in.ActorUserID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('content_integration'), hashtext($1))`, in.IntegrationID); err != nil {
			return err
		}
		var existing *IntegrationRecord
		rows, err := sqlc.New(tx).ListContentIntegrations(ctx)
		if err != nil {
			return err
		}
		for _, e := range rows {
			if e.IntegrationID == in.IntegrationID {
				rec := toRecord(e)
				existing = &rec
			}
		}
		in, err = finalize(existing, in)
		if err != nil {
			return err
		}
		sha := in.IntegrationEntrySHA256
		row, err := sqlc.New(tx).AdminUpsertContentIntegration(ctx, sqlc.AdminUpsertContentIntegrationParams{
			ActorUserID: in.ActorUserID, IntegrationID: in.IntegrationID, DisplayName: in.DisplayName,
			Enabled: in.Enabled, Status: in.Status, Descriptor: in.Descriptor, Abilities: in.Abilities,
			MinVersion: in.MinVersion, MaxTestedVersion: in.MaxTestedVersion, MinWpVersion: in.MinWPVersion,
			IntegrationEntrySha256: &sha, ThemeSlug: in.ThemeSlug,
		})
		if err != nil {
			return err
		}
		out = toRecord(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ListIntegrations(ctx context.Context, actor uuid.UUID) ([]IntegrationRecord, error) {
	var out []IntegrationRecord
	err := r.pool.InUserTx(ctx, actor, func(tx pgx.Tx) error {
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
		IntegrationEntrySHA256: row.IntegrationEntrySha256, ThemeSlug: row.ThemeSlug, UpdatedAt: row.UpdatedAt,
	}
}
