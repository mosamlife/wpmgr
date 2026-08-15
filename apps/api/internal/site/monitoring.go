package site

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #414 phase 1 of 5 — "pause monitoring", the schema/API/audit foundation.
//
// PHASE 1 DELIBERATELY CHANGES NO BEHAVIOUR. It persists the operator's
// intent and nothing reads it: no scheduler consults MonitoringPausedAt yet.
// Teaching a dozen workers a new predicate is the risky half and it lands in
// phases 2-3, on a foundation that is already reviewed.
//
// WHAT PAUSE WILL EVENTUALLY STOP (phases 2-3): uptime probes and their
// alerts, update inventory refresh, scheduled security scans, vulnerability
// rescans and their alerts, screenshots.
//
// WHAT IT MUST NEVER STOP, and why, because a later phase will be tempted:
//
//   - BACKUPS. Data protection is not monitoring. Someone pausing
//     "monitoring" before a migration might assume everything stops; if
//     backups stopped silently that is the one failure people do not
//     recover from.
//   - The CONNECTION SWEEP (site_connection_sweep, site_health_check).
//     Stopping it would freeze a paused site at connection_state 'connected'
//     forever after its agent died. Pause means "do not tell me", never
//     "lie to me".
//   - RUM beacon ingestion, which is agent-pushed and has no server-side
//     switch.
//   - Retention and cleanup jobs.
//   - Anything a person clicks. Pause governs the schedule, never the
//     operator.
//
// WHY A NEW COLUMN AND NOT A ConnectionState. ConnectionState (ADR-041,
// connection.go) is a state machine whose every member describes whether the
// AGENT IS REACHABLE. Pause describes whether WE CHOOSE TO ACT. They are
// orthogonal: a connected site can be paused, and a paused site can lose its
// agent. Folding pause into that enum makes both facts unrepresentable at
// once.

// maxBulkMonitoringSites caps one pause/resume request. Same order as the tag
// bulk-apply cap: the request holds one transaction open over that many rows,
// and an unbounded list is a self-inflicted lock-duration problem.
const maxBulkMonitoringSites = 200

// MonitoringState is the persisted pause state of one site, as it stands
// AFTER the request that returned it. A zero PausedAt means monitoring is
// active.
type MonitoringState struct {
	SiteID uuid.UUID
	// PausedAt is nil when monitoring is active. Non-nil is the instant the
	// pause began — a timestamp rather than a boolean so "paused since when"
	// and "who paused it" are answerable without a separate audit query.
	PausedAt *time.Time
	// PausedBy is the user who paused, nil for an API-key actor or after that
	// user's account was deleted (the FK is ON DELETE SET NULL).
	PausedBy *uuid.UUID
	// PausedReason is the free-text note the operator typed. Empty string when
	// none was given; never NULL.
	PausedReason string
	// ResumeAt is the optional instant a later phase's sweep will auto-resume
	// at. Nil means "paused until someone resumes it".
	ResumeAt *time.Time
	// Changed reports whether THIS request altered the row. False on a
	// re-pause of an already-paused site or a resume of an already-active one;
	// both are successes, not errors.
	Changed bool
}

// Paused reports whether monitoring is currently paused for the site.
func (s MonitoringState) Paused() bool { return s.PausedAt != nil }

// PauseMonitoringInput is one bulk pause request, already authenticated.
type PauseMonitoringInput struct {
	TenantID uuid.UUID
	// ActorUserID is the authenticated user, written to
	// sites.monitoring_paused_by. It is NEVER taken from request input: the FK
	// is to users(id) alone and Postgres cannot check that the referenced user
	// belongs to the site's tenant, so the only safe source is the principal.
	// uuid.Nil (an API-key actor) stores NULL.
	ActorUserID uuid.UUID
	SiteIDs     []uuid.UUID
	Reason      string
	// ResumeAt, when non-nil, must be in the future. A past instant would be a
	// pause that instantly un-pauses, which is a typo, not an intent.
	ResumeAt *time.Time
}

// ResumeMonitoringInput is one bulk resume request.
type ResumeMonitoringInput struct {
	TenantID    uuid.UUID
	ActorUserID uuid.UUID
	SiteIDs     []uuid.UUID
}

// maxPauseReasonLen bounds the stored note. The column is unbounded text; the
// cap keeps a pasted logfile out of the audit metadata and the sites list.
const maxPauseReasonLen = 500

// PauseMonitoring validates the request and pauses every named site that
// exists in the tenant.
//
// IDEMPOTENT BY CONSTRUCTION: a site that is already paused keeps its original
// PausedAt, PausedBy, PausedReason and ResumeAt untouched and comes back with
// Changed=false. A client that retries on a timeout is normal, and a retry
// must never silently erase the reason someone typed.
func (s *Service) PauseMonitoring(ctx context.Context, in PauseMonitoringInput) ([]MonitoringState, error) {
	if in.TenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if len(in.SiteIDs) == 0 {
		return nil, domain.Validation("site_ids_required", "site_ids must not be empty")
	}
	if len(in.SiteIDs) > maxBulkMonitoringSites {
		return nil, domain.Validation("too_many_sites", "site_ids must contain at most 200 entries per request")
	}
	if len(in.Reason) > maxPauseReasonLen {
		return nil, domain.Validation("reason_too_long", "reason must be at most 500 characters")
	}
	if in.ResumeAt != nil {
		if in.ResumeAt.IsZero() {
			return nil, domain.Validation("invalid_resume_at", "resume_at is not a valid timestamp")
		}
		// A resume instant in the past is a pause that un-pauses on the next
		// sweep, i.e. a no-op the operator did not ask for. Reject it rather
		// than storing it.
		if !in.ResumeAt.After(s.clock.Now()) {
			return nil, domain.Validation("resume_at_in_past", "resume_at must be in the future")
		}
	}
	return s.repo.PauseMonitoring(ctx, in)
}

// ResumeMonitoring clears the pause on every named site that exists in the
// tenant. Resuming an already-active site succeeds with Changed=false.
//
// The repo clears monitoring_paused_at and monitoring_resume_at in the SAME
// UPDATE: the sites_monitoring_resume_requires_pause_check constraint rejects
// a resume instant on an unpaused row, so clearing one without the other
// raises 23514.
func (s *Service) ResumeMonitoring(ctx context.Context, in ResumeMonitoringInput) ([]MonitoringState, error) {
	if in.TenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if len(in.SiteIDs) == 0 {
		return nil, domain.Validation("site_ids_required", "site_ids must not be empty")
	}
	if len(in.SiteIDs) > maxBulkMonitoringSites {
		return nil, domain.Validation("too_many_sites", "site_ids must contain at most 200 entries per request")
	}
	return s.repo.ResumeMonitoring(ctx, in)
}
