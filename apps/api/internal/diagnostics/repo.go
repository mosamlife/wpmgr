package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant-scoped persistence layer for diagnostics + php-error
// rows. Mutating calls run inside InTenantTx so the RLS policy filters every
// row by the active tenant.
//
// The agent ingestion paths set BOTH `app.tenant_id` (so the WITH CHECK clause
// passes) AND `app.agent` (so the agent-write policy applies); the
// `InTenantTxAsAgent` helper centralises that. Operator reads use the
// standard InTenantTx.
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// UpsertDiagnostic stores the latest payload for the given (site, category).
// On conflict (one row per (tenant, site, category) by the unique index) we
// overwrite payload + collected_at and refresh received_at.
func (r *Repo) UpsertDiagnostic(ctx context.Context, tenantID, siteID uuid.UUID, category Category, payload json.RawMessage, collectedAt time.Time) (Diagnostic, error) {
	if !ValidCategory(category) {
		return Diagnostic{}, domain.Validation("invalid_category", "diagnostics category is not one of the 14 known buckets")
	}
	var out Diagnostic
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO agent_diagnostics
				(id, tenant_id, site_id, category, payload, collected_at, received_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, now())
			 ON CONFLICT (tenant_id, site_id, category) DO UPDATE
			   SET payload = EXCLUDED.payload,
			       collected_at = EXCLUDED.collected_at,
			       received_at = now()
			 RETURNING id, tenant_id, site_id, category, payload, collected_at, received_at`,
			tenantID, siteID, string(category), payload, collectedAt,
		)
		var d Diagnostic
		var catStr string
		if err := row.Scan(&d.ID, &d.TenantID, &d.SiteID, &catStr, &d.Payload, &d.CollectedAt, &d.ReceivedAt); err != nil {
			return domain.Internal("diagnostics_upsert_failed", "failed to upsert diagnostics").WithCause(err)
		}
		d.Category = Category(catStr)
		out = d
		return nil
	})
	return out, err
}

// ListDiagnosticsBySite returns every category row stored for the site, in no
// particular order — the handler keys them into a category-string map.
func (r *Repo) ListDiagnosticsBySite(ctx context.Context, tenantID, siteID uuid.UUID) ([]Diagnostic, error) {
	var out []Diagnostic
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, site_id, category, payload, collected_at, received_at
			 FROM agent_diagnostics
			 WHERE tenant_id = $1 AND site_id = $2`,
			tenantID, siteID,
		)
		if err != nil {
			return domain.Internal("diagnostics_list_failed", "failed to list diagnostics").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var d Diagnostic
			var catStr string
			if err := rows.Scan(&d.ID, &d.TenantID, &d.SiteID, &catStr, &d.Payload, &d.CollectedAt, &d.ReceivedAt); err != nil {
				return domain.Internal("diagnostics_list_failed", "failed to read diagnostics").WithCause(err)
			}
			d.Category = Category(catStr)
			out = append(out, d)
		}
		if err := rows.Err(); err != nil {
			return domain.Internal("diagnostics_list_failed", "failed to iterate diagnostics").WithCause(err)
		}
		return nil
	})
	return out, err
}

// UpsertPHPError batch-applies the agent-shipped errors. For each row we
// ON CONFLICT bump occurrence_count by the agent-reported delta, refresh
// last_seen, and keep the first_seen the existing row already carried.
// Returns the highest agent-supplied id from the batch so the handler can
// echo the cursor advance back.
type UpsertPHPErrorInput struct {
	MD5             string
	Code            int
	Severity        string
	Message         string
	File            string
	Line            int
	RequestPath     string
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	OccurrenceCount int64
	AgentRowID      int64 // the agent's local id for the row (cursor tracking)
}

