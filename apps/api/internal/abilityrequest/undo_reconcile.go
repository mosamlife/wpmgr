package abilityrequest

// The stuck-undo reconciler (GH #824). An undo whose start committed but
// whose result was never recorded (the process died between the two) stays
// in_progress. This periodic pass asks the site's ledger whether the revert
// happened and settles the row: undone if it did, released for a retry if it
// did not, left alone while the site cannot tell.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

const (
	// UndoReconcileInterval is how often stuck undos are looked for.
	UndoReconcileInterval = time.Minute
	// StuckUndoAfter is how long an undo may stay in_progress before the
	// reconciler asks the site. It is past the revert's send budget, the
	// finish budget and the command token's settle time, so no revert sent
	// by the request that started the undo can still arrive.
	StuckUndoAfter = undoSendBudget + undoFinishBudget + tokenSettled + 30*time.Second
	// undoReconcileRowLimit bounds one pass; each row is one site call.
	undoReconcileRowLimit = 25
	// undoLedgerBudget bounds one ledger call.
	undoLedgerBudget = 10 * time.Second
)

// UndoReconcileArgs is the periodic stuck-undo pass.
type UndoReconcileArgs struct{}

// Kind implements river.JobArgs.
func (UndoReconcileArgs) Kind() string { return "ability_request_undo_reconcile" }

// UndoReconcileWorker runs the stuck-undo pass.
type UndoReconcileWorker struct {
	river.WorkerDefaults[UndoReconcileArgs]
	svc *Service
}

// NewUndoReconcileWorker builds the stuck-undo reconciler.
func NewUndoReconcileWorker(svc *Service) *UndoReconcileWorker { return &UndoReconcileWorker{svc: svc} }

// Timeout bounds one pass: every row's ledger call is bounded.
func (w *UndoReconcileWorker) Timeout(*river.Job[UndoReconcileArgs]) time.Duration {
	return undoReconcileRowLimit*undoLedgerBudget + time.Minute
}

// Work runs one pass.
func (w *UndoReconcileWorker) Work(ctx context.Context, _ *river.Job[UndoReconcileArgs]) error {
	return w.svc.reconcileStuckUndos(ctx)
}

// undoVerdict is what one ledger answer says about a stuck undo.
type undoVerdict int

const (
	undoVerdictUnknown undoVerdict = iota
	undoVerdictReverted
	undoVerdictNotReverted
)

// undoVerdictFor decides a stuck undo of a request for ability from one
// ledger answer. A failed call or a request still running on the site
// decides nothing. No ledger row means nothing was ever there to revert. The
// ledger marks a reverted draft 'trashed' and an undone page edit
// 'restored'; any other state means the revert did not happen.
func undoVerdictFor(ability string, resp agentcmd.AbilityRunResponse, err error) undoVerdict {
	if err != nil || resp.Inflight {
		return undoVerdictUnknown
	}
	if !resp.Found {
		return undoVerdictNotReverted
	}
	reverted := "trashed"
	if ability == mcp.AbilityPageEdit {
		reverted = "restored"
	}
	if resp.UndoState == reverted {
		return undoVerdictReverted
	}
	return undoVerdictNotReverted
}

func (s *Service) reconcileStuckUndos(ctx context.Context) error {
	var rows []sqlc.ListStuckAbilityRequestUndosRow
	if err := s.runAgentScan(ctx, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListStuckAbilityRequestUndos(ctx, sqlc.ListStuckAbilityRequestUndosParams{
			StaleAfterSeconds: int32(StuckUndoAfter / time.Second), RowLimit: undoReconcileRowLimit,
		})
		return err
	}); err != nil {
		return fmt.Errorf("scan stuck ability request undos: %w", err)
	}
	var errs []error
	for _, r := range rows {
		errs = append(errs, s.reconcileUndoOne(ctx, r))
	}
	return errors.Join(errs...)
}

func (s *Service) reconcileUndoOne(ctx context.Context, r sqlc.ListStuckAbilityRequestUndosRow) error {
	if s.agent == nil {
		return nil
	}
	var site sqlc.GetSiteRow
	var row sqlc.AssistantAbilityRequest
	var found bool
	if err := s.pool.InTenantTx(ctx, r.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		site, err = q.GetSite(ctx, sqlc.GetSiteParams{TenantID: r.TenantID, ID: r.SiteID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		row, err = q.GetAbilityRequestForSite(ctx, sqlc.GetAbilityRequestForSiteParams{TenantID: r.TenantID, ID: r.ID, SiteID: r.SiteID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	}); err != nil {
		return fmt.Errorf("read site for stuck undo: %w", err)
	}
	if !found {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, undoLedgerBudget)
	resp, err := s.agent.AbilityRun(callCtx, r.SiteID, site.Url, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModeLedger, RequestID: r.ID,
	})
	cancel()
	verdict := undoVerdictFor(row.AbilityName, resp, err)
	if verdict == undoVerdictUnknown {
		return nil
	}
	return s.pool.InTenantTx(ctx, r.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		md := map[string]any{"site_id": r.SiteID.String(), "closed_by": "undo_reconciler"}
		if verdict == undoVerdictReverted {
			n, err := q.FinishAbilityRequestUndo(ctx, sqlc.FinishAbilityRequestUndoParams{
				UndoResult: UndoDone, TenantID: r.TenantID, ID: r.ID,
			})
			if err != nil || n != 1 {
				return err
			}
			md["phase"], md["result"] = "finished", UndoDone
			return s.record(ctx, tx, r.TenantID, r.ID, audit.ActionAssistantRequestUndone, md)
		}
		settled, err := releaseOrFail(ctx, q, r.TenantID, r.ID)
		if err != nil || settled == "" {
			return err
		}
		md["phase"] = "released"
		if settled == UndoFailed {
			md["phase"], md["result"] = "finished", UndoFailed
		}
		return s.record(ctx, tx, r.TenantID, r.ID, audit.ActionAssistantRequestUndone, md)
	})
}
