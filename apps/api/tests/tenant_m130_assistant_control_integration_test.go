// tenant_m130_assistant_control_integration_test.go: the m130 assistant kill
// switch driven THROUGH tenant.Service, against the real schema, RLS and
// application role.
//
// WHY THIS FILE EXISTS AND WHY THE UNIT SUITE IS NOT ENOUGH. internal/tenant's
// unit tests drive a fake repo, and a fake repo can only fail in the ways it
// was built to fail. Two of its assertions — that neither verb writes
// assistant_enabled_at — were shown to be UNABLE TO FIRE: fakeRepo has no path
// that writes EnabledAt and tenant.Repo exposes no enablement method, so an
// extra `UPDATE tenants SET assistant_enabled_at = now()` planted inside the
// real ResumeAssistant transaction left both guards GREEN. This file is where
// that mutation goes red, because it reads the actual column back off the
// actual row.
//
// It also answers the question no unit test can reach: that the column
// tenant.Service.PauseAssistant WRITES is the column the assistant request path
// READS, proven by authenticating a live bearer either side of the pause. The
// role is asserted and printed from inside the transaction under test —
// wpmgr_app, NOSUPERUSER, NOBYPASSRLS.
//
// mcp_m130_kill_switch_integration_test.go is the sibling that proves the same
// enforcement engaged via raw sqlc; this one proves the OPERATOR PATH reaches
// it.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

func TestM130PauseThroughServiceStopsTheNextRequestAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	mcpRepo := mcp.NewRepo(pool)
	mcpSvc := mcp.NewService(mcpRepo)
	siteRepo := site.NewRepo(pool)

	tenantSvc := tenant.NewService(tenant.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	orgA := seedTenant(t, pool, "m130ctl-a-"+uuid.NewString()[:8])
	orgB := seedTenant(t, pool, "m130ctl-b-"+uuid.NewString()[:8])

	ownerA := domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(),
		TenantID: orgA, Role: "owner", Scope: domain.ScopeOrg,
	}
	ownerB := domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(),
		TenantID: orgB, Role: "owner", Scope: domain.ScopeOrg,
	}

	// Prove the role the whole test runs as, from inside the same helper the
	// new repo uses. wpmgr_app is NOSUPERUSER NOBYPASSRLS.
	if err := pool.InTenantTxAsUser(ctx, orgA, ownerA.UserID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTxAsUser (tenant.pgRepo write path)")
		return nil
	}); err != nil {
		t.Fatalf("open InTenantTxAsUser: %v", err)
	}

	siteA, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: orgA, URL: "https://m130ctl-a.example.com", Name: "m130ctl-a"})
	if err != nil {
		t.Fatalf("create site A: %v", err)
	}
	siteB, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: orgB, URL: "https://m130ctl-b.example.com", Name: "m130ctl-b"})
	if err != nil {
		t.Fatalf("create site B: %v", err)
	}

	grantA, bearerA := s7GrantWithBearer(t, mcpRepo, orgA, "list", []uuid.UUID{siteA.ID})
	_, bearerB := s7GrantWithBearer(t, mcpRepo, orgB, "list", []uuid.UUID{siteB.ID})

	// 1. BASELINE.
	if _, err := mcpSvc.Authenticate(ctx, bearerA); err != nil {
		t.Fatalf("BASELINE: bearer A refused before any pause: %v", err)
	}
	if _, err := mcpSvc.Authenticate(ctx, bearerB); err != nil {
		t.Fatalf("BASELINE: bearer B refused before any pause: %v", err)
	}

	// 2. CROSS-TENANT, EXECUTED AGAINST A REAL ROW. Owner of A aims the switch
	//    at B. `tenants` has no RLS, so nothing downstream would stop this.
	_, xerr := tenantSvc.PauseAssistant(ctx, ownerA, orgB, tenant.PauseInput{Reason: "aimed at another org"}, rec)
	de, ok := domain.AsDomain(xerr)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("cross-tenant pause was not refused as NotFound: %v", xerr)
	}
	if _, err := mcpSvc.Authenticate(ctx, bearerB); err != nil {
		t.Fatalf("CROSS-TENANT KILL SWITCH: org B's assistant is refusing after org A's "+
			"owner aimed the pause at it: %v", err)
	}
	stB := m130State(t, pool, orgB)
	if stB.AssistantPausedAt.Valid {
		t.Fatalf("CROSS-TENANT KILL SWITCH: org B's assistant_paused_at is %v", stB.AssistantPausedAt)
	}

	// 3. IMMEDIACY. Pause org A THROUGH THE NEW SERVICE, then the very next
	//    request on the already-issued bearer must be refused.
	stA, perr := tenantSvc.PauseAssistant(ctx, ownerA, orgA, tenant.PauseInput{Reason: "m130 control proof"}, rec)
	if perr != nil {
		t.Fatalf("PauseAssistant: %v", perr)
	}
	if !stA.Paused() {
		t.Fatal("PauseAssistant returned a state that is not paused")
	}
	if _, err := mcpSvc.Authenticate(ctx, bearerA); err == nil {
		t.Fatalf("KILL SWITCH DID NOT FIRE THROUGH THE SERVICE: grant %s still authenticates "+
			"after tenant.Service.PauseAssistant committed. The column the service writes is "+
			"not the column the verdict reads.", grantA.ID)
	} else {
		t.Logf("engaged via tenant.Service.PauseAssistant; next request refused with: %v", err)
	}
	if _, err := mcpSvc.Authenticate(ctx, bearerB); err != nil {
		t.Fatalf("ISOLATION: org A's pause refused org B: %v", err)
	}

	// The audit entry shares the write transaction and must be there.
	entries, aerr := rec.List(ctx, orgA, 50, 0)
	if aerr != nil {
		t.Fatalf("list audit: %v", aerr)
	}
	var paused, resumed int
	for _, e := range entries {
		switch e.Action {
		case "tenant.assistant.paused":
			paused++
		case "tenant.assistant.resumed":
			resumed++
		}
	}
	if paused != 1 {
		t.Fatalf("want 1 tenant.assistant.paused audit entry, got %d", paused)
	}
	if resumed != 0 {
		t.Fatalf("want 0 resume entries before any resume, got %d", resumed)
	}

	// 4. CROSS-TENANT RESUME during A's incident: owner of B must not lift it.
	_, rxerr := tenantSvc.ResumeAssistant(ctx, ownerB, orgA, rec)
	if de, ok := domain.AsDomain(rxerr); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("cross-tenant resume was not refused as NotFound: %v", rxerr)
	}
	if _, err := mcpSvc.Authenticate(ctx, bearerA); err == nil {
		t.Fatal("CROSS-TENANT RESUME: org B's owner lifted org A's incident stop")
	}

	// 5. RELEASE through the service; the same bearer works again, and
	//    assistant_enabled_at was never written by either verb.
	stA2, rerr := tenantSvc.ResumeAssistant(ctx, ownerA, orgA, rec)
	if rerr != nil {
		t.Fatalf("ResumeAssistant: %v", rerr)
	}
	if stA2.Paused() {
		t.Fatal("ResumeAssistant returned a state that is still paused")
	}
	if stA2.EnabledAt != nil {
		t.Fatal("RESUME ENABLED A DISABLED ORG: assistant_enabled_at is set after pause+resume")
	}
	rowA := m130State(t, pool, orgA)
	if rowA.AssistantEnabledAt.Valid {
		t.Fatalf("assistant_enabled_at was written by the kill-switch verbs: %v", rowA.AssistantEnabledAt)
	}
	if rowA.AssistantPausedReason != nil {
		t.Fatalf("the reason outlived the pause: %q", *rowA.AssistantPausedReason)
	}
	if _, err := mcpSvc.Authenticate(ctx, bearerA); err != nil {
		t.Fatalf("bearer A still refused after ResumeAssistant: %v", err)
	}

	// 6. A resume that released nothing writes no incident-end marker.
	if _, err := tenantSvc.ResumeAssistant(ctx, ownerA, orgA, rec); err != nil {
		t.Fatalf("second resume should be a successful no-op: %v", err)
	}
	entries2, aerr2 := rec.List(ctx, orgA, 50, 0)
	if aerr2 != nil {
		t.Fatalf("list audit 2: %v", aerr2)
	}
	resumed = 0
	for _, e := range entries2 {
		if e.Action == "tenant.assistant.resumed" {
			resumed++
		}
	}
	if resumed != 1 {
		t.Fatalf("want exactly 1 tenant.assistant.resumed entry after one real release "+
			"and one no-op, got %d", resumed)
	}
}

