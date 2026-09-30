package tests

// billing_org_delete_guard_integration_test.go — DELETE /orgs/{orgId} on a
// hosted instance asks billing whether the tenant's subscription state
// allows the delete. Driven through the real org handler and the real
// billing service, as wpmgr_app, with the tenant state seeded as that role.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
)

// odgBillingState is the tenant billing state one matrix row seeds.
type odgBillingState struct {
	status          string
	provider        string
	subscriptionID  string
	cancelAtEnd     bool
	cancelAtOffset  *time.Duration // relative to now; nil leaves cancel_at NULL
	wantAllowed     bool
	wantBlockReason string
}

func odgDur(d time.Duration) *time.Duration { return &d }

// odgSeedBilling writes the billing columns as wpmgr_app (tenants carries no
// row security, and this is the role every install runs as).
func odgSeedBilling(t *testing.T, pool *db.Pool, tenant uuid.UUID, s odgBillingState) {
	t.Helper()
	var provider, sub any
	if s.provider != "" {
		provider = s.provider
	}
	if s.subscriptionID != "" {
		sub = s.subscriptionID
	}
	var cancelAt any
	if s.cancelAtOffset != nil {
		cancelAt = time.Now().Add(*s.cancelAtOffset)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE tenants SET plan_status = $1, billing_provider = $2, provider_subscription_id = $3,
		        cancel_at_period_end = $4, cancel_at = $5 WHERE id = $6`,
		s.status, provider, sub, s.cancelAtEnd, cancelAt, tenant); err != nil {
		t.Fatalf("seed billing state: %v", err)
	}
}

// odgEngine mounts the real org handler, hosted, with guard as its billing
// delete guard (nil leaves it unwired).
func odgEngine(t *testing.T, pool *db.Pool, guard org.BillingDeleteGuard, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	authSvc, rec := odNewAuthSvc(pool)
	h := org.NewHandler(pool, nil, nil, authSvc, rec)
	h.SetHosted(true)
	if guard != nil {
		h.SetBillingGuard(guard)
	}
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	h.Register(engine.Group("/api/v1"))
	return engine
}

// odgSeedOrg creates an org owned by a fresh user, plus a separate home org
// so the delete never targets the caller's active org, and one site so the
// delete takes the soft lane.
func odgSeedOrg(t *testing.T, pool, admin *db.Pool, prefix string) (target uuid.UUID, slug string, p domain.Principal) {
	t.Helper()
	slug = prefix + "-" + uuid.NewString()[:8]
	target = seedTenant(t, pool, slug)
	owner := seedUserRow(t, admin, "odg-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, target, "owner")
	home := seedTenant(t, pool, "odg-home-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")
	odSeedSite(t, admin, target)
	p = domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	return target, slug, p
}

func odgDelete(engine *gin.Engine, target uuid.UUID, slug string) *httptest.ResponseRecorder {
	return odDo(engine, http.MethodDelete, "/api/v1/orgs/"+target.String(), `{"confirm_name":"`+slug+`"}`)
}

func odgReason(t *testing.T, w *httptest.ResponseRecorder) (code, reason string) {
	t.Helper()
	var body struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	r, _ := body.Details["reason"].(string)
	return body.Code, r
}

func odgBillingService(pool *db.Pool) *billing.Service {
	return billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
}

// TestBillingOrgDeleteGuard_Matrix is the full delete matrix through DELETE
// /orgs/{orgId}.
func TestBillingOrgDeleteGuard_Matrix(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	svc := odgBillingService(pool)

	rows := map[string]odgBillingState{
		// Allowed.
		"no subscription stored":             {status: "active", provider: "stripe", wantAllowed: true},
		"canceled":                           {status: "canceled", provider: "stripe", subscriptionID: "sub_x", wantAllowed: true},
		"active, period-end cancel":          {status: "active", provider: "stripe", subscriptionID: "sub_x", cancelAtEnd: true, wantAllowed: true},
		"trialing, cancel_at only":           {status: "trialing", provider: "stripe", subscriptionID: "sub_x", cancelAtOffset: odgDur(48 * time.Hour), wantAllowed: true},
		"paused, period-end cancel":          {status: "paused", provider: "razorpay", subscriptionID: "sub_x", cancelAtEnd: true, wantAllowed: true},
		"stripe past due, cancel now marker": {status: "past_due", provider: "stripe", subscriptionID: "sub_x", cancelAtEnd: true, cancelAtOffset: odgDur(-time.Minute), wantAllowed: true},

		// Refused.
		"active, no cancel":                  {status: "active", provider: "stripe", subscriptionID: "sub_x", wantBlockReason: billing.OrgDeleteReasonCancelRequired},
		"trialing, no cancel":                {status: "trialing", provider: "stripe", subscriptionID: "sub_x", wantBlockReason: billing.OrgDeleteReasonCancelRequired},
		"paused, no cancel":                  {status: "paused", provider: "stripe", subscriptionID: "sub_x", wantBlockReason: billing.OrgDeleteReasonCancelRequired},
		"stripe past due, no marker":         {status: "past_due", provider: "stripe", subscriptionID: "sub_x", wantBlockReason: billing.OrgDeleteReasonPastDue},
		"stripe past due, period-end cancel": {status: "past_due", provider: "stripe", subscriptionID: "sub_x", cancelAtEnd: true, cancelAtOffset: odgDur(72 * time.Hour), wantBlockReason: billing.OrgDeleteReasonPastDue},
		"razorpay past due, past cancel_at":  {status: "past_due", provider: "razorpay", subscriptionID: "sub_x", cancelAtEnd: true, cancelAtOffset: odgDur(-time.Minute), wantBlockReason: billing.OrgDeleteReasonPastDue},
		"comped, subscription attached":      {status: "comped", provider: "stripe", subscriptionID: "sub_x", wantBlockReason: billing.OrgDeleteReasonComped},
		"none, subscription attached":        {status: "none", provider: "stripe", subscriptionID: "sub_x", cancelAtEnd: true, wantBlockReason: billing.OrgDeleteReasonPending},
	}

	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			target, slug, p := odgSeedOrg(t, pool, admin, "odg")
			row := row
			if row.subscriptionID != "" {
				row.subscriptionID = row.subscriptionID + "_" + uuid.NewString()[:8]
			}
			odgSeedBilling(t, pool, target, row)
			w := odgDelete(odgEngine(t, pool, svc, p), target, slug)
			deletedAt := odTenantDeletedAt(t, admin, target)
			if row.wantAllowed {
				if w.Code != http.StatusOK {
					t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
				}
				if deletedAt == nil {
					t.Fatal("delete returned 200 but the org is not soft-deleted")
				}
				return
			}
			if w.Code != http.StatusConflict {
				t.Fatalf("want 409, got %d body=%s", w.Code, w.Body.String())
			}
			code, reason := odgReason(t, w)
			if code != billing.OrgDeleteBlockedCode || reason != row.wantBlockReason {
				t.Fatalf("code/reason = %s/%s, want %s/%s", code, reason, billing.OrgDeleteBlockedCode, row.wantBlockReason)
			}
			if deletedAt != nil {
				t.Fatal("delete was refused but the org is soft-deleted")
			}
		})
	}
}

// odgLockedOnly passes the unlocked check and keeps the locked one, so a
// refusal can only come from the check inside the delete's transaction.
type odgLockedOnly struct{ svc *billing.Service }

func (g odgLockedOnly) CheckOrgDeletable(context.Context, uuid.UUID) error { return nil }
func (g odgLockedOnly) CheckOrgDeletableLocked(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	return g.svc.CheckOrgDeletableLocked(ctx, tx, id)
}

// TestBillingOrgDeleteGuard_LockedCheckDecides proves the check inside the
// delete's transaction refuses on its own, for both the soft and the hard
// lane, when the unlocked check let the request through.
func TestBillingOrgDeleteGuard_LockedCheckDecides(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	guard := odgLockedOnly{svc: odgBillingService(pool)}

	// Soft lane (the org has a site).
	target, slug, p := odgSeedOrg(t, pool, admin, "odg-locked")
	odgSeedBilling(t, pool, target, odgBillingState{status: "active", provider: "stripe", subscriptionID: "sub_locked_soft"})
	w := odgDelete(odgEngine(t, pool, guard, p), target, slug)
	if w.Code != http.StatusConflict {
		t.Fatalf("soft lane: want 409 from the locked check, got %d body=%s", w.Code, w.Body.String())
	}
	if odTenantDeletedAt(t, admin, target) != nil {
		t.Fatal("soft lane: the org was soft-deleted despite the refusal")
	}

	// Hard lane (an empty org).
	emptySlug := "odg-empty-" + uuid.NewString()[:8]
	empty := seedTenant(t, pool, emptySlug)
	odSeedMembership(t, admin, p.UserID, empty, "owner")
	odgSeedBilling(t, pool, empty, odgBillingState{status: "past_due", provider: "stripe", subscriptionID: "sub_locked_hard"})
	w = odgDelete(odgEngine(t, pool, guard, p), empty, emptySlug)
	if w.Code != http.StatusConflict {
		t.Fatalf("hard lane: want 409 from the locked check, got %d body=%s", w.Code, w.Body.String())
	}
	if !tenantExists(t, admin, empty) {
		t.Fatal("hard lane: the org was hard-deleted despite the refusal")
	}
}

// TestBillingOrgDeleteGuard_WaitsForBillingLock proves the locked check
// takes the billing lock: a billing write that holds the lock and has not
// committed is waited for, and the delete decides on the state it commits.
// Without the lock the delete would read the older, deletable state.
func TestBillingOrgDeleteGuard_WaitsForBillingLock(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	svc := odgBillingService(pool)
	ctx := context.Background()

	target, slug, p := odgSeedOrg(t, pool, admin, "odg-lock")
	odgSeedBilling(t, pool, target, odgBillingState{status: "canceled", provider: "stripe", subscriptionID: "sub_lock_old"})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := billing.LockTenantBilling(ctx, tx, target); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tenants SET plan_status = 'active', provider_subscription_id = 'sub_lock_new', cancel_at_period_end = false WHERE id = $1`,
		target); err != nil {
		t.Fatalf("update under lock: %v", err)
	}

	engine := odgEngine(t, pool, svc, p)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- odgDelete(engine, target, slug) }()

	select {
	case w := <-done:
		t.Fatalf("the delete finished while the billing lock was held: %d %s", w.Code, w.Body.String())
	case <-time.After(750 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	select {
	case w := <-done:
		if w.Code != http.StatusConflict {
			t.Fatalf("want 409 on the committed state, got %d body=%s", w.Code, w.Body.String())
		}
		if _, reason := odgReason(t, w); reason != billing.OrgDeleteReasonCancelRequired {
			t.Fatalf("reason = %s, want %s", reason, billing.OrgDeleteReasonCancelRequired)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the delete did not finish within 15s of the billing lock being released")
	}
}

// TestBillingOrgDeleteGuard_UnwiredGuardRefuses proves a hosted handler with
// no guard wired refuses rather than skips the check.
func TestBillingOrgDeleteGuard_UnwiredGuardRefuses(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	target, slug, p := odgSeedOrg(t, pool, admin, "odg-unwired")

	w := odgDelete(odgEngine(t, pool, nil, p), target, slug)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 with no guard wired, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "org_delete_billing_guard_unwired") {
		t.Fatalf("body %s does not name the unwired guard", w.Body.String())
	}
	if odTenantDeletedAt(t, admin, target) != nil {
		t.Fatal("the org was deleted with no billing guard wired")
	}
}
