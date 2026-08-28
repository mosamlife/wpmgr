// adr064_s4_govcontext_integration_test.go: ADR-064 slice S4's write path
// (internal/govcontext), asserted against the REAL schema, RLS and application
// role — the same "no second connection, no hand-set GUC" discipline
// adr064_governed_context_rls_integration_test.go already documents for m122/
// m123. That file proves the DATABASE'S policies; this file proves the GO
// LAYER built on top of them: the never-widen check actually runs before a
// write lands, and the fail-closed audit append actually makes the version
// insert and its ledger entry commit-or-fail together — neither of which m122/
// m123's own file could exercise, because S4 did not exist yet when it was
// written (see that file's header).
//
// Every call below goes through govcontext.Service / govcontext.Repo exactly
// as the real handler does — no raw SQL against the two context tables, no
// second pool, no hand-set GUC. A site-scoped principal is placed in context
// via domain.WithPrincipal, and Repo.scopedTenantTx (repo.go) is what decides,
// from that principal, whether the write runs under InTenantTx or
// InScopedTenantTx — the same dispatch email.Repo documents and this codebase
// has shipped the email-domain version of this exact bug from before.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
)

// adr064S4Actors builds a govcontext.Service wired the same way
// cmd/wpmgr/main.go wires it (Resolver.Store == the same Repo the Service
// writes through), against pool.
func adr064S4Service(pool *db.Pool) (*govcontext.Service, *govcontext.Repo) {
	repo := govcontext.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	resolver := &govcontext.Resolver{Store: repo}
	return govcontext.NewService(repo, rec, resolver), repo
}

func adr064S4OrgMemberCtx(tenant, userID uuid.UUID) context.Context {
	return domain.WithPrincipal(context.Background(), domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenant,
	})
}

func adr064S4CollaboratorCtx(tenant, userID uuid.UUID, sites []uuid.UUID) context.Context {
	return domain.WithPrincipal(context.Background(), domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenant,
		Scope: domain.ScopeSite, AllowedSiteIDs: sites,
	})
}

// ---------------------------------------------------------------------------
// Property 1 (end-to-end): a lower layer's write that would widen a higher
// layer's restriction is refused at the write path, and nothing commits.
// ---------------------------------------------------------------------------