// m130FailingRecorder satisfies tenant.AuditRecorder and always fails, so the
// fail-closed rollback is proved against the REAL transaction rather than
// against a fake repo that models it.
type m130FailingRecorder struct{ calls int }

func (f *m130FailingRecorder) RecordInTx(context.Context, pgx.Tx, audit.Event) (audit.Entry, error) {
	f.calls++
	return audit.Entry{}, errors.New("m130 control proof: audit chain unavailable")
}

func TestM130AuditFailureRollsBackTheRealPauseAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	mcpRepo := mcp.NewRepo(pool)
	mcpSvc := mcp.NewService(mcpRepo)
	siteRepo := site.NewRepo(pool)
	tenantSvc := tenant.NewService(tenant.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})

	org := seedTenant(t, pool, "m130ctl-rb-"+uuid.NewString()[:8])
	owner := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(),
		TenantID: org, Role: "owner", Scope: domain.ScopeOrg}

	s, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: org, URL: "https://m130ctl-rb.example.com", Name: "m130ctl-rb"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	_, bearer := s7GrantWithBearer(t, mcpRepo, org, "list", []uuid.UUID{s.ID})
	if _, err := mcpSvc.Authenticate(ctx, bearer); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	bad := &m130FailingRecorder{}
	if _, err := tenantSvc.PauseAssistant(ctx, owner, org,
		tenant.PauseInput{Reason: "rollback proof"}, bad); err == nil {
		t.Fatal("PauseAssistant SUCCEEDED while its audit append failed")
	}
	if bad.calls != 1 {
		t.Fatalf("the audit hook ran %d times, want 1 — the rollback proof would be vacuous", bad.calls)
	}

	// The row must be untouched and the surface must still be RUNNING.
	row := m130State(t, pool, org)
	if row.AssistantPausedAt.Valid {
		t.Fatalf("THE PAUSE COMMITTED WITHOUT ITS AUDIT ENTRY: assistant_paused_at=%v", row.AssistantPausedAt)
	}
	if _, err := mcpSvc.Authenticate(ctx, bearer); err != nil {
		t.Fatalf("the surface is stopped after a rolled-back pause: %v", err)
	}
	t.Log("audit append failed -> the UPDATE rolled back; assistant_paused_at is NULL and the bearer still authenticates")
}
