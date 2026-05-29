package activity

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant-scoped persistence for the activity log. The agent ingest
// path and the operator read path both run under InTenantTx, which sets
// app.tenant_id so the tenant_isolation RLS policy filters every row; the
// agent INSERT additionally satisfies the agent_activity_log_agent policy via
// the app.agent GUC. The ingest path threads its own pgx.Tx so the prior-row
// lookup + upsert are a single atomic, RLS-scoped unit.
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// IngestTx runs fn inside a single tenant-scoped transaction so the prior-row
// lookup + upsert are one atomic, RLS-scoped unit. The agent ingest path
// resolves the tenant from the verified Ed25519 identity (NOT a client header)
// before calling here, and InTenantTx sets app.tenant_id so the
// tenant_isolation policy's WITH CHECK passes on INSERT — mirroring the
// Sprint 2 diagnostics ingest path. (The agent_activity_log_agent policy is an
// additional permissive escape for cross-tenant maintenance; the tenant-scoped
// write here is satisfied by tenant_isolation alone.)
func (r *Repo) IngestTx(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	return r.pool.InTenantTx(ctx, tenantID, fn)
}

// GetPriorHash returns the this_hash of the row with the greatest seq strictly
// below the given seq for this (tenant, site). Returns ("", false, nil) when
// there is no prior row (the given seq is at/before the chain start).
func GetPriorHash(ctx context.Context, tx pgx.Tx, tenantID, siteID uuid.UUID, seq int64) (string, bool, error) {
	var h string
	err := tx.QueryRow(ctx,
		`SELECT this_hash FROM agent_activity_log
		  WHERE tenant_id = $1 AND site_id = $2 AND seq < $3
		  ORDER BY seq DESC LIMIT 1`,
		tenantID, siteID, seq,
	).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, domain.Internal("activity_prior_hash_failed", "failed to read prior chain hash").WithCause(err)
	}
	return h, true, nil
}

