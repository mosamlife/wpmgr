// Integration tests for the M16 Phase C1 superadmin billing-admin panel
// (internal/admin's accounts / account-detail / revenue / manual controls).
// Require Docker and skip when it is unavailable (via startPostgres).
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Shared seeding helpers (admin = superuser pool, bypasses RLS; mirrors
// admin_orphan_cleanup_integration_test.go's own convention exactly).
// ---------------------------------------------------------------------------

func seedBackupChunk(t *testing.T, admin *db.Pool, tenantID uuid.UUID, size int64) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO backup_chunks (tenant_id, blake3, s3_key, size) VALUES ($1, $2, $3, $4)`,
		tenantID, uuid.NewString(), "chunks/"+tenantID.String()+"/"+uuid.NewString(), size,
	); err != nil {
		t.Fatalf("seed backup chunk: %v", err)
	}
}

func markSuperadmin(t *testing.T, admin *db.Pool, userID uuid.UUID) {
	t.Helper()
	if _, err := admin.Exec(context.Background(), `UPDATE users SET is_superadmin = true WHERE id = $1`, userID); err != nil {
		t.Fatalf("mark superadmin: %v", err)
	}
}

// newAdminBillingService builds a fully-wired admin.Service with the M16
// Phase C1 billing panel attached, over the given app pool.
func newAdminBillingService(pool *db.Pool, billingSvc *billing.Service, rec *audit.Recorder) *admin.Service {
	adminRepo := admin.NewRepo(pool)
	svc := admin.NewService(adminRepo, nil)
	billingRepo := admin.NewBillingRepo(pool)
	svc.SetBillingPanel(billingRepo, billingSvc, rec, false)
	return svc
}

// lastAuditEntry returns the newest audit_log row for tenantID with the given
// action, or nil if none exists.
func lastAuditEntry(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string) *auditRowT {
	t.Helper()
	var out auditRowT
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT actor_type, actor_id, metadata FROM audit_log
			 WHERE tenant_id = $1 AND action = $2 ORDER BY created_at DESC LIMIT 1`,
			tenantID, action,
		).Scan(&out.ActorType, &out.ActorID, &out.Metadata)
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil
		}
		t.Fatalf("lastAuditEntry: %v", err)
	}
	return &out
}

type auditRowT struct {
	ActorType string
	ActorID   string
	Metadata  []byte
}

