// A ledger write that genuinely fails on approve, as wpmgr_app. The fault is
// injected in the database, not in Go: INSERT on audit_log is revoked from the
// application role, so the approval's own RecordInTx reaches the append and
// the append is refused. The approval must not survive it.
package tests

import (
	"context"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/assistantrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// TestAssistantRequestApproveLedgerAppendFailsAsAppRole: with the audit
// append refused, approve answers assistant_ledger_failed, the row stays
// pending with no approver, and no approval row exists. With the privilege
// restored the same approval then succeeds, which proves the refusal came
// from the fault and not from the fixture.
func TestAssistantRequestApproveLedgerAppendFailsAsAppRole(t *testing.T) {
	ctx := context.Background()
	st := newARStack(t)
	s1 := st.site(t)
	g1 := st.grant(t, s1)
	row := st.pending(t, s1, g1.ID)

	appendAllowed := func() bool {
		t.Helper()
		var ok bool
		if err := st.admin.QueryRow(ctx,
			`SELECT has_table_privilege('wpmgr_app', 'audit_log', 'INSERT')`).Scan(&ok); err != nil {
			t.Fatalf("read the audit_log privilege: %v", err)
		}
		return ok
	}
	if !appendAllowed() {
		t.Fatal("fixture: wpmgr_app cannot append to audit_log before the fault is planted")
	}
	st.exec(t, `REVOKE INSERT ON audit_log FROM wpmgr_app`)
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		st.exec(t, `GRANT INSERT ON audit_log TO wpmgr_app`)
	}
	t.Cleanup(restore)
	if appendAllowed() {
		t.Fatal("fixture: the revoke did not take; wpmgr_app still holds INSERT on audit_log (a grant through another role?)")
	}

	_, err := st.svc.Approve(ctx, st.approver(), s1, row.ID, row.PresentedDigest)
	arWantCode(t, "approve with the audit append refused", err, 500, assistantrequest.CodeLedgerFailed)

	restore()
	if !appendAllowed() {
		t.Fatal("fixture: INSERT on audit_log was not restored")
	}
	got := st.state(t, row.ID)
	if got.State != assistantrequest.StatePending || got.DecidedBy != nil {
		t.Fatalf("a refused ledger append left the row %s decided by %v; want pending with no approver", got.State, got.DecidedBy)
	}
	if n := len(st.auditRows(t, audit.ActionAssistantRequestApproved, row.ID)); n != 0 {
		t.Fatalf("%d approval audit rows after a refused append, want 0", n)
	}

	// The same approval, with the ledger writable again, goes through.
	if _, err := st.svc.Approve(ctx, st.approver(), s1, row.ID, row.PresentedDigest); err != nil {
		t.Fatalf("approve after the privilege was restored: %v", err)
	}
	if got := st.state(t, row.ID); got.State != assistantrequest.StateApproved || got.DecidedBy == nil || *got.DecidedBy != st.user {
		t.Fatalf("after the restored approve: %+v", got)
	}
}
