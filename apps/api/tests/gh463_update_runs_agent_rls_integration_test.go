// gh463_update_runs_agent_rls_integration_test.go — GH #463 Phase 0.
//
// These tests are the entire justification for shipping m118 alone and before
// any dispatcher code exists. They assert, against a real Postgres, that the
// cross-tenant deferred-dispatch scan can both SEE and CLAIM a due run.
//
// WITHOUT m118 THEY FAIL BY RETURNING ZERO ROWS AND NO ERROR. That is the
// failure mode worth the whole phase: update_runs carried only
// update_runs_tenant_isolation (m3), so under FORCE ROW LEVEL SECURITY a
// transaction with app.agent='on' and app.tenant_id UNSET satisfied no policy
// at all, and Postgres answered "no rows" rather than raising. A dispatcher
// built against that database would log "0 due runs" on every tick forever and
// look like it worked. The same defect shipped twice before, as m84/#96
// (backup_schedules) and m89/#131 (update_tasks — this table's sibling).
//
// Every assertion reaches the database on the pool startPostgres hands back,
// which connects as the NON-superuser, NOBYPASSRLS wpmgr_app role — the role
// every real install runs as — and every transaction goes through the real
// db.Pool wrappers (InAgentTx / InTenantTx) that production uses, so the
// policies under test are live rather than inert. A test that opened its own
// connection, or that connected as the container superuser, would pass against
// the broken schema and prove nothing.
package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// seedScheduledRun inserts a due 'scheduled' run for one tenant through the
// REAL tenant wrapper, so update_runs_tenant_isolation's WITH CHECK applies to
// the insert exactly as it would for an operator-created run. There is no repo
// method for a deferred run yet — creating one is Phase 1's Go work, which by
// design does not exist while this migration ships.
func seedScheduledRun(t *testing.T, pool *db.Pool, tenantID uuid.UUID, scheduledAt time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO update_runs (tenant_id, status, dry_run, scheduled_at)
			VALUES ($1, 'scheduled', false, $2)
			RETURNING id`, tenantID, scheduledAt).Scan(&id)
	}); err != nil {
		t.Fatalf("seed scheduled run: %v", err)
	}
	return id
}

// TestGH463_Phase0_AgentTxSeesAndClaimsDueRunCrossTenant is the core proof.
//
// It walks the three database operations the #463 dispatcher will perform, in
// order, all under InAgentTx with NO tenant GUC set — the plain read, the
// locking read, and the claim. Each is a separate assertion because each fails
// differently without the policy, and the second and third are the ones a
// FOR SELECT policy would still break.
func TestGH463_Phase0_AgentTxSeesAndClaimsDueRunCrossTenant(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "gh463-tenant-a")
	tenantB := seedTenant(t, pool, "gh463-tenant-b")

	due := time.Now().Add(-time.Minute)
	runA := seedScheduledRun(t, pool, tenantA, due)
	runB := seedScheduledRun(t, pool, tenantB, due)

	// A run that is not yet due, to prove the scan is selective and not simply
	// returning everything once the policy admits rows.
	notYet := seedScheduledRun(t, pool, tenantA, time.Now().Add(time.Hour))

	// (1) THE PLAIN CROSS-TENANT READ. No tenant_id in the WHERE and no
	// app.tenant_id in the session: this is admitted only by update_runs_agent.
	var seen []uuid.UUID
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id FROM update_runs
			WHERE status = 'scheduled' AND scheduled_at <= now()
			ORDER BY scheduled_at`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			seen = append(seen, id)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("cross-tenant due scan: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("cross-tenant due scan returned %d rows, want 2 (runs %s and %s, "+
			"one per tenant). Zero rows with no error is the m84/#96 + m89/#131 "+
			"failure: update_runs has no app.agent policy, so FORCE ROW LEVEL "+
			"SECURITY silently admits nothing. Got: %v",
			len(seen), runA, runB, seen)
	}
	for _, id := range seen {
		if id == notYet {
			t.Fatalf("due scan returned the not-yet-due run %s", notYet)
		}
	}

	// (2) THE LOCKING READ. This is the shape every claim in this codebase
	// uses, and it is the half that surprises people: PostgreSQL applies the
	// UPDATE policy to SELECT ... FOR UPDATE as well as the SELECT policy,
	// because taking a row lock declares intent to write. A FOR SELECT-only
	// agent policy would make THIS return zero rows while step (1) above still
	// passed — precisely how Issue #96 stopped every backup schedule from
	// advancing while the same query worked by hand in psql.
	var locked []uuid.UUID
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id FROM update_runs
			WHERE status = 'scheduled' AND scheduled_at <= now()
			ORDER BY scheduled_at
			FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			locked = append(locked, id)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("cross-tenant locking scan: %v", err)
	}
	if len(locked) != 2 {
		t.Fatalf("SELECT ... FOR UPDATE returned %d rows, want 2. If the plain "+
			"read above passed and this did not, the agent policy is FOR SELECT "+
			"where it must be FOR ALL (Issue #96)", len(locked))
	}

	// (3) THE CLAIM. 'scheduled' -> 'dispatching', cross-tenant. This needs the
	// UPDATE policy's USING to admit the old row AND the WITH CHECK to admit
	// the new one. Without m118 this reports 0 rows affected and raises
	// nothing, leaving every run at 'scheduled' forever.
	var claimed int64
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE update_runs
			   SET status = 'dispatching', updated_at = now()
			 WHERE status = 'scheduled' AND scheduled_at <= now()`)
		if err != nil {
			return err
		}
		claimed = tag.RowsAffected()
		return nil
	}); err != nil {
		t.Fatalf("cross-tenant claim: %v", err)
	}
	if claimed != 2 {
		t.Fatalf("claim affected %d rows, want 2. Zero-rows-affected with no "+
			"error is the silent failure this phase exists to prevent", claimed)
	}

	// The claim must have COMMITTED, and must be visible to the owning tenant.
	// Reading it back through InTenantTx also proves the agent path did not
	// somehow write a row the tenant path can no longer see.
	for _, tc := range []struct {
		tenantID uuid.UUID
		runID    uuid.UUID
	}{{tenantA, runA}, {tenantB, runB}} {
		var status string
		if err := pool.InTenantTx(ctx, tc.tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT status FROM update_runs WHERE id = $1`, tc.runID).Scan(&status)
		}); err != nil {
			t.Fatalf("read back run %s: %v", tc.runID, err)
		}
		if status != "dispatching" {
			t.Fatalf("run %s status = %q, want \"dispatching\"", tc.runID, status)
		}
	}
}