// countAdminBillingEvents returns how many billing_events rows exist for
// tenantID with provider='admin' and the given kind.
func countAdminBillingEvents(t *testing.T, pool *db.Pool, tenantID uuid.UUID, kind string) int {
	t.Helper()
	var n int
	err := pool.InAgentTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM billing_events WHERE tenant_id = $1 AND provider = 'admin' AND kind = $2`,
			tenantID, kind,
		).Scan(&n)
	})
	if err != nil {
		t.Fatalf("countAdminBillingEvents: %v", err)
	}
	return n
}

func getPlanOverrides(t *testing.T, pool *db.Pool, tenantID uuid.UUID) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT plan_overrides FROM tenants WHERE id = $1`, tenantID).Scan(&raw); err != nil {
		t.Fatalf("getPlanOverrides: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal plan_overrides: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Accounts-list aggregate correctness.
// ---------------------------------------------------------------------------

func TestAdminBilling_AccountsAggregate_MRRUsageStatus(t *testing.T) {
	app := startPostgres(t)
	adminPool := connectAdmin(t, app)
	ctx := context.Background()

	tenantActive := seedTenant(t, app, "acct-active-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tenantActive, string(billing.TierStarter), "active")
	owner := seedUserRow(t, adminPool, "owner-"+uuid.NewString()[:8]+"@example.com")
	seedMembershipRow(t, adminPool, owner, tenantActive)
	seedSiteRow(t, adminPool, tenantActive)
	seedSiteRow(t, adminPool, tenantActive)
	seedBackupChunk(t, adminPool, tenantActive, 5*1024*1024*1024) // 5 GiB

	tenantFree := seedTenant(t, app, "acct-free-"+uuid.NewString()[:8])
	// tenants default to plan=free/plan_status=none.

	fp := newFakeProvider("fake")
	billingSvc := newTestBillingService(app, fp)
	rec := audit.NewRecorder(app, domain.SystemClock{})
	svc := newAdminBillingService(app, billingSvc, rec)

	resp, err := svc.ListAccounts(ctx, admin.AccountsListOptions{})
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}

	var gotActive, gotFree *admin.AccountListItem
	for i := range resp.Items {
		switch resp.Items[i].TenantID {
		case tenantActive:
			gotActive = &resp.Items[i]
		case tenantFree:
			gotFree = &resp.Items[i]
		}
	}
	if gotActive == nil || gotFree == nil {
		t.Fatalf("expected both seeded tenants in the accounts list, got %d items", len(resp.Items))
	}

	if gotActive.MRRCents != int64(billing.MonthlyPriceCentsForTier(billing.TierStarter)) {
		t.Fatalf("active starter tenant MRRCents = %d, want %d", gotActive.MRRCents, billing.MonthlyPriceCentsForTier(billing.TierStarter))
	}
	if gotActive.SitesUsed != 2 {
		t.Fatalf("active tenant SitesUsed = %d, want 2", gotActive.SitesUsed)
	}
	if gotActive.SitesCap != 10 {
		t.Fatalf("starter tenant SitesCap = %d, want 10", gotActive.SitesCap)
	}
	if gotActive.StorageUsedBytesApprox != 5*1024*1024*1024 {
		t.Fatalf("active tenant StorageUsedBytesApprox = %d, want 5GiB", gotActive.StorageUsedBytesApprox)
	}
	if gotActive.OwnerEmail == "" {
		t.Fatalf("active tenant OwnerEmail is empty, want the seeded owner's email")
	}

	if gotFree.MRRCents != 0 {
		t.Fatalf("free/none tenant MRRCents = %d, want 0", gotFree.MRRCents)
	}
	if gotFree.SitesCap != 3 {
		t.Fatalf("free tenant SitesCap = %d, want 3", gotFree.SitesCap)
	}

	// Tiles must reflect the full instance regardless of filters.
	if resp.Tiles.AccountsTotal < 2 {
		t.Fatalf("Tiles.AccountsTotal = %d, want >= 2", resp.Tiles.AccountsTotal)
	}
	if resp.Tiles.ActiveSubs < 1 {
		t.Fatalf("Tiles.ActiveSubs = %d, want >= 1", resp.Tiles.ActiveSubs)
	}
}

// ---------------------------------------------------------------------------
// Mutations: each writes audit-under-target-tenant + billing_events +
// persists.
// ---------------------------------------------------------------------------

func TestAdminBilling_CompAccount_AuditBillingEventPersist(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "comp-"+uuid.NewString()[:8])

	fp := newFakeProvider("fake")
	billingSvc := newTestBillingService(app, fp)
	rec := audit.NewRecorder(app, domain.SystemClock{})
	svc := newAdminBillingService(app, billingSvc, rec)

	actor := uuid.New()
	if err := svc.CompAccount(ctx, actor, tenant, billing.TierAgency, "sales exception #123"); err != nil {
		t.Fatalf("CompAccount: %v", err)
	}

	plan, status := getTenantPlanStatus(t, app, tenant)
	if plan != string(billing.TierAgency) || status != "comped" {
		t.Fatalf("plan/status = %s/%s, want agency/comped", plan, status)
	}

	entry := lastAuditEntry(t, app, tenant, audit.ActionAdminBillingCompGranted)
	if entry == nil {
		t.Fatalf("no audit entry recorded under the target tenant's chain for %s", audit.ActionAdminBillingCompGranted)
	}
	if entry.ActorType != audit.ActorUser || entry.ActorID != actor.String() {
		t.Fatalf("audit actor = %s/%s, want user/%s (the superadmin's REAL user id)", entry.ActorType, entry.ActorID, actor)
	}
	var meta map[string]any
	if err := json.Unmarshal(entry.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal audit metadata: %v", err)
	}
	if meta["reason"] != "sales exception #123" {
		t.Fatalf("audit metadata reason = %v, want the supplied reason", meta["reason"])
	}

	if n := countAdminBillingEvents(t, app, tenant, "admin.comp.granted"); n != 1 {
		t.Fatalf("admin billing_events count for admin.comp.granted = %d, want 1", n)
	}
}

func TestAdminBilling_CompAccount_ReasonRequired(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "comp-reason-"+uuid.NewString()[:8])
	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))

	err := svc.CompAccount(ctx, uuid.New(), tenant, billing.TierAgency, "")
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("CompAccount with empty reason: want KindValidation, got %v", err)
	}
}

