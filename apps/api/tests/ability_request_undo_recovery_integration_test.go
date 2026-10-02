// GH #824, #826, #828: retryable undo, the recovery undo of a draft a failed
// write left, the stuck-undo reconciler and the organisation-wide queue, all
// through internal/abilityrequest's service on the wpmgr_app pool with the
// real audit recorder (newE2World). The superuser connection only arranges
// row state and reads it back.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// failedDraft runs ask, approve and a write the site answers with a
// verification mismatch whose cleanup did not trash the draft: the row is
// failed, with the created post recorded.
func (w *e2World) failedDraft(t *testing.T) uuid.UUID {
	t.Helper()
	id := w.ask(t)
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	w.agent.setOverride(func(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool) {
		if call.Mode == agentcmd.AbilityRunModeWrite {
			return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "verify_mismatch", PostID: 42, Trashed: false}, true
		}
		return agentcmd.AbilityRunResponse{}, nil, false
	})
	w.dispatch(t, id)
	w.agent.setOverride(nil)
	state, outcome, _, undo, post := w.row(t, id)
	if state != "failed" || outcome == nil || *outcome != abilityrequest.OutcomeVerifyMismatch || post == nil || *post != 42 || undo != nil {
		t.Fatalf("arranged row: state=%s outcome=%v post=%v undo=%v", state, outcome, post, undo)
	}
	return id
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

// TestE2RecoveryUndoOfFailedDraftAsAppRole (GH #826): a failed write that
// left its draft can be undone. A transport failure on the first try puts
// the row back to "no undo" (still offered); the retry trashes the draft.
func TestE2RecoveryUndoOfFailedDraftAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.failedDraft(t)

	w.agent.setOverride(func(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool) {
		if call.Mode == agentcmd.AbilityRunModeRevert {
			return agentcmd.AbilityRunResponse{}, errors.New("dial tcp: connection reset"), true
		}
		return agentcmd.AbilityRunResponse{}, nil, false
	})
	_, err := w.svc.Undo(context.Background(), w.person, w.site, id)
	wantCode(t, err, abilityrequest.CodeUndoRetry)
	if state, _, _, undo, _ := w.row(t, id); state != "failed" || undo != nil {
		t.Fatalf("after a transport failure: state=%s undo=%v, want failed with no undo (offered again)", state, undo)
	}

	w.agent.setOverride(nil)
	got, err := w.svc.Undo(context.Background(), w.person, w.site, id)
	if err != nil {
		t.Fatalf("recovery undo: %v", err)
	}
	if got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("undo_state = %v, want undone", got.UndoState)
	}
	if w.agent.last.Mode != agentcmd.AbilityRunModeRevert || len(w.agent.last.Input) != 0 || w.agent.last.RequestID != id {
		t.Fatalf("revert not bound to the request or carried input: %+v", w.agent.last)
	}
	_, err = w.svc.Undo(context.Background(), w.person, w.site, id)
	wantCode(t, err, abilityrequest.CodeUndoUnavailable)
}

// TestE2RecoveryUndoNotRevertibleIsFinalAsAppRole: the site's definite
// refusal finishes the recovery undo as failed; it is not offered again.
func TestE2RecoveryUndoNotRevertibleIsFinalAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.failedDraft(t)
	w.agent.setOverride(func(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool) {
		if call.Mode == agentcmd.AbilityRunModeRevert {
			return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "not_revertible"}, true
		}
		return agentcmd.AbilityRunResponse{}, nil, false
	})
	got, err := w.svc.Undo(context.Background(), w.person, w.site, id)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got.UndoState == nil || *got.UndoState != abilityrequest.UndoFailed {
		t.Fatalf("undo_state = %v, want failed", got.UndoState)
	}
}

// TestE2UndoRetryableReleasesDoneRowAsAppRole (GH #824): a done row's undo
// answered target_in_flight goes back to available inside its window.
func TestE2UndoRetryableReleasesDoneRowAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.ask(t)
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	w.dispatch(t, id)
	w.agent.setOverride(func(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool) {
		if call.Mode == agentcmd.AbilityRunModeRevert {
			return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "target_in_flight"}, true
		}
		return agentcmd.AbilityRunResponse{}, nil, false
	})
	_, err := w.svc.Undo(context.Background(), w.person, w.site, id)
	wantCode(t, err, abilityrequest.CodeUndoRetry)
	if _, _, _, undo, _ := w.row(t, id); undo == nil || *undo != "available" {
		t.Fatalf("undo_state after target_in_flight = %v, want available", undo)
	}
	w.agent.setOverride(nil)
	got, err := w.svc.Undo(context.Background(), w.person, w.site, id)
	if err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("retry: %v %v", got.UndoState, err)
	}
}

