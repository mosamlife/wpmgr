package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func newAuthStack(pool *db.Pool) (*auth.Service, *audit.Recorder) {
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	svc := auth.NewService(auth.NewRepo(pool), rec, domain.NewValidator())
	return svc, rec
}

// TestBootstrapAndLogin exercises the first-run bootstrap then a password login.
func TestBootstrapAndLogin(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)

	createTenant := func(ctx context.Context, name, slug string) (uuid.UUID, error) {
		return seedTenant(t, pool, slug), nil
	}

	res, err := svc.Bootstrap(ctx, auth.RegisterInput{
		Email:    "owner@example.com",
		Password: "a-very-strong-password",
		Name:     "Owner",
	}, createTenant)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(res.Memberships) != 1 || res.Memberships[0].Role != authz.RoleOwner {
		t.Fatalf("expected one owner membership, got %+v", res.Memberships)
	}
	if res.ActiveTenant == uuid.Nil {
		t.Fatal("bootstrap did not set an active tenant")
	}

	// Second bootstrap must be refused (registration closed).
	if _, err := svc.Bootstrap(ctx, auth.RegisterInput{Email: "second@example.com", Password: "another-strong-pass"}, createTenant); err == nil {
		t.Fatal("second bootstrap should be refused")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindForbidden {
		t.Fatalf("want forbidden, got %v", err)
	}

	// Login success.
	login, err := svc.Login(ctx, "owner@example.com", "a-very-strong-password")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if login.User.ID != res.User.ID {
		t.Fatal("login returned a different user")
	}

	// Login failure: wrong password.
	if _, err := svc.Login(ctx, "owner@example.com", "wrong"); err == nil {
		t.Fatal("login with wrong password should fail")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindUnauthorized {
		t.Fatalf("want unauthorized, got %v", err)
	}

	// Login failure: unknown user (must not reveal existence).
	if _, err := svc.Login(ctx, "nobody@example.com", "whatever"); err == nil {
		t.Fatal("login with unknown user should fail")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindUnauthorized {
		t.Fatalf("want unauthorized, got %v", err)
	}
}

// TestMembershipRLSIsolation proves a session scoped to tenant B cannot read
// tenant A's memberships, and a user's self-read sees only their own rows.
func TestMembershipRLSIsolation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := auth.NewRepo(pool)

	tenantA := seedTenant(t, pool, "mem-a")
	tenantB := seedTenant(t, pool, "mem-b")

	userA, err := repo.CreateUser(ctx, "a@example.com", "", "A", "", "")
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	userB, err := repo.CreateUser(ctx, "b@example.com", "", "B", "", "")
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}
	if _, err := repo.CreateMembership(ctx, userA.ID, tenantA, authz.RoleOwner); err != nil {
		t.Fatalf("membership A: %v", err)
	}
	if _, err := repo.CreateMembership(ctx, userB.ID, tenantB, authz.RoleOwner); err != nil {
		t.Fatalf("membership B: %v", err)
	}

	// Tenant B scope: count of tenant A's memberships must be zero.
	err = pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE tenant_id = $1", tenantA).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("RLS failed: tenant B saw %d of tenant A's memberships", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant membership tx: %v", err)
	}

	// Self-read: user A sees exactly one membership (their own).
	msA, err := repo.ListMembershipsForUser(ctx, userA.ID)
	if err != nil {
		t.Fatalf("self-read A: %v", err)
	}
	if len(msA) != 1 || msA[0].TenantID != tenantA {
		t.Fatalf("user A self-read leaked: %+v", msA)
	}
}

// TestAPIKeyLifecycle covers create -> authenticate -> revoke -> reject.
func TestAPIKeyLifecycle(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := apikey.NewService(pool)

	tenant := seedTenant(t, pool, "apikeys")

	created, err := svc.Create(ctx, tenant, "ci", authz.RoleOperator)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Token == "" {
		t.Fatal("token not returned on creation")
	}

	// Authenticate with the real token.
	key, err := svc.Authenticate(ctx, created.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if key.TenantID != tenant || key.Role != authz.RoleOperator {
		t.Fatalf("authenticated key mismatch: %+v", key)
	}

	// A tampered secret must be rejected.
	if _, err := svc.Authenticate(ctx, created.Token+"x"); err == nil {
		t.Fatal("tampered token should be rejected")
	}

	// Revoke, then the same token must be rejected.
	if err := svc.Revoke(ctx, tenant, created.Key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Authenticate(ctx, created.Token); err == nil {
		t.Fatal("revoked token should be rejected")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindUnauthorized {
		t.Fatalf("want unauthorized, got %v", err)
	}
}

// TestAPIKeyRLSIsolation proves api_keys are tenant-isolated.
func TestAPIKeyRLSIsolation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := apikey.NewService(pool)

	tenantA := seedTenant(t, pool, "ak-a")
	tenantB := seedTenant(t, pool, "ak-b")

	if _, err := svc.Create(ctx, tenantA, "a-key", authz.RoleViewer); err != nil {
		t.Fatalf("create A: %v", err)
	}

	// Tenant B's list must not include tenant A's key.
	keysB, err := svc.List(ctx, tenantB, 100, 0)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(keysB) != 0 {
		t.Fatalf("tenant B saw %d of tenant A's keys", len(keysB))
	}
}

// TestAuditHashChainTamperDetection verifies an intact chain and detects a row
// mutated out-of-band (which is only possible as the superuser, since the app
// role is denied UPDATE on audit_log).
func TestAuditHashChainTamperDetection(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "audit")

	for i := 0; i < 3; i++ {
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

	ok, _, err := rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("intact chain reported as broken")
	}

	// The app role cannot UPDATE audit_log (privilege revoked). Confirm that.
	err = pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, uerr := tx.Exec(ctx, "UPDATE audit_log SET action = 'tampered' WHERE tenant_id = $1", tenant)
		return uerr
	})
	if err == nil {
		t.Fatal("app role was able to UPDATE append-only audit_log; privilege not revoked")
	}

	// Tamper as the superuser (bypassing the privilege) to prove Verify detects
	// content changes.
	adminPool := connectAdmin(t, pool)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx,
		"UPDATE audit_log SET action = 'tampered' WHERE tenant_id = $1 AND action = 'test.event'", tenant); err != nil {
		t.Fatalf("admin tamper: %v", err)
	}

	ok, brokenAt, err := rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify after tamper: %v", err)
	}
	if ok {
		t.Fatal("tampered chain reported as intact")
	}
	if brokenAt == uuid.Nil {
		t.Fatal("verify did not report the broken row")
	}
}
