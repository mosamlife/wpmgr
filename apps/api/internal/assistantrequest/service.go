package assistantrequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

// GrantVerdicts reads a connection's grant-level verdict with no token in
// hand. *mcp.Repo satisfies it.
type GrantVerdicts interface {
	ReCheckGrantAuthorization(ctx context.Context, tenantID, grantID uuid.UUID) (mcp.GrantVerdict, error)
}

// GrantAuthorizer derives what a verdict confers and reads the operator's AI
// rules. *mcp.Service satisfies it.
type GrantAuthorizer interface {
	AuthorizeGrant(ctx context.Context, tenantID uuid.UUID, v mcp.GrantVerdict) (mcp.AuthorizedRequest, error)
	ForbiddenByContext(ctx context.Context, tenantID, siteID uuid.UUID, tool string) (matchedEntry string, forbidden bool, err error)
}

// PurgeSender sends an approved clear and announces it. *perf.Service
// satisfies it. Neither method touches the database.
type PurgeSender interface {
	SendAssistantPurge(ctx context.Context, siteID uuid.UUID, siteURL string, cdn perf.CDNCiphertext, req perf.AssistantPurge) (perf.AssistantPurgeResult, error)
	PublishAssistantPurge(ctx context.Context, tenantID, siteID uuid.UUID, scope string)
}

// SiteCacheStore reads and writes the site's cache settings on a transaction
// the caller holds. *perf.Repo satisfies it.
type SiteCacheStore interface {
	GetCDNCredentialsCiphertextTx(ctx context.Context, tx pgx.Tx, tenantID, siteID uuid.UUID) (perf.CDNCiphertext, error)
	MarkCachePurgedTx(ctx context.Context, tx pgx.Tx, tenantID, siteID uuid.UUID, kind string) error
}

// Service is the queue, approve and decline, and the dispatch worker's logic.
type Service struct {
	repo    *Repo
	grants  GrantVerdicts
	authz   GrantAuthorizer
	audit   *audit.Recorder
	sender  PurgeSender
	store   SiteCacheStore
	logger  *slog.Logger
	enabled bool
}

// NewService builds the service. The write-tools switch starts OFF; call
// SetWriteToolsEnabled.
func NewService(repo *Repo, grants GrantVerdicts, authz GrantAuthorizer, rec *audit.Recorder, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, grants: grants, authz: authz, audit: rec, logger: logger}
}

// SetSender wires the send path. Without it nothing is ever sent: approve is
// refused as switched off and the worker records write_tools_disabled.
func (s *Service) SetSender(sender PurgeSender, store SiteCacheStore) {
	s.sender = sender
	s.store = store
}

// SetWriteToolsEnabled is the server-wide switch. Off: approve is refused and
// the worker records write_tools_disabled on every approved row, which the
// sweeper then closes at its deadline. The sweeper, the reconciler and decline
// keep working either way.
func (s *Service) SetWriteToolsEnabled(on bool) { s.enabled = on }

// writeToolsOn is the switch as the approve and dispatch paths apply it.
func (s *Service) writeToolsOn() bool {
	return s.enabled && s.sender != nil && s.store != nil
}

// WriteToolsFromEnv parses WPMGR_MCP_WRITE_TOOLS. Unset or "off" is off; "on"
// is on; anything else is an error, so a typo fails boot rather than silently
// choosing a side.
func WriteToolsFromEnv(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("WPMGR_MCP_WRITE_TOOLS must be \"on\" or \"off\", got %q", value)
	}
}

// ---------------------------------------------------------------------------
// Refusals a person sees. None of them carries a grant field.
// ---------------------------------------------------------------------------

const (
	CodeRequestChanged     = "assistant_request_changed"
	CodeAssistantPaused    = "assistant_paused"
	CodeWriteToolsDisabled = "assistant_write_tools_disabled"
	CodeConnectionInactive = "assistant_connection_inactive"
	CodeSiteLeftScope      = "assistant_site_left_scope"
	CodeCapabilityNotHeld  = "assistant_capability_not_held"
	CodeForbiddenByContext = "assistant_forbidden_by_context"
	CodeContextUnavailable = "assistant_context_unavailable"
	CodeAgentOutdated      = "assistant_agent_outdated"
	CodeSiteAbsent         = "assistant_site_absent"
	CodeSessionRequired    = "assistant_session_required"
	CodeLedgerFailed       = "assistant_ledger_failed"
)

// Each refusal is built fresh per call: *domain.Error is mutable (WithCause),
// so a shared value would leak one request's cause into another's answer.
func errRequestChanged() error {
	return domain.Conflict(CodeRequestChanged,
		"This request changed or was decided while you were reading it. Nothing ran.")
}

func errAssistantPaused() error {
	return domain.Conflict(CodeAssistantPaused,
		"The AI assistant is paused for this organisation. Nothing can be approved until it is resumed.")
}

