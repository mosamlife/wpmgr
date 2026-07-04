package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestAuditRebaselineAcknowledgesHistoricalBreak covers scenario (a): a
// tenant with a historical (pre-existing, unrepairable) chain break reports
// broken; setting the integrity baseline at the current head makes Verify
// report ok again, without altering the tampered row.
func TestAuditRebaselineAcknowledgesHistoricalBreak(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "audit-rebaseline-a")

	// Three normal entries.
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		e, err := rec.Record(ctx, audit.Event{
			TenantID:  tenant,
			ActorType: audit.ActorUser,
			ActorID:   uuid.New().String(),
			Action:    "test.event",
			Metadata:  map[string]any{"i": i},
		})
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		ids = append(ids, e.ID)
	}

	ok, _, err := rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify (intact): %v", err)
	}
	if !ok {
		t.Fatal("intact chain reported as broken")
	}

	// Simulate the historical race: tamper the SECOND row's action as the
	// superuser (the app role cannot — UPDATE is revoked on audit_log). This
	// is the class of pre-advisory-lock corruption re-baseline exists for.
	adminPool := connectAdmin(t, pool)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx,
		"UPDATE audit_log SET action = 'tampered.historical' WHERE id = $1", ids[1]); err != nil {
		t.Fatalf("admin tamper: %v", err)
	}

	ok, brokenAt, err := rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify (tampered): %v", err)
	}
	if ok {
		t.Fatal("tampered chain reported as intact before re-baseline")
	}
	if brokenAt != ids[1] {
		t.Fatalf("brokenAt = %s, want %s (the tampered row)", brokenAt, ids[1])
	}

	// Two more normal entries land AFTER the tampered row — exactly what
	// happens in production: the tenant keeps operating for a long time after
	// the historical race, oblivious to the permanent badge.
	for i := 3; i < 5; i++ {
		if _, err := rec.Record(ctx, audit.Event{
			TenantID:  tenant,
			ActorType: audit.ActorUser,
			ActorID:   uuid.New().String(),
			Action:    "test.event",
			Metadata:  map[string]any{"i": i},
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	// Still broken — the historical row is still walked from genesis.
	ok, _, err = rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify (still tampered): %v", err)
	}
	if ok {
		t.Fatal("expected the historical break to still be reported before re-baseline")
	}

	operator := seedNamedUserRow(t, pool, "operator@audit-rebaseline-a.example.com", "Operator")
	baseline, err := rec.Rebaseline(ctx, tenant, audit.ActorUser, operator.String(), operator, &brokenAt)
	if err != nil {
		t.Fatalf("rebaseline: %v", err)
	}
	if baseline.TenantID != tenant {
		t.Fatalf("baseline.TenantID = %s, want %s", baseline.TenantID, tenant)
	}
	if baseline.SetBy == nil || *baseline.SetBy != operator {
		t.Fatalf("baseline.SetBy = %v, want %s", baseline.SetBy, operator)
	}

	// Verify now reports ok: the historical break is acknowledged, not fixed.
	ok, _, err = rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify (post-rebaseline): %v", err)
	}
	if !ok {
		t.Fatal("Verify still reports broken after re-baseline at head")
	}

	// The tampered row itself was never touched — forensic evidence stays.
	var action string
	if err := adminPool.QueryRow(ctx, "SELECT action FROM audit_log WHERE id = $1", ids[1]).Scan(&action); err != nil {
		t.Fatalf("read tampered row: %v", err)
	}
	if action != "tampered.historical" {
		t.Fatalf("tampered row was altered by re-baseline: action = %q", action)
	}

	// The re-baseline itself is durably recorded in the chain.
	entries, err := rec.List(ctx, tenant, 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == audit.ActionAuditIntegrityRebaselined {
			found = true
			if e.Metadata["acknowledged_broken_at"] != brokenAt.String() {
				t.Fatalf("rebaseline entry metadata acknowledged_broken_at = %v, want %s", e.Metadata["acknowledged_broken_at"], brokenAt)
			}
		}
	}
	if !found {
		t.Fatal("no audit.integrity.rebaselined entry was recorded")
	}

	// GetBaseline round-trips what Rebaseline returned.
	got, err := rec.GetBaseline(ctx, tenant)
	if err != nil {
		t.Fatalf("get baseline: %v", err)
	}
	if got == nil || got.ID != baseline.ID || got.Hash != baseline.Hash {
		t.Fatalf("GetBaseline = %+v, want %+v", got, baseline)
	}
}

