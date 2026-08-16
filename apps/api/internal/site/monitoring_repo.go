package site

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #414 m117 — the pause/resume writes.
//
// These are hand-written SQL rather than sqlc queries on purpose:
// apps/api/db/query/**.sql belongs to database-engineer, and raw SQL inside a
// tx helper is the established shape in this tree (internal/scan/repo.go,
// internal/security/repo.go, internal/activity/repo.go and ~17 others).
//
// EVERY STATEMENT BELOW RUNS THROUGH pool.RunTenantTx, NEVER InTenantTxAsUser
// DIRECTLY. That dispatch (internal/db/db.go) is the only thing that sets
// app.site_scope and app.allowed_site_ids for a site-scoped principal, which
// is what the RESTRICTIVE sites_site_scope policy keys on. Hard-coding
// InTenantTxAsUser here left that policy inert and reduced a site-scoped
// collaborator's containment to one Go `if` in the handler: the same UPDATE
// wrote 1 row through the repo and 0 rows under InScopedTenantTx. The Go gate
// stays, but it is now the second layer rather than the only one.
// internal/site/repo.go:List is the same dispatch in this package.
//
// For an org-scoped user principal RunTenantTx still lands on
// InTenantTxAsUser, which the audit hash chain needs: it keys on app.user_id
// and the handler records one audit event per site in the same request.
//
// Every statement also carries an explicit tenant_id in its WHERE even though
// RLS scopes the rows: defence in depth, and it keeps the index in play.

// pauseMonitoringSQL pauses every named site in the tenant, idempotently.
//
// THE IDEMPOTENCE IS IN THE SET LIST, NOT IN A PRIOR READ. In an UPDATE the
// right-hand side of every SET expression sees the OLD row, so
// COALESCE(s.monitoring_paused_at, now()) keeps the original pause instant and
// each CASE keeps the original by/reason/resume_at when the row was already
// paused. A retried request therefore cannot overwrite the reason someone
// typed, and no read-then-write race exists to lose.
//
// The FROM subquery takes FOR UPDATE so two concurrent pauses of the same row
// serialize, and it is also what lets RETURNING report the PRIOR state: prior
// .monitoring_paused_at is the value before this statement, which is how
// `changed` is computed without a second round trip.
//
// NOT PAUSABLE: archived and revoked. Both are lifecycle dead-ends for this
// purpose. 'archived' is the soft-delete state the default sites list hides, so
// a pause applied there could never be resumed from the interface; 'revoked' is
// an operator-disconnected site that has no monitoring left to pause. They come
// back in the report as site_archived / site_revoked rather than silently
// succeeding, and the state list is bound as $6 so Go owns it in one place
// (monitoringPauseBlockedStates) instead of being spelled twice.
//
// UPDATED_AT IS DELIBERATELY NOT TOUCHED. sites.updated_at is not a private
// mtime: handler.go:577 serves it as `as_of` on GET /sites/{id}/updates, the
// freshness stamp for the plugin/theme inventory. Bumping it on a pause would
// claim the inventory had just been refreshed when nothing was fetched, which
// is the one thing monitoring.go says pause must never do — pause means "do not
// tell me", never "lie to me". "When did the pause change" is already answerable
// from monitoring_paused_at, which is the timestamp that actually moved.
//
// $1 tenant_id, $2 site ids, $3 actor user id (NULL for an API-key actor),
// $4 reason, $5 resume_at, $6 the connection_states that refuse a pause.
const pauseMonitoringSQL = `
WITH prior AS (
        SELECT id,
               monitoring_paused_at,
               monitoring_paused_by,
               monitoring_paused_reason,
               monitoring_resume_at,
               connection_state
          FROM sites
         WHERE tenant_id = $1 AND id = ANY($2::uuid[])
           FOR UPDATE
), updated AS (
        UPDATE sites s
           SET monitoring_paused_at     = COALESCE(s.monitoring_paused_at, now()),
               monitoring_paused_by     = CASE WHEN s.monitoring_paused_at IS NULL
                                               THEN $3::uuid ELSE s.monitoring_paused_by END,
               monitoring_paused_reason = CASE WHEN s.monitoring_paused_at IS NULL
                                               THEN $4::text ELSE s.monitoring_paused_reason END,
               monitoring_resume_at     = CASE WHEN s.monitoring_paused_at IS NULL
                                               THEN $5::timestamptz ELSE s.monitoring_resume_at END
          FROM prior
         WHERE s.id = prior.id
           AND s.tenant_id = $1
           AND NOT (prior.connection_state::text = ANY($6::text[]))
        RETURNING s.id,
                  s.monitoring_paused_at,
                  s.monitoring_paused_by,
                  s.monitoring_paused_reason,
                  s.monitoring_resume_at,
                  (prior.monitoring_paused_at IS NOT NULL) AS was_paused,
                  prior.connection_state::text            AS connection_state
)
SELECT id, monitoring_paused_at, monitoring_paused_by,
       monitoring_paused_reason, monitoring_resume_at, was_paused, connection_state
  FROM updated
 UNION ALL
SELECT p.id, p.monitoring_paused_at, p.monitoring_paused_by,
       p.monitoring_paused_reason, p.monitoring_resume_at,
       (p.monitoring_paused_at IS NOT NULL), p.connection_state::text
  FROM prior p
 WHERE p.connection_state::text = ANY($6::text[])`

