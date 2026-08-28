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
	"github.com/jackc/pgx/v5/pgconn"

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
	// Security-review finding: "err != nil" alone passes for the wrong
	// reason too — a dropped connection, a typo'd column name, or any other
	// unrelated failure would ALSO satisfy it, and the test would still
	// claim m123's policy fired. adr064IsRowSecurityRefusal (same package,
	// adr064_governed_context_rls_integration_test.go) is the exact helper
	// that file's own header explains is necessary because Postgres reports
	// BOTH the RESTRICTIVE policy refusing the row and the append-only
	// REVOKE refusing the privilege as the identical SQLSTATE 42501 — only
	// the message text tells them apart, and repo.go's CreateOrgVersion
	// preserves the underlying *pgconn.PgError via domain.Error.WithCause,
	// which errors.As (inside the helper) walks through Unwrap
	// transparently.
	if !adr064IsRowSecurityRefusal(err) {
		t.Fatalf("got %v, want SQLSTATE 42501 with \"row-level security policy\" in the message "+
			"(m123's RESTRICTIVE INSERT policy specifically, not some other failure)", err)
	}
	if _, gerr := repo.LatestOrgVersion(context.Background(), tenant); !errors.Is(gerr, govcontext.ErrNotFound) {
		t.Errorf("LatestOrgVersion = %v, want ErrNotFound", gerr)
	}
}

