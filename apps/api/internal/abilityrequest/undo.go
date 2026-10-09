package abilityrequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
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
// undone or started, past its window, or a page edit with a later edit of
// the same page still in effect.
const CodeUndoUnavailable = "ability_request_undo_unavailable"

// CodeUndoBusy refuses, before anything is sent, the undo of a page-builder
// draft while a page edit of that draft is still being made or undone. The
// undo stays available.
const CodeUndoBusy = "ability_request_undo_busy"

// Undo refusal copy.
const (
	msgUndoUnavailable = "This change can no longer be undone from WPMgr."
	msgUndoLaterFirst  = "Undo the later change first."
	msgUndoBusy        = "A change to this draft is still being finished. Try again in a minute."
	msgUndoChainLong   = "WPMgr has changed this draft too many times for one undo to check, so it cannot move it to the trash. Remove it in WordPress if you do not want it."
)

func errUndoUnavailable() error { return domain.Conflict(CodeUndoUnavailable, msgUndoUnavailable) }

// editListLimit is ListEditRequestsForPost's row limit. A list that long may
// be missing the newest edits, so a draft with that many edit requests is
// never trashed on its strength.
const editListLimit = 200

// undoResultFor maps the agent's revert answer onto an undo result.
//
// A page edit's undo answers refused_published when the page is published,
// scheduled or private now, target_not_draft when it is no longer a draft or
// is gone, and conflict when the page is open, has an autosave, or a later
// change rewrote what this change wrote (changed_after_this_change): nothing
// was written in any of these. snapshot_tampered (the kept copy, its ledger
// record and the hash this undo sent are not one) and restore_mismatch (the
// put-back did not read back as the copy) finish it as failed.
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
		case "created_post_published", "refused_published":
			return UndoRefusedPublished
		case "conflict", "created_post_touched", "target_in_flight", "post_touched", "target_not_draft":
			return UndoRefusedConflict
		case "snapshot_tampered", "restore_mismatch":
			return UndoFailed
		}
	}
	return UndoFailed
}

