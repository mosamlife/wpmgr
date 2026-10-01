package abilityrequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Undo results (m156 undo_state).
const (
	UndoDone             = "undone"
	UndoRefusedConflict  = "refused_conflict"
	UndoRefusedPublished = "refused_published"
	UndoFailed           = "failed"
)

// CodeUndoUnavailable refuses an undo that is not open: not done, already
// undone or started, or past its window.
const CodeUndoUnavailable = "ability_request_undo_unavailable"

// undoResultFor maps the agent's revert answer onto an undo result.
func undoResultFor(resp agentcmd.AbilityRunResponse, err error) string {
	if err == nil {
		if resp.Outcome == "reverted" || resp.Outcome == "already_reverted" {
			return UndoDone
		}
		return UndoFailed
	}
	var refusal *agentcmd.AbilityRunRefusal
	if errors.As(err, &refusal) {
		switch refusal.Code {
		case "created_post_published":
			return UndoRefusedPublished
		case "conflict", "target_in_flight":
			return UndoRefusedConflict
		}
	}
	return UndoFailed
}

// Undo starts and finishes a person's undo of a done request. The agent
// takes the object from its own ledger row for this request id (W3): the
// revert carries no input and never the created post id.
func (s *Service) Undo(ctx context.Context, p domain.Principal, siteID, requestID uuid.UUID) (sqlc.AssistantAbilityRequest, error) {
	var none sqlc.AssistantAbilityRequest
	if err := requireSession(p); err != nil {
		return none, err
	}
	if s.agent == nil || s.entry == nil {
		return none, errWriteToolsDisabled()
	}
	var row sqlc.AssistantAbilityRequest
	var siteURL string
	var entryBytes []byte
	var entrySum string
	err := s.runAsCaller(ctx, p, func(q *sqlc.Queries, tx pgx.Tx) error {
		var err error
		row, err = q.GetAbilityRequestForSite(ctx, sqlc.GetAbilityRequestForSiteParams{TenantID: p.TenantID, ID: requestID, SiteID: siteID})
		if err != nil {
			return err
		}
		if err := s.requireOperatorPermission(ctx, p, row); err != nil {
			return err
		}
		site, err := q.GetSite(ctx, sqlc.GetSiteParams{TenantID: p.TenantID, ID: siteID})
		if err != nil {
			return err
		}
		siteURL = site.Url
		e, err := q.GetAbilityCatalogueEntry(ctx, row.EntryID)
		if err != nil {
			return err
		}
		entryBytes, entrySum, err = s.entry(e)
		if err != nil {
			return fmt.Errorf("encode catalogue entry: %w", err)
		}
		n, err := q.BeginAbilityRequestUndo(ctx, sqlc.BeginAbilityRequestUndoParams{
			UndoByUserID: p.UserID, TenantID: p.TenantID, ID: requestID, SiteID: siteID,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return domain.Conflict(CodeUndoUnavailable, "This change can no longer be undone from WPMgr.")
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID: p.TenantID, ActorType: audit.ActorUser, ActorID: p.UserID.String(),
			Action: audit.ActionAssistantRequestUndone, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: requestID.String(),
			Metadata: map[string]any{"request_id": requestID.String(), "site_id": siteID.String(), "phase": "started"},
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
		return none, domain.Internal("ability_request_undo_failed", "Undo failed to start. Nothing changed.").WithCause(err)
	}

	resp, sendErr := s.agent.AbilityRun(ctx, siteID, siteURL, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModeRevert, RequestID: requestID, Entry: entryBytes, EntrySHA256: entrySum,
	})
	result := undoResultFor(resp, sendErr)

	var after sqlc.AssistantAbilityRequest
	err = s.runAsCaller(ctx, p, func(q *sqlc.Queries, tx pgx.Tx) error {
		if _, err := q.FinishAbilityRequestUndo(ctx, sqlc.FinishAbilityRequestUndoParams{
			UndoResult: result, TenantID: p.TenantID, ID: requestID,
		}); err != nil {
			return err
		}
		md := map[string]any{"request_id": requestID.String(), "site_id": siteID.String(), "phase": "finished", "result": result}
		var refusal *agentcmd.AbilityRunRefusal
		if errors.As(sendErr, &refusal) {
			md["code"] = refusal.Code
		}
		if _, err := s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID: p.TenantID, ActorType: audit.ActorUser, ActorID: p.UserID.String(),
			Action: audit.ActionAssistantRequestUndone, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: requestID.String(), Metadata: md,
		}); err != nil {
			return err
		}
		var err error
		after, err = q.GetAbilityRequestForSite(ctx, sqlc.GetAbilityRequestForSiteParams{TenantID: p.TenantID, ID: requestID, SiteID: siteID})
		return err
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "ability request: undo result not recorded",
			slog.String("request_id", requestID.String()), slog.String("result", result), slog.Any("error", err))
		return none, domain.Internal("ability_request_undo_unrecorded", "The undo ran but its result could not be recorded.").WithCause(err)
	}
	return after, nil
}