// resumeMonitoringSQL clears the pause on every named site in the tenant.
//
// It clears monitoring_paused_at AND monitoring_resume_at in ONE statement.
// sites_monitoring_resume_requires_pause_check rejects a resume instant on a
// row that is not paused, so clearing the pause while leaving the resume
// instant behind raises 23514 — the two columns must move together.
//
// Resuming an already-active site writes NULL over NULL: a success with
// changed=false, never an error.
//
// NO connection_state GATE HERE, unlike pause: an archived or revoked site that
// is already paused must still be resumable, otherwise a row could be stranded
// paused forever by archiving it. Refusing the pause is what keeps that from
// arising; refusing the resume would create it.
//
// updated_at is untouched for the same reason as the pause — see above.
const resumeMonitoringSQL = `
UPDATE sites s
   SET monitoring_paused_at     = NULL,
       monitoring_paused_by     = NULL,
       monitoring_paused_reason = '',
       monitoring_resume_at     = NULL
  FROM (
        SELECT id, monitoring_paused_at
          FROM sites
         WHERE tenant_id = $1 AND id = ANY($2::uuid[])
           FOR UPDATE
       ) prior
 WHERE s.id = prior.id
   AND s.tenant_id = $1
RETURNING s.id,
          s.monitoring_paused_at,
          s.monitoring_paused_by,
          s.monitoring_paused_reason,
          s.monitoring_resume_at,
          (prior.monitoring_paused_at IS NOT NULL) AS was_paused,
          s.connection_state::text                 AS connection_state`

// monitoringTx is the ONE place these two writes acquire a transaction, so
// there is a single line to audit for the site-scope dispatch.
//
// A nil principal is refused rather than quietly falling back to a tenant-wide
// transaction: the fallback is exactly the shape that made the RESTRICTIVE
// policy inert here in the first place, and every caller of these two methods
// arrives from an authenticated request that has one. It returns an error
// rather than dereferencing nil — a nil interface would panic inside
// RunTenantTx, and nothing in a request path may panic.
func (r *pgRepo) monitoringTx(ctx context.Context, p ScopedPrincipal, fn func(tx pgx.Tx) error) error {
	if p == nil {
		return errMonitoringPrincipalRequired
	}
	return r.pool.RunTenantTx(ctx, p, fn)
}

var errMonitoringPrincipalRequired = errors.New("monitoring: principal required for tenant transaction")

// monitoringTxFailureCause classifies a monitoringTx error for the diagnostic
// log line only. It never changes what PauseMonitoring/ResumeMonitoring
// return to their caller — the client-facing domain.Internal code stays
// generic and stable on purpose, since callers and the integration tests
// (tests/gh414_*_test.go) key on "monitoring_pause_failed"/
// "monitoring_resume_failed" — but a check violation, a foreign-key
// violation, a deadlock, a context cancellation and a plain connection
// failure otherwise land in the SAME response with nothing logged, which is
// exactly the gap that stayed invisible until an incident.
//
// errMonitoringPrincipalRequired gets its own cause label rather than falling
// into "unknown": it is not a database failure at all. Service.PauseMonitoring
// and Service.ResumeMonitoring (monitoring.go) already refuse a nil principal
// before ever calling the repo, so monitoringTx's own nil check is a second,
// defense-in-depth layer that should be unreachable from a real request. If
// it fires, an on-call reader needs to know immediately that the first guard
// was bypassed by some caller — a worker, a future direct repo caller — not
// spend time down the pgconn/SQLSTATE branch chasing a phantom DB incident.
// It still comes back as the same generic domain.Internal code: the caller
// did nothing wrong here (there is no request input that could trigger this),
// so there is nothing for a 4xx to tell them.
func monitoringTxFailureCause(err error) (cause, sqlstate, pgMessage string) {
	switch {
	case errors.Is(err, errMonitoringPrincipalRequired):
		return "nil_principal_invariant", "", ""
	case errors.Is(err, context.Canceled):
		return "context_canceled", "", ""
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded", "", ""
	default:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// pgErr.Message only, never pgErr.Detail: Detail can embed the
			// offending row's values (e.g. a unique-violation's
			// "Key (col)=(value) already exists"), which is exactly the
			// tenant data this log line must not carry.
			return "pg_error", pgErr.Code, pgErr.Message
		}
		return "unknown", "", ""
	}
}

