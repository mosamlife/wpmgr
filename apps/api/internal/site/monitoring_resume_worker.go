package site

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// GH #414 phase 5 — the auto-resume sweep.
//
// sites.monitoring_resume_at has been written since phase 1 and read by nothing.
// An operator who paused "until Friday 09:00" got a pause that lasted until
// somebody remembered, which is a worse promise than not offering the field.
// This worker is what makes the field mean something.
//
// IT ONLY EVER UN-PAUSES. There is no path here that pauses a site, changes a
// schedule, or touches any column other than the four monitoring_* ones. The
// failure mode of a bug in this file is that a site resumes early or late, never
// that a site stops being monitored — which is the right direction for the
// blast radius of a cross-tenant sweep to point.
//
// WHY IT IS CROSS-TENANT (InAgentTx) AND NOT A PER-TENANT FAN-OUT. The due set
// is a rare subset of a rare subset: rows with BOTH a live pause and a scheduled
// resume. m117 shipped sites_monitoring_resume_due_idx as a partial index over
// exactly that predicate, so one indexed range read finds every due row across
// the whole fleet. Enumerating tenants first would be one query per tenant to
// discover that almost none of them have a due row.

// autoResumeBatchSize bounds one tick.
//
// The predicate is near-empty by construction, so this cap is not a throughput
// knob — it is a blast-radius bound. If a bug (or an operator scripting a bulk
// pause with a past resume_at) ever made the due set enormous, one tick writes
// at most this many rows and holds its transaction for a bounded time instead of
// locking a large slice of the sites table while the audit writes trail behind
// it. Anything left over is picked up on the next tick a minute later.
const autoResumeBatchSize = 200

// claimDueAutoResumesSQL clears the pause on every site whose scheduled resume
// instant has arrived, and returns what the row looked like BEFORE the clear so
// the audit entry can name the pause it ended.
//
// IDEMPOTENT AND CONCURRENCY-SAFE, and both properties come from the statement
// rather than from a Go-side lock:
//
//   - EXACTLY ONCE. The UPDATE clears monitoring_resume_at in the same statement
//     that clears monitoring_paused_at, so the row stops matching this query's
//     own predicate the instant it commits. A second tick, a second replica, or
//     a retried River job finds nothing to do. RETURNING therefore yields a row
//     if and only if THIS statement is the one that resumed it, which is what
//     makes "audit exactly once" follow from "update exactly once" rather than
//     from a separate bookkeeping column.
//
//   - CONCURRENT WORKERS. FOR UPDATE SKIP LOCKED in the inner select: two
//     replicas ticking together take disjoint row sets and neither blocks. The
//     leader election in cmd/wpmgr/main.go already makes a second concurrent
//     tick unlikely, but "unlikely" is not a correctness argument for a
//     cross-tenant write, and SKIP LOCKED costs nothing.
//
//   - AN OPERATOR RESUMING BY HAND AT THE SAME MOMENT. resumeMonitoringSQL takes
//     FOR UPDATE on the same row. Whichever transaction locks first wins; the
//     loser re-evaluates its predicate against the committed row under READ
//     COMMITTED and finds monitoring_paused_at already NULL, so it updates
//     nothing and returns nothing. The site is resumed once and audited once,
//     under whichever actor actually did it. This is why the outer UPDATE
//     repeats `s.monitoring_paused_at IS NOT NULL` even though the inner select
//     already filtered on it: the inner select's snapshot can be stale by the
//     time the lock is granted, and that repeated predicate is the re-check.
//
// BOTH COLUMNS MOVE IN ONE UPDATE, which the schema insists on:
// sites_monitoring_resume_requires_pause_check is
// `monitoring_resume_at IS NULL OR monitoring_paused_at IS NOT NULL`, so
// clearing the pause while leaving the resume instant behind raises 23514 and
// would abort the whole sweep.
//
// monitoring_paused_reason is reset to ” rather than left as stale text,
// matching resumeMonitoringSQL. The constraint does not require it; consistency
// between the two resume paths does, because the sites list renders the reason
// whenever it is non-empty in some future revision and two paths that disagree
// is how that becomes a bug.
//
// BOTH `IS NOT NULL` CLAUSES ARE LOAD-BEARING FOR THE PLAN, NOT JUST THE LOGIC,
// and this is the trap for anyone writing a SECOND sweep by copying this one.
//
// sites_monitoring_resume_due_idx (m117) is a PARTIAL index:
//
//	ON sites (monitoring_resume_at)
//	WHERE monitoring_resume_at IS NOT NULL AND monitoring_paused_at IS NOT NULL
//
// sites_monitoring_resume_requires_pause_check makes the second half of that
// predicate logically redundant — a row with a resume instant always has a
// pause instant, so the two WHERE clauses select exactly the same rows. The
// planner does not know that. It proves a partial index usable from the
// QUERY's own clauses alone; it does not consult check constraints to discharge
// an index predicate. So the redundant-looking clause is what makes the index
// reachable, and dropping it silently costs the index rather than raising an
// error.
//
// Measured on 200k sites (95 MB table, 300 of them due), EXPLAIN ANALYZE as
// wpmgr_app with app.agent set:
//
//	both clauses (this query):   Index Scan ...resume_due_idx    13.8 ms
//	monitoring_resume_at only:   Seq Scan, 199,700 rows removed  395.0 ms
//
// A sweep written as the natural `WHERE monitoring_resume_at <= now()` is
// correct, passes every test, and scans the whole fleet. Repeat both clauses.
//
// $1 the sweep instant, $2 the batch cap.
const claimDueAutoResumesSQL = `
WITH due AS (
        SELECT id,
               monitoring_paused_at,
               monitoring_paused_reason,
               monitoring_resume_at
          FROM sites
         WHERE monitoring_resume_at IS NOT NULL
           AND monitoring_paused_at IS NOT NULL
           AND monitoring_resume_at <= $1::timestamptz
         ORDER BY monitoring_resume_at
         LIMIT $2
           FOR UPDATE SKIP LOCKED
)
UPDATE sites s
   SET monitoring_paused_at     = NULL,
       monitoring_paused_by     = NULL,
       monitoring_paused_reason = '',
       monitoring_resume_at     = NULL
  FROM due
 WHERE s.id = due.id
   AND s.monitoring_paused_at IS NOT NULL
   AND s.monitoring_resume_at IS NOT NULL
   AND s.monitoring_resume_at <= $1::timestamptz
RETURNING s.id,
          s.tenant_id,
          due.monitoring_paused_at,
          due.monitoring_paused_reason,
          due.monitoring_resume_at`

