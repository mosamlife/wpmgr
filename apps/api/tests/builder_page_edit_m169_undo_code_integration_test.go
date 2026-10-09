package tests

// m169's undo_code column, its two CHECKs and its grant. Every statement
// goes through the production dispatch (RunTenantTx / InTenantTx) on the
// wpmgr_app pool, and every transaction asserts from inside that it is
// wpmgr_app with neither SUPERUSER nor BYPASSRLS. Migration bodies run as
// wpmgr_owner (m169ApplyMessage).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

const (
	m169UndoCodeValueCheck = "assistant_ability_requests_undo_code_check"
	m169UndoCodeStateCheck = "assistant_ability_requests_undo_code_only_when_failed_check"
)

// m169UCFinish is the shipped FinishAbilityRequestUndo as wpmgr_app.
func m169UCFinish(t *testing.T, pool *db.Pool, row sqlc.AssistantAbilityRequest, result string, code *string) (int64, error) {
	t.Helper()
	var n int64
	err := pool.RunTenantTx(context.Background(), acprOrgPrincipal(row.TenantID), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m169 finish undo)")
		var err error
		n, err = sqlc.New(tx).FinishAbilityRequestUndo(context.Background(), sqlc.FinishAbilityRequestUndoParams{
			UndoResult: result, UndoCode: code, TenantID: row.TenantID, ID: row.ID,
		})
		return err
	})
	return n, err
}

// m169UCSetCode is a bare UPDATE of undo_code as wpmgr_app: the statement
// any future query could send under the column grant.
func m169UCSetCode(t *testing.T, pool *db.Pool, row sqlc.AssistantAbilityRequest, code *string) (int64, error) {
	t.Helper()
	var n int64
	err := pool.RunTenantTx(context.Background(), acprOrgPrincipal(row.TenantID), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m169 bare undo_code update)")
		tag, err := tx.Exec(context.Background(),
			`UPDATE assistant_ability_requests SET undo_code = $2 WHERE id = $1`, row.ID, code)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

func m169UCRead(t *testing.T, pool *db.Pool, row sqlc.AssistantAbilityRequest) (undoState, undoCode *string) {
	t.Helper()
	if err := pool.InTenantTx(context.Background(), row.TenantID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m169 undo read)")
		return tx.QueryRow(context.Background(),
			`SELECT undo_state, undo_code FROM assistant_ability_requests WHERE id = $1`, row.ID).Scan(&undoState, &undoCode)
	}); err != nil {
		t.Fatalf("read undo of %s: %v", row.ID, err)
	}
	return undoState, undoCode
}

func m169UCStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// m169UCUndoing is an applied page edit whose undo is in progress.
func m169UCUndoing(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, post int64, seed string) sqlc.AssistantAbilityRequest {
	t.Helper()
	row := m169Applied(t, pool, tenant, site, post, seed)
	m169BeginUndo(t, pool, row)
	return row
}