// undoRevertFor is the revert's signed parameters for row, read in the
// transaction that starts the undo, or the refusal that stops the undo
// before anything is sent. Nil parameters send no p.revert.
//
// A wpmgr/page-edit undo sends the snapshot hash its applied outcome
// recorded, and runs only for the newest applied edit of its post that has
// not been undone: undo goes newest first.
//
// A wpmgr/page-create undo names every applied wpmgr/page-edit of the draft
// it created, in the order they were applied, so the agent can tell WPMgr's
// own changes to the draft from anyone else's. Each applied edit's precheck
// read the page after the edit before it had been applied, so the order the
// requests were made in is the order they were applied in. An undone edit is
// named too: its revisions are still WPMgr's. The undo waits while an edit of
// the draft is approved and not yet sent, sent and not answered, still being
// resolved from the site's ledger, or being undone. With no applied edit
// nothing is sent, as before page edits existed.
func undoRevertFor(ctx context.Context, q *sqlc.Queries, r sqlc.AssistantAbilityRequest) (*agentcmd.AbilityRunRevert, error) {
	switch r.AbilityName {
	case mcp.AbilityPageEdit:
		if r.TargetPostID == nil || r.SnapshotSha256 == nil || !snapshotHashPattern.MatchString(*r.SnapshotSha256) {
			return nil, errUndoUnavailable()
		}
		newest, err := q.NewestUndoableEditForPost(ctx, sqlc.NewestUndoableEditForPostParams{
			TenantID: r.TenantID, SiteID: r.SiteID, PostID: *r.TargetPostID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errUndoUnavailable()
		}
		if err != nil {
			return nil, err
		}
		if newest.ID != r.ID {
			return nil, domain.Conflict(CodeUndoUnavailable, msgUndoLaterFirst)
		}
		return &agentcmd.AbilityRunRevert{SnapshotSHA256: *r.SnapshotSha256}, nil
	case mcp.AbilityPageCreate:
		if r.CreatedPostID == nil || *r.CreatedPostID < 1 {
			return nil, nil
		}
		edits, err := q.ListEditRequestsForPost(ctx, sqlc.ListEditRequestsForPostParams{
			TenantID: r.TenantID, SiteID: r.SiteID, PostID: *r.CreatedPostID,
		})
		if err != nil {
			return nil, err
		}
		return revertChainFor(r, edits)
	}
	return nil, nil
}

// revertChainFor is a page-create undo's chain from the edit requests of
// the post it created (ListEditRequestsForPost, oldest first): see
// undoRevertFor. Only edits made since this creation are its own.
func revertChainFor(r sqlc.AssistantAbilityRequest, edits []sqlc.ListEditRequestsForPostRow) (*agentcmd.AbilityRunRevert, error) {
	if len(edits) >= editListLimit {
		return nil, domain.Conflict(CodeUndoUnavailable, msgUndoChainLong)
	}
	var chain []uuid.UUID
	for _, e := range edits {
		if e.CreatedAt.Before(r.CreatedAt) {
			continue
		}
		if editSettling(e) {
			return nil, domain.Conflict(CodeUndoBusy, msgUndoBusy)
		}
		if e.State == "done" && e.Outcome != nil && *e.Outcome == OutcomeApplied {
			chain = append(chain, e.ID)
		}
	}
	if len(chain) == 0 {
		return nil, nil
	}
	if len(chain) > agentcmd.AbilityRunMaxRevertChain {
		return nil, domain.Conflict(CodeUndoUnavailable, msgUndoChainLong)
	}
	return &agentcmd.AbilityRunRevert{Chain: chain}, nil
}

// editSettling: the site may still apply this edit, or still be undoing it.
// Approved and not yet sent, sent and not answered, its outcome still being
// read from the site's ledger (outcome_unknown with no outcome), or its undo
// started and not finished. A write WPMgr gave up resolving is settled.
func editSettling(e sqlc.ListEditRequestsForPostRow) bool {
	switch {
	case e.State == "approved", e.State == "dispatched":
		return true
	case e.State == "outcome_unknown" && e.Outcome == nil:
		return true
	case e.UndoState != nil && *e.UndoState == "in_progress":
		return true
	}
	return false
}

// Undo starts and finishes a person's undo of a done request. The agent
// takes the object from its own ledger row for this request id (W3): the
// revert carries no input and never a post id. Its only parameters are
// p.revert (undoRevertFor): the snapshot hash a page edit's outcome
// recorded, or the page edits of a draft WPMgr created, which name ledger
// rows on the site and nothing the agent acts on directly.
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
	var recovery bool
	var revert *agentcmd.AbilityRunRevert
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
		kind := undoKindFor(row, site.AgentVersion, s.clock())
		if kind == undoKindNone {
			return errUndoUnavailable()
		}
		recovery = kind == undoKindRecovery
		if revert, err = undoRevertFor(ctx, q, row); err != nil {
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
		var n int64
		if recovery {
			n, err = q.BeginAbilityRequestRecoveryUndo(ctx, sqlc.BeginAbilityRequestRecoveryUndoParams{
				UndoByUserID: p.UserID, WindowSeconds: int32(recoveryUndoWindow / time.Second),
				TenantID: p.TenantID, ID: requestID, SiteID: siteID,
			})
		} else {
			n, err = q.BeginAbilityRequestUndo(ctx, sqlc.BeginAbilityRequestUndoParams{
				UndoByUserID: p.UserID, TenantID: p.TenantID, ID: requestID, SiteID: siteID,
			})
		}
		if err != nil {
			return err
		}
		if n != 1 {
			return errUndoUnavailable()
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		md := map[string]any{"request_id": requestID.String(), "site_id": siteID.String(), "phase": "started", "recovery": recovery}
		if revert != nil && revert.Chain != nil {
			md["chain_length"] = len(revert.Chain)
		}
		_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID: p.TenantID, ActorType: audit.ActorUser, ActorID: p.UserID.String(),
			Action: audit.ActionAssistantRequestUndone, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: requestID.String(), Metadata: md,
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
		Revert: revert,
	})
	cancelSend()
	if undoRetryable(sendErr) {
		after, released, err := s.recordUndoRelease(ctx, s.runAsCaller, p, siteID, requestID, sendErr)
		if err != nil {
			return none, err
		}
		if released {
			return none, errUndoRetry()
		}
		return after, nil
	}
	result := undoResultFor(resp, sendErr)
	return s.recordUndoFinish(ctx, s.runAsCaller, p, siteID, requestID, result, sendErr, revertReport(resp, sendErr))
}

// revertReport is a rest-write revert's restore report for the audit row:
// restored is false when the title and excerpt were put back but other post
// columns the site changed during the failed write remain, which
// columns_still_changed names from the closed post column set. The undo is
// still recorded as undone: what WPMgr changed is back. Nil when the answer
// carries no report.
func revertReport(resp agentcmd.AbilityRunResponse, err error) map[string]any {
	if err != nil || resp.Restored == nil {
		return nil
	}
	md := map[string]any{"restored": *resp.Restored}
	if cols := agentcmd.DecodePostColumns(resp.ColumnsStillChanged); len(cols) > 0 {
		md["columns_still_changed"] = cols
	}
	return md
}

