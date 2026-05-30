package tests

// account_settings_test.go — integration tests for:
//   - Service.UpdateProfile (name trimming, length cap, result shape)
//   - Service.ChangePassword (wrong current rejected, OIDC rejected, success)
//   - Service.ResolveActors / ResolveActor (UserDirectory)
//   - restoreRunDTO triggered_by_email/name resolution in the restore handler

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// seedPasswordUser creates a user with a real argon2id hash so password checks
// can be exercised against the actual database.
func seedPasswordUser(t *testing.T, pool interface{ CreateUser(context.Context, string, string, string, string, string) (auth.User, error) }, email, password, name string) auth.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	// pool here is *auth.Repo; type assertion is avoided by using the interface.
	u, err := pool.CreateUser(context.Background(), email, hash, name, "", "")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

// ---------------------------------------------------------------------------
// TestUpdateProfile
// ---------------------------------------------------------------------------

func TestUpdateProfile(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	authRepo := auth.NewRepo(pool)
	tenantID := seedTenant(t, pool, "profile-update")

	u := seedPasswordUser(t, authRepo, "profile@example.com", "supersecretpassword1", "Original Name")
	if _, err := authRepo.CreateMembership(ctx, u.ID, tenantID, authz.RoleOwner); err != nil {
		t.Fatalf("membership: %v", err)
	}

	t.Run("updates name", func(t *testing.T) {
		updated, memberships, err := svc.UpdateProfile(ctx, u.ID, "  New Name  ")
		if err != nil {
			t.Fatalf("UpdateProfile: %v", err)
		}
		if updated.Name != "New Name" {
			t.Errorf("want Name %q, got %q", "New Name", updated.Name)
		}
		if updated.Email != "profile@example.com" {
			t.Errorf("email should be unchanged, got %q", updated.Email)
		}
		if len(memberships) != 1 {
			t.Errorf("want 1 membership, got %d", len(memberships))
		}
	})

	t.Run("empty name is allowed (clear it)", func(t *testing.T) {
		updated, _, err := svc.UpdateProfile(ctx, u.ID, "   ")
		if err != nil {
			t.Fatalf("UpdateProfile with empty name: %v", err)
		}
		if updated.Name != "" {
			t.Errorf("want empty name, got %q", updated.Name)
		}
	})

	t.Run("name at exactly 120 chars is accepted", func(t *testing.T) {
		name120 := strings.Repeat("a", 120)
		_, _, err := svc.UpdateProfile(ctx, u.ID, name120)
		if err != nil {
			t.Fatalf("120-char name should be accepted: %v", err)
		}
	})

	t.Run("name over 120 chars is rejected with validation error", func(t *testing.T) {
		name121 := strings.Repeat("b", 121)
		_, _, err := svc.UpdateProfile(ctx, u.ID, name121)
		if err == nil {
			t.Fatal("should reject name > 120 chars")
		}
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindValidation {
			t.Fatalf("want Validation error, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// TestChangePassword
// ---------------------------------------------------------------------------

func TestChangePassword(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	authRepo := auth.NewRepo(pool)
	tenantID := seedTenant(t, pool, "change-password")

	// Password user.
	const originalPwd = "originalPwd12345"
	u := seedPasswordUser(t, authRepo, "pwchange@example.com", originalPwd, "PW User")
	if _, err := authRepo.CreateMembership(ctx, u.ID, tenantID, authz.RoleOwner); err != nil {
		t.Fatalf("membership: %v", err)
	}

	// OIDC-only user (no password_hash).
	oidcUser, err := authRepo.CreateUser(ctx, "oidc@example.com", "", "OIDC User", "https://idp.example", "sub123")
	if err != nil {
		t.Fatalf("create oidc user: %v", err)
	}
	if _, err := authRepo.CreateMembership(ctx, oidcUser.ID, tenantID, authz.RoleViewer); err != nil {
		t.Fatalf("oidc membership: %v", err)
	}

	t.Run("OIDC account rejected with validation error", func(t *testing.T) {
		err := svc.ChangePassword(ctx, oidcUser.ID, "", "newpassword123")
		if err == nil {
			t.Fatal("expected rejection for OIDC account")
		}
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindValidation {
			t.Fatalf("want Validation, got %v", err)
		}
		if de.Code != "sso_account_no_password" {
			t.Errorf("want code sso_account_no_password, got %q", de.Code)
		}
	})

	t.Run("wrong current password rejected", func(t *testing.T) {
		err := svc.ChangePassword(ctx, u.ID, "wrongpassword", "newpassword123")
		if err == nil {
			t.Fatal("expected rejection for wrong current password")
		}
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindUnauthorized {
			t.Fatalf("want Unauthorized, got %v", err)
		}
	})

	t.Run("new password too short rejected", func(t *testing.T) {
		err := svc.ChangePassword(ctx, u.ID, originalPwd, "short")
		if err == nil {
			t.Fatal("expected rejection for short new password")
		}
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindValidation {
			t.Fatalf("want Validation, got %v", err)
		}
	})

	t.Run("success: password updated and new password verifies", func(t *testing.T) {
		const newPwd = "newGoodPassword123"
		if err := svc.ChangePassword(ctx, u.ID, originalPwd, newPwd); err != nil {
			t.Fatalf("ChangePassword: %v", err)
		}
		// Verify new password works at login.
		res, err := svc.Login(ctx, "pwchange@example.com", newPwd)
		if err != nil {
			t.Fatalf("login with new password: %v", err)
		}
		if res.User.ID != u.ID {
			t.Fatal("login returned unexpected user")
		}
		// Old password should no longer work.
		if _, err := svc.Login(ctx, "pwchange@example.com", originalPwd); err == nil {
			t.Fatal("old password should no longer be valid")
		}
	})
}

// ---------------------------------------------------------------------------
// TestResolveActors (service-level)
// ---------------------------------------------------------------------------

func TestResolveActors(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	authRepo := auth.NewRepo(pool)
	tenantID := seedTenant(t, pool, "resolve-actors")

	alice, err := authRepo.CreateUser(ctx, "alice@example.com", "", "Alice", "", "")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := authRepo.CreateMembership(ctx, alice.ID, tenantID, authz.RoleOwner); err != nil {
		t.Fatalf("membership: %v", err)
	}

	t.Run("known user resolves", func(t *testing.T) {
		m, err := svc.ResolveActors(ctx, []uuid.UUID{alice.ID})
		if err != nil {
			t.Fatalf("ResolveActors: %v", err)
		}
		a, ok := m[alice.ID]
		if !ok {
			t.Fatal("alice not in result map")
		}
		if a.Email != "alice@example.com" {
			t.Errorf("email: want alice@example.com, got %q", a.Email)
		}
		if a.Name != "Alice" {
			t.Errorf("name: want Alice, got %q", a.Name)
		}
	})

	t.Run("unknown IDs are silently omitted", func(t *testing.T) {
		m, err := svc.ResolveActors(ctx, []uuid.UUID{uuid.New()})
		if err != nil {
			t.Fatalf("ResolveActors with unknown id: %v", err)
		}
		if len(m) != 0 {
			t.Errorf("expected empty map for unknown IDs, got %v", m)
		}
	})

	t.Run("empty id list returns empty map", func(t *testing.T) {
		m, err := svc.ResolveActors(ctx, nil)
		if err != nil {
			t.Fatalf("ResolveActors nil: %v", err)
		}
		if len(m) != 0 {
			t.Errorf("expected empty map for nil ids, got %v", m)
		}
	})
}

// ---------------------------------------------------------------------------
// TestResolveActor (UserDirectory single-call interface on *auth.Service)
// ---------------------------------------------------------------------------

func TestResolveActorInterface(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	authRepo := auth.NewRepo(pool)
	tenantID := seedTenant(t, pool, "resolve-actor-iface")

	bob, err := authRepo.CreateUser(ctx, "bob@example.com", "", "Bob", "", "")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if _, err := authRepo.CreateMembership(ctx, bob.ID, tenantID, authz.RoleAdmin); err != nil {
		t.Fatalf("membership: %v", err)
	}

	// svc satisfies backup.UserDirectory via the ResolveActor method.
	var dir backup.UserDirectory = svc

	t.Run("valid UUID resolves", func(t *testing.T) {
		email, name, ok := dir.ResolveActor(ctx, bob.ID.String())
		if !ok {
			t.Fatal("expected ok=true for known user")
		}
		if email != "bob@example.com" {
			t.Errorf("email: want bob@example.com, got %q", email)
		}
		if name != "Bob" {
			t.Errorf("name: want Bob, got %q", name)
		}
	})

	t.Run("invalid UUID returns ok=false", func(t *testing.T) {
		_, _, ok := dir.ResolveActor(ctx, "not-a-uuid")
		if ok {
			t.Fatal("expected ok=false for non-UUID")
		}
	})

	t.Run("unknown UUID returns ok=false", func(t *testing.T) {
		_, _, ok := dir.ResolveActor(ctx, uuid.New().String())
		if ok {
			t.Fatal("expected ok=false for unknown user UUID")
		}
	})
}