// TestM169UndoCodeGrantComesFromTheMigrationAsAppRole: with the request
// grants an install had before undo_code (m156's columns and
// snapshot_sha256), the shipped finish is refused even with a NULL code,
// because it names the column; re-running m169 grants undo_code and nothing
// else.
func TestM169UndoCodeGrantComesFromTheMigrationAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m169-uc-grant-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	row := m169UCUndoing(t, pool, tenant, site, 42, "uc-grant")

	owner := connectOwner(t, pool)
	for _, stmt := range []string{
		"REVOKE UPDATE ON assistant_ability_requests FROM wpmgr_app",
		"GRANT UPDATE (" + m156UpdateColumns + ", snapshot_sha256) ON assistant_ability_requests TO wpmgr_app",
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			owner.Close()
			t.Fatalf("restore the grants before undo_code (%s): %v", stmt, err)
		}
	}
	owner.Close()

	tampered := "snapshot_tampered"
	for _, c := range []struct {
		result string
		code   *string
	}{{"failed", &tampered}, {"undone", nil}} {
		_, err := m169UCFinish(t, pool, row, c.result, c.code)
		if got, _ := m169Refusal(err); got != "42501" {
			t.Fatalf("precondition: without the undo_code grant, finish %s/%s got %v, want 42501; this proof would be vacuous",
				c.result, m169UCStr(c.code), err)
		}
	}

	if code, msg := m169ApplyMessage(t, pool, m169Body(t)); code != "" {
		t.Fatalf("re-running m169 failed with SQLSTATE %s: %s", code, msg)
	}

	if n, err := m169UCFinish(t, pool, row, "failed", &tampered); err != nil || n != 1 {
		t.Fatalf("GRANT MISSING: after m169 the finish could not write undo_code: n=%d err=%v", n, err)
	}
	if st, code := m169UCRead(t, pool, row); m169UCStr(st) != "failed" || m169UCStr(code) != "snapshot_tampered" {
		t.Fatalf("stored undo_state %s undo_code %s, want failed snapshot_tampered", m169UCStr(st), m169UCStr(code))
	}
	err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m169 target update after the undo_code grant)")
		_, err := tx.Exec(ctx, `UPDATE assistant_ability_requests SET target_post_id = 7 WHERE id = $1`, row.ID)
		return err
	})
	if got, _ := m169Refusal(err); got != "42501" {
		t.Fatalf("m169 widened the grant: UPDATE of target_post_id got %v, want 42501", err)
	}
}

// TestM169UndoCodeChecksAsAppRole: the shipped finish stores either code on
// a failed undo and NULL on every result; it is refused a code outside the
// set, and a code on an undo that did not fail; a bare UPDATE of undo_code
// is refused on a row whose undo is available, in progress, or absent
// (undo_state NULL, where a plain comparison would be NULL and pass).
func TestM169UndoCodeChecksAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m169-uc-chk-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")

	for i, c := range []struct {
		result string
		code   *string
	}{
		{"failed", acprStr("snapshot_tampered")},
		{"failed", acprStr("restore_mismatch")},
		{"failed", nil},
		{"undone", nil},
		{"refused_conflict", nil},
		{"refused_published", nil},
	} {
		row := m169UCUndoing(t, pool, tenant, site, int64(100+i), fmt.Sprintf("uc-ok-%d", i))
		if n, err := m169UCFinish(t, pool, row, c.result, c.code); err != nil || n != 1 {
			t.Fatalf("OVER-FIRE: honest finish %s/%s: n=%d err=%v", c.result, m169UCStr(c.code), n, err)
		}
		if st, code := m169UCRead(t, pool, row); m169UCStr(st) != c.result || m169UCStr(code) != m169UCStr(c.code) {
			t.Fatalf("finish %s/%s stored undo_state %s undo_code %s", c.result, m169UCStr(c.code), m169UCStr(st), m169UCStr(code))
		}
	}

	for i, c := range []struct {
		name, result, code, constraint string
	}{
		{"a code outside the set", "failed", "bogus", m169UndoCodeValueCheck},
		{"an upper-case code", "failed", "SNAPSHOT_TAMPERED", m169UndoCodeValueCheck},
		{"an empty code", "failed", "", m169UndoCodeValueCheck},
		{"a code on an undone undo", "undone", "restore_mismatch", m169UndoCodeStateCheck},
		{"a code on a conflict refusal", "refused_conflict", "snapshot_tampered", m169UndoCodeStateCheck},
		{"a code on a published refusal", "refused_published", "restore_mismatch", m169UndoCodeStateCheck},
	} {
		row := m169UCUndoing(t, pool, tenant, site, int64(200+i), fmt.Sprintf("uc-bad-%d", i))
		code := c.code
		_, err := m169UCFinish(t, pool, row, c.result, &code)
		if got, con := m169Refusal(err); got != "23514" || con != c.constraint {
			t.Fatalf("%s: got %v (code %q, constraint %q), want 23514 on %s", c.name, err, got, con, c.constraint)
		}
		if st, stored := m169UCRead(t, pool, row); m169UCStr(st) != "in_progress" || stored != nil {
			t.Fatalf("%s: after the refusal undo_state %s undo_code %s, want in_progress and NULL", c.name, m169UCStr(st), m169UCStr(stored))
		}
	}

	pending := aarInsert(t, pool, acprSitePrincipal(tenant, site), m169EditParams(tenant, site, uuid.New(), 300, "uc-no-undo"))
	available := m169Applied(t, pool, tenant, site, 301, "uc-available")
	running := m169UCUndoing(t, pool, tenant, site, 302, "uc-running")
	for _, c := range []struct {
		name string
		row  sqlc.AssistantAbilityRequest
		want string
	}{
		{"a request with no undo (undo_state NULL)", pending, "<nil>"},
		{"an applied edit whose undo is available", available, "available"},
		{"an undo in progress", running, "in_progress"},
	} {
		if st, _ := m169UCRead(t, pool, c.row); m169UCStr(st) != c.want {
			t.Fatalf("%s: undo_state %s, want %s", c.name, m169UCStr(st), c.want)
		}
		// Positive control: the same statement writing NULL matches the row,
		// so a refusal below is the CHECK and not row security.
		if n, err := m169UCSetCode(t, pool, c.row, nil); err != nil || n != 1 {
			t.Fatalf("%s: positive control (undo_code = NULL) n=%d err=%v, want one row", c.name, n, err)
		}
		_, err := m169UCSetCode(t, pool, c.row, acprStr("restore_mismatch"))
		if got, con := m169Refusal(err); got != "23514" || con != m169UndoCodeStateCheck {
			t.Fatalf("%s: a bare undo_code update got %v (code %q, constraint %q), want 23514 on %s", c.name, err, got, con, m169UndoCodeStateCheck)
		}
	}
}

