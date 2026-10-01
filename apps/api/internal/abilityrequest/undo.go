package abilityrequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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

// Undo budgets. Once Begin has committed in_progress, the agent call and the
// finish transaction run on a context detached from the request, so a client
// that disconnects cannot leave the row in_progress.
const (
	// undoSendBudget bounds the revert call to the agent (the MCP layer's
	// ability-run budget).
	undoSendBudget = 10 * time.Second
	// undoFinishBudget bounds the transaction that records the result.
	undoFinishBudget = 10 * time.Second
)

// undoTxRunner runs fn in the caller's tenant transaction.
type undoTxRunner func(ctx context.Context, p domain.Principal, fn func(q *sqlc.Queries, tx pgx.Tx) error) error

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
		case "conflict", "created_post_touched", "target_in_flight":
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

	sendCtx, cancelSend := context.WithTimeout(context.WithoutCancel(ctx), undoSendBudget)
	resp, sendErr := s.agent.AbilityRun(sendCtx, siteID, siteURL, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModeRevert, RequestID: requestID, Entry: entryBytes, EntrySHA256: entrySum,
	})
	cancelSend()
	result := undoResultFor(resp, sendErr)
	return s.recordUndoFinish(ctx, s.runAsCaller, p, siteID, requestID, result, sendErr)
}

// recordUndoFinish records the undo result on a context detached from the
// request's cancellation, with its own budget: the row is already
// in_progress and must not be stranded there by a client disconnect.
func (s *Service) recordUndoFinish(ctx context.Context, run undoTxRunner, p domain.Principal, siteID, requestID uuid.UUID, result string, sendErr error) (sqlc.AssistantAbilityRequest, error) {
	var none sqlc.AssistantAbilityRequest
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoFinishBudget)
	defer cancel()
	var after sqlc.AssistantAbilityRequest
	err := run(fctx, p, func(q *sqlc.Queries, tx pgx.Tx) error {
		if _, err := q.FinishAbilityRequestUndo(fctx, sqlc.FinishAbilityRequestUndoParams{
			UndoResult: result, TenantID: p.TenantID, ID: requestID,
		}); err != nil {
			return err
		}
		md := map[string]any{"request_id": requestID.String(), "site_id": siteID.String(), "phase": "finished", "result": result}
		var refusal *agentcmd.AbilityRunRefusal
		if errors.As(sendErr, &refusal) {
			md["code"] = refusal.Code
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		if _, err := s.audit.RecordInTx(fctx, tx, audit.Event{
			TenantID: p.TenantID, ActorType: audit.ActorUser, ActorID: p.UserID.String(),
			Action: audit.ActionAssistantRequestUndone, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: requestID.String(), Metadata: md,
		}); err != nil {
			return err
		}
		var err error
		after, err = q.GetAbilityRequestForSite(fctx, sqlc.GetAbilityRequestForSiteParams{TenantID: p.TenantID, ID: requestID, SiteID: siteID})
		return err
	})
	if err != nil {
		s.logger.ErrorContext(fctx, "ability request: undo result not recorded",
			slog.String("request_id", requestID.String()), slog.String("result", result), slog.Any("error", err))
		return none, domain.Internal("ability_request_undo_unrecorded", "The undo ran but its result could not be recorded.").WithCause(err)
	}
	return after, nil
}
