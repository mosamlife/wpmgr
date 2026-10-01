// Package abilityrequest is the person's side of the ability engine's write
// rail (engine v4 §1.4, §4; amendments W1, W3): the queue, approve and
// decline of assistant_ability_requests rows created by site_ability_run.
//
// Shape follows internal/assistantrequest: session principal only, the
// approval is digest-bound and happens once, and every decision commits with
// its audit row. W1: the approver's hold on the row's operator_permission is
// re-checked HERE, in the service, not only by route middleware.
package abilityrequest

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// dispatchWindowSeconds is how long an approved row may wait to be sent.
const dispatchWindowSeconds int32 = 15 * 60

// Refusal codes a person sees.
const (
	CodeRequestChanged     = "ability_request_changed"
	CodeSessionRequired    = "ability_request_session_required"
	CodePermissionRequired = "ability_request_permission_required"
	CodeWriteToolsDisabled = "ability_request_write_tools_disabled"
	CodeConnectionInactive = "ability_request_connection_inactive"
	CodeAssistantPaused    = "ability_request_assistant_paused"
	CodeCapabilityNotHeld  = "ability_request_capability_not_held"
	CodeSiteLeftScope      = "ability_request_site_left_scope"
	CodeLedgerFailed       = "ability_request_ledger_failed"
)

func errRequestChanged() error {
	return domain.Conflict(CodeRequestChanged,
		"This request changed or was decided while you were reading it. Nothing ran.")
}

func errSessionRequired() error {
	return domain.Forbidden(CodeSessionRequired,
		"Only a person signed in to WPMgr can approve or decline an AI request.")
}

func errPermissionRequired() error {
	return domain.Forbidden(CodePermissionRequired,
		"You do not have permission to approve this change on this site.")
}

func errWriteToolsDisabled() error {
	return domain.Conflict(CodeWriteToolsDisabled,
		"AI site changes are switched off on this server. Nothing was approved.")
}

func errConnectionInactive() error {
	return domain.Conflict(CodeConnectionInactive,
		"This connection was revoked or has expired, so this request cannot run. Decline it or let it close.")
}

func errAssistantPaused() error {
	return domain.Conflict(CodeAssistantPaused,
		"The AI assistant is paused for this organisation. Nothing can be approved until it is resumed.")
}

func errCapabilityNotHeld() error {
	return domain.Conflict(CodeCapabilityNotHeld,
		"This connection no longer holds the capability to ask for site changes, so this request cannot run. Decline it or let it close.")
}

func errSiteLeftScope() error {
	return domain.Conflict(CodeSiteLeftScope,
		"This site is no longer in the connection's scope, so this request cannot run. Decline it or let it close.")
}

// GrantVerdicts and GrantAuthorizer are internal/assistantrequest's.
type GrantVerdicts interface {
	ReCheckGrantAuthorization(ctx context.Context, tenantID, grantID uuid.UUID) (mcp.GrantVerdict, error)
}

// GrantAuthorizer derives what a verdict confers.
type GrantAuthorizer interface {
	AuthorizeGrant(ctx context.Context, tenantID uuid.UUID, v mcp.GrantVerdict) (mcp.AuthorizedRequest, error)
}

// Service is the queue, approve and decline.
type Service struct {
	pool    *db.Pool
	grants  GrantVerdicts
	authz   GrantAuthorizer
	audit   *audit.Recorder
	logger  *slog.Logger
	enabled bool
	// siteAccess decides whether p may act on siteID. Production is
	// authz.AuthorizeSite; tests replace it.
	siteAccess func(ctx context.Context, p domain.Principal, siteID uuid.UUID) bool
}

// NewService builds the service; the write switch starts off.
func NewService(pool *db.Pool, grants GrantVerdicts, az GrantAuthorizer, rec *audit.Recorder, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, grants: grants, authz: az, audit: rec, logger: logger, siteAccess: authz.AuthorizeSite}
}

// SetWriteToolsEnabled is WPMGR_MCP_WRITE_TOOLS.
func (s *Service) SetWriteToolsEnabled(on bool) { s.enabled = on }