// TestGH463_Phase0_WithoutAgentPolicyTheScanIsSilentlyEmpty plants the real
// failure and watches it go red, then restores the policy and watches it go
// green — in one test, against one container, through the same code path.
//
// Dropping update_runs_agent returns the table to EXACTLY its pre-m118 state:
// FORCE ROW LEVEL SECURITY with update_runs_tenant_isolation as its only
// policy, which is what m3 shipped and what origin/main carried until this
// commit. So the first half of this test is a measurement of the bug, not a
// simulation of it.
//
// The assertion that matters is NOT that the scan fails. It is that it does
// NOT fail: err is nil, no SQLSTATE, no log line, and the answer is simply
// "no rows" and "0 rows affected". A dispatcher cannot tell that apart from a
// quiet fleet, which is why this defect has now shipped three times here.
func TestGH463_Phase0_WithoutAgentPolicyTheScanIsSilentlyEmpty(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "gh463-red-a")
	runA := seedScheduledRun(t, pool, tenantA, time.Now().Add(-time.Minute))

	admin := connectAdmin(t, pool)
	defer admin.Close()

	// --- RED: reproduce the pre-m118 schema -------------------------------
	if _, err := admin.Exec(ctx, `DROP POLICY update_runs_agent ON update_runs`); err != nil {
		t.Fatalf("drop agent policy (is m118 applied?): %v", err)
	}

	rows, claimed, err := agentDueScanAndClaim(ctx, pool)
	if err != nil {
		t.Fatalf("pre-m118 scan returned an ERROR (%v). The documented failure is "+
			"a silent empty result, not an error; if Postgres now raises here, "+
			"this test's premise has changed and the migration's rationale needs "+
			"re-reading", err)
	}
	if rows != 0 || claimed != 0 {
		t.Fatalf("pre-m118 scan saw %d rows and claimed %d, want 0 and 0. The "+
			"table should be unreadable under app.agent with only "+
			"update_runs_tenant_isolation present", rows, claimed)
	}
	t.Logf("RED confirmed: with only update_runs_tenant_isolation, the "+
		"cross-tenant scan returned %d rows and claimed %d, err=%v — run %s is "+
		"invisible and unclaimable, silently", rows, claimed, err, runA)

	// --- GREEN: restore exactly what m118 creates -------------------------
	if _, err := admin.Exec(ctx, `
		CREATE POLICY update_runs_agent ON update_runs
			FOR ALL
			USING (current_setting('app.agent', true) = 'on')
			WITH CHECK (current_setting('app.agent', true) = 'on')`); err != nil {
		t.Fatalf("recreate agent policy: %v", err)
	}

	rows, claimed, err = agentDueScanAndClaim(ctx, pool)
	if err != nil {
		t.Fatalf("post-m118 scan: %v", err)
	}
	if rows != 1 || claimed != 1 {
		t.Fatalf("post-m118 scan saw %d rows and claimed %d, want 1 and 1", rows, claimed)
	}
	t.Logf("GREEN confirmed: with update_runs_agent present, the same scan saw "+
		"%d row and claimed %d", rows, claimed)
}