// TestAdminBilling_CompBlocksWebhookMutation proves the admin-driven comp
// path benefits from the SAME webhook immunity Phase B already ships
// (billing.Service.ProcessWebhook's comped-tenant check) — admin.CompAccount
// only ever sets plan_status='comped' via the same tenants row the webhook
// consumer reads, so there is no separate bypass surface.
func TestAdminBilling_CompBlocksWebhookMutation(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "comp-blocks-webhook-"+uuid.NewString()[:8])
	setTenantProvider(t, app, tenant, "fake", "cus_admincomp", "sub_admincomp")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_admincomp"] = billing.Subscription{ID: "sub_admincomp", Status: billing.StatusCanceled}
	billingSvc := newTestBillingService(app, fp)
	svc := newAdminBillingService(app, billingSvc, audit.NewRecorder(app, domain.SystemClock{}))

	if err := svc.CompAccount(ctx, uuid.New(), tenant, billing.TierScale, "loyalty comp"); err != nil {
		t.Fatalf("CompAccount: %v", err)
	}

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_admincomp_1", Type: "customer.subscription.deleted", Kind: "canceled", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_admincomp", ProviderSubscriptionID: "sub_admincomp", OccurredAt: time.Now(),
	})
	if err := billingSvc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}

	plan, status := getTenantPlanStatus(t, app, tenant)
	if plan != string(billing.TierScale) || status != "comped" {
		t.Fatalf("a comped (via admin panel) tenant was mutated by a webhook: plan=%s status=%s, want scale/comped unchanged", plan, status)
	}
}

func TestAdminBilling_RevokeComp_NoLiveSubscriptionFallsBackToFree(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "revoke-comp-"+uuid.NewString()[:8])
	fp := newFakeProvider("fake")
	billingSvc := newTestBillingService(app, fp)
	svc := newAdminBillingService(app, billingSvc, audit.NewRecorder(app, domain.SystemClock{}))

	if err := svc.CompAccount(ctx, uuid.New(), tenant, billing.TierAgency, "temp comp"); err != nil {
		t.Fatalf("CompAccount: %v", err)
	}
	actor := uuid.New()
	if err := svc.RevokeComp(ctx, actor, tenant, "comp period ended"); err != nil {
		t.Fatalf("RevokeComp: %v", err)
	}

	plan, status := getTenantPlanStatus(t, app, tenant)
	if plan != "free" || status != "none" {
		t.Fatalf("plan/status after revoke (no live sub) = %s/%s, want free/none", plan, status)
	}
	if entry := lastAuditEntry(t, app, tenant, audit.ActionAdminBillingCompRevoked); entry == nil {
		t.Fatalf("no audit entry recorded for %s", audit.ActionAdminBillingCompRevoked)
	}
	if n := countAdminBillingEvents(t, app, tenant, "admin.comp.revoked"); n != 1 {
		t.Fatalf("admin billing_events count for admin.comp.revoked = %d, want 1", n)
	}
}

