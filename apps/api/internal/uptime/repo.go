package uptime

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the Postgres persistence for M5 uptime: the cross-tenant probe-job
// reads (under app.agent), the per-site health/alert-state writes, and the
// tenant-scoped alert-config CRUD. Every tenant-scoped method runs under RLS.
type Repo interface {
	// Probe-job path (app.agent GUC, cross-tenant).
	ListEnrolledForProbe(ctx context.Context) ([]EnrolledSite, error)
	SetSiteHealth(ctx context.Context, siteID uuid.UUID, status string) (bool, error)
	GetAlertState(ctx context.Context, siteID uuid.UUID) (AlertState, bool, error)
	UpsertAlertState(ctx context.Context, st AlertState) error

	// TransitionAlertState atomically reads (with a row lock via FOR UPDATE),
	// evaluates, and persists the next alert state for one site in a SINGLE
	// transaction. This is the probe worker's entry point — it MUST be used
	// instead of a separate GetAlertState + UpsertAlertState pair, which is a
	// classic lost-update race: when a sweep runs longer than the probe
	// interval (exactly what happens during a real, fleet-wide outage — down
	// probes each take up to the probe timeout, so a sweep with many down
	// sites can overlap the NEXT periodic sweep), two overlapping sweeps for
	// the same site would each read the same stale ConsecutiveDown and never
	// let it accumulate past 1, so the down threshold is never crossed and no
	// alert ever fires — silently, with no error to log.
	TransitionAlertState(ctx context.Context, siteID, tenantID uuid.UUID, up bool, threshold int, now time.Time) (Transition, error)

	// Evaluator path (app.agent GUC, cross-tenant).
	ListAlertConfigsAllTenants(ctx context.Context) ([]AlertConfig, error)

	// Tenant-scoped config CRUD (RLS).
	GetAlertConfig(ctx context.Context, tenantID uuid.UUID) (AlertConfig, bool, error)
	UpsertAlertConfig(ctx context.Context, cfg AlertConfig) (AlertConfig, error)

	// Fleet uptime queries (tenant-scoped, InTenantTx). Implemented via raw SQL
	// because sqlc generates non-nullable types for nullable columns.

	// GetFleetSiteInfo returns the Postgres-resident fields for the requested
	// sites: name, url, connection_state, health_status, in_incident. Probe /
	// uptime metrics are NOT included — the service layer merges those from the
	// metrics.Store so both ClickHouse and Postgres deployments work correctly.
	GetFleetSiteInfo(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) ([]FleetSiteInfo, error)

	// GetFleetIncidents returns open incidents and recently-alerted sites.
	// NOTE: full historical reconstruction is not possible — see FleetIncidentItem.
	GetFleetIncidents(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, since time.Time, limit int) ([]FleetIncidentItem, error)
}

type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo { return &pgRepo{pool: pool} }

func (r *pgRepo) ListEnrolledForProbe(ctx context.Context) ([]EnrolledSite, error) {
	var out []EnrolledSite
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListEnrolledSitesForProbe(ctx)
		if err != nil {
			return domain.Internal("uptime_list_enrolled_failed", "failed to list enrolled sites").WithCause(err)
		}
		out = make([]EnrolledSite, 0, len(rows))
		for _, row := range rows {
			out = append(out, EnrolledSite{ID: row.ID, TenantID: row.TenantID, URL: row.Url, HealthStatus: row.HealthStatus})
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) SetSiteHealth(ctx context.Context, siteID uuid.UUID, status string) (bool, error) {
	var changed bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).SetSiteHealthStatus(ctx, sqlc.SetSiteHealthStatusParams{ID: siteID, HealthStatus: status})
		if err != nil {
			return domain.Internal("uptime_set_health_failed", "failed to set site health").WithCause(err)
		}
		changed = n > 0
		return nil
	})
	return changed, err
}

func (r *pgRepo) GetAlertState(ctx context.Context, siteID uuid.UUID) (AlertState, bool, error) {
	var st AlertState
	var found bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetSiteAlertState(ctx, siteID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("uptime_get_state_failed", "failed to read site alert state").WithCause(err)
		}
		st = alertStateFromRow(row)
		found = true
		return nil
	})
	return st, found, err
}

func (r *pgRepo) UpsertAlertState(ctx context.Context, st AlertState) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).UpsertSiteAlertState(ctx, sqlc.UpsertSiteAlertStateParams{
			SiteID:          st.SiteID,
			TenantID:        st.TenantID,
			LastStatus:      st.LastStatus,
			ConsecutiveDown: st.ConsecutiveDown,
			InIncident:      st.InIncident,
			LastAlertAt:     toTimestamptz(st.LastAlertAt),
		})
		if err != nil {
			return domain.Internal("uptime_upsert_state_failed", "failed to upsert site alert state").WithCause(err)
		}
		return nil
	})
}