// TestGH463_Phase0_ForSelectPolicyStillBreaksTheClaim is the second half of the
// FOR ALL decision, proved rather than argued. It installs the policy a
// reasonable person would write — FOR SELECT, because "the dispatcher scans" —
// and shows the read succeeding while the claim silently matches nothing.
//
// This is Issue #96 reproduced on this table. Without this test, "FOR ALL not
// FOR SELECT" is a claim in a migration comment that nobody has ever seen fail.
func TestGH463_Phase0_ForSelectPolicyStillBreaksTheClaim(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "gh463-forselect-a")
	seedScheduledRun(t, pool, tenantA, time.Now().Add(-time.Minute))

	admin := connectAdmin(t, pool)
	defer admin.Close()

	if _, err := admin.Exec(ctx, `DROP POLICY update_runs_agent ON update_runs`); err != nil {
		t.Fatalf("drop agent policy: %v", err)
	}
	if _, err := admin.Exec(ctx, `
		CREATE POLICY update_runs_agent ON update_runs
			FOR SELECT
			USING (current_setting('app.agent', true) = 'on')`); err != nil {
		t.Fatalf("create FOR SELECT policy: %v", err)
	}

	rows, claimed, err := agentDueScanAndClaim(ctx, pool)
	if err != nil {
		t.Fatalf("FOR SELECT scan: %v", err)
	}
	if rows != 1 {
		t.Fatalf("FOR SELECT plain read saw %d rows, want 1 — the read half "+
			"should work, that is what makes this bug so hard to spot", rows)
	}
	if claimed != 0 {
		t.Fatalf("FOR SELECT policy claimed %d rows, want 0. If a read-only "+
			"policy can now claim, PostgreSQL's behaviour has changed and the "+
			"FOR ALL rationale in m118 must be revisited", claimed)
	}
	t.Logf("Issue #96 reproduced on update_runs: a FOR SELECT agent policy read "+
		"%d row but claimed %d, with err=%v. The read works, the write silently "+
		"matches nothing. This is why m118's policy is FOR ALL", rows, claimed, err)

	// And the locking read — the shape the real claim uses — returns nothing at
	// all under the same FOR SELECT policy, even though the plain read above
	// returned a row.
	var lockedCount int
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT id FROM update_runs
				 WHERE status = 'scheduled' AND scheduled_at <= now()
				 FOR UPDATE SKIP LOCKED
			) s`).Scan(&lockedCount)
	}); err != nil {
		t.Fatalf("FOR SELECT locking read: %v", err)
	}
	if lockedCount != 0 {
		t.Fatalf("SELECT ... FOR UPDATE under a FOR SELECT policy returned %d "+
			"rows, want 0", lockedCount)
	}
	t.Logf("and SELECT ... FOR UPDATE returned %d rows under the same policy "+
		"that let the plain SELECT through — the counter-intuitive half of #96",
		lockedCount)
}

// agentDueScanAndClaim performs the two operations the #463 dispatcher will
// perform, in one InAgentTx, and reports what each saw. It returns an error
// only if Postgres actually raised one — the whole point is to distinguish
// "refused" from "returned nothing".
func agentDueScanAndClaim(ctx context.Context, pool *db.Pool) (seen int, claimed int64, err error) {
	err = pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, `
			SELECT count(*) FROM update_runs
			 WHERE status = 'scheduled' AND scheduled_at <= now()`).Scan(&seen); e != nil {
			return e
		}
		tag, e := tx.Exec(ctx, `
			UPDATE update_runs
			   SET status = 'dispatching', updated_at = now()
			 WHERE status = 'scheduled' AND scheduled_at <= now()`)
		if e != nil {
			return e
		}
		claimed = tag.RowsAffected()
		return nil
	})
	return seen, claimed, err
}

// TestGH463_Phase0_AgentPolicyDoesNotWidenTenantIsolation is the over-fire
// guard. update_runs_agent is PERMISSIVE, and permissive policies are OR'd, so
// a policy written even slightly wrong here would admit every tenant's runs to
// every operator request. It must admit nothing outside InAgentTx.
//
// The load-bearing detail is that the GUC is compared against the literal 'on'
// and that current_setting('app.agent', true) returns NULL — not 'off', not
// '' — when the GUC was never set, so the comparison is NULL and the row is
// not admitted.
func TestGH463_Phase0_AgentPolicyDoesNotWidenTenantIsolation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "gh463-widen-a")
	tenantB := seedTenant(t, pool, "gh463-widen-b")
	runA := seedScheduledRun(t, pool, tenantA, time.Now().Add(-time.Minute))

	// Tenant B, on the ordinary operator path, must not see tenant A's run —
	// neither by unfiltered enumeration nor by direct id.
	var n int
	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM update_runs`).Scan(&n)
	}); err != nil {
		t.Fatalf("tenant B enumerate: %v", err)
	}
	if n != 0 {
		t.Fatalf("tenant B saw %d update_runs rows, want 0: the agent policy has "+
			"widened the operator path", n)
	}

	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM update_runs WHERE id = $1`, runA).Scan(&n)
	}); err != nil {
		t.Fatalf("tenant B fetch by id: %v", err)
	}
	if n != 0 {
		t.Fatalf("tenant B fetched tenant A's run %s by id", runA)
	}

	// And tenant B must not be able to CLAIM tenant A's run either — the
	// WITH CHECK half. This must affect zero rows.
	var affected int64
	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE update_runs SET status = 'dispatching' WHERE id = $1`, runA)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	}); err != nil {
		t.Fatalf("tenant B claim attempt: %v", err)
	}
	if affected != 0 {
		t.Fatalf("tenant B claimed %d of tenant A's runs, want 0", affected)
	}

	// Tenant A still sees its own run, untouched. A guard that blocked the
	// legitimate path too would be switched off, and then it would guard
	// nothing.
	var status string
	if err := pool.InTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM update_runs WHERE id = $1`, runA).Scan(&status)
	}); err != nil {
		t.Fatalf("tenant A read own run: %v", err)
	}
	if status != "scheduled" {
		t.Fatalf("tenant A's run status = %q, want \"scheduled\"", status)
	}
}

// TestGH463_Phase0_DueIndexExistsWithExpectedPredicate asserts the partial
// index m118 creates is present and partial on the status the dispatcher
// filters by. Presence is worth asserting on its own: the index is created
// while 'scheduled' matches zero rows, so nothing else in the suite would
// notice if it silently failed to be created, and the dispatcher would ship
// against a sequential scan of the entire run history on every tick.
func TestGH463_Phase0_DueIndexExistsWithExpectedPredicate(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	var indexdef string
	err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		 WHERE schemaname = 'public'
		   AND tablename  = 'update_runs'
		   AND indexname  = 'update_runs_due_idx'`).Scan(&indexdef)
	if err != nil {
		t.Fatalf("update_runs_due_idx not found (m118 did not apply?): %v", err)
	}
	for _, want := range []string{"scheduled_at", "status", "'scheduled'"} {
		if !strings.Contains(indexdef, want) {
			t.Fatalf("update_runs_due_idx definition %q does not mention %q", indexdef, want)
		}
	}
}
