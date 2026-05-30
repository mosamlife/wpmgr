package security

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant-scoped persistence layer for the security domain.
//
// Operator reads/writes use InTenantTx (app.tenant_id GUC).
// Agent ingest uses InTenantTx as well — the agent authenticator already
// resolved the (tenantID, siteID) pair, so we rely on the same RLS policy.
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// GetConfig returns the stored security config for (tenantID, siteID).
// found=false (and no error) when no row exists yet; callers should return
// the built-in default config.
func (r *Repo) GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (SecurityConfig, bool, error) {
	var out SecurityConfig
	var found bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, site_id, mode, thresholds, ip_header,
			        allow_cidrs, deny_cidrs, updated_at
			 FROM site_security_config
			 WHERE tenant_id = $1 AND site_id = $2`,
			tenantID, siteID,
		)
		var thresholds agentcmd.SecurityThresholds
		var allowCIDRs, denyCIDRs []string
		if err := row.Scan(
			&out.TenantID, &out.SiteID, &out.Mode,
			&thresholds,
			&out.IPHeader, &allowCIDRs, &denyCIDRs, &out.UpdatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("security_config_get_failed", "failed to get security config").WithCause(err)
		}
		out.Thresholds = thresholds
		if allowCIDRs == nil {
			allowCIDRs = []string{}
		}
		if denyCIDRs == nil {
			denyCIDRs = []string{}
		}
		out.AllowCIDRs = allowCIDRs
		out.DenyCIDRs = denyCIDRs
		found = true
		return nil
	})
	return out, found, err
}

// UpsertConfig inserts or replaces the security config for (tenantID, siteID).
// updated_at is refreshed on every upsert. Returns the stored config.
func (r *Repo) UpsertConfig(ctx context.Context, cfg SecurityConfig) (SecurityConfig, error) {
	var out SecurityConfig
	allowCIDRs := cfg.AllowCIDRs
	if allowCIDRs == nil {
		allowCIDRs = []string{}
	}
	denyCIDRs := cfg.DenyCIDRs
	if denyCIDRs == nil {
		denyCIDRs = []string{}
	}
	err := r.pool.InTenantTx(ctx, cfg.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO site_security_config
				(tenant_id, site_id, mode, thresholds, ip_header, allow_cidrs, deny_cidrs, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, now())
			 ON CONFLICT (site_id) DO UPDATE
			   SET mode        = EXCLUDED.mode,
			       thresholds  = EXCLUDED.thresholds,
			       ip_header   = EXCLUDED.ip_header,
			       allow_cidrs = EXCLUDED.allow_cidrs,
			       deny_cidrs  = EXCLUDED.deny_cidrs,
			       updated_at  = now()
			 RETURNING tenant_id, site_id, mode, thresholds, ip_header,
			           allow_cidrs, deny_cidrs, updated_at`,
			cfg.TenantID, cfg.SiteID, cfg.Mode, cfg.Thresholds,
			cfg.IPHeader, allowCIDRs, denyCIDRs,
		)
		var thresholds agentcmd.SecurityThresholds
		var ac, dc []string
		if err := row.Scan(
			&out.TenantID, &out.SiteID, &out.Mode,
			&thresholds,
			&out.IPHeader, &ac, &dc, &out.UpdatedAt,
		); err != nil {
			return domain.Internal("security_config_upsert_failed", "failed to upsert security config").WithCause(err)
		}
		out.Thresholds = thresholds
		if ac == nil {
			ac = []string{}
		}
		if dc == nil {
			dc = []string{}
		}
		out.AllowCIDRs = ac
		out.DenyCIDRs = dc
		return nil
	})
	return out, err
}

// InsertLoginEventsBatch bulk-inserts the agent-shipped login events, ignoring
// duplicates (ON CONFLICT DO NOTHING on the dedup index). Returns the highest
// agent_event_id in the batch so the handler can return it to the agent for
// cursor advancement.
func (r *Repo) InsertLoginEventsBatch(ctx context.Context, tenantID, siteID uuid.UUID, events []LoginEvent) (int64, error) {
	if len(events) == 0 {
		return 0, nil
	}
	var highest int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		for _, e := range events {
			if _, err := tx.Exec(ctx,
				`INSERT INTO agent_login_events
					(tenant_id, site_id, agent_event_id, ip, status, category,
					 username, request_id, occurred_at, ingested_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
				 ON CONFLICT (tenant_id, site_id, agent_event_id) DO NOTHING`,
				tenantID, siteID, e.AgentEventID, e.IP, e.Status, e.Category,
				e.Username, e.RequestID, e.OccurredAt,
			); err != nil {
				return domain.Internal("login_events_insert_failed", "failed to insert login event").WithCause(err)
			}
			if e.AgentEventID > highest {
				highest = e.AgentEventID
			}
		}
		return nil
	})
	return highest, err
}

// ListLoginEvents returns login events for a site, ordered by occurred_at DESC.
// limit is clamped to [1, 500]. statusFilter is a tri-state: nil = all statuses.
func (r *Repo) ListLoginEvents(ctx context.Context, tenantID, siteID uuid.UUID, limit int, statusFilter *LoginEventStatus) ([]LoginEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []LoginEvent
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		args := []any{tenantID, siteID, limit}
		sqlText := `SELECT id, tenant_id, site_id, agent_event_id, ip, status,
				        category, username, request_id, occurred_at, ingested_at
				 FROM agent_login_events
				 WHERE tenant_id = $1 AND site_id = $2`
		if statusFilter != nil {
			args = append(args, int16(*statusFilter))
			sqlText += ` AND status = $` + itoa(len(args))
		}
		sqlText += ` ORDER BY occurred_at DESC LIMIT $3`

		rows, err := tx.Query(ctx, sqlText, args...)
		if err != nil {
			return domain.Internal("login_events_list_failed", "failed to list login events").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var ev LoginEvent
			var occurredAt *time.Time
			if err := rows.Scan(
				&ev.ID, &ev.TenantID, &ev.SiteID, &ev.AgentEventID,
				&ev.IP, &ev.Status, &ev.Category, &ev.Username,
				&ev.RequestID, &occurredAt, &ev.IngestedAt,
			); err != nil {
				return domain.Internal("login_events_list_failed", "failed to read login event").WithCause(err)
			}
			if occurredAt != nil {
				ev.OccurredAt = *occurredAt
			}
			out = append(out, ev)
		}
		if err := rows.Err(); err != nil {
			return domain.Internal("login_events_list_failed", "failed to iterate login events").WithCause(err)
		}
		return nil
	})
	return out, err
}

// itoa is a tiny helper for building $-arg numbers in dynamic WHERE clauses.
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// keep errors import honest.
var _ = errors.Is