// TransitionAlertState is the race-safe replacement for a separate
// GetAlertState + UpsertAlertState pair: it locks the site's row (or, for a
// brand-new site, relies on the ON CONFLICT upsert's implicit row lock),
// evaluates the transition, and writes the result — all inside one
// transaction — so an overlapping sweep for the same site blocks on the SELECT
// FOR UPDATE until this one commits, then observes the fresh state instead of
// a stale one.
func (r *pgRepo) TransitionAlertState(ctx context.Context, siteID, tenantID uuid.UUID, up bool, threshold int, now time.Time) (Transition, error) {
	var tr Transition
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var prev AlertState
		row, err := q.GetSiteAlertStateForUpdate(ctx, siteID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return domain.Internal("uptime_get_state_failed", "failed to read site alert state").WithCause(err)
			}
			// No row yet: zero-value prior state, no lock to hold (nothing exists
			// to lock). The INSERT below takes the DB's own conflict-resolution
			// lock on the newly-inserted row, so a concurrent first-insert for the
			// same site still serializes correctly via ON CONFLICT DO UPDATE.
		} else {
			prev = alertStateFromRow(row)
		}
		prev.SiteID = siteID
		prev.TenantID = tenantID
		if prev.LastStatus == "" {
			prev.LastStatus = StatusUnknown
		}

		tr = Evaluate(prev, up, threshold, now)

		if _, err := q.UpsertSiteAlertState(ctx, sqlc.UpsertSiteAlertStateParams{
			SiteID:          tr.NewState.SiteID,
			TenantID:        tr.NewState.TenantID,
			LastStatus:      tr.NewState.LastStatus,
			ConsecutiveDown: tr.NewState.ConsecutiveDown,
			InIncident:      tr.NewState.InIncident,
			LastAlertAt:     toTimestamptz(tr.NewState.LastAlertAt),
		}); err != nil {
			return domain.Internal("uptime_upsert_state_failed", "failed to upsert site alert state").WithCause(err)
		}
		return nil
	})
	return tr, err
}

func (r *pgRepo) ListAlertConfigsAllTenants(ctx context.Context) ([]AlertConfig, error) {
	var out []AlertConfig
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListAlertConfigsAllTenants(ctx)
		if err != nil {
			return domain.Internal("uptime_list_configs_failed", "failed to list alert configs").WithCause(err)
		}
		out = make([]AlertConfig, 0, len(rows))
		for _, row := range rows {
			out = append(out, alertConfigFromRow(row))
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetAlertConfig(ctx context.Context, tenantID uuid.UUID) (AlertConfig, bool, error) {
	var cfg AlertConfig
	var found bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetAlertConfig(ctx, tenantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("uptime_get_config_failed", "failed to read alert config").WithCause(err)
		}
		cfg = alertConfigFromRow(row)
		found = true
		return nil
	})
	return cfg, found, err
}

