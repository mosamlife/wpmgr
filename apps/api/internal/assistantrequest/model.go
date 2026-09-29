// Package assistantrequest is everything a person and the dispatch worker do
// with an AI connection's cache-clear request once the connection has asked:
// the queue, approve and decline, the worker that sends an approved request to
// the site, and the sweeper and reconciler that close whatever is left open.
//
// Creation, the status tool and the revoke cascade live in internal/mcp. This
// package imports mcp and never the reverse.
//
// No automation approves anything here. Approve and decline are refused to
// every principal except a person signed in to the dashboard, and the worker
// sends only a row a person approved.
package assistantrequest

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Request states (assistant_cache_purge_requests.state).
const (
	StatePending    = "pending"
	StateApproved   = "approved_undispatched"
	StateDispatched = "dispatched"
	StateRejected   = "rejected"
	StateWithdrawn  = "withdrawn"
	StateExpired    = "expired"
)

// Outcomes of a dispatched request.
const (
	OutcomePurged              = "purged"
	OutcomeSiteReportedFailure = "site_reported_failure"
	OutcomeAgentFailed         = "agent_failed"
	OutcomeUnknown             = "outcome_unknown"
	OutcomeNotSent             = "not_sent"
)

// Reasons a request closed without being sent.
const (
	ReasonGrantInactive          = "grant_inactive"
	ReasonAssistantPaused        = "assistant_paused"
	ReasonOrganisationDeleted    = "organisation_deleted"
	ReasonCapabilityNotHeld      = "capability_not_held"
	ReasonSiteAbsent             = "site_absent"
	ReasonForbiddenByContext     = "forbidden_by_context"
	ReasonAgentOutdated          = "agent_outdated"
	ReasonDispatchDeadlinePassed = "dispatch_deadline_passed"
	ReasonTransportPreSend       = "transport_pre_send"
)

// Transient reasons an approved request has not started yet. The row stays
// approved and the worker tries again.
const (
	AttemptSiteUnreachable    = "site_unreachable"
	AttemptSiteCooldown       = "site_cooldown"
	AttemptSiteHourlyCap      = "site_hourly_cap"
	AttemptSiteBusy           = "site_busy"
	AttemptOrgBusy            = "org_busy"
	AttemptContextUnavailable = "context_unavailable"
	AttemptWriteToolsDisabled = "write_tools_disabled"
)

// Timing and limits. Every window is measured by the database clock; these
// are passed to it as whole seconds.
const (
	// DispatchDeadline is how long after approval a request may still start.
	// The worker enforces it, and the sweeper closes anything past it.
	DispatchDeadline = time.Hour
	// WholeSiteCooldown is the minimum gap after any whole-site clear on a
	// site, whoever made it, before an approved AI whole-site clear runs.
	WholeSiteCooldown = 5 * time.Minute
	// SiteHourlyCap is the most AI clears one site takes in an hour.
	SiteHourlyCap = 12
	// StaleDispatchAfter is how long a sent request may wait for its outcome
	// before the reconciler records that the outcome is unknown.
	StaleDispatchAfter = 10 * time.Minute
	// ScanInterval is how often the worker looks for approved requests.
	ScanInterval = 15 * time.Second
	// SweepInterval is how often the sweeper and the reconciler run.
	SweepInterval = time.Minute
	// BackoffBase and BackoffCap bound how soon a request that could not start
	// is tried again: BackoffBase, doubling per attempt, capped.
	BackoffBase = 30 * time.Second
	BackoffCap  = 5 * time.Minute

	// scanRowLimit bounds one scan, sweep or reconcile pass.
	scanRowLimit = 200
	// maxSiteReportedText is the byte cap on site-reported text.
	maxSiteReportedText = 512
	// siteDispatchLockKey namespaces the per-site reservation lock.
	siteDispatchLockKey = "assistant_site_dispatch"
	// lifecycleLockKey is the organisation lifecycle lock key. It must equal
	// org.LifecycleLockKey; a test pins that.
	lifecycleLockKey = "org_lifecycle"
	// CardCopyVersion names the wording of the approval card a person
	// approved. It is recorded on assistant.request.approved.
	CardCopyVersion = "2026-09-29"
	// operatorMessageAction is the action name an agent failure is described
	// with.
	operatorMessageAction = "Cache clear"
)

// Request is one request as the queue shows it to a person.
type Request struct {
	ID                   uuid.UUID
	SiteID               uuid.UUID
	Scope                string
	URL                  *string
	SiteLabel            string
	SiteHost             string
	GrantLabel           string
	GrantVia             string
	SetupClient          *string
	PresentedDigest      *string
	State                string
	CreatedAt            time.Time
	ExpiresAt            time.Time
	DecidedAt            *time.Time
	DecidedByUserID      *uuid.UUID
	DecidedByName        *string
	DecidedByDeleted     bool
	WithdrawnAt          *time.Time
	ClaimedAt            *time.Time
	DispatchAttempts     int32
	LastAttemptAt        *time.Time
	LastAttemptCode      *string
	Outcome              *string
	NotSentReason        *string
	OutcomeAt            *time.Time
	HostingCachesCleared []string
	HostingCachesSkipped []string
	OriginOnlyConfirmed  *bool
	WpmgrCDN             *string
	SiteReportedText     *string
}

// Queue is a page of requests and the number waiting for a decision.
type Queue struct {
	Requests     []Request
	PendingCount int64
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func uuidPtr(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	v := uuid.UUID(u.Bytes)
	return &v
}