// reportRestored is the revert report's restored flag for the ledger row,
// nil when the report carries none, which keeps the stored value.
func reportRestored(report map[string]any) *bool {
	if b, ok := report["restored"].(bool); ok {
		return &b
	}
	return nil
}

// recoveryUndoWindow is how long a recovery undo, once started, stays open
// for a retry (GH #826). A released recovery undo starts afresh.
const recoveryUndoWindow = 15 * time.Minute

// CodeUndoRetry: the site did not settle the undo; it is open again.
const CodeUndoRetry = "ability_request_undo_retry"

func errUndoRetry() error {
	return domain.ServiceUnavailable(CodeUndoRetry,
		"The site did not confirm the undo. It is still available; try again in a moment.")
}

func (s *Service) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

type undoKind int

const (
	undoKindNone undoKind = iota
	undoKindDone
	undoKindRecovery
)

// undoKindFor decides which undo, if any, a row offers a person now. A done
// row offers the normal undo while it is available and inside its window. A
// failed or given-up write that recorded the post it created, whose draft
// was not already trashed, and whose site record is still retained, offers
// the recovery undo while no undo has started, but only when the site's
// recorded agent version (agentVersion) ships the recovery revert: an older
// agent answers not_revertible. The database statements re-check the row
// conditions; this decides which one to run and what the card offers.
func undoKindFor(r sqlc.AssistantAbilityRequest, agentVersion string, now time.Time) undoKind {
	switch r.State {
	case "done":
		if r.UndoState != nil && *r.UndoState == "available" &&
			r.UndoAvailableUntil.Valid && r.UndoAvailableUntil.Time.After(now) {
			return undoKindDone
		}
	case "failed", "outcome_unknown":
		if r.UndoState != nil || r.Outcome == nil || r.CreatedPostID == nil || *r.CreatedPostID < 1 {
			return undoKindNone
		}
		if r.Trashed != nil && *r.Trashed {
			return undoKindNone
		}
		if !r.OutcomeAt.Valid || now.Sub(r.OutcomeAt.Time) >= undoRetention {
			return undoKindNone
		}
		if !recoveryUndoSupported(agentVersion) {
			return undoKindNone
		}
		return undoKindRecovery
	}
	return undoKindNone
}

// recoveryUndoSupported: the site's recorded agent version ships the
// recovery revert. An empty or unparseable version offers nothing.
func recoveryUndoSupported(agentVersion string) bool {
	return agentVersion != "" && wpversion.Compare(agentVersion, agentcmd.MinAgentVersionForRecoveryUndo) >= 0
}

// UndoOffered is the card's "Undo" button, computed server-side. newestEdit
// says whether a wpmgr/page-edit row is the newest applied edit of its post
// that has not been undone (Service.NewestEdits); an older edit offers no
// undo, since undo goes newest first. It is ignored for every other ability.
func UndoOffered(r sqlc.AssistantAbilityRequest, agentVersion string, newestEdit bool, now time.Time) bool {
	if undoKindFor(r, agentVersion, now) == undoKindNone {
		return false
	}
	return r.AbilityName != mcp.AbilityPageEdit || newestEdit
}

// undoRetryable is true when the revert's answer settles nothing: the call
// did not reach the site, timed out, or its reply was lost, or the site was
// busy with this request or its post, or could not read its own database
// (a page edit's kept copy, or the page). The undo is released so the person
// can retry; a revert that did run answers already_reverted next time.
//
// A definite answer that a resend cannot change is not retryable and
// finishes the undo as failed: any other refusal, a 4xx from the site (the
// token rejected, the route gone because the plugin was removed, a bad
// request) other than 408, 425 and 429, and a reply that did not decode.
//
// A recovery undo answered not_revertible is final too, and is not released:
// the gate on MinAgentVersionForRecoveryUndo means the site's agent knows the
// recovery revert, so not_revertible is its ledger saying this request left
// nothing it can undo. Released, the button would come back and fail the
// same way on every press; failed, the card says so and the person deals
// with the draft on the site.
func undoRetryable(err error) bool {
	if err == nil {
		return false
	}
	var refusal *agentcmd.AbilityRunRefusal
	if errors.As(err, &refusal) {
		switch refusal.Code {
		case "target_in_flight", "request_in_flight", "snapshot_unreadable", "data_unreadable":
			return true
		}
		return false
	}
	if errors.Is(err, agentcmd.ErrAbilityRunMalformed) {
		return false
	}
	var cmdErr *agentcmd.CommandError
	if errors.As(err, &cmdErr) {
		return !permanentHTTPStatus(cmdErr.Status)
	}
	return true
}