func errWriteToolsDisabled() error {
	return domain.Conflict(CodeWriteToolsDisabled,
		"AI cache clears are switched off on this server. Nothing was approved.")
}

func errConnectionInactive() error {
	return domain.Conflict(CodeConnectionInactive,
		"This connection was revoked or has expired, so this request cannot run. Decline it or let it close.")
}

func errSiteLeftScope() error {
	return domain.Conflict(CodeSiteLeftScope,
		"This site is no longer in the connection's scope, so this request cannot run. Decline it or let it close.")
}

func errCapabilityNotHeld() error {
	return domain.Conflict(CodeCapabilityNotHeld,
		"This connection no longer holds the capability to ask for cache clears, so this request cannot run. Decline it or let it close.")
}

func errForbiddenByContext() error {
	return domain.Conflict(CodeForbiddenByContext,
		"Your AI rules forbid this on this site, so this request cannot run. Decline it or let it close.")
}

func errContextUnavailable() error {
	return domain.Conflict(CodeContextUnavailable,
		"We could not read your AI rules for this site. Nothing was approved. Try again.")
}

func errAgentOutdated() error {
	return domain.Conflict(CodeAgentOutdated,
		"This site's agent is too old to clear only this site's cache, so this request cannot run. Update the agent, or decline it.")
}

func errSiteAbsent() error {
	return domain.Conflict(CodeSiteAbsent,
		"This site can no longer be read, so this request cannot run. Decline it or let it close.")
}

func errSessionRequired() error {
	return domain.Forbidden(CodeSessionRequired,
		"Only a person signed in to WPMgr can approve or decline an AI request.")
}

func errLedgerFailed(cause error) error {
	return domain.Internal(CodeLedgerFailed,
		"Approval failed and nothing will run. Try again.").WithCause(cause)
}

// requireSession refuses every principal but a person signed in to the
// dashboard. API keys never approve or decline, and MCP tokens never reach
// these routes at all.
func requireSession(p domain.Principal) error {
	if p.Type != domain.PrincipalUser || p.UserID == uuid.Nil || p.TenantID == uuid.Nil {
		return errSessionRequired()
	}
	return nil
}

// agentMeetsFloor reports whether a site's agent can clear only its own
// cache when asked. It is the creation rail's own function, so creation,
// approval and dispatch apply one rule.
func agentMeetsFloor(v string) bool {
	return mcp.AgentMeetsOriginOnlyFloor(v)
}

// connectedEnough reports whether the site's agent can be sent a command now.
func connectedEnough(state string) bool {
	return state == "connected" || state == "degraded"
}

// ---------------------------------------------------------------------------
// Queue
// ---------------------------------------------------------------------------

// List returns a page of this organisation's requests visible to p, newest
// first, and the number waiting. Row security narrows a site collaborator to
// their own sites. The digest is included only for a signed-in person.
func (s *Service) List(ctx context.Context, p domain.Principal, siteID *uuid.UUID, limit, offset int32) (Queue, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	withDigest := p.Type == domain.PrincipalUser
	var out Queue
	err := s.repo.runAsCaller(ctx, p, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var rows []sqlc.AssistantCachePurgeRequest
		var err error
		if siteID != nil {
			rows, err = q.ListAssistantCachePurgeRequestsForSite(ctx, sqlc.ListAssistantCachePurgeRequestsForSiteParams{
				TenantID: p.TenantID, SiteID: *siteID, RowLimit: limit, RowOffset: offset,
			})
			if err == nil {
				out.PendingCount, err = q.CountLivePendingAssistantCachePurgeRequestsForSite(ctx,
					sqlc.CountLivePendingAssistantCachePurgeRequestsForSiteParams{TenantID: p.TenantID, SiteID: *siteID})
			}
		} else {
			rows, err = q.ListAssistantCachePurgeRequests(ctx, sqlc.ListAssistantCachePurgeRequestsParams{
				TenantID: p.TenantID, RowLimit: limit, RowOffset: offset,
			})
			if err == nil {
				out.PendingCount, err = q.CountLivePendingAssistantCachePurgeRequests(ctx, p.TenantID)
			}
		}
		if err != nil {
			return err
		}
		names := map[uuid.UUID]*string{}
		out.Requests = make([]Request, 0, len(rows))
		for _, row := range rows {
			req := requestFromRow(row, withDigest)
			if req.DecidedByUserID != nil {
				id := *req.DecidedByUserID
				name, seen := names[id]
				if !seen {
					u, uerr := q.GetUserByID(ctx, id)
					switch {
					case uerr == nil:
						n := u.Name
						name = &n
					case errNoRow(uerr):
						name = nil
					default:
						return uerr
					}
					names[id] = name
				}
				req.DecidedByName = name
				req.DecidedByDeleted = name == nil
			}
			out.Requests = append(out.Requests, req)
		}
		return nil
	})
	if err != nil {
		return Queue{}, domain.Internal("assistant_requests_list_failed", "failed to list AI requests").WithCause(err)
	}
	return out, nil
}

