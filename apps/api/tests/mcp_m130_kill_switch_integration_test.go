// mcp_m130_kill_switch_integration_test.go: m130's per-tenant kill switch,
// executed against the REAL schema as wpmgr_app.
//
// WHY THIS FILE EXISTS. The MCP transport shipped live with no off switch.
// m130 adds one and folds it into the `authorized` verdict that already runs on
// every request. The claim that makes it a KILL SWITCH rather than a
// registration gate is not decidable by reading the SQL:
//
//	engaging it must refuse the VERY NEXT REQUEST on a connection that was
//	already issued, already authenticated, and is otherwise perfectly valid.
//
// A switch that only blocks new grants while existing tokens keep reading is
// not the control anyone wants at 3am, and the difference between the two is
// invisible in a schema diff. So it is executed here, through mcp.Service --
// the same method the transport calls, the same generated query, the same tx
// helpers. No connection is opened by this file and no GUC is hand-set.
//
// THE ROLE IS LOAD-BEARING AND IS ASSERTED INSIDE THE TRANSACTION UNDER TEST.
// wpmgr_app is NOSUPERUSER NOBYPASSRLS; either privilege would make an RLS
// proof pass vacuously, which is exactly how m112's proofs were green while
// every policy was inert.
//
// FOUR THINGS ARE IN QUESTION, and the first is not optional. Without it the
// other three prove only that a broken query refuses everything:
//
//  1. BASELINE. A tenant with NO explicit assistant state -- both m130 columns
//     NULL, which is every tenant in the fleet the moment m130 applies --
//     authenticates exactly as it does today. This is m130's central safety
//     claim and the one that would be an outage if it were false.
//
//  2. ENGAGED. The same bearer, on the same grant, is refused once the switch
//     is on. Nothing about the grant or the token changed.
//
//  3. RELEASED. The same bearer works again. A switch that cannot be released
//     is a deletion, not a switch, and an incident that ends with a permanently
//     dead surface is not a resolved incident.
//
//  4. ISOLATION. One organisation's switch does not touch another's. The two
//     tenants are exercised in the same test with live bearers on both, so a
//     predicate that ignored tenant_id -- or a switch stored somewhere global
//     -- fails here rather than in production during someone else's incident.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// m130Engage turns the kill switch on for one tenant, through the generated
// query and pool.InTenantTx -- never through a connection this file opened.
// The role is re-asserted inside the write transaction, because a write that
// only succeeds as superuser proves nothing about the operator console.
func m130Engage(t *testing.T, pool interface {
	InTenantTx(context.Context, uuid.UUID, func(pgx.Tx) error) error
}, tenantID uuid.UUID, reason string) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (EngageTenantAssistantKillSwitch)")
		rows, err := sqlc.New(tx).EngageTenantAssistantKillSwitch(context.Background(),
			sqlc.EngageTenantAssistantKillSwitchParams{
				TenantID: tenantID,
				Reason:   &reason,
			})
		if err != nil {
			return err
		}
		if rows != 1 {
			t.Fatalf("EngageTenantAssistantKillSwitch affected %d rows, want 1", rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("engage kill switch for %s: %v", tenantID, err)
	}
}

// m130Release turns it back off. Clears the reason in the same statement --
// tenants_assistant_paused_reason_check refuses a reason without a pause, so a
// release that cleared only the timestamp would fail loudly here.
func m130Release(t *testing.T, pool interface {
	InTenantTx(context.Context, uuid.UUID, func(pgx.Tx) error) error
}, tenantID uuid.UUID) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (ReleaseTenantAssistantKillSwitch)")
		rows, err := sqlc.New(tx).ReleaseTenantAssistantKillSwitch(context.Background(), tenantID)
		if err != nil {
			return err
		}
		if rows != 1 {
			t.Fatalf("ReleaseTenantAssistantKillSwitch affected %d rows, want 1", rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("release kill switch for %s: %v", tenantID, err)
	}
}

// m130State reads the columns back through the generated operator-console
// query, so the test observes what the console will observe.
func m130State(t *testing.T, pool interface {
	InTenantTx(context.Context, uuid.UUID, func(pgx.Tx) error) error
}, tenantID uuid.UUID) sqlc.GetTenantAssistantStateRow {
	t.Helper()
	var out sqlc.GetTenantAssistantStateRow
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = sqlc.New(tx).GetTenantAssistantState(context.Background(), tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("read assistant state for %s: %v", tenantID, err)
	}
	return out
}