// TestM169UndoCodeCountsSeeRowsUnderForceRLS: with each new CHECK dropped
// and one row it refuses stored as wpmgr_app, re-running m169 as
// wpmgr_owner stops on that block's count, naming one row, and not only on
// the VALIDATE that follows it.
func TestM169UndoCodeCountsSeeRowsUnderForceRLS(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		constraint, prefix string
		plant              func(t *testing.T, pool *db.Pool, tenant, site uuid.UUID)
	}{
		{m169UndoCodeValueCheck, "m169: 1 assistant_ability_requests row(s) carry an undo_code outside the m169 set",
			func(t *testing.T, pool *db.Pool, tenant, site uuid.UUID) {
				row := m169UCUndoing(t, pool, tenant, site, 42, "uc-count-value")
				if n, err := m169UCFinish(t, pool, row, "failed", acprStr("bogus")); err != nil || n != 1 {
					t.Fatalf("store a bogus code once its check is gone: n=%d err=%v", n, err)
				}
			}},
		{m169UndoCodeStateCheck, "m169: 1 assistant_ability_requests row(s) carry an undo_code on an undo that did not fail",
			func(t *testing.T, pool *db.Pool, tenant, site uuid.UUID) {
				row := aarInsert(t, pool, acprSitePrincipal(tenant, site), m169EditParams(tenant, site, uuid.New(), 43, "uc-count-state"))
				if n, err := m169UCSetCode(t, pool, row, acprStr("restore_mismatch")); err != nil || n != 1 {
					t.Fatalf("store a code on a request with no undo once its check is gone: n=%d err=%v", n, err)
				}
			}},
	} {
		t.Run(c.constraint, func(t *testing.T) {
			pool := startPostgres(t)
			tenant := seedTenant(t, pool, "m169-uc-count-"+uuid.NewString()[:8])
			site := seedSite(t, pool, tenant, "")

			owner := connectOwner(t, pool)
			_, err := owner.Exec(ctx, `ALTER TABLE assistant_ability_requests DROP CONSTRAINT `+c.constraint)
			owner.Close()
			if err != nil {
				t.Fatalf("drop %s: %v", c.constraint, err)
			}
			c.plant(t, pool, tenant, site)

			code, msg := m169ApplyMessage(t, pool, m169Body(t))
			if code != "23514" || !strings.HasPrefix(msg, c.prefix) {
				t.Fatalf("COUNT SAW NO ROW: re-running m169 gave %q %q; want 23514 from the count: %q", code, msg, c.prefix)
			}
		})
	}
}