func requestFromRow(r sqlc.AssistantCachePurgeRequest, withDigest bool) Request {
	out := Request{
		ID:                   r.ID,
		SiteID:               r.SiteID,
		Scope:                r.Scope,
		URL:                  r.Url,
		SiteLabel:            r.SiteLabel,
		SiteHost:             r.SiteHost,
		GrantLabel:           r.GrantLabel,
		GrantVia:             r.GrantVia,
		SetupClient:          r.SetupClient,
		State:                r.State,
		CreatedAt:            r.CreatedAt,
		ExpiresAt:            r.ExpiresAt,
		DecidedAt:            tsPtr(r.DecidedAt),
		DecidedByUserID:      uuidPtr(r.DecidedByUserID),
		WithdrawnAt:          tsPtr(r.WithdrawnAt),
		ClaimedAt:            tsPtr(r.ClaimedAt),
		DispatchAttempts:     r.DispatchAttempts,
		LastAttemptAt:        tsPtr(r.LastAttemptAt),
		LastAttemptCode:      r.LastAttemptCode,
		Outcome:              r.Outcome,
		NotSentReason:        r.NotSentReason,
		OutcomeAt:            tsPtr(r.OutcomeAt),
		HostingCachesCleared: r.HostingCachesCleared,
		HostingCachesSkipped: r.HostingCachesSkipped,
		OriginOnlyConfirmed:  r.OriginOnlyConfirmed,
		WpmgrCDN:             r.WpmgrCdn,
		SiteReportedText:     r.SiteReportedText,
	}
	if withDigest {
		d := r.PresentedDigest
		out.PresentedDigest = &d
	}
	return out
}

// ---------------------------------------------------------------------------
// Approve and decline
// ---------------------------------------------------------------------------

// Approve records one person's approval of one request. The grant checks run
// first and are advisory: the worker re-runs every one of them before
// sending. The approval and its audit row commit together or not at all.
func (s *Service) Approve(ctx context.Context, approver domain.Principal, siteID, requestID uuid.UUID, presentedDigest string) (Request, error) {
	if err := requireSession(approver); err != nil {
		return Request{}, err
	}
	if !s.writeToolsOn() {
		return Request{}, errWriteToolsDisabled()
	}
	if strings.TrimSpace(presentedDigest) == "" {
		return Request{}, domain.Validation("presented_digest_required", "presented_digest is required")
	}

	var row sqlc.AssistantCachePurgeRequest
	err := s.repo.runAsCaller(ctx, approver, func(tx pgx.Tx) error {
		var err error
		row, err = sqlc.New(tx).GetPendingAssistantCachePurgeRequestForSite(ctx,
			sqlc.GetPendingAssistantCachePurgeRequestForSiteParams{TenantID: approver.TenantID, ID: requestID, SiteID: siteID})
		return err
	})
	if errNoRow(err) {
		return Request{}, errRequestChanged()
	}
	if err != nil {
		return Request{}, domain.Internal("assistant_request_read_failed", "failed to read the request").WithCause(err)
	}

	if err := s.checkGrantForApproval(ctx, approver.TenantID, row); err != nil {
		return Request{}, err
	}

	var approved sqlc.AssistantCachePurgeRequest
	err = s.repo.runAsCaller(ctx, approver, func(tx pgx.Tx) error {
		var err error
		approved, err = sqlc.New(tx).ApproveAssistantCachePurgeRequest(ctx, sqlc.ApproveAssistantCachePurgeRequestParams{
			DecidedByUserID: approver.UserID,
			TenantID:        approver.TenantID,
			ID:              requestID,
			SiteID:          siteID,
			PresentedDigest: presentedDigest,
		})
		if err != nil {
			return err
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   approver.TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    approver.UserID.String(),
			Action:     audit.ActionAssistantRequestApproved,
			TargetType: audit.TargetTypeAssistantCachePurgeRequest,
			TargetID:   approved.ID.String(),
			Metadata:   approvedMetadata(approved),
		})
		return err
	})
	if errNoRow(err) {
		return Request{}, errRequestChanged()
	}
	if err != nil {
		s.logger.ErrorContext(ctx, "assistant request: approval not recorded",
			slog.String("request_id", requestID.String()), slog.Any("error", err))
		return Request{}, errLedgerFailed(err)
	}
	return requestFromRow(approved, true), nil
}

