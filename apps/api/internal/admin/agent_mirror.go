package admin

// agent_mirror.go: superadmin manual trigger for the upstream agent-release
// mirror (GH #322). The route lives under /api/v1/admin/agent-mirror/, already
// gated by requireSuperadmin (see Register in handler.go).
//
//	POST /admin/agent-mirror/check: queue one immediate mirror run
//
// Deliberately NOT under /api/v1/fleet: the mirror is ONE PER INSTALL, not per
// tenant (internal/agentupstream: MirrorArgs carries no tenant/site identity),
// and a check spends this install's SHARED unauthenticated GitHub request
// budget (60/hour/IP) and rewrites the install's shared release pointer. A
// tenant-scoped route would let any org admin on a shared install spend that
// budget and rewrite that pointer, which is the wrong permission model for a
// resource with no tenant. Mirrors POST /admin/vuln-feed/sync exactly, for the
// identical reason (a shared feed, a shared rate limit).

import (
	"context"
	"fmt"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentmirror"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentupstream"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// AgentMirrorStateLoader reads the persisted mirror sentinel. Satisfied by
// *agentmirror.Repo.
type AgentMirrorStateLoader interface {
	Load(ctx context.Context) (agentmirror.State, error)
}

// AgentMirrorCheckEnqueuer inserts one manual mirror-check job. Satisfied by
// *agentupstream.ManualCheckEnqueuer (kept in agentupstream so the River
// insert options a caller must use stay next to the job they belong to).
type AgentMirrorCheckEnqueuer interface {
	EnqueueManualMirrorCheck(ctx context.Context) (queued bool, err error)
}

// AgentMirrorCheckService gates and triggers a manual upstream check.
type AgentMirrorCheckService struct {
	// enabled/wired mirror the two independent gates
	// cmd/wpmgr/main.go's periodic worker itself checks: enabled is
	// cfg.Update.AgentMirrorEnabled, wired is "object storage is actually
	// configured" (the *agentupstream.Mirror instance is non-nil). Both must
	// hold before a manual check can do anything.
	enabled bool
	wired   bool
	state   AgentMirrorStateLoader
	enqueue AgentMirrorCheckEnqueuer
}

// NewAgentMirrorCheckService builds the service. state/enqueue may be nil
// (persistence/River not wired yet); TriggerCheck then degrades to an honest
// refusal rather than a panic.
func NewAgentMirrorCheckService(enabled, wired bool, state AgentMirrorStateLoader, enqueue AgentMirrorCheckEnqueuer) *AgentMirrorCheckService {
	return &AgentMirrorCheckService{enabled: enabled, wired: wired, state: state, enqueue: enqueue}
}

// AgentMirrorCheckResult is TriggerCheck's success payload.
type AgentMirrorCheckResult struct {
	QueuedAt time.Time
}

// TriggerCheck queues one manual mirror run, or refuses honestly:
//
//   - agent_mirror_disabled (503): WPMGR_UPDATE_AGENT_MIRROR_ENABLED is off
//     (the default, and the hosted service's setting).
//   - agent_mirror_not_configured (503): enabled, but object storage or the
//     enqueuer is not wired.
//   - agent_mirror_rate_limited (429): the last ACTUAL upstream request was
//     less than agentupstream.MinRequestSpacing ago. Nothing is queued.
//     Computed from the PERSISTED last_request_at (internal/agentmirror), not
//     any in-memory value, because this HTTP handler runs in a different
//     process/request than the one that will work the job, see
//     agentmirror.State.LastRequestAt's doc.
//   - agent_mirror_check_in_flight (409): an identical manual check is
//     already queued or running (see agentupstream.ManualInsertOpts).
//
// This pre-flight rate-limit check is a COURTESY, not a lock: it makes the
// common case answer honestly without ever inserting a job, but it cannot
// prevent a genuine race with the periodic tick. If one occurs, the enqueued
// manual job still runs Mirror.Run's own authoritative guard, and THAT
// outcome (agentmirror.OutcomeRateLimited) is what actually lands in
// persisted state, so the two paths can never disagree for long, only
// briefly race, and the operator always sees the true outcome on refresh.
func (s *AgentMirrorCheckService) TriggerCheck(ctx context.Context) (AgentMirrorCheckResult, error) {
	if !s.enabled {
		return AgentMirrorCheckResult{}, domain.ServiceUnavailable("agent_mirror_disabled",
			"the upstream agent-release mirror is disabled on this install (WPMGR_UPDATE_AGENT_MIRROR_ENABLED)")
	}
	if !s.wired || s.enqueue == nil {
		return AgentMirrorCheckResult{}, domain.ServiceUnavailable("agent_mirror_not_configured",
			"the upstream agent-release mirror is enabled but cannot run (object storage is not configured, WPMGR_S3_*)")
	}

	if s.state != nil {
		if st, err := s.state.Load(ctx); err == nil && st.LastRequestAt != nil {
			elapsed := time.Since(*st.LastRequestAt)
			if elapsed < agentupstream.MinRequestSpacing {
				remaining := (agentupstream.MinRequestSpacing - elapsed).Round(time.Second)
				retryAfter := int(remaining.Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				return AgentMirrorCheckResult{}, domain.RateLimited("agent_mirror_rate_limited",
					fmt.Sprintf("the upstream release was last requested %s ago; the mirror waits at least %s between requests to GitHub, so nothing was checked",
						elapsed.Round(time.Second), agentupstream.MinRequestSpacing)).
					WithDetails(map[string]any{
						"retry_after_seconds": retryAfter,
						"next_check_after":    time.Now().Add(remaining).UTC().Format(time.RFC3339),
						"last_request_at":     st.LastRequestAt.UTC().Format(time.RFC3339),
					})
			}
		}
		// A read failure, or no request ever recorded, is NOT a reason to
		// refuse; see the doc above: this pre-flight is a courtesy, and the
		// safe default when it cannot answer is to let the request through.
	}

	queued, err := s.enqueue.EnqueueManualMirrorCheck(ctx)
	if err != nil {
		return AgentMirrorCheckResult{}, domain.Internal("agent_mirror_check_failed", "failed to queue an upstream mirror check").WithCause(err)
	}
	if !queued {
		return AgentMirrorCheckResult{}, domain.Conflict("agent_mirror_check_in_flight",
			"a mirror check is already queued or running; its result will appear on the fleet agent view when it finishes")
	}
	return AgentMirrorCheckResult{QueuedAt: time.Now().UTC()}, nil
}