// TestAdminBilling_OverridesAreDeltasAndPersistAcrossPlanChange proves the
// PUT /overrides delta semantics: the requested delta is applied against the
// CURRENT plan's ladder base and persisted as an absolute plan_overrides
// value that a LATER, unrelated plan-column change never resets or clobbers
// (plan_overrides is a separate column nothing else in this codebase touches
// on a plan transition — see billing_service.go's OverrideDeltas doc comment
// for the full design rationale).
func TestAdminBilling_OverridesAreDeltasAndPersistAcrossPlanChange(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "overrides-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tenant, string(billing.TierStarter), "active") // starter base: 10 sites

	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))

	sitesDelta := 5
	actor := uuid.New()
	if err := svc.SetAccountOverrides(ctx, actor, tenant, admin.OverrideDeltas{SitesDelta: &sitesDelta}, "extra capacity for a launch"); err != nil {
		t.Fatalf("SetAccountOverrides: %v", err)
	}

	overrides := getPlanOverrides(t, app, tenant)
	maxSites, ok := overrides["max_sites"].(float64)
	if !ok || int(maxSites) != 15 {
		t.Fatalf("plan_overrides.max_sites = %v, want 15 (starter base 10 + delta 5)", overrides["max_sites"])
	}

	if n := countAdminBillingEvents(t, app, tenant, "admin.override.set"); n != 1 {
		t.Fatalf("admin billing_events count for admin.override.set = %d, want 1", n)
	}

	// An unrelated plan change never resets or auto-clobbers the override.
	setTenantPlan(t, app, tenant, string(billing.TierAgency), "active")
	overridesAfter := getPlanOverrides(t, app, tenant)
	maxSitesAfter, ok := overridesAfter["max_sites"].(float64)
	if !ok || int(maxSitesAfter) != 15 {
		t.Fatalf("plan_overrides.max_sites after plan change = %v, want unchanged 15", overridesAfter["max_sites"])
	}

	// Clearing (delta 0/nil) removes the key entirely.
	zero := 0
	if err := svc.SetAccountOverrides(ctx, actor, tenant, admin.OverrideDeltas{SitesDelta: &zero}, "no longer needed"); err != nil {
		t.Fatalf("SetAccountOverrides (clear): %v", err)
	}
	cleared := getPlanOverrides(t, app, tenant)
	if _, exists := cleared["max_sites"]; exists {
		t.Fatalf("plan_overrides.max_sites still present after a 0-delta clear: %v", cleared)
	}
}

func TestAdminBilling_ExtendGrace_90dClampAndForwardOnly(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "grace-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tenant, string(billing.TierStarter), "past_due")

	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))

	actor := uuid.New()
	requested := time.Now().Add(200 * 24 * time.Hour)
	if err := svc.ExtendGrace(ctx, actor, tenant, requested, "goodwill extension"); err != nil {
		t.Fatalf("ExtendGrace: %v", err)
	}

	var graceUntil time.Time
	if err := app.QueryRow(ctx, `SELECT grace_until FROM tenants WHERE id = $1`, tenant).Scan(&graceUntil); err != nil {
		t.Fatalf("read grace_until: %v", err)
	}
	maxAllowed := time.Now().Add(90 * 24 * time.Hour)
	if graceUntil.After(maxAllowed.Add(time.Minute)) {
		t.Fatalf("grace_until = %v, want clamped to <= ~90 days out (%v)", graceUntil, maxAllowed)
	}

	// Forward-only: a second call with an EARLIER instant than the current
	// grace_until must be rejected.
	earlier := time.Now().Add(10 * 24 * time.Hour)
	err := svc.ExtendGrace(ctx, actor, tenant, earlier, "should be rejected")
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("ExtendGrace backward: want KindValidation, got %v", err)
	}
}