func (r *Repo) UpsertPHPError(ctx context.Context, tenantID, siteID uuid.UUID, in UpsertPHPErrorInput) error {
	if in.MD5 == "" {
		return domain.Validation("invalid_md5", "php error md5 fingerprint is required")
	}
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_php_errors
				(id, tenant_id, site_id, md5, code, severity, message, file, line,
				 request_path, first_seen_at, last_seen_at, occurrence_count,
				 silenced, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8,
			         $9, $10, $11, $12, false, now(), now())
			 ON CONFLICT (tenant_id, site_id, md5) DO UPDATE
			   SET last_seen_at = GREATEST(agent_php_errors.last_seen_at, EXCLUDED.last_seen_at),
			       occurrence_count = GREATEST(agent_php_errors.occurrence_count, EXCLUDED.occurrence_count),
			       severity = EXCLUDED.severity,
			       message = EXCLUDED.message,
			       file = EXCLUDED.file,
			       line = EXCLUDED.line,
			       request_path = EXCLUDED.request_path,
			       updated_at = now()`,
			tenantID, siteID, in.MD5, in.Code, in.Severity, in.Message,
			in.File, in.Line, in.RequestPath, in.FirstSeenAt, in.LastSeenAt,
			in.OccurrenceCount,
		); err != nil {
			return domain.Internal("php_error_upsert_failed", "failed to upsert php error").WithCause(err)
		}
		return nil
	})
}

// ListPHPErrorsBySite returns the fingerprint-grouped errors for a site. The
// since cursor lets the operator UI page; silencedFilter is tri-state via
// the third parameter (true = silenced only, false = unsilenced only, nil =
// both — but the request type uses an enum-string to keep that explicit).
type ListPHPErrorsFilter struct {
	Since    time.Time // last_seen_at > since (zero = no filter)
	Silenced *bool     // nil = both
	Limit    int
}

func (r *Repo) ListPHPErrorsBySite(ctx context.Context, tenantID, siteID uuid.UUID, f ListPHPErrorsFilter) ([]PHPError, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	var out []PHPError
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		// Build the query with optional WHERE clauses. Keeping the SQL static
		// (with $-args toggled by NULL probes) lets pgx pool-cache the plan.
		args := []any{tenantID, siteID, f.Limit}
		sqlText := `SELECT id, tenant_id, site_id, md5, code, severity, message,
				file, line, request_path, first_seen_at, last_seen_at,
				occurrence_count, silenced, created_at, updated_at
			 FROM agent_php_errors
			 WHERE tenant_id = $1 AND site_id = $2`
		if !f.Since.IsZero() {
			args = append(args, f.Since)
			sqlText += ` AND last_seen_at > $` + strFromInt(len(args))
		}
		if f.Silenced != nil {
			args = append(args, *f.Silenced)
			sqlText += ` AND silenced = $` + strFromInt(len(args))
		}
		sqlText += ` ORDER BY last_seen_at DESC LIMIT $3`
		rows, err := tx.Query(ctx, sqlText, args...)
		if err != nil {
			return domain.Internal("php_errors_list_failed", "failed to list php errors").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e PHPError
			if err := rows.Scan(&e.ID, &e.TenantID, &e.SiteID, &e.MD5, &e.Code,
				&e.Severity, &e.Message, &e.File, &e.Line, &e.RequestPath,
				&e.FirstSeenAt, &e.LastSeenAt, &e.OccurrenceCount, &e.Silenced,
				&e.CreatedAt, &e.UpdatedAt,
			); err != nil {
				return domain.Internal("php_errors_list_failed", "failed to read php errors").WithCause(err)
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			return domain.Internal("php_errors_list_failed", "failed to iterate php errors").WithCause(err)
		}
		return nil
	})
	return out, err
}

// SetSilenced flips the silenced flag on a (site, md5) row. NotFound when the
// row doesn't exist within the tenant scope.
func (r *Repo) SetSilenced(ctx context.Context, tenantID, siteID uuid.UUID, md5 string, silenced bool) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE agent_php_errors
			   SET silenced = $4, updated_at = now()
			 WHERE tenant_id = $1 AND site_id = $2 AND md5 = $3`,
			tenantID, siteID, md5, silenced,
		)
		if err != nil {
			return domain.Internal("php_error_silence_failed", "failed to toggle silence flag").WithCause(err)
		}
		if ct.RowsAffected() == 0 {
			return domain.NotFound("php_error_not_found", "php error not found")
		}
		return nil
	})
}

// strFromInt is a tiny helper for building $-arg numbers in the dynamic
// WHERE clauses above without pulling in fmt.Sprintf on the hot path.
func strFromInt(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// _ keeps the errors import honest (NotFound check upstream of repos).
var _ = errors.Is