// TestAuditRebaselineStillCatchesNewTampering covers scenario (b): after a
// re-baseline, tampering AFTER the anchor is still detected by Verify — the
// baseline only forgives history, never the future.
func TestAuditRebaselineStillCatchesNewTampering(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "audit-rebaseline-b")

	for i := 0; i < 2; i++ {
		if _, err := rec.Record(ctx, audit.Event{
			TenantID:  tenant,
			ActorType: audit.ActorUser,
			ActorID:   uuid.New().String(),
			Action:    "test.event",
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	operator := seedNamedUserRow(t, pool, "operator@audit-rebaseline-b.example.com", "Operator")
	if _, err := rec.Rebaseline(ctx, tenant, audit.ActorUser, operator.String(), operator, nil); err != nil {
		t.Fatalf("rebaseline: %v", err)
	}

	ok, _, err := rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify (post-rebaseline, clean): %v", err)
	}
	if !ok {
		t.Fatal("expected ok immediately after re-baseline")
	}

	// Forge a row AFTER the baseline with a prev_hash that does not match the
	// real chain — simulating tampering (or a lost-update race) that happens
	// AFTER the operator acknowledged history. Inserted as the superuser since
	// the app role cannot bypass the hash-chain logic via direct INSERT
	// either, but this test is specifically about detecting it once present.
	adminPool := connectAdmin(t, pool)
	defer adminPool.Close()

	var lastCreatedAt time.Time
	if err := adminPool.QueryRow(ctx,
		"SELECT created_at FROM audit_log WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1", tenant).
		Scan(&lastCreatedAt); err != nil {
		t.Fatalf("read last created_at: %v", err)
	}
	forgedAt := lastCreatedAt.Add(time.Second)

	if _, err := adminPool.Exec(ctx,
		`INSERT INTO audit_log (tenant_id, actor_type, actor_id, action, prev_hash, hash, created_at)
		 VALUES ($1, 'system', '', 'test.forged', 'not-the-real-prev-hash', 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef', $2)`,
		tenant, forgedAt); err != nil {
		t.Fatalf("insert forged row: %v", err)
	}

	ok, brokenAt, err := rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify (post-forge): %v", err)
	}
	if ok {
		t.Fatal("Verify reported ok after a forged row was appended past the baseline — new tampering must still be caught")
	}
	if brokenAt == uuid.Nil {
		t.Fatal("Verify did not report the broken row")
	}
}

// TestAuditRebaselineEndpointAuthzAndAudit covers scenario (c): the HTTP
// endpoint is owner-gated (a lower-priv role, including a read-only viewer,
// gets 403) and a successful call both persists the baseline and records the
// audit.integrity.rebaselined entry.
func TestAuditRebaselineEndpointAuthzAndAudit(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	tenant := seedTenant(t, pool, "audit-rebaseline-c")

	if _, err := rec.Record(ctx, audit.Event{
		TenantID:  tenant,
		ActorType: audit.ActorUser,
		ActorID:   uuid.New().String(),
		Action:    "test.event",
	}); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	gin.SetMode(gin.TestMode)
	newEngine := func(p domain.Principal) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
			c.Next()
		})
		v1 := r.Group("/api/v1")
		audit.NewHandler(rec).Register(v1)
		return r
	}

	post := func(p domain.Principal, body string) *httptest.ResponseRecorder {
		r := newEngine(p)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/audit/integrity/rebaseline", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	user := seedNamedUserRow(t, pool, "owner@audit-rebaseline-c.example.com", "Owner")
	// A read-only viewer must never be able to acknowledge an integrity break.
	for _, role := range []authz.Role{authz.RoleClient, authz.RoleViewer, authz.RoleOperator, authz.RoleAdmin} {
		p := domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenant, Role: string(role), Scope: domain.ScopeOrg}
		w := post(p, "{}")
		if w.Code != http.StatusForbidden {
			t.Fatalf("role %s: status = %d, want 403 (forbidden)", role, w.Code)
		}
	}

	// A site-scoped collaborator (even if somehow granted an owner-ranked
	// role string) must also be rejected — audit is an org-level permission.
	siteScoped := domain.Principal{
		Type: domain.PrincipalUser, UserID: user, TenantID: tenant, Role: string(authz.RoleOwner),
		Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{uuid.New()},
	}
	if w := post(siteScoped, "{}"); w.Code != http.StatusForbidden {
		t.Fatalf("site-scoped owner: status = %d, want 403 (org_scope_required)", w.Code)
	}

	// The owner succeeds.
	owner := domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenant, Role: string(authz.RoleOwner), Scope: domain.ScopeOrg}
	w := post(owner, `{"broken_at":"`+uuid.New().String()+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("owner: status = %d, body = %s", w.Code, w.Body.String())
	}
	var out struct {
		Ok       bool   `json:"ok"`
		BrokenAt string `json:"broken_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !out.Ok {
		t.Fatalf("expected ok=true immediately after re-baseline, got %+v", out)
	}

	// The baseline was actually persisted...
	baseline, err := rec.GetBaseline(ctx, tenant)
	if err != nil {
		t.Fatalf("get baseline: %v", err)
	}
	if baseline == nil {
		t.Fatal("no baseline was persisted by the endpoint")
	}
	if baseline.SetBy == nil || *baseline.SetBy != user {
		t.Fatalf("baseline.SetBy = %v, want %s", baseline.SetBy, user)
	}

	// ...and the acknowledgment is in the tamper-evident trail.
	entries, err := rec.List(ctx, tenant, 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == audit.ActionAuditIntegrityRebaselined {
			found = true
		}
	}
	if !found {
		t.Fatal("no audit.integrity.rebaselined entry was recorded by the endpoint")
	}
}

// TestAuditRebaselineEmptyTenant proves an empty tenant (no audit history at
// all) cannot be re-baselined — there is no chain head to anchor to.
func TestAuditRebaselineEmptyTenant(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	tenant := seedTenant(t, pool, "audit-rebaseline-empty")

	operator := uuid.New()
	if _, err := rec.Rebaseline(ctx, tenant, audit.ActorUser, operator.String(), operator, nil); err == nil {
		t.Fatal("expected an error re-baselining a tenant with no audit history")
	}
}