func TestAdminBilling_SuspendAndRestore(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "suspend-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tenant, string(billing.TierStarter), "active")

	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))
	actor := uuid.New()

	if err := svc.SuspendAccount(ctx, actor, tenant, "unpaid invoice escalation"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	var suspendedAt *time.Time
	if err := app.QueryRow(ctx, `SELECT suspended_at FROM tenants WHERE id = $1`, tenant).Scan(&suspendedAt); err != nil {
		t.Fatalf("read suspended_at: %v", err)
	}
	if suspendedAt == nil {
		t.Fatalf("suspended_at is NULL after SuspendAccount")
	}
	// plan/plan_status are untouched by suspension (a separate field).
	plan, status := getTenantPlanStatus(t, app, tenant)
	if plan != string(billing.TierStarter) || status != "active" {
		t.Fatalf("plan/status changed by suspend: %s/%s, want starter/active unchanged", plan, status)
	}

	if err := svc.RestoreAccount(ctx, actor, tenant, "invoice paid"); err != nil {
		t.Fatalf("RestoreAccount: %v", err)
	}
	var suspendedAfter *time.Time
	if err := app.QueryRow(ctx, `SELECT suspended_at FROM tenants WHERE id = $1`, tenant).Scan(&suspendedAfter); err != nil {
		t.Fatalf("read suspended_at after restore: %v", err)
	}
	if suspendedAfter != nil {
		t.Fatalf("suspended_at still set after RestoreAccount: %v", *suspendedAfter)
	}
	plan2, status2 := getTenantPlanStatus(t, app, tenant)
	if plan2 != string(billing.TierStarter) || status2 != "active" {
		t.Fatalf("plan/status after restore = %s/%s, want starter/active (lossless restore)", plan2, status2)
	}
}

func TestAdminBilling_ForceState(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "forcestate-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tenant, string(billing.TierStarter), "past_due")

	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))
	actor := uuid.New()

	if err := svc.ForceAccountState(ctx, actor, tenant, billing.TierAgency, billing.StatusActive, "webhook drift repair"); err != nil {
		t.Fatalf("ForceAccountState: %v", err)
	}
	plan, status := getTenantPlanStatus(t, app, tenant)
	if plan != string(billing.TierAgency) || status != "active" {
		t.Fatalf("plan/status = %s/%s, want agency/active", plan, status)
	}
	var graceUntil *time.Time
	if err := app.QueryRow(ctx, `SELECT grace_until FROM tenants WHERE id = $1`, tenant).Scan(&graceUntil); err != nil {
		t.Fatalf("read grace_until: %v", err)
	}
	if graceUntil != nil {
		t.Fatalf("grace_until = %v, want NULL after a forced state", *graceUntil)
	}
	if entry := lastAuditEntry(t, app, tenant, audit.ActionAdminBillingStateForced); entry == nil {
		t.Fatalf("no audit entry recorded for %s", audit.ActionAdminBillingStateForced)
	}
}