// TestE2StuckUndoReconciledFromLedgerAsAppRole (GH #824): an undo left
// in_progress is released when the ledger says the draft is still there and
// finished undone when the ledger says it was trashed. The scan runs under
// the agent principal, each settle in a tenant transaction.
func TestE2StuckUndoReconciledFromLedgerAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.ask(t)
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	w.dispatch(t, id)
	strand := func() {
		t.Helper()
		if _, err := w.admin.Exec(context.Background(),
			`UPDATE assistant_ability_requests SET undo_state = 'in_progress', undo_by_user_id = $2,
			        undo_started_at = now() - interval '10 minutes'
			  WHERE id = $1`, id, w.person.UserID); err != nil {
			t.Fatalf("strand undo: %v", err)
		}
	}
	ledger := func(undoState string) {
		w.agent.setOverride(func(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool) {
			if call.Mode == agentcmd.AbilityRunModeLedger {
				return agentcmd.AbilityRunResponse{OK: true, Outcome: "ledger", Found: true, UndoState: undoState}, nil, true
			}
			return agentcmd.AbilityRunResponse{}, nil, false
		})
	}
	reconcile := func() {
		t.Helper()
		if err := abilityrequest.NewUndoReconcileWorker(w.svc).Work(context.Background(), &river.Job[abilityrequest.UndoReconcileArgs]{}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	strand()
	ledger("available")
	reconcile()
	if _, _, _, undo, _ := w.row(t, id); undo == nil || *undo != "available" {
		t.Fatalf("not reverted on the site: undo_state = %v, want available", undo)
	}

	strand()
	w.agent.setOverride(func(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool) {
		if call.Mode == agentcmd.AbilityRunModeLedger {
			return agentcmd.AbilityRunResponse{}, errors.New("unreachable"), true
		}
		return agentcmd.AbilityRunResponse{}, nil, false
	})
	reconcile()
	if _, _, _, undo, _ := w.row(t, id); undo == nil || *undo != "in_progress" {
		t.Fatalf("site unreachable: undo_state = %v, want in_progress left alone", undo)
	}

	ledger("trashed")
	reconcile()
	if _, _, _, undo, _ := w.row(t, id); undo == nil || *undo != abilityrequest.UndoDone {
		t.Fatalf("reverted on the site: undo_state = %v, want undone", undo)
	}
}

// TestE2OrgAbilityRequestListAsAppRole (GH #828): the organisation-wide
// queue lists this tenant's requests with the badge count, narrows by
// state, and shows another tenant nothing.
func TestE2OrgAbilityRequestListAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.ask(t)
	ctx := context.Background()

	q, err := w.svc.ListOrg(ctx, w.person, nil, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(q.Requests) != 1 || q.Requests[0].ID != id || q.PendingCount != 1 {
		t.Fatalf("own org: %d rows, pending %d; want the one waiting request", len(q.Requests), q.PendingCount)
	}
	done := "done"
	q, err = w.svc.ListOrg(ctx, w.person, &done, 50, 0)
	if err != nil || len(q.Requests) != 0 {
		t.Fatalf("state=done: %d rows, err %v; want none", len(q.Requests), err)
	}

	other := seedTenant(t, w.pool, "e2-other-"+uuid.NewString()[:8])
	user := seedUserRow(t, w.admin, "e2-other-"+uuid.NewString()[:8]+"@example.test")
	seedMembershipRow(t, w.admin, user, other)
	stranger := domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: other, Scope: domain.ScopeOrg, Role: "owner"}
	q, err = w.svc.ListOrg(ctx, stranger, nil, 50, 0)
	if err != nil {
		t.Fatalf("other tenant list: %v", err)
	}
	if len(q.Requests) != 0 || q.PendingCount != 0 {
		t.Fatalf("another tenant sees %d rows, pending %d; want none", len(q.Requests), q.PendingCount)
	}
}