func (r *pgRepo) UpsertAlertConfig(ctx context.Context, cfg AlertConfig) (AlertConfig, error) {
	recipients := cfg.EmailRecipients
	if recipients == nil {
		recipients = []string{}
	}
	var out AlertConfig
	err := r.pool.InTenantTx(ctx, cfg.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).UpsertAlertConfig(ctx, sqlc.UpsertAlertConfigParams{
			TenantID:        cfg.TenantID,
			EmailRecipients: recipients,
			WebhookUrl:      cfg.WebhookURL,
			WebhookSecret:   cfg.WebhookSecret,
			Enabled:         cfg.Enabled,
			NotifySecurity:  cfg.NotifySecurity,
		})
		if err != nil {
			return domain.Internal("uptime_upsert_config_failed", "failed to save alert config").WithCause(err)
		}
		out = alertConfigFromRow(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Fleet uptime repo methods (raw SQL — Postgres-resident fields only)
// ---------------------------------------------------------------------------

// GetFleetSiteInfo returns the Postgres-resident fields for each requested
// site: name, url, connection_state, health_status, in_incident. Probe /
// uptime metrics are intentionally excluded — the service merges those from
// the metrics.Store so the endpoint works on both ClickHouse and Postgres
// deployments (previously these were read directly from site_uptime_probes,
// which is empty on ClickHouse installs).
func (r *pgRepo) GetFleetSiteInfo(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) ([]FleetSiteInfo, error) {
	const q = `
SELECT
    s.id,
    s.name,
    s.url,
    s.connection_state,
    s.health_status,
    COALESCE(ast.in_incident, false) AS in_incident
FROM sites s
LEFT JOIN site_alert_state ast ON ast.site_id = s.id
WHERE s.tenant_id = $1
  AND s.id = ANY($2::uuid[])
ORDER BY s.name ASC
`
	var out []FleetSiteInfo
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, siteIDs)
		if err != nil {
			return domain.Internal("fleet_site_info_failed", "failed to query fleet site info").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var info FleetSiteInfo
			if err := rows.Scan(
				&info.SiteID, &info.Name, &info.URL,
				&info.ConnectionState, &info.HealthStatus, &info.InIncident,
			); err != nil {
				return domain.Internal("fleet_site_info_scan_failed", "failed to scan fleet site info row").WithCause(err)
			}
			out = append(out, info)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []FleetSiteInfo{}
	}
	return out, nil
}

// deriveFleetStatus computes the FleetSiteStatus from the latest probe result.
func deriveFleetStatus(up *bool, totalMs *float64, connectionState string) FleetSiteStatus {
	if up == nil {
		return FleetStatusUnknown
	}
	if !*up {
		return FleetStatusDown
	}
	// Site is up — check for degraded: slow response OR degraded connection state.
	if connectionState == "degraded" {
		return FleetStatusDegraded
	}
	if totalMs != nil && *totalMs > slowThresholdMs {
		return FleetStatusDegraded
	}
	return FleetStatusUp
}

// GetFleetIncidents returns open incidents and recently-alerted sites.
// Open incidents: in_incident=true. Derivable recoveries: in_incident=false
// AND last_alert_at >= since. Full historical incident logs are NOT stored;
// ended_at is estimated from alert-state updated_at for closed incidents.
func (r *pgRepo) GetFleetIncidents(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, since time.Time, limit int) ([]FleetIncidentItem, error) {
	const q = `
SELECT
    s.id,
    s.name,
    s.url,
    ast.last_alert_at,
    ast.updated_at,
    ast.in_incident,
    p.total_ms
FROM site_alert_state ast
JOIN sites s ON s.id = ast.site_id AND s.tenant_id = ast.tenant_id
LEFT JOIN LATERAL (
    SELECT total_ms
    FROM site_uptime_probes
    WHERE site_id = s.id AND tenant_id = s.tenant_id
    ORDER BY probed_at DESC
    LIMIT 1
) p ON true
WHERE ast.tenant_id = $1
  AND s.id = ANY($2::uuid[])
  AND (ast.in_incident = true OR ast.last_alert_at >= $3)
ORDER BY ast.last_alert_at DESC NULLS LAST
LIMIT $4
`
	var out []FleetIncidentItem
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, siteIDs, since, limit)
		if err != nil {
			return domain.Internal("fleet_incidents_failed", "failed to query fleet incidents").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				siteID      uuid.UUID
				siteName    string
				siteURL     string
				lastAlertAt pgtype.Timestamptz
				updatedAt   pgtype.Timestamptz
				inIncident  bool
				totalMs     *float64
			)
			if err := rows.Scan(&siteID, &siteName, &siteURL, &lastAlertAt, &updatedAt, &inIncident, &totalMs); err != nil {
				return domain.Internal("fleet_incidents_scan_failed", "failed to scan incident row").WithCause(err)
			}
			item := FleetIncidentItem{
				SiteID:        siteID,
				Kind:          string(AlertDown),
				SiteName:      siteName,
				SiteURL:       siteURL,
				Ongoing:       inIncident,
				LatestTotalMs: totalMs,
			}
			if lastAlertAt.Valid {
				t := lastAlertAt.Time
				item.StartedAt = &t
			}
			// For closed incidents, estimate ended_at from state updated_at.
			if !inIncident && updatedAt.Valid {
				t := updatedAt.Time
				item.EndedAt = &t
				if item.StartedAt != nil {
					dur := int64(t.Sub(*item.StartedAt).Seconds())
					if dur >= 0 {
						item.DurationSeconds = &dur
					}
				}
			}
			out = append(out, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []FleetIncidentItem{}
	}
	return out, nil
}

func alertConfigFromRow(row sqlc.AlertConfig) AlertConfig {
	recipients := row.EmailRecipients
	if recipients == nil {
		recipients = []string{}
	}
	return AlertConfig{
		TenantID:        row.TenantID,
		EmailRecipients: recipients,
		WebhookURL:      row.WebhookUrl,
		WebhookSecret:   row.WebhookSecret,
		Enabled:         row.Enabled,
		NotifySecurity:  row.NotifySecurity,
		UpdatedAt:       row.UpdatedAt,
	}
}

func alertStateFromRow(row sqlc.SiteAlertState) AlertState {
	st := AlertState{
		SiteID:          row.SiteID,
		TenantID:        row.TenantID,
		LastStatus:      row.LastStatus,
		ConsecutiveDown: row.ConsecutiveDown,
		InIncident:      row.InIncident,
	}
	if row.LastAlertAt.Valid {
		t := row.LastAlertAt.Time
		st.LastAlertAt = &t
	}
	return st
}

func toTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