// Reason-required validation on every mutation.
func TestAdminBilling_ReasonRequiredOnEveryMutation(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "reason-required-"+uuid.NewString()[:8])
	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))
	actor := uuid.New()

	cases := map[string]func() error{
		"RevokeComp": func() error { return svc.RevokeComp(ctx, actor, tenant, "") },
		"SetOverrides": func() error {
			n := 1
			return svc.SetAccountOverrides(ctx, actor, tenant, admin.OverrideDeltas{SitesDelta: &n}, "")
		},
		"ExtendGrace": func() error { return svc.ExtendGrace(ctx, actor, tenant, time.Now().Add(24*time.Hour), "") },
		"Suspend":     func() error { return svc.SuspendAccount(ctx, actor, tenant, "") },
		"Restore":     func() error { return svc.RestoreAccount(ctx, actor, tenant, "") },
		"ForceState": func() error {
			return svc.ForceAccountState(ctx, actor, tenant, billing.TierFree, billing.StatusNone, "")
		},
	}
	for name, fn := range cases {
		err := fn()
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindValidation {
			t.Errorf("%s with empty reason: want KindValidation, got %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Revenue tiles math.
// ---------------------------------------------------------------------------

func TestAdminBilling_RevenueTilesMath(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()

	tActive := seedTenant(t, app, "rev-active-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tActive, string(billing.TierStarter), "active")
	tPastDue := seedTenant(t, app, "rev-pastdue-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tPastDue, string(billing.TierAgency), "past_due")
	tTrialing := seedTenant(t, app, "rev-trial-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tTrialing, string(billing.TierScale), "trialing")
	tComped := seedTenant(t, app, "rev-comped-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tComped, string(billing.TierAgency), "comped")

	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))

	rev, err := svc.GetRevenue(ctx)
	if err != nil {
		t.Fatalf("GetRevenue: %v", err)
	}

	wantStarter := int64(billing.MonthlyPriceCentsForTier(billing.TierStarter))
	wantAgency := int64(billing.MonthlyPriceCentsForTier(billing.TierAgency))

	if rev.Tiles.MRRCents < wantStarter+wantAgency {
		t.Fatalf("Tiles.MRRCents = %d, want >= %d (active starter + past_due agency)", rev.Tiles.MRRCents, wantStarter+wantAgency)
	}
	if rev.Tiles.MRRPastDueCents < wantAgency {
		t.Fatalf("Tiles.MRRPastDueCents = %d, want >= %d", rev.Tiles.MRRPastDueCents, wantAgency)
	}
	if rev.Tiles.PastDueAtRiskCents != rev.Tiles.MRRPastDueCents {
		t.Fatalf("Tiles.PastDueAtRiskCents (%d) != Tiles.MRRPastDueCents (%d)", rev.Tiles.PastDueAtRiskCents, rev.Tiles.MRRPastDueCents)
	}
	if rev.Tiles.ActiveSubs < 1 {
		t.Fatalf("Tiles.ActiveSubs = %d, want >= 1", rev.Tiles.ActiveSubs)
	}
	if rev.Tiles.TrialingSubs < 1 {
		t.Fatalf("Tiles.TrialingSubs = %d, want >= 1", rev.Tiles.TrialingSubs)
	}
	if rev.Tiles.PastDueCount < 1 {
		t.Fatalf("Tiles.PastDueCount = %d, want >= 1", rev.Tiles.PastDueCount)
	}
	if rev.Comped.Count < 1 {
		t.Fatalf("Comped.Count = %d, want >= 1", rev.Comped.Count)
	}
	if rev.Comped.HypotheticalCents < wantAgency {
		t.Fatalf("Comped.HypotheticalCents = %d, want >= %d", rev.Comped.HypotheticalCents, wantAgency)
	}

	var foundPastDue bool
	for _, row := range rev.PastDue {
		if row.TenantID == tPastDue {
			foundPastDue = true
			if row.AmountCents != wantAgency {
				t.Fatalf("past-due row AmountCents = %d, want %d", row.AmountCents, wantAgency)
			}
		}
	}
	if !foundPastDue {
		t.Fatalf("past_due tenant %s not present in the revenue page's past-due list", tPastDue)
	}
}

// ---------------------------------------------------------------------------
// HTTP-level: non-superadmin 403 on every route; suspend gates tenant routes
// but not billing/auth.
// ---------------------------------------------------------------------------

// buildAdminEngine mounts the full admin.Handler (superadmin-gated) on a gin
// engine with a fake session middleware that injects the given principal.
func buildAdminEngine(t *testing.T, pool *db.Pool, svc *admin.Service, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := admin.NewHandler(svc, pool)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1Auth := engine.Group("/api/v1")
	h.Register(v1Auth)
	return engine
}

// TestAdminBilling_SuperadminReachesAccountsRoute is the positive-path
// counterpart to TestAdminBilling_NonSuperadminForbiddenOnEveryRoute: a
// caller whose users.is_superadmin is true reaches GET /admin/accounts (200).
func TestAdminBilling_SuperadminReachesAccountsRoute(t *testing.T) {
	app := startPostgres(t)
	adminPool := connectAdmin(t, app)
	tenant := seedTenant(t, app, "issuperadmin-"+uuid.NewString()[:8])

	userID := seedUserRow(t, adminPool, "superadmin-"+uuid.NewString()[:8]+"@example.com")
	markSuperadmin(t, adminPool, userID)

	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))
	p := domain.Principal{Type: domain.PrincipalUser, UserID: userID, TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildAdminEngine(t, app, svc, p)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/admin/accounts as a superadmin = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestAdminBilling_NonSuperadminForbiddenOnEveryRoute(t *testing.T) {
	app := startPostgres(t)
	tenant := seedTenant(t, app, "nonadmin-"+uuid.NewString()[:8])

	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))

	// A regular (non-superadmin) authenticated user.
	p := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildAdminEngine(t, app, svc, p)

	// The comp/state bodies' tier values are built from billing.Tier
	// constants (never a bare string literal here) — this whole file lives
	// outside internal/billing, and TestNoPlanLiteralsOutsideBilling enforces
	// that only internal/billing may spell out a paid-tier name (see
	// grep_guard_test.go).
	compBody := `{"tier":"` + string(billing.TierAgency) + `","reason":"x"}`
	stateBody := `{"plan":"` + string(billing.TierFree) + `","plan_status":"none","reason":"x"}`

	reqs := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/accounts", ""},
		{http.MethodGet, "/api/v1/admin/accounts/" + tenant.String(), ""},
		{http.MethodGet, "/api/v1/admin/revenue", ""},
		{http.MethodPost, "/api/v1/admin/accounts/" + tenant.String() + "/comp", compBody},
		{http.MethodDelete, "/api/v1/admin/accounts/" + tenant.String() + "/comp", `{"reason":"x"}`},
		{http.MethodPut, "/api/v1/admin/accounts/" + tenant.String() + "/overrides", `{"sites":5,"reason":"x"}`},
		{http.MethodPost, "/api/v1/admin/accounts/" + tenant.String() + "/grace", `{"until":"2027-01-01T00:00:00Z","reason":"x"}`},
		{http.MethodPost, "/api/v1/admin/accounts/" + tenant.String() + "/suspend", `{"reason":"x"}`},
		{http.MethodPost, "/api/v1/admin/accounts/" + tenant.String() + "/restore", `{"reason":"x"}`},
		{http.MethodPost, "/api/v1/admin/accounts/" + tenant.String() + "/state", stateBody},
	}
	for _, r := range reqs {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 for a non-superadmin caller", r.method, r.path, w.Code)
		}
	}
}

// TestAdminBilling_SuspendGatesTenantRoutesNotBillingOrAuth proves the
// suspension middleware (billing.Service.SuspensionGate) 402s an ordinary
// tenant-scoped route while leaving /api/v1/billing/* reachable.
func TestAdminBilling_SuspendGatesTenantRoutesNotBillingOrAuth(t *testing.T) {
	app := startPostgres(t)
	tenant := seedTenant(t, app, "suspend-gate-"+uuid.NewString()[:8])

	fp := newFakeProvider("fake")
	billingSvc := newTestBillingService(app, fp)
	adminSvc := newAdminBillingService(app, billingSvc, audit.NewRecorder(app, domain.SystemClock{}))

	if err := adminSvc.SuspendAccount(context.Background(), uuid.New(), tenant, "test suspend"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	p := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg}
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	v1.Use(authz.RequireAuth(), authz.RequireTenant(), billingSvc.SuspensionGate())
	v1.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	billingH := billing.NewHandler(billingSvc, nil, "https://cp.example.com")
	billingH.Register(v1)

	w1 := httptest.NewRecorder()
	engine.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	if w1.Code != http.StatusPaymentRequired {
		t.Fatalf("GET /api/v1/ping on a suspended tenant = %d, want 402", w1.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w1.Body.Bytes(), &body)
	if body["code"] != "account_suspended" {
		t.Fatalf("error body code = %v, want account_suspended", body["code"])
	}

	w2 := httptest.NewRecorder()
	engine.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/v1/billing", nil))
	if w2.Code == http.StatusPaymentRequired {
		t.Fatalf("GET /api/v1/billing on a suspended tenant returned 402 — billing routes must stay reachable")
	}

	// Auth routes are never even wrapped by this middleware in production
	// (mounted on a completely separate, non-tenant-gated group — see
	// server.go) — nothing to assert here beyond that structural fact,
	// already exercised by every existing auth test.
}