func TestADR064S4_PatchSiteContext_WideningOrgPolicy_RefusedAndNothingCommitted(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	tenant := seedTenant(t, pool, "s4-widen-"+uuid.NewString()[:8])
	site := adr064SeedSite(t, admin, tenant)
	user := uuid.New()
	svc, repo := adr064S4Service(pool)

	ctx := adr064S4OrgMemberCtx(tenant, user)

	// The organisation sets a restriction (layer 2).
	if _, err := svc.PatchOrgContext(ctx, tenant, govcontext.PatchOrgContextInput{
		BaseVersion:  0,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"evil.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user}); err != nil {
		t.Fatalf("seed org context: %v", err)
	}

	// A site-level write attempts to drop what the organisation set.
	_, err := svc.PatchSiteContext(ctx, tenant, site, govcontext.PatchSiteContextInput{
		BaseVersion:  0,
		Restrictions: &govcontext.RestrictionSet{}, // empty — drops evil.example.com
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user})

	if err == nil {
		t.Fatal("PatchSiteContext succeeded on a widening write, want a refusal")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("got a non-domain error: %v", err)
	}
	if de.Code != "context_widen_forbidden" {
		t.Errorf("Code = %q, want context_widen_forbidden", de.Code)
	}
	if de.Details["layer"] != 2 {
		t.Errorf("Details[layer] = %v, want 2 (organisation default)", de.Details["layer"])
	}

	// Nothing committed: the site still has no context version at all.
	if _, err := repo.LatestSiteVersion(ctx, tenant, site); !errors.Is(err, govcontext.ErrNotFound) {
		t.Errorf("LatestSiteVersion = %v, want ErrNotFound (the rejected write must not have committed)", err)
	}
}

// TestADR064S4_PatchSiteContext_NarrowingIsAccepted is the honest-cases
// control: a site write that ADDS to what the organisation restricted must
// succeed. A guard that reddens correct work guards nothing.
func TestADR064S4_PatchSiteContext_NarrowingIsAccepted(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	tenant := seedTenant(t, pool, "s4-narrow-"+uuid.NewString()[:8])
	site := adr064SeedSite(t, admin, tenant)
	user := uuid.New()
	svc, _ := adr064S4Service(pool)
	ctx := adr064S4OrgMemberCtx(tenant, user)

	if _, err := svc.PatchOrgContext(ctx, tenant, govcontext.PatchOrgContextInput{
		BaseVersion:  0,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"evil.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user}); err != nil {
		t.Fatalf("seed org context: %v", err)
	}

	v, err := svc.PatchSiteContext(ctx, tenant, site, govcontext.PatchSiteContextInput{
		BaseVersion:  0,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"evil.example.com", "also-evil.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user})
	if err != nil {
		t.Fatalf("PatchSiteContext rejected a non-widening (narrowing) write: %v", err)
	}
	if v.Version != 1 {
		t.Errorf("Version = %d, want 1", v.Version)
	}
}

// ---------------------------------------------------------------------------
// Fail-closed audit (ADR-064 Decision 7): if the audit append fails, the
// version write fails with it and nothing commits.
// ---------------------------------------------------------------------------

// TestADR064S4_CreateOrgVersion_AuditFailureAbortsTheWholeWrite exercises the
// EXACT hook Service.PatchOrgContext / PatchSiteContext wires
// audit.Recorder.RecordInTx into (Repo.CreateOrgVersion's `record` parameter,
// repo.go). RecordInTx is new work — audit.Record is documented best-effort
// (audit.go:498-500) and opens its OWN transaction, so it structurally cannot
// provide this guarantee; RecordInTx (added alongside this slice) instead
// runs inside the CALLER's transaction and returns its error for the caller
// to propagate. This test simulates a RecordInTx-shaped failure (any error
// returned from the hook) and asserts the guarantee Decision 7 requires:
// nothing commits.
//
// Confirmed RED against a neutered CreateOrgVersion (repo.go's `if aerr :=
// record(tx, v.ID); aerr != nil { return aerr }` replaced with a bare
// `record(tx, v.ID)`, dropping the error):
//
//	$ go test ./tests/... -run TestADR064S4_CreateOrgVersion_AuditFailureAbortsTheWholeWrite -v
//	    adr064_s4_govcontext_integration_test.go:169: CreateOrgVersion succeeded despite the audit hook failing, want an error
//	--- FAIL: TestADR064S4_CreateOrgVersion_AuditFailureAbortsTheWholeWrite (2.09s)
//	FAIL
//
// Restored, it is GREEN:
//
//	$ go test ./tests/... -run TestADR064S4_CreateOrgVersion_AuditFailureAbortsTheWholeWrite -v
//	--- PASS: TestADR064S4_CreateOrgVersion_AuditFailureAbortsTheWholeWrite (1.50s)
//	PASS
func TestADR064S4_CreateOrgVersion_AuditFailureAbortsTheWholeWrite(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "s4-auditfail-"+uuid.NewString()[:8])
	repo := govcontext.NewRepo(pool)
	ctx := adr064S4OrgMemberCtx(tenant, uuid.New())

	simulated := errors.New("simulated: audit ledger append failed")
	_, err := repo.CreateOrgVersion(ctx, tenant, 1, govcontext.CreateOrgVersionInput{
		Snapshot:   govcontext.Snapshot{Restrictions: govcontext.RestrictionSet{ForbiddenDomains: []string{"x.example.com"}}},
		AuthorType: govcontext.AuthorSystem,
		Provenance: govcontext.ProvenanceManual,
	}, func(tx pgx.Tx, versionID uuid.UUID) error {
		return simulated
	})

	if err == nil {
		t.Fatal("CreateOrgVersion succeeded despite the audit hook failing, want an error")
	}
	if !errors.Is(err, simulated) {
		t.Errorf("error = %v, want it to wrap the simulated audit failure", err)
	}
	if _, gerr := repo.LatestOrgVersion(ctx, tenant); !errors.Is(gerr, govcontext.ErrNotFound) {
		t.Errorf("LatestOrgVersion = %v, want ErrNotFound — the version row must not have "+
			"committed when its audit entry failed to append", gerr)
	}
}

// TestADR064S4_PatchOrgContext_AppendsARealAuditEntryInTheSameTransaction is
// the positive half: a SUCCESSFUL write really does append a real,
// hash-chained audit_log row, not merely "some hook ran without error".
func TestADR064S4_PatchOrgContext_AppendsARealAuditEntryInTheSameTransaction(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "s4-auditok-"+uuid.NewString()[:8])
	user := uuid.New()
	svc, _ := adr064S4Service(pool)
	ctx := adr064S4OrgMemberCtx(tenant, user)

	v, err := svc.PatchOrgContext(ctx, tenant, govcontext.PatchOrgContextInput{
		BaseVersion: 0,
		Guidance:    &govcontext.GuidanceSet{BrandVoice: "warm and direct"},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user})
	if err != nil {
		t.Fatalf("PatchOrgContext failed: %v", err)
	}

	var n int
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE target_type = 'org_context_version' AND target_id = $1 AND action = 'context.org.patched'`,
			v.ID.String(),
		).Scan(&n)
	}); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if n != 1 {
		t.Errorf("found %d audit_log rows for version %s, want exactly 1", n, v.ID)
	}
}

// ---------------------------------------------------------------------------
// Belt-and-braces: the database itself refuses a site-scoped collaborator's
// attempt to author organisation context, even calling the service directly
// (bypassing the app-layer authz.RequirePermission org-level gate a real HTTP
// request would hit first). This is m123's RESTRICTIVE INSERT policy,
// exercised through Repo.scopedTenantTx's dispatch rather than raw SQL.
// ---------------------------------------------------------------------------

func TestADR064S4_PatchOrgContext_SiteScopedCollaboratorRefusedEvenBypassingTheAppLayerGate(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	tenant := seedTenant(t, pool, "s4-collab-"+uuid.NewString()[:8])
	site := adr064SeedSite(t, admin, tenant)
	repo := govcontext.NewRepo(pool)

	ctx := adr064S4CollaboratorCtx(tenant, uuid.New(), []uuid.UUID{site})

	_, err := repo.CreateOrgVersion(ctx, tenant, 1, govcontext.CreateOrgVersionInput{
		Snapshot:   govcontext.Snapshot{Guidance: govcontext.GuidanceSet{BrandVoice: "attempted takeover"}},
		AuthorType: govcontext.AuthorUser,
		AuthorID:   uuid.New(),
		Provenance: govcontext.ProvenanceManual,
	}, nil)

	if err == nil {
		t.Fatal("a site-scoped collaborator authored an ORGANISATION context version — m123's policy did not fire")
	}
	if _, gerr := repo.LatestOrgVersion(context.Background(), tenant); !errors.Is(gerr, govcontext.ErrNotFound) {
		t.Errorf("LatestOrgVersion = %v, want ErrNotFound", gerr)
	}
}