func requireSession(p domain.Principal) error {
	if p.Type != domain.PrincipalUser || p.UserID == uuid.Nil || p.TenantID == uuid.Nil {
		return errSessionRequired()
	}
	return nil
}

// requireOperatorPermission is W1: the approver must hold the permission the
// catalogue named when the request was created, on this site. An unknown
// permission refuses.
func (s *Service) requireOperatorPermission(ctx context.Context, p domain.Principal, row sqlc.AssistantAbilityRequest) error {
	perm := authz.Permission(row.OperatorPermission)
	if !authz.KnownPermission(perm) || !authz.PrincipalAllows(p, perm) {
		return errPermissionRequired()
	}
	if s.siteAccess == nil || !s.siteAccess(ctx, p, row.SiteID) {
		return errPermissionRequired()
	}
	return nil
}

// checkGrant re-derives the connection's authority: still active, still
// holding the request capability, and the site still in its scope. Advisory;
// the worker re-checks before sending.
func (s *Service) checkGrant(ctx context.Context, row sqlc.AssistantAbilityRequest) error {
	v, err := s.grants.ReCheckGrantAuthorization(ctx, row.TenantID, row.ProposedByGrantID)
	if err != nil {
		return domain.Internal("ability_request_grant_read_failed", "failed to check the connection").WithCause(err)
	}
	if !v.Found || !v.Authorized {
		if v.TenantAssistantPaused {
			return errAssistantPaused()
		}
		return errConnectionInactive()
	}
	auth, err := s.authz.AuthorizeGrant(ctx, row.TenantID, v)
	if errors.Is(err, mcp.ErrGrantNotAuthorized) {
		return errConnectionInactive()
	}
	if err != nil {
		return domain.Internal("ability_request_grant_derive_failed", "failed to check the connection").WithCause(err)
	}
	if !auth.Capabilities.Allows(mcp.CapAbilityRequest) {
		return errCapabilityNotHeld()
	}
	if _, err := mcp.SingleSitePrincipal(auth, row.SiteID); err != nil {
		return errSiteLeftScope()
	}
	return nil
}

func (s *Service) runAsCaller(ctx context.Context, p domain.Principal, fn func(q *sqlc.Queries, tx pgx.Tx) error) error {
	return s.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error { return fn(sqlc.New(tx), tx) })
}

