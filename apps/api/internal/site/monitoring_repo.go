package site

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #414 m117 — the pause/resume writes.
//
// These are hand-written SQL rather than sqlc queries on purpose:
// apps/api/db/query/**.sql belongs to database-engineer, and raw SQL inside a
// tx helper is the established shape in this tree (internal/scan/repo.go,
// internal/security/repo.go, internal/activity/repo.go and ~17 others). The
// RLS contract is unchanged either way — every statement below runs inside
// InTenantTxAsUser, which sets app.tenant_id and app.user_id, and carries an
// explicit tenant_id in its WHERE as defence in depth.
//
// InTenantTxAsUser rather than InTenantTx: the audit hash chain keys on
// app.user_id, and the handler records one audit event per site inside the
// same request.

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
// $1 tenant_id, $2 site ids, $3 actor user id (NULL for an API-key actor),
// $4 reason, $5 resume_at.
const pauseMonitoringSQL = `
UPDATE sites s
   SET monitoring_paused_at     = COALESCE(s.monitoring_paused_at, now()),
       monitoring_paused_by     = CASE WHEN s.monitoring_paused_at IS NULL
                                       THEN $3::uuid ELSE s.monitoring_paused_by END,
       monitoring_paused_reason = CASE WHEN s.monitoring_paused_at IS NULL
                                       THEN $4::text ELSE s.monitoring_paused_reason END,
       monitoring_resume_at     = CASE WHEN s.monitoring_paused_at IS NULL
                                       THEN $5::timestamptz ELSE s.monitoring_resume_at END,
       updated_at               = CASE WHEN s.monitoring_paused_at IS NULL
                                       THEN now() ELSE s.updated_at END
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
          (prior.monitoring_paused_at IS NOT NULL) AS was_paused`

// resumeMonitoringSQL clears the pause on every named site in the tenant.
//
// It clears monitoring_paused_at AND monitoring_resume_at in ONE statement.
// sites_monitoring_resume_requires_pause_check rejects a resume instant on a
// row that is not paused, so clearing the pause while leaving the resume
// instant behind raises 23514 — the two columns must move together.
//
// Resuming an already-active site writes NULL over NULL: a success with
// changed=false, never an error.
const resumeMonitoringSQL = `
UPDATE sites s
   SET monitoring_paused_at     = NULL,
       monitoring_paused_by     = NULL,
       monitoring_paused_reason = '',
       monitoring_resume_at     = NULL,
       updated_at               = CASE WHEN s.monitoring_paused_at IS NULL
                                       THEN s.updated_at ELSE now() END
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
          (prior.monitoring_paused_at IS NOT NULL) AS was_paused`

func (r *pgRepo) PauseMonitoring(ctx context.Context, in PauseMonitoringInput) ([]MonitoringState, error) {
	var out []MonitoringState
	err := r.pool.InTenantTxAsUser(ctx, in.TenantID, in.ActorUserID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, pauseMonitoringSQL,
			in.TenantID,
			in.SiteIDs,
			nullableUUID(in.ActorUserID),
			in.Reason,
			in.ResumeAt,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanMonitoringStates(rows)
		return err
	})
	if err != nil {
		return nil, domain.Internal("monitoring_pause_failed", "could not pause monitoring")
	}
	return out, nil
}

func (r *pgRepo) ResumeMonitoring(ctx context.Context, in ResumeMonitoringInput) ([]MonitoringState, error) {
	var out []MonitoringState
	err := r.pool.InTenantTxAsUser(ctx, in.TenantID, in.ActorUserID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, resumeMonitoringSQL, in.TenantID, in.SiteIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanMonitoringStates(rows)
		return err
	})
	if err != nil {
		return nil, domain.Internal("monitoring_resume_failed", "could not resume monitoring")
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
		)
		if err := rows.Scan(&id, &pausedAt, &pausedBy, &reason, &resumeAt, &wasPaused); err != nil {
			return nil, err
		}
		st := MonitoringState{SiteID: id, PausedReason: reason}
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
