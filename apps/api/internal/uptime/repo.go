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

	// Evaluator path (app.agent GUC, cross-tenant).
	ListAlertConfigsAllTenants(ctx context.Context) ([]AlertConfig, error)

	// Tenant-scoped config CRUD (RLS).
	GetAlertConfig(ctx context.Context, tenantID uuid.UUID) (AlertConfig, bool, error)
	UpsertAlertConfig(ctx context.Context, cfg AlertConfig) (AlertConfig, error)
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