// approvedMetadata is the assistant.request.approved metadata. The audit
// hash covers it.
func approvedMetadata(r sqlc.AssistantCachePurgeRequest) map[string]any {
	md := map[string]any{
		"request_id":           r.ID.String(),
		"site_id":              r.SiteID.String(),
		"scope":                r.Scope,
		"presented_digest":     r.PresentedDigest,
		"proposed_by_grant_id": r.ProposedByGrantID.String(),
		"grant_label":          r.GrantLabel,
		"grant_via":            r.GrantVia,
		"site_label":           r.SiteLabel,
		"site_host":            r.SiteHost,
		"expires_at":           r.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
		"copy_version":         CardCopyVersion,
	}
	if r.Url != nil {
		md["url"] = *r.Url
	} else {
		md["url"] = nil
	}
	if r.SetupClient != nil {
		md["setup_client"] = *r.SetupClient
	} else {
		md["setup_client"] = nil
	}
	return md
}

// checkGrantForApproval re-derives the connection's authority and checks the
// request could run now. Every failure is a 409 naming the reason, and
// nothing is written.
func (s *Service) checkGrantForApproval(ctx context.Context, tenantID uuid.UUID, row sqlc.AssistantCachePurgeRequest) error {
	v, err := s.grants.ReCheckGrantAuthorization(ctx, tenantID, row.ProposedByGrantID)
	if err != nil {
		return domain.Internal("assistant_grant_read_failed", "failed to check the connection").WithCause(err)
	}
	if !v.Found || !v.Authorized {
		if v.TenantAssistantPaused {
			return errAssistantPaused()
		}
		return errConnectionInactive()
	}
	auth, err := s.authz.AuthorizeGrant(ctx, tenantID, v)
	if errors.Is(err, mcp.ErrGrantNotAuthorized) {
		return errConnectionInactive()
	}
	if err != nil {
		return domain.Internal("assistant_grant_derive_failed", "failed to check the connection").WithCause(err)
	}
	if !mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, auth) {
		return errCapabilityNotHeld()
	}
	p, err := mcp.SingleSitePrincipal(auth, row.SiteID)
	if errors.Is(err, mcp.ErrSiteNotInScope) {
		return errSiteLeftScope()
	}
	if err != nil {
		return domain.Internal("assistant_site_principal_failed", "failed to check the connection").WithCause(err)
	}
	var site sqlc.Site
	var found bool
	err = s.repo.runSiteTx(ctx, p, row.SiteID, func(tx pgx.Tx) error {
		var err error
		site, found, err = readScopedSite(ctx, tx, tenantID, row.SiteID)
		return err
	})
	if err != nil {
		return domain.Internal("assistant_site_read_failed", "failed to read the site").WithCause(err)
	}
	if !found {
		return errSiteAbsent()
	}
	_, forbidden, err := s.authz.ForbiddenByContext(ctx, tenantID, row.SiteID, mcp.ToolSiteCachePurgeRequest)
	if err != nil {
		return errContextUnavailable()
	}
	if forbidden {
		return errForbiddenByContext()
	}
	if !agentMeetsFloor(site.AgentVersion) {
		return errAgentOutdated()
	}
	return nil
}

// Decline records one person's refusal. There are no grant checks, so
// declining always works while the request is waiting.
func (s *Service) Decline(ctx context.Context, decider domain.Principal, siteID, requestID uuid.UUID) (Request, error) {
	if err := requireSession(decider); err != nil {
		return Request{}, err
	}
	var declined sqlc.AssistantCachePurgeRequest
	err := s.repo.runAsCaller(ctx, decider, func(tx pgx.Tx) error {
		var err error
		declined, err = sqlc.New(tx).DeclineAssistantCachePurgeRequest(ctx, sqlc.DeclineAssistantCachePurgeRequestParams{
			DecidedByUserID: decider.UserID,
			TenantID:        decider.TenantID,
			ID:              requestID,
			SiteID:          siteID,
		})
		if err != nil {
			return err
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		md := map[string]any{
			"request_id":           declined.ID.String(),
			"site_id":              declined.SiteID.String(),
			"scope":                declined.Scope,
			"proposed_by_grant_id": declined.ProposedByGrantID.String(),
			"grant_label":          declined.GrantLabel,
			"site_label":           declined.SiteLabel,
			"site_host":            declined.SiteHost,
		}
		if declined.Url != nil {
			md["url"] = *declined.Url
		}
		_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   decider.TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    decider.UserID.String(),
			Action:     audit.ActionAssistantRequestDeclined,
			TargetType: audit.TargetTypeAssistantCachePurgeRequest,
			TargetID:   declined.ID.String(),
			Metadata:   md,
		})
		return err
	})
	if errNoRow(err) {
		return Request{}, errRequestChanged()
	}
	if err != nil {
		return Request{}, domain.Internal("assistant_decline_failed", "Declining failed. Try again.").WithCause(err)
	}
	return requestFromRow(declined, true), nil
}
