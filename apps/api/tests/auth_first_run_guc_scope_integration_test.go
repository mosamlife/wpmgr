package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// TestFirstRunOwnership_ProbeHandsBackTheAgentScope proves that the ownership
// probe leaves no elevated scope behind in the transaction that called it.
//
// WHY THIS NEEDS A TEST OF ITS OWN. The probe reads across every tenant, which
// needs app.agent, and it has to do that inside the transaction holding the
// install lock rather than in one of its own. set_config(..., true) is
// TRANSACTION-local, not statement-local, so an unreset GUC stays on for every
// later statement in that transaction — and app.agent is the widest scope in
// this schema, granting cross-tenant write on dozens of tables. Nothing about
// the bootstrap's own writes would have looked wrong, because they all carry a
// server-derived tenant_id; the reach would simply have been there, in the one
// unauthenticated-reachable critical section, for whoever edited it next.
//
// It runs through Pool.InInstallLockTx and calls the real
// auth.OwnershipEstablishedInTx, as the non-superuser wpmgr_app role, so the
// scope under test is the scope production gets.
//
// To watch it go red: delete the reset — the second set_config in
// OwnershipEstablishedInTx (internal/auth/bootstrap_repo.go) — and this reports
// the GUC still on and the cross-tenant write succeeding.
func TestFirstRunOwnership_ProbeHandsBackTheAgentScope(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	// Two tenants, so a cross-tenant write has somewhere to aim.
	scoped := seedTenant(t, pool, "scoped-org")
	other := seedTenant(t, pool, "other-org")

	err := pool.InInstallLockTx(ctx, auth.InstallBootstrapLockKey, func(tx pgx.Tx, scopeTenant func(uuid.UUID) error) error {
		if _, perr := auth.OwnershipEstablishedInTx(ctx, tx); perr != nil {
			t.Fatalf("ownership probe: %v", perr)
		}

		// 1. The GUC itself, read in a LATER statement of the same transaction.
		var agent string
		if serr := tx.QueryRow(ctx, `SELECT current_setting('app.agent', true)`).Scan(&agent); serr != nil {
			t.Fatalf("read app.agent: %v", serr)
		}
		if agent == "on" {
			t.Fatalf("app.agent is still %q in a later statement of the same transaction; the probe did not hand the scope back", agent)
		}

		// 2. The capability that GUC confers, exercised rather than inferred.
		//    Scope the transaction to one tenant, then attempt a write for the
		//    other. Under the tenant policy alone this must be refused; with
		//    app.agent left on, the agent policies would admit it.
		if serr := scopeTenant(scoped); serr != nil {
			t.Fatalf("scope tenant: %v", serr)
		}
		_, werr := tx.Exec(ctx,
			`INSERT INTO sites (tenant_id, name, url, status)
			 VALUES ($1, 'cross-tenant probe', 'https://cross-tenant.example', 'pending')`,
			other,
		)
		if werr == nil {
			t.Fatalf("a cross-tenant write succeeded inside the bootstrap transaction: wrote a sites row for tenant %s while scoped to %s", other, scoped)
		}

		// Refused is the whole assertion; the transaction is aborted by the
		// failed INSERT, so it is rolled back rather than continued.
		return errProbeDone
	})
	if err != nil && err != errProbeDone {
		t.Fatalf("install lock tx: %v", err)
	}

	// Nothing was written either way.
	var sites int
	if qerr := pool.QueryRow(ctx, `SELECT count(*) FROM sites`).Scan(&sites); qerr != nil {
		t.Fatalf("count sites: %v", qerr)
	}
	if sites != 0 {
		t.Fatalf("sites = %d, want 0", sites)
	}
}

// errProbeDone unwinds the transaction once the assertions have run, so the
// helper rolls back rather than committing a transaction whose only purpose was
// to be inspected.
var errProbeDone = errSentinel("probe complete")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// compile-time guard: the test above depends on the pool exposing the install
// lock helper, which is what puts the probe inside the critical section.
var _ = func(p *db.Pool) {}