// UpsertEvent inserts (or, on agent retry, refreshes) one activity row by
// (tenant_id, site_id, seq). Idempotent: a re-shipped event overwrites the
// stored hashes + chain_valid so a corrected agent re-ship can heal a row.
func UpsertEvent(ctx context.Context, tx pgx.Tx, e Event) error {
	metaJSON, err := json.Marshal(orEmptyMeta(e.Meta))
	if err != nil {
		return domain.Internal("activity_meta_marshal_failed", "failed to encode activity meta").WithCause(err)
	}
	metaRaw := e.MetaRaw
	if metaRaw == "" {
		metaRaw = "{}"
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO agent_activity_log
			(tenant_id, site_id, seq, event_type, object_type, object_id, object_label,
			 actor_user_id, actor_login, actor_ip, summary, meta, meta_raw, severity,
			 prev_hash, this_hash, chain_valid, occurred_at, received_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18, now())
		 ON CONFLICT (tenant_id, site_id, seq) DO UPDATE SET
			event_type   = EXCLUDED.event_type,
			object_type  = EXCLUDED.object_type,
			object_id    = EXCLUDED.object_id,
			object_label = EXCLUDED.object_label,
			actor_user_id= EXCLUDED.actor_user_id,
			actor_login  = EXCLUDED.actor_login,
			actor_ip     = EXCLUDED.actor_ip,
			summary      = EXCLUDED.summary,
			meta         = EXCLUDED.meta,
			meta_raw     = EXCLUDED.meta_raw,
			severity     = EXCLUDED.severity,
			prev_hash    = EXCLUDED.prev_hash,
			this_hash    = EXCLUDED.this_hash,
			chain_valid  = EXCLUDED.chain_valid,
			occurred_at  = EXCLUDED.occurred_at,
			received_at  = now()`,
		e.TenantID, e.SiteID, e.Seq, e.EventType, e.ObjectType, e.ObjectID, e.ObjectLabel,
		e.ActorUserID, e.ActorLogin, e.ActorIP, e.Summary, metaJSON, metaRaw, e.Severity,
		e.PrevHash, e.ThisHash, e.ChainValid, e.OccurredAt,
	)
	if err != nil {
		return domain.Internal("activity_upsert_failed", "failed to upsert activity event").WithCause(err)
	}
	return nil
}

// List returns a filtered, paginated page of a site's activity, newest first.
func (r *Repo) List(ctx context.Context, tenantID, siteID uuid.UUID, f ListFilter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	var sb strings.Builder
	sb.WriteString(`SELECT id, tenant_id, site_id, seq, event_type, object_type, object_id,
		object_label, actor_user_id, actor_login, actor_ip, summary, meta, meta_raw, severity,
		prev_hash, this_hash, chain_valid, occurred_at, received_at
		FROM agent_activity_log
		WHERE tenant_id = $1 AND site_id = $2`)
	args := []any{tenantID, siteID}
	add := func(clause string, val any) {
		args = append(args, val)
		sb.WriteString(clause)
		sb.WriteString("$")
		// positional index is len(args)
		sb.WriteString(itoa(len(args)))
	}
	if f.EventType != "" {
		add(" AND event_type = ", f.EventType)
	}
	if f.ObjectType != "" {
		add(" AND object_type = ", f.ObjectType)
	}
	if f.ActorLogin != "" {
		add(" AND actor_login = ", f.ActorLogin)
	}
	if f.Severity != "" {
		add(" AND severity = ", f.Severity)
	}
	if !f.Since.IsZero() {
		add(" AND occurred_at >= ", f.Since)
	}
	if !f.Until.IsZero() {
		add(" AND occurred_at <= ", f.Until)
	}
	sb.WriteString(" ORDER BY seq DESC LIMIT ")
	args = append(args, limit)
	sb.WriteString("$")
	sb.WriteString(itoa(len(args)))
	if f.Offset > 0 {
		args = append(args, f.Offset)
		sb.WriteString(" OFFSET $")
		sb.WriteString(itoa(len(args)))
	}

	var out []Event
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sb.String(), args...)
		if err != nil {
			return domain.Internal("activity_list_failed", "failed to list activity").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			e, serr := scanEvent(rows)
			if serr != nil {
				return serr
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ListForVerify returns every row for a (tenant, site) ordered seq ASC so the
// chain can be folded from genesis. Mirrors audit.ListAuditEntriesForVerify.
func (r *Repo) ListForVerify(ctx context.Context, tenantID, siteID uuid.UUID) ([]Event, error) {
	var out []Event
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, site_id, seq, event_type, object_type, object_id,
				object_label, actor_user_id, actor_login, actor_ip, summary, meta, meta_raw, severity,
				prev_hash, this_hash, chain_valid, occurred_at, received_at
				FROM agent_activity_log
				WHERE tenant_id = $1 AND site_id = $2
				ORDER BY seq ASC`,
			tenantID, siteID,
		)
		if err != nil {
			return domain.Internal("activity_verify_load_failed", "failed to load activity for verify").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			e, serr := scanEvent(rows)
			if serr != nil {
				return serr
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func scanEvent(rows pgx.Rows) (Event, error) {
	var e Event
	var metaJSON []byte   // the parsed JSONB meta column (display/query)
	var metaRawCol string // the verbatim agent bytes (hash re-verification)
	if err := rows.Scan(
		&e.ID, &e.TenantID, &e.SiteID, &e.Seq, &e.EventType, &e.ObjectType, &e.ObjectID,
		&e.ObjectLabel, &e.ActorUserID, &e.ActorLogin, &e.ActorIP, &e.Summary, &metaJSON, &metaRawCol, &e.Severity,
		&e.PrevHash, &e.ThisHash, &e.ChainValid, &e.OccurredAt, &e.ReceivedAt,
	); err != nil {
		return Event{}, domain.Internal("activity_scan_failed", "failed to scan activity row").WithCause(err)
	}
	// MetaRaw is the hash preimage source — NEVER re-derive it from the parsed
	// map (Postgres JSONB normalizes/re-orders keys). Use the stored text col.
	e.MetaRaw = metaRawCol
	// Display from the SAME authoritative bytes the hash covers (meta_raw), not
	// the convenience JSONB column. This closes a tamper gap: editing only the
	// jsonb "meta" column would otherwise change what the operator sees without
	// tripping the chain. The jsonb column is retained for potential future
	// meta-field querying but is never the display source of truth.
	src := metaRawCol
	if src == "" && len(metaJSON) > 0 {
		src = string(metaJSON)
	}
	if src != "" {
		_ = json.Unmarshal([]byte(src), &e.Meta)
	}
	return e, nil
}

func orEmptyMeta(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func itoa(n int) string { return strconv.Itoa(n) }