func TestMCPKillSwitchStopsInFlightConnectionsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	svc := mcp.NewService(repo)

	tenantA := seedTenant(t, pool, "m130-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m130-b-"+uuid.NewString()[:8])

	if err := pool.InTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (verdict path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	siteA, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantA, URL: "https://m130-a.example.com", Name: "m130-alpha"})
	if err != nil {
		t.Fatalf("create site A: %v", err)
	}
	siteB, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantB, URL: "https://m130-b.example.com", Name: "m130-bravo"})
	if err != nil {
		t.Fatalf("create site B: %v", err)
	}

	grantA, bearerA := s7GrantWithBearer(t, repo, tenantA, "list", []uuid.UUID{siteA.ID})
	_, bearerB := s7GrantWithBearer(t, repo, tenantB, "list", []uuid.UUID{siteB.ID})

	// ---------------------------------------------------------------------
	// 1. BASELINE. Neither tenant has any explicit assistant state. This is
	//    the state of every tenant in the fleet the instant m130 applies, and
	//    it must behave exactly as today.
	// ---------------------------------------------------------------------
	stateA := m130State(t, pool, tenantA)
	if stateA.AssistantPausedAt.Valid {
		t.Fatalf("assistant_paused_at = %v on a fresh tenant, want NULL -- m130 "+
			"must not pause anything by applying", stateA.AssistantPausedAt)
	}
	if stateA.AssistantPausedReason != nil {
		t.Fatalf("assistant_paused_reason = %v on a fresh tenant, want NULL",
			*stateA.AssistantPausedReason)
	}

	if _, err := svc.Authenticate(ctx, bearerA); err != nil {
		t.Fatalf("BASELINE FAILED: Authenticate refused a tenant with no explicit "+
			"assistant state: %v. m130 changed the behaviour of an untouched "+
			"tenant, which is the outage this migration must not cause.", err)
	}
	if _, err := svc.Authenticate(ctx, bearerB); err != nil {
		t.Fatalf("BASELINE FAILED for tenant B: %v", err)
	}

	// ---------------------------------------------------------------------
	// 2. ENGAGED. Nothing about grantA or its token changes -- the grant is
	//    still active, still unexpired, still fully capable. Only the tenant
	//    row moved, and the next request on the ALREADY-ISSUED bearer must be
	//    refused. That is what makes this a kill switch.
	// ---------------------------------------------------------------------
	m130Engage(t, pool, tenantA, "m130 integration proof: simulated incident")

	engaged := m130State(t, pool, tenantA)
	if !engaged.AssistantPausedAt.Valid {
		t.Fatal("assistant_paused_at is NULL after engaging the kill switch")
	}
	if engaged.AssistantPausedReason == nil {
		t.Fatal("assistant_paused_reason is NULL after engaging with a reason")
	}

	if _, err := svc.Authenticate(ctx, bearerA); err == nil {
		t.Fatalf("KILL SWITCH DID NOT FIRE: Authenticate ADMITTED grant %s on an "+
			"organisation whose kill switch is engaged. An operator who engaged "+
			"this switch during an incident believes the surface is stopped and "+
			"it is still reading.", grantA.ID)
	}

	// ---------------------------------------------------------------------
	// 4. ISOLATION, asserted while A is still paused. Tenant B never touched
	//    its switch and must be entirely unaffected. A predicate that dropped
	//    tenant_id, or state stored anywhere global, fails here.
	// ---------------------------------------------------------------------
	if _, err := svc.Authenticate(ctx, bearerB); err != nil {
		t.Fatalf("CROSS-TENANT LEAK: tenant A's kill switch refused tenant B: %v. "+
			"One organisation's incident action must never stop another's "+
			"surface.", err)
	}
	stateB := m130State(t, pool, tenantB)
	if stateB.AssistantPausedAt.Valid {
		t.Fatalf("tenant B's assistant_paused_at = %v after only tenant A was "+
			"paused, want NULL", stateB.AssistantPausedAt)
	}

	// ---------------------------------------------------------------------
	// 3. RELEASED. The same bearer works again, unchanged. A switch that
	//    cannot be released is a deletion.
	// ---------------------------------------------------------------------
	m130Release(t, pool, tenantA)

	released := m130State(t, pool, tenantA)
	if released.AssistantPausedAt.Valid {
		t.Fatalf("assistant_paused_at = %v after release, want NULL",
			released.AssistantPausedAt)
	}
	if released.AssistantPausedReason != nil {
		t.Fatalf("assistant_paused_reason survived the release as %q; the reason "+
			"is part of the pause and must not outlive it",
			*released.AssistantPausedReason)
	}

	if _, err := svc.Authenticate(ctx, bearerA); err != nil {
		t.Fatalf("Authenticate still refuses after the kill switch was released: "+
			"%v. The switch is a deletion, not a switch.", err)
	}
}

// TestMCPKillSwitchReleaseDoesNotEnableADisabledTenantAsAppRole is m130
// DECISION 2 executed: the reason the two facts are two columns.
//
// An organisation that was deliberately never enabled, then paused during an
// incident, then un-paused, must STILL BE NOT-ENABLED afterwards. With a single
// tri-state column "un-pause" means "write on", so the incident response
// silently reverses a configuration decision nobody revisited. This test is the
// one that fails if anyone ever collapses the two columns into one.
func TestMCPKillSwitchReleaseDoesNotEnableADisabledTenantAsAppRole(t *testing.T) {
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "m130-cfg-"+uuid.NewString()[:8])

	// A fresh tenant has never been enabled: assistant_enabled_at IS NULL.
	before := m130State(t, pool, tenant)
	if before.AssistantEnabledAt.Valid {
		t.Fatalf("assistant_enabled_at = %v on a fresh tenant, want NULL -- "+
			"off by default at the tenant level is ADR-061's requirement",
			before.AssistantEnabledAt)
	}

	m130Engage(t, pool, tenant, "m130 integration proof: pause a disabled org")
	m130Release(t, pool, tenant)

	after := m130State(t, pool, tenant)
	if after.AssistantEnabledAt.Valid {
		t.Fatalf("RELEASING THE KILL SWITCH ENABLED THE SURFACE: "+
			"assistant_enabled_at went from NULL to %v across an "+
			"engage/release cycle. Incident response must never reverse a "+
			"configuration decision. This is why m130 uses two columns and "+
			"not one tri-state.", after.AssistantEnabledAt)
	}
	if after.AssistantPausedAt.Valid {
		t.Fatalf("assistant_paused_at = %v after release, want NULL",
			after.AssistantPausedAt)
	}
}