// TestADR064S4_RowSecurityRefusalCheck_DoesNotAcceptAnyError is a fast,
// DB-free companion to the test above: it proves adr064IsRowSecurityRefusal
// genuinely discriminates "the RESTRICTIVE policy refused this row" from any
// other failure, rather than accepting err != nil generally. Without this,
// TestADR064S4_PatchOrgContext_SiteScopedCollaboratorRefusedEvenBypassing
// TheAppLayerGate's tightened assertion would itself be an unverified claim.
func TestADR064S4_RowSecurityRefusalCheck_DoesNotAcceptAnyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic error", errors.New("boom"), false},
		{"govcontext.ErrNotFound (a real, different, expected sentinel with no wrapped pgconn.PgError at all)",
			govcontext.ErrNotFound, false},
		{"42501 permission denied (the REVOKE, not a policy)",
			&pgconn.PgError{Code: "42501", Message: `permission denied for table org_context_versions`}, false},
		{"42501 row-level security policy (the actual target)",
			&pgconn.PgError{Code: "42501", Message: `new row violates row-level security policy "org_context_versions_site_scope_insert" for table "org_context_versions"`}, true},
		{"23505 unique violation (a different SQLSTATE entirely)",
			&pgconn.PgError{Code: "23505", Message: `duplicate key value violates unique constraint`}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := adr064IsRowSecurityRefusal(c.err); got != c.want {
				t.Errorf("adr064IsRowSecurityRefusal(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Security-review finding: the widen-check must not over-fire on a
// guidance-only patch. PatchOrgContext never touches any site row, so the
// instant an organisation narrows its policy, every EXISTING site's stored
// restrictions stop restating it — permanently, not for a transaction-width
// window. Comparing that carried-forward, stale value against the org's
// CURRENT restrictions on a write that never touches restrictions at all
// locked every site under a narrowed org out of even a guidance-only edit.
// ---------------------------------------------------------------------------

// TestADR064S4_PatchSiteContext_GuidanceOnlyEditSucceedsAfterOrgNarrows
// reproduces the exact sequence the review used: a site restates the org's
// restriction at write time (so its OWN write-time check passed), the org
// later ADDS a further restriction (a write that, by design, never touches
// any site row), and the site then submits a patch that touches ONLY
// guidance. That write must succeed — it never proposed a restrictions value
// at all.
//
// Confirmed RED against the pre-fix code (service.go's PatchSiteContext
// running checkNoWiden unconditionally, over next.Restrictions regardless of
// whether in.Restrictions was supplied):
//
//	$ go test ./tests/... -run TestADR064S4_PatchSiteContext_GuidanceOnlyEditSucceedsAfterOrgNarrows -v
//	    adr064_s4_govcontext_integration_test.go:332: guidance-only PatchSiteContext failed: this write would remove [b.example.com] from forbidden_domains, which was set by organisation default (layer 2) — a lower layer may narrow or add to a restriction but never remove what a higher layer set
//	--- FAIL: TestADR064S4_PatchSiteContext_GuidanceOnlyEditSucceedsAfterOrgNarrows
//
// Restored (in.Restrictions != nil guard added), it is GREEN:
//
//	$ go test ./tests/... -run TestADR064S4_PatchSiteContext_GuidanceOnlyEditSucceedsAfterOrgNarrows -v
//	--- PASS: TestADR064S4_PatchSiteContext_GuidanceOnlyEditSucceedsAfterOrgNarrows
func TestADR064S4_PatchSiteContext_GuidanceOnlyEditSucceedsAfterOrgNarrows(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	tenant := seedTenant(t, pool, "s4-guideonly-"+uuid.NewString()[:8])
	site := adr064SeedSite(t, admin, tenant)
	user := uuid.New()
	svc, _ := adr064S4Service(pool)
	ctx := adr064S4OrgMemberCtx(tenant, user)

	// Org v1: sets a restriction.
	if _, err := svc.PatchOrgContext(ctx, tenant, govcontext.PatchOrgContextInput{
		BaseVersion:  0,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"a.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user}); err != nil {
		t.Fatalf("seed org v1: %v", err)
	}

	// Site v1: restates it, satisfying the widen-check as of NOW.
	if _, err := svc.PatchSiteContext(ctx, tenant, site, govcontext.PatchSiteContextInput{
		BaseVersion:  0,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"a.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user}); err != nil {
		t.Fatalf("seed site v1: %v", err)
	}

	// Org v2: narrows further. This NEVER touches the site row (verified by
	// the reviewer: PatchOrgContext's body has zero site references) — the
	// site's own stored restrictions are now stale relative to the org's
	// current policy, permanently, until the site itself is next edited.
	if _, err := svc.PatchOrgContext(ctx, tenant, govcontext.PatchOrgContextInput{
		BaseVersion:  1,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"a.example.com", "b.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user}); err != nil {
		t.Fatalf("narrow org to v2: %v", err)
	}

	// Site: a GUIDANCE-ONLY patch. Restrictions is nil — this request never
	// proposes a restrictions value.
	v, err := svc.PatchSiteContext(ctx, tenant, site, govcontext.PatchSiteContextInput{
		BaseVersion: 1,
		Guidance:    &govcontext.GuidanceSet{BrandVoice: "warmer and more direct"},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user})
	if err != nil {
		t.Fatalf("guidance-only PatchSiteContext failed: %v", err)
	}
	if v.Version != 2 {
		t.Errorf("Version = %d, want 2", v.Version)
	}
	if v.Snapshot.Guidance.BrandVoice != "warmer and more direct" {
		t.Errorf("guidance was not applied: %+v", v.Snapshot.Guidance)
	}
	// The site's restrictions carry forward UNCHANGED — this write never
	// claimed to update them, so the stored row still says exactly what it
	// said before (still stale relative to the org's current "b.example.com",
	// which is fine: unionRestrictions, not this row, is what enforcement
	// reads — see model.go's ResolvedContext.Restrictions doc comment).
	if len(v.Snapshot.Restrictions.ForbiddenDomains) != 1 || v.Snapshot.Restrictions.ForbiddenDomains[0] != "a.example.com" {
		t.Errorf("Restrictions = %+v, want unchanged [a.example.com]", v.Snapshot.Restrictions)
	}

	// Enforcement is unaffected by the site's stale row: resolving right now
	// still returns BOTH forbidden domains, because the union re-reads the
	// org's CURRENT row fresh.
	rc, err := svc.GetEffectiveContext(ctx, tenant, site)
	if err != nil {
		t.Fatalf("GetEffectiveContext failed: %v", err)
	}
	got := map[string]bool{}
	for _, d := range rc.Restrictions.ForbiddenDomains {
		got[d] = true
	}
	if !got["a.example.com"] || !got["b.example.com"] {
		t.Errorf("resolved Restrictions = %+v, want BOTH a.example.com and b.example.com regardless of the site's stale row", rc.Restrictions)
	}
}

// TestADR064S4_RestoreSiteContext_OfAStaleVersionIsStillRefused is the
// control the review explicitly asked to be kept green: restoring a site's
// OWN old version, whose stored restrictions are stale relative to the org's
// CURRENT policy, is a genuine proposal to write that stale value back as
// the new current one — and must still be refused, unlike a guidance-only
// patch.
func TestADR064S4_RestoreSiteContext_OfAStaleVersionIsStillRefused(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	tenant := seedTenant(t, pool, "s4-stalerestore-"+uuid.NewString()[:8])
	site := adr064SeedSite(t, admin, tenant)
	user := uuid.New()
	svc, _ := adr064S4Service(pool)
	ctx := adr064S4OrgMemberCtx(tenant, user)

	if _, err := svc.PatchOrgContext(ctx, tenant, govcontext.PatchOrgContextInput{
		BaseVersion:  0,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"a.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user}); err != nil {
		t.Fatalf("seed org v1: %v", err)
	}

	siteV1, err := svc.PatchSiteContext(ctx, tenant, site, govcontext.PatchSiteContextInput{
		BaseVersion:  0,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"a.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user})
	if err != nil {
		t.Fatalf("seed site v1: %v", err)
	}

	if _, err := svc.PatchOrgContext(ctx, tenant, govcontext.PatchOrgContextInput{
		BaseVersion:  1,
		Restrictions: &govcontext.RestrictionSet{ForbiddenDomains: []string{"a.example.com", "b.example.com"}},
	}, govcontext.Actor{Type: govcontext.AuthorUser, ID: user}); err != nil {
		t.Fatalf("narrow org to v2: %v", err)
	}

	// Restoring site v1 EXPLICITLY proposes writing back {a.example.com} as
	// the site's new current restrictions — a genuine widen against the org's
	// now-current {a.example.com, b.example.com} — and must be refused.
	_, err = svc.RestoreSiteContext(ctx, tenant, site, siteV1.ID, govcontext.Actor{Type: govcontext.AuthorUser, ID: user})
	if err == nil {
		t.Fatal("RestoreSiteContext succeeded restoring a stale (widening) version, want a refusal")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "context_widen_forbidden" {
		t.Fatalf("got %v, want context_widen_forbidden", err)
	}
}

// ---------------------------------------------------------------------------
// Fail-closed audit, site scope: CreateSiteVersion's audit hook is line-for-
// line identical to CreateOrgVersion's (repo.go); this is the site-scope
// twin of TestADR064S4_CreateOrgVersion_AuditFailureAbortsTheWholeWrite so
// both write paths, not only the organisation one, have a direct proof.
// ---------------------------------------------------------------------------

func TestADR064S4_CreateSiteVersion_AuditFailureAbortsTheWholeWrite(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	tenant := seedTenant(t, pool, "s4-siteauditfail-"+uuid.NewString()[:8])
	site := adr064SeedSite(t, admin, tenant)
	repo := govcontext.NewRepo(pool)
	ctx := adr064S4OrgMemberCtx(tenant, uuid.New())

	simulated := errors.New("simulated: audit ledger append failed")
	_, err := repo.CreateSiteVersion(ctx, tenant, site, 1, govcontext.CreateSiteVersionInput{
		Snapshot:   govcontext.Snapshot{Guidance: govcontext.GuidanceSet{BrandVoice: "x"}},
		AuthorType: govcontext.AuthorSystem,
		Provenance: govcontext.ProvenanceManual,
	}, func(tx pgx.Tx, versionID uuid.UUID) error {
		return simulated
	})

	if err == nil {
		t.Fatal("CreateSiteVersion succeeded despite the audit hook failing, want an error")
	}
	if !errors.Is(err, simulated) {
		t.Errorf("error = %v, want it to wrap the simulated audit failure", err)
	}
	if _, gerr := repo.LatestSiteVersion(ctx, tenant, site); !errors.Is(gerr, govcontext.ErrNotFound) {
		t.Errorf("LatestSiteVersion = %v, want ErrNotFound — the version row must not have "+
			"committed when its audit entry failed to append", gerr)
	}
}