// permanentHTTPStatus is a 4xx a resend of the same call would get again.
// 408, 425 and 429 are about timing, not the call.
func permanentHTTPStatus(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return true
}

// releaseOrFail puts an in-progress undo back for a retry, in the caller's
// transaction. Past its window nothing is released and the undo is finished
// as failed instead. It returns which happened, or "" when the undo was no
// longer in progress (another path settled it first).
func releaseOrFail(ctx context.Context, q *sqlc.Queries, tenantID, requestID uuid.UUID) (string, error) {
	n, err := q.ReleaseAbilityRequestUndo(ctx, sqlc.ReleaseAbilityRequestUndoParams{TenantID: tenantID, ID: requestID})
	if err != nil {
		return "", err
	}
	if n == 1 {
		return undoReleased, nil
	}
	n, err = q.FinishAbilityRequestUndo(ctx, sqlc.FinishAbilityRequestUndoParams{
		UndoResult: UndoFailed, TenantID: tenantID, ID: requestID,
	})
	if err != nil || n != 1 {
		return "", err
	}
	return UndoFailed, nil
}

// undoReleased is releaseOrFail's "open again" answer; never stored.
const undoReleased = "released"

// recordUndoRelease records a retryable undo answer on a detached context,
// as recordUndoFinish does.
func (s *Service) recordUndoRelease(ctx context.Context, run undoTxRunner, p domain.Principal, siteID, requestID uuid.UUID, sendErr error) (sqlc.AssistantAbilityRequest, bool, error) {
	var none sqlc.AssistantAbilityRequest
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoFinishBudget)
	defer cancel()
	var after sqlc.AssistantAbilityRequest
	var released bool
	err := run(fctx, p, func(q *sqlc.Queries, tx pgx.Tx) error {
		settled, err := releaseOrFail(fctx, q, p.TenantID, requestID)
		if err != nil {
			return err
		}
		released = settled == undoReleased
		if settled == "" {
			// Another path settled the undo first: nothing changed here, so
			// nothing is audited; the caller gets the row as it now stands.
			var err error
			after, err = q.GetAbilityRequestForSite(fctx, sqlc.GetAbilityRequestForSiteParams{TenantID: p.TenantID, ID: requestID, SiteID: siteID})
			return err
		}
		md := map[string]any{"request_id": requestID.String(), "site_id": siteID.String(), "phase": "released"}
		if !released {
			md["phase"], md["result"] = "finished", UndoFailed
		}
		var refusal *agentcmd.AbilityRunRefusal
		if errors.As(sendErr, &refusal) {
			md["code"] = refusal.Code
		} else {
			md["code"] = "transport"
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
		after, err = q.GetAbilityRequestForSite(fctx, sqlc.GetAbilityRequestForSiteParams{TenantID: p.TenantID, ID: requestID, SiteID: siteID})
		return err
	})
	if err != nil {
		s.logger.ErrorContext(fctx, "ability request: undo release not recorded",
			slog.String("request_id", requestID.String()), slog.Any("error", err))
		return none, false, domain.Internal("ability_request_undo_unrecorded", "The undo's result could not be recorded.").WithCause(err)
	}
	return after, released, nil
}

// recordUndoFinish records the undo result on a context detached from the
// request's cancellation, with its own budget: the row is already
// in_progress and must not be stranded there by a client disconnect.
func (s *Service) recordUndoFinish(ctx context.Context, run undoTxRunner, p domain.Principal, siteID, requestID uuid.UUID, result string, sendErr error, report map[string]any) (sqlc.AssistantAbilityRequest, error) {
	var none sqlc.AssistantAbilityRequest
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoFinishBudget)
	defer cancel()
	var after sqlc.AssistantAbilityRequest
	err := run(fctx, p, func(q *sqlc.Queries, tx pgx.Tx) error {
		if _, err := q.FinishAbilityRequestUndo(fctx, sqlc.FinishAbilityRequestUndoParams{
			UndoResult: result, Restored: reportRestored(report), TenantID: p.TenantID, ID: requestID,
		}); err != nil {
			return err
		}
		md := map[string]any{"request_id": requestID.String(), "site_id": siteID.String(), "phase": "finished", "result": result}
		for k, v := range report {
			md[k] = v
		}
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
