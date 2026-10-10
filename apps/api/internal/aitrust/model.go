// Package aitrust holds what a person chooses about how much AI connections
// may do without asking (ADR-065): a site's mode, a connection's switch for
// automatic changes, the connection's automatic usage against the server's
// fixed limits, and the AI activity feed.
//
// Loosening any of it needs a person signed in to WPMgr, checked here in the
// service and again by the database's guard triggers; tightening needs only
// the route's permission. Every write takes the tenant's policy lock (a
// site's mode also takes that site's dispatch lock after it), and commits
// its audit row in the same transaction.
//
// internal/mcp never imports this package: no tool can change a setting.
package aitrust

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Refusal codes, in the Error envelope.
const (
	CodeSessionRequired  = "session_required"
	CodeRoleRequired     = "role_required"
	CodeOrgScopeRequired = "org_scope_required"
	CodeStaleVersion     = "stale_version"
	CodeAgentOutdated    = "agent_outdated"
	CodePaused           = "paused"
	CodeUseFullAutoRoute = "use_full_auto_route"
	// CodeConnectionInactive: the connection is revoked or expired.
	CodeConnectionInactive = "connection_not_active"
)

// The mode sources a write records.
const (
	// SourcePerson: a signed-in person chose the mode.
	SourcePerson = "person"
	// SourceTightened: lowered to ask by a caller that is not a person.
	SourceTightened = "tightened"
)

// Audit actions.
const (
	ActionModeChanged           = "assistant.ai_mode.changed"
	ActionConnectionAutoChanged = "assistant.ai_connection_auto.changed"
)

// SiteMode is a site's mode and what the dashboard needs to offer a change.
type SiteMode struct {
	SiteID              uuid.UUID
	Mode                aipolicy.Mode
	Source              string
	Version             int64
	SetByUserID         *uuid.UUID
	SetByName           *string
	SetByAccountDeleted bool
	SetAt               *time.Time
	SetterValid         bool
	AIPaused            bool
	MinAgentVersion     string
	Options             []ModeOption
	Kinds               []ChangeKind
}

// ModeOption is one mode the dashboard offers and whether this caller may
// choose it now. Reason is empty when Choosable.
type ModeOption struct {
	Mode      aipolicy.Mode
	Choosable bool
	Reason    string
}

// ChangeKind is one row of the table of what each mode covers.
type ChangeKind struct {
	Class     aipolicy.Class
	Name      string
	Abilities []KindAbility
	Decisions []ModeDecision
}

// KindAbility is a reviewed tool whose changes are of one kind.
type KindAbility struct {
	Name  string
	Title string
}

// ModeDecision is what happens to a kind of change under one mode: "auto"
// or "ask".
type ModeDecision struct {
	Mode    aipolicy.Mode
	Outcome string
}

// ConnectionAuto is a connection's switch and the person whose authority
// keeps it on.
type ConnectionAuto struct {
	GrantID             uuid.UUID
	AIAuto              aipolicy.ConnectionAuto
	SetByUserID         *uuid.UUID
	SetByName           *string
	SetByAccountDeleted bool
	SetAt               *time.Time
	SetterValid         bool
	CreatedWithAPIKey   bool
}

// UsageBucket is one count against one fixed limit.
type UsageBucket struct {
	Used  int32
	Limit int32
}

// ConnectionUsage is a connection's switch and its automatic usage in the
// rolling window.
type ConnectionUsage struct {
	ConnectionAuto
	WindowMinutes int32
	DraftChanges  UsageBucket
	DraftSites    UsageBucket
}

// The activity filters, as the contract names them.
const (
	FilterAll              = "all"
	FilterRanAutomatically = "ran_automatically"
	FilterApprovedByPerson = "approved_by_person"
	FilterFailedOrUnknown  = "failed_or_unknown"
	FilterUndone           = "undone"
)

// KnownFilter reports whether f is one of the activity filters.
func KnownFilter(f string) bool {
	switch f {
	case FilterAll, FilterRanAutomatically, FilterApprovedByPerson, FilterFailedOrUnknown, FilterUndone:
		return true
	}
	return false
}

// The activity item kinds.
const (
	KindAbilityRequest    = "ability_request"
	KindCachePurgeRequest = "cache_purge_request"
)

// The activity page bounds.
const (
	ActivityDefaultLimit = 50
	ActivityMaxLimit     = 100
)

// Cursor is a keyset position in the activity feed: the (created_at, id) of
// the last item of the previous page.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ActivityQuery is one page request.
type ActivityQuery struct {
	Filter  string
	SiteID  *uuid.UUID
	GrantID *uuid.UUID
	Limit   int32
	After   *Cursor
}

// ActivityItem is one approved request of either kind, rendered as the
// request queues render it.
type ActivityItem struct {
	Kind    string
	Request any
}

// ActivityPage is one page of the feed. Next is nil on the last page.
type ActivityPage struct {
	Items []ActivityItem
	Next  *Cursor
}

// SetterResolver builds a person's current principal in a tenant and their
// account's status: the session authenticator's own code path
// (middleware.Authenticator.ResolveSetter). It opens its own transactions, so
// it is never called inside a write's transaction.
type SetterResolver interface {
	ResolveSetter(ctx context.Context, tenantID, userID uuid.UUID) aipolicy.Setter
}

// AbilityRequestRenderer renders site-change rows exactly as the request
// queues return them. *abilityrequest.Handler satisfies it.
type AbilityRequestRenderer interface {
	RenderAbilityRequests(ctx context.Context, p domain.Principal, rows []sqlc.AssistantAbilityRequest) ([]any, error)
}

// CachePurgeRenderer renders cache-clear rows exactly as the request queue
// returns them. *assistantrequest.Handler satisfies it.
type CachePurgeRenderer interface {
	RenderCachePurgeRequests(ctx context.Context, p domain.Principal, rows []sqlc.AssistantCachePurgeRequest) ([]any, error)
}

// LaunchNoticeSite is one site the launch notice lists.
type LaunchNoticeSite struct {
	ID   uuid.UUID
	Name string
	URL  string
}

// LaunchNoticeClaim is one organisation's sites claimed for the launch
// notice, the stamp the claim wrote on them, and the addresses of the owners
// and admins who are told. A claim with no sites claimed nothing.
type LaunchNoticeClaim struct {
	Sites      []LaunchNoticeSite
	ClaimedAt  time.Time
	Recipients []string
}