// AutoResumed is one site this sweep un-paused, carrying the pause it ended so
// the audit entry can describe it.
type AutoResumed struct {
	SiteID   uuid.UUID
	TenantID uuid.UUID
	// PausedAt is when the pause it ended began; nil only if the row raced in a
	// way that lost the value, which the audit metadata then simply omits.
	PausedAt *time.Time
	// PausedReason is the note the operator typed at pause time, carried into
	// the audit entry: without it the resume event says nothing about what was
	// being waited on, and the pause event that would have said so may be far
	// back in the log.
	PausedReason string
	// ResumeAt is the scheduled instant that came due — the reason this row was
	// touched at all. Recorded so an operator reading the audit log can tell an
	// on-time sweep from one that ran late.
	ResumeAt *time.Time
}

// AutoResumeRepo is the database half of the sweep. Narrow on purpose: a worker
// that can only claim due resumes cannot be talked into doing anything else.
type AutoResumeRepo interface {
	ClaimDueAutoResumes(ctx context.Context, now time.Time, limit int) ([]AutoResumed, error)
}

// ClaimDueAutoResumes runs one batch of the sweep cross-tenant.
//
// InAgentTx, not InTenantTx: the sweep spans every tenant and there is no
// principal. sites carries the `sites_agent` policy (m2) with no FOR clause,
// which is FOR ALL, so the locking read and the UPDATE are both admitted — the
// trap in .claude/rules/go-control-plane.md about a FOR SELECT-only agent policy
// silently excluding every row from a locking read does not apply here, and it
// was checked rather than assumed.
func (r *pgRepo) ClaimDueAutoResumes(ctx context.Context, now time.Time, limit int) ([]AutoResumed, error) {
	if limit <= 0 {
		limit = autoResumeBatchSize
	}
	var out []AutoResumed
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, claimDueAutoResumesSQL, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var (
				id       uuid.UUID
				tenantID uuid.UUID
				pausedAt pgtype.Timestamptz
				reason   string
				resumeAt pgtype.Timestamptz
			)
			if err := rows.Scan(&id, &tenantID, &pausedAt, &reason, &resumeAt); err != nil {
				return err
			}
			ar := AutoResumed{SiteID: id, TenantID: tenantID, PausedReason: reason}
			if pausedAt.Valid {
				t := pausedAt.Time
				ar.PausedAt = &t
			}
			if resumeAt.Valid {
				t := resumeAt.Time
				ar.ResumeAt = &t
			}
			out = append(out, ar)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AutoResumer runs the sweep and audits what it did. Pure logic over the repo
// and the audit recorder so it is testable without River, matching Sweeper.
type AutoResumer struct {
	repo   AutoResumeRepo
	audit  *audit.Recorder
	logger *slog.Logger
	limit  int
}

// NewAutoResumer builds the sweep. rec may be nil only in tests that are not
// asserting on the audit trail; in production the audit entry is the point of
// the feature, so main.go always supplies one.
func NewAutoResumer(repo AutoResumeRepo, rec *audit.Recorder, logger *slog.Logger) *AutoResumer {
	return &AutoResumer{repo: repo, audit: rec, logger: logger, limit: autoResumeBatchSize}
}

// SetBatchSize overrides the per-tick cap. Zero or negative is ignored.
func (a *AutoResumer) SetBatchSize(n int) {
	if n > 0 {
		a.limit = n
	}
}

// Sweep resumes every site whose monitoring_resume_at has arrived and writes one
// audit entry per site. Returns the number of sites resumed.
//
// THE AUDIT WRITE IS BEST-EFFORT AND DELIBERATELY OUTSIDE THE CLAIM
// TRANSACTION, which is a real trade-off and not an oversight.
// audit.Recorder.Record opens its own InTenantTx and takes a per-tenant advisory
// lock on the hash chain; it cannot join the cross-tenant InAgentTx this sweep
// claims in, and holding a fleet-wide lock on sites while serialising against
// every tenant's audit chain in turn is a far worse failure than a missing log
// line. So the claim commits first and the entries follow.
//
// The cost is bounded and stated: if the process dies between the commit and the
// Record, that site is resumed with no audit entry, and no retry will recreate
// it because the row no longer matches the claim predicate. That is why a failed
// Record is logged at ERROR with the site id rather than swallowed — the log
// line is the fallback record, and it names the row precisely enough to
// reconstruct the event. The alternative ordering, audit-then-resume, was
// rejected outright: it produces an audit entry for a resume that did not
// happen, and a log that says things that are not true is worse than a log with
// a hole in it.
func (a *AutoResumer) Sweep(ctx context.Context, now time.Time) (int, error) {
	resumed, err := a.repo.ClaimDueAutoResumes(ctx, now, a.limit)
	if err != nil {
		return 0, err
	}
	for _, r := range resumed {
		a.recordAutoResume(ctx, r)
	}
	return len(resumed), nil
}

// recordAutoResume writes the audit entry for one auto-resumed site.
//
// The action is the SAME audit.ActionSiteMonitoringResumed the manual route
// writes, not a new one. Someone auditing a site asks "when did monitoring come
// back", and splitting that into two action strings means every such query has
// to know both or silently miss half the answers. WHO did it is already carried
// by actor_type: the manual route records the user, this records
// audit.ActorSystem, and metadata.auto=true makes the distinction explicit for a
// reader who is looking at the entry rather than filtering on it.
func (a *AutoResumer) recordAutoResume(ctx context.Context, r AutoResumed) {
	if a.audit == nil {
		return
	}
	meta := map[string]any{"auto": true}
	if r.PausedAt != nil {
		meta["paused_at"] = r.PausedAt.UTC().Format(time.RFC3339)
	}
	if r.PausedReason != "" {
		meta["reason"] = r.PausedReason
	}
	if r.ResumeAt != nil {
		meta["resume_at"] = r.ResumeAt.UTC().Format(time.RFC3339)
	}
	if _, err := a.audit.Record(ctx, audit.Event{
		TenantID:   r.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     audit.ActionSiteMonitoringResumed,
		TargetType: "site",
		TargetID:   r.SiteID.String(),
		Metadata:   meta,
	}); err != nil && a.logger != nil {
		// ERROR, not Warn: this line is the only surviving record of a state
		// change that already committed. See the Sweep doc comment.
		a.logger.Error("monitoring auto-resume: audit record failed",
			slog.String("site_id", r.SiteID.String()),
			slog.String("tenant_id", r.TenantID.String()),
			slog.Any("error", err),
		)
	}
}

// ---- River worker ----

// AutoResumeArgs is the River job payload for the periodic auto-resume sweep.
type AutoResumeArgs struct{}

// Kind implements river.JobArgs.
func (AutoResumeArgs) Kind() string { return "site_monitoring_auto_resume" }

// AutoResumeWorker runs the auto-resume sweep as a River job.
type AutoResumeWorker struct {
	river.WorkerDefaults[AutoResumeArgs]
	resumer *AutoResumer
	logger  *slog.Logger
}

// NewAutoResumeWorker builds the River worker around an AutoResumer.
func NewAutoResumeWorker(a *AutoResumer, logger *slog.Logger) *AutoResumeWorker {
	return &AutoResumeWorker{resumer: a, logger: logger}
}

// Work runs one auto-resume sweep.
func (w *AutoResumeWorker) Work(ctx context.Context, _ *river.Job[AutoResumeArgs]) error {
	n, err := w.resumer.Sweep(ctx, time.Now())
	if err != nil {
		return err
	}
	if n > 0 && w.logger != nil {
		w.logger.Info("monitoring auto-resume swept", slog.Int("resumed", n))
	}
	return nil
}
