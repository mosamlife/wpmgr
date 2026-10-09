// E3-A3 review: a failed rest-write's own-undo report, through the same
// dispatch worker and wpmgr_app pool as ability_rest_e3_integration_test.go.
//
// Run serially, on its own: go test -run TestE3PartialRestore ./tests/ -count=1 -p 1
package tests

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

func e3Bool(b bool) *bool { return &b }

type e3FailedRow struct {
	state, outcome, code string
	restored             *bool
	undo                 *string
}

func (w *e3World) failedRow(t *testing.T, id uuid.UUID) e3FailedRow {
	t.Helper()
	var r e3FailedRow
	var outcome, code *string
	if err := w.admin.QueryRow(context.Background(),
		`SELECT state, outcome, outcome_code, restored, undo_state FROM assistant_ability_requests WHERE id = $1`, id).
		Scan(&r.state, &outcome, &code, &r.restored, &r.undo); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if outcome != nil {
		r.outcome = *outcome
	}
	if code != nil {
		r.code = *code
	}
	return r
}

func (w *e3World) failedAudit(t *testing.T, id uuid.UUID) map[string]any {
	t.Helper()
	var raw []byte
	if err := w.admin.QueryRow(context.Background(),
		`SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3 ORDER BY created_at DESC LIMIT 1`,
		w.tenant, audit.ActionAbilityRequestFailed, id.String()).Scan(&raw); err != nil {
		t.Fatalf("read failed audit row: %v", err)
	}
	var md map[string]any
	if err := json.Unmarshal(raw, &md); err != nil {
		t.Fatalf("audit metadata: %s", raw)
	}
	return md
}

// TestE3PartialRestoreRecordedAsAppRole: the write is refused with
// side_effect_detected and the agent could not put the whole row back. The
// request is failed, restored is false on the row and on the wire, and the
// closed columns_still_changed list is on the audit row.
func TestE3PartialRestoreRecordedAsAppRole(t *testing.T) {
	w := newE3World(t)
	w.rest.writeErr = &agentcmd.AbilityRunRefusal{
		Code: "side_effect_detected", Restored: e3Bool(false), Exact: e3Bool(false),
		ColumnsStillChanged: []string{"post_status", "post_name"},
	}
	id := w.askRetitle(t, "Spring sale 2026")
	w.approve(t, id)
	w.dispatch(t, id)
	r := w.failedRow(t, id)
	if r.state != "failed" || r.outcome != "refused" || r.code != "side_effect_detected" {
		t.Fatalf("row: %+v", r)
	}
	if r.restored == nil || *r.restored {
		t.Fatalf("restored = %v, want false", r.restored)
	}
	md := w.failedAudit(t, id)
	cols, _ := md["columns_still_changed"].([]any)
	if md["restored"] != false || len(cols) != 2 || cols[0] != "post_status" || cols[1] != "post_name" {
		t.Fatalf("audit metadata: %v", md)
	}
	// The person's read, as wpmgr_app through the service, sees it too
	// (the DTO's mapping of this field is unit-tested in abilityrequest).
	site := w.site
	rows, err := w.svc.List(context.Background(), w.person, &site, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := false
	for _, q := range rows {
		if q.ID == id {
			seen = true
			if q.Restored == nil || *q.Restored {
				t.Fatalf("restored in the person's read: %v", q.Restored)
			}
		}
	}
	if !seen {
		t.Fatal("request missing from the person's list")
	}
}

// TestE3PartialRestoreCleanFailureUnchanged: a failed write whose own undo
// put the whole row back records restored true and no columns.
func TestE3PartialRestoreCleanFailureUnchanged(t *testing.T) {
	w := newE3World(t)
	w.rest.writeErr = &agentcmd.AbilityRunRefusal{Code: "side_effect_detected", Restored: e3Bool(true), Exact: e3Bool(true)}
	id := w.askRetitle(t, "Spring sale 2026")
	w.approve(t, id)
	w.dispatch(t, id)
	r := w.failedRow(t, id)
	if r.state != "failed" || r.restored == nil || !*r.restored || r.undo != nil {
		t.Fatalf("row: %+v", r)
	}
	if _, has := w.failedAudit(t, id)["columns_still_changed"]; has {
		t.Fatal("a clean restore named columns")
	}
}