// logMonitoringTxFailure preserves the cause of a pause/resume failure for
// diagnosis. Only tenant_id and a site COUNT are logged — never the site ids,
// the pause reason (free text an operator typed), or a pg error's Detail —
// so nothing here can put tenant data or a site secret in the log line.
func logMonitoringTxFailure(ctx context.Context, op string, tenantID uuid.UUID, siteCount int, err error) {
	cause, sqlstate, pgMessage := monitoringTxFailureCause(err)
	attrs := []any{
		slog.String("op", op),
		slog.String("tenant_id", tenantID.String()),
		slog.Int("site_count", siteCount),
		slog.String("cause", cause),
	}
	switch cause {
	case "pg_error":
		attrs = append(attrs, slog.String("sqlstate", sqlstate), slog.String("pg_message", pgMessage))
	case "unknown":
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	slog.ErrorContext(ctx, "monitoring: tx failed", attrs...)
}

func (r *pgRepo) PauseMonitoring(ctx context.Context, in PauseMonitoringInput) ([]MonitoringState, error) {
	var out []MonitoringState
	err := r.monitoringTx(ctx, in.Principal, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, pauseMonitoringSQL,
			in.TenantID,
			in.SiteIDs,
			nullableUUID(in.ActorUserID),
			in.Reason,
			in.ResumeAt,
			monitoringPauseBlockedStates,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanMonitoringStates(rows)
		return err
	})
	if err != nil {
		logMonitoringTxFailure(ctx, "pause", in.TenantID, len(in.SiteIDs), err)
		return nil, domain.Internal("monitoring_pause_failed", "could not pause monitoring").WithCause(err)
	}
	return out, nil
}

func (r *pgRepo) ResumeMonitoring(ctx context.Context, in ResumeMonitoringInput) ([]MonitoringState, error) {
	var out []MonitoringState
	err := r.monitoringTx(ctx, in.Principal, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, resumeMonitoringSQL, in.TenantID, in.SiteIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanMonitoringStates(rows)
		return err
	})
	if err != nil {
		logMonitoringTxFailure(ctx, "resume", in.TenantID, len(in.SiteIDs), err)
		return nil, domain.Internal("monitoring_resume_failed", "could not resume monitoring").WithCause(err)
	}
	return out, nil
}

func scanMonitoringStates(rows pgx.Rows) ([]MonitoringState, error) {
	out := make([]MonitoringState, 0, 8)
	for rows.Next() {
		var (
			id        uuid.UUID
			pausedAt  pgtype.Timestamptz
			pausedBy  pgtype.UUID
			reason    string
			resumeAt  pgtype.Timestamptz
			wasPaused bool
			connState string
		)
		if err := rows.Scan(&id, &pausedAt, &pausedBy, &reason, &resumeAt, &wasPaused, &connState); err != nil {
			return nil, err
		}
		st := MonitoringState{SiteID: id, PausedReason: reason, ConnectionState: connState}
		if pausedAt.Valid {
			t := pausedAt.Time
			st.PausedAt = &t
		}
		if resumeAt.Valid {
			t := resumeAt.Time
			st.ResumeAt = &t
		}
		if pausedBy.Valid {
			u := uuid.UUID(pausedBy.Bytes)
			st.PausedBy = &u
		}
		// Changed is "the pause bit moved": paused when it was not, or
		// unpaused when it was. A re-pause and a re-resume both land here as
		// false, which is what the caller needs to distinguish a real change
		// from an accepted retry.
		st.Changed = st.Paused() != wasPaused
		out = append(out, st)
	}
	return out, rows.Err()
}

func nullableUUID(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	return &u
}