// Approve records one person's digest-bound approval.
func (s *Service) Approve(ctx context.Context, approver domain.Principal, siteID, requestID uuid.UUID, presentedDigest string) (sqlc.AssistantAbilityRequest, error) {
	var none sqlc.AssistantAbilityRequest
	if err := requireSession(approver); err != nil {
		return none, err
	}
	if !s.enabled {
		return none, errWriteToolsDisabled()
	}
	if strings.TrimSpace(presentedDigest) == "" {
		return none, domain.Validation("presented_digest_required", "presented_digest is required")
	}
	var row sqlc.AssistantAbilityRequest
	err := s.runAsCaller(ctx, approver, func(q *sqlc.Queries, _ pgx.Tx) error {
		var err error
		row, err = q.GetPendingAbilityRequestForSite(ctx, sqlc.GetPendingAbilityRequestForSiteParams{
			TenantID: approver.TenantID, ID: requestID, SiteID: siteID,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return none, errRequestChanged()
	}
	if err != nil {
		return none, domain.Internal("ability_request_read_failed", "failed to read the request").WithCause(err)
	}
	if err := s.requireOperatorPermission(ctx, approver, row); err != nil {
		return none, err
	}
	if err := s.checkGrant(ctx, row); err != nil {
		return none, err
	}
	var approved sqlc.AssistantAbilityRequest
	err = s.runAsCaller(ctx, approver, func(q *sqlc.Queries, tx pgx.Tx) error {
		var err error
		approved, err = q.ApproveAbilityRequest(ctx, sqlc.ApproveAbilityRequestParams{
			DecidedByUserID: approver.UserID, DispatchWindowSeconds: dispatchWindowSeconds,
			TenantID: approver.TenantID, ID: requestID, SiteID: siteID, PresentedDigest: presentedDigest,
		})
		if err != nil {
			return err
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID: approver.TenantID, ActorType: audit.ActorUser, ActorID: approver.UserID.String(),
			Action: audit.ActionAssistantRequestApproved, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: approved.ID.String(), Metadata: decisionMetadata(approved, true),
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return none, errRequestChanged()
	}
	if err != nil {
		s.logger.ErrorContext(ctx, "ability request: approval not recorded",
			slog.String("request_id", requestID.String()), slog.Any("error", err))
		return none, domain.Internal(CodeLedgerFailed, "Approval failed and nothing will run. Try again.").WithCause(err)
	}
	return approved, nil
}

// Decline records one person's refusal. W1 applies to decline too: only a
// person who could approve may decide.
func (s *Service) Decline(ctx context.Context, decider domain.Principal, siteID, requestID uuid.UUID) (sqlc.AssistantAbilityRequest, error) {
	var none sqlc.AssistantAbilityRequest
	if err := requireSession(decider); err != nil {
		return none, err
	}
	var declined sqlc.AssistantAbilityRequest
	err := s.runAsCaller(ctx, decider, func(q *sqlc.Queries, tx pgx.Tx) error {
		row, err := q.GetPendingAbilityRequestForSite(ctx, sqlc.GetPendingAbilityRequestForSiteParams{
			TenantID: decider.TenantID, ID: requestID, SiteID: siteID,
		})
		if err != nil {
			return err
		}
		if err := s.requireOperatorPermission(ctx, decider, row); err != nil {
			return err
		}
		declined, err = q.DeclineAbilityRequest(ctx, sqlc.DeclineAbilityRequestParams{
			DecidedByUserID: decider.UserID, TenantID: decider.TenantID, ID: requestID, SiteID: siteID,
		})
		if err != nil {
			return err
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID: decider.TenantID, ActorType: audit.ActorUser, ActorID: decider.UserID.String(),
			Action: audit.ActionAssistantRequestDeclined, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: declined.ID.String(), Metadata: decisionMetadata(declined, false),
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return none, errRequestChanged()
	}
	if de, ok := domain.AsDomain(err); ok {
		return none, de
	}
	if err != nil {
		return none, domain.Internal("ability_request_decline_failed", "Declining failed. Try again.").WithCause(err)
	}
	return declined, nil
}

// List is the queue for one site (or the organisation), newest first.
func (s *Service) List(ctx context.Context, p domain.Principal, siteID *uuid.UUID, limit, offset int32) ([]sqlc.AssistantAbilityRequest, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []sqlc.AssistantAbilityRequest
	err := s.runAsCaller(ctx, p, func(q *sqlc.Queries, _ pgx.Tx) error {
		var err error
		if siteID != nil {
			rows, err = q.ListAbilityRequestsForSite(ctx, sqlc.ListAbilityRequestsForSiteParams{
				TenantID: p.TenantID, SiteID: *siteID, RowLimit: limit, RowOffset: offset,
			})
			return err
		}
		rows, err = q.ListAbilityRequests(ctx, sqlc.ListAbilityRequestsParams{TenantID: p.TenantID, RowLimit: limit, RowOffset: offset})
		return err
	})
	if err != nil {
		return nil, domain.Internal("ability_requests_list_failed", "failed to list AI requests").WithCause(err)
	}
	return rows, nil
}

// decisionMetadata is the audit metadata of approve and decline. The hash
// chain covers it.
func decisionMetadata(r sqlc.AssistantAbilityRequest, withDigest bool) map[string]any {
	md := map[string]any{
		"request_id":           r.ID.String(),
		"site_id":              r.SiteID.String(),
		"ability":              r.AbilityName,
		"entry_sha256":         r.EntrySha256,
		"input_sha256":         r.InputSha256,
		"operator_permission":  r.OperatorPermission,
		"proposed_by_grant_id": r.ProposedByGrantID.String(),
		"grant_label":          r.GrantLabel,
		"site_label":           r.SiteLabel,
		"site_host":            r.SiteHost,
		"copy_version":         r.CardCopyVersion,
	}
	if withDigest {
		md["presented_digest"] = r.PresentedDigest
		md["precheck_digest"] = r.PrecheckDigest
	}
	return md
}
