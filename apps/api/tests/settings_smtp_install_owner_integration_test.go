package tests

// settings_smtp_install_owner_integration_test.go: on a SELF-HOSTED install the
// account that set the install up keeps the instance SMTP relay after a second
// organisation exists (admingate.ArmInstallOwner). On hosted it does not.
//
// The install owner is created through the REAL first-run path,
// auth.Service.Bootstrap with the provisioning claim, before anything else is
// seeded, so the install_owner row under test is the one BootstrapInstall
// writes. Every request goes through the real router (server.New) over the
// wpmgr_app pool, and GET /auth/me is read from the same engine, exactly as in
// settings_smtp_instance_authority_integration_test.go, whose harness this
// file reuses.
//
// NOT RUN BY CI. Run with `make test-integration` from the repository root.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/settings"
)

// installOwnerRow reads the install_owner singleton as wpmgr_app. found=false
// means the table is empty.
func (s *smtpIAStack) installOwnerRow(t *testing.T) (userID, tenantID uuid.UUID, source string, found bool) {
	t.Helper()
	ctx := context.Background()
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		qerr := tx.QueryRow(ctx, `SELECT user_id, tenant_id, source FROM install_owner`).Scan(&userID, &tenantID, &source)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return nil
		}
		if qerr != nil {
			return qerr
		}
		found = true
		return nil
	})
	if err != nil {
		t.Fatalf("read install_owner: %v", err)
	}
	return userID, tenantID, source, found
}

// systemAuditAdmittedAs counts system_audit_log SMTP updates by actorID whose
// admitted_as is arm.
func (s *smtpIAStack) systemAuditAdmittedAs(t *testing.T, actorID uuid.UUID, arm admingate.Arm) int {
	t.Helper()
	ctx := context.Background()
	var n int
	err := s.pool.InUserTx(ctx, actorID, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM system_audit_log
			WHERE action = $1 AND actor_id = $2 AND metadata->>'admitted_as' = $3`,
			settings.AuditActionUpdate, actorID, arm.String()).Scan(&n)
	})
	if err != nil {
		t.Fatalf("read system_audit_log: %v", err)
	}
	return n
}

// requireSQLState runs stmt as wpmgr_app and asserts it fails with code.
func (s *smtpIAStack) requireSQLState(t *testing.T, code, stmt string, args ...any) {
	t.Helper()
	ctx := context.Background()
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		_, e := tx.Exec(ctx, stmt, args...)
		return e
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: got %v, want SQLSTATE %s", stmt, err, code)
	}
}

func TestSMTPSettings_SelfHosted_InstallOwner_InstanceAuthority(t *testing.T) {
	s := newSMTPIAStack(t)
	ctx := context.Background()
	s.authSvc.SetBootstrapClaimSecret(testClaim)
	selfHosted := admingate.NewInstanceEmailPoolStore(s.pool, false)
	e := s.engine(t, selfHosted)
	eHosted := s.engine(t, admingate.NewInstanceEmailPoolStore(s.pool, true))
	adminRepo := admin.NewRepo(s.pool)

	sfx := uuid.NewString()[:8]
	boot, err := s.authSvc.Bootstrap(ctx, auth.RegisterInput{
		Email:      "smtp-io-first-" + sfx + "@example.com",
		Password:   "install-owner-strong-pass",
		Name:       "First Run",
		TenantName: "Home " + sfx,
		TenantSlug: "smtp-io-home-" + sfx,
	}, testClaim)
	if err != nil {
		t.Fatalf("bootstrap the install: %v", err)
	}
	installOwner, home := boot.User.ID, boot.ActiveTenant

	// A second owner of the home organisation, a second organisation with its
	// own owner, and a signed-in user with no membership anywhere.
	homeOwner2 := seedUserMembership(t, s.authRepo, "smtp-io-home-owner2-"+sfx+"@example.com", home, authz.RoleOwner).ID
	tenantB := seedTenant(t, s.pool, "smtp-io-b-"+sfx)
	ownerB := seedUserMembership(t, s.authRepo, "smtp-io-owner-b-"+sfx+"@example.com", tenantB, authz.RoleOwner).ID
	stranger := seedUserOnly(t, s.authRepo, "smtp-io-stranger-"+sfx+"@example.com").ID
	s.seedRelay(t, installOwner)

	// THE PREMISE, asserted rather than assumed.
	if n := s.liveTenantCount(t, home); n != 2 {
		t.Fatalf("PREMISE FAILED: %d live organisations, want 2", n)
	}
	for name, id := range map[string]uuid.UUID{"install owner": installOwner, "home owner 2": homeOwner2, "owner B": ownerB, "stranger": stranger} {
		if isSA, err := selfHosted.IsSuperadmin(ctx, id); err != nil || isSA {
			t.Fatalf("PREMISE FAILED: IsSuperadmin(%s) = %v, %v; want false, nil", name, isSA, err)
		}
	}
	if role := s.membershipRole(t, home, installOwner); role != string(authz.RoleOwner) {
		t.Fatalf("PREMISE FAILED: install owner's role in home is %q", role)
	}
	if uid, tid, src, found := s.installOwnerRow(t); !found || uid != installOwner || tid != home || src != "bootstrap" {
		t.Fatalf("PREMISE FAILED: install_owner = (%s, %s, %q, found=%v); want (%s, %s, \"bootstrap\")",
			uid, tid, src, found, installOwner, home)
	}

	// I1. Catches the bootstrap insert being removed or made best-effort.
	t.Run("install owner is admitted with two organisations and recorded in the home organisation", func(t *testing.T) {
		sctx := s.session(t, installOwner, home)
		requireCapability(t, e, sctx, true)
		sysBefore := s.systemAuditAdmittedAs(t, installOwner, admingate.ArmInstallOwner)
		homeBefore := s.tenantAuditUpdates(t, home, installOwner)
		bBefore := s.tenantAuditUpdates(t, tenantB, installOwner)
		requireAllAdmitted(t, s, e, sctx)
		if n := s.systemAuditAdmittedAs(t, installOwner, admingate.ArmInstallOwner); n != sysBefore+1 {
			t.Errorf("system_audit_log rows admitted_as install_owner went %d -> %d; want exactly one more", sysBefore, n)
		}
		if n := s.tenantAuditUpdates(t, home, installOwner); n != homeBefore+1 {
			t.Errorf("home organisation audit_log rows went %d -> %d; want exactly one more", homeBefore, n)
		}
		if n := s.tenantAuditUpdates(t, tenantB, installOwner); n != bBefore {
			t.Errorf("second organisation audit_log rows went %d -> %d; want unchanged", bBefore, n)
		}
	})

	// I2. Catches keying on "any owner" or "owner of the earliest tenant"
	// instead of the recorded identity.
	for _, tc := range []struct {
		name   string
		user   uuid.UUID
		tenant uuid.UUID
	}{
		{"second organisation's owner is refused", ownerB, tenantB},
		{"another owner of the home organisation is refused", homeOwner2, home},
		{"a user with no organisation is refused", stranger, uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, found := s.smtpRow(t)
			if !found {
				t.Fatal("PREMISE FAILED: no smtp_settings row to protect")
			}
			sctx := s.session(t, tc.user, tc.tenant)
			requireAllRefused(t, e, sctx)
			if after, _ := s.smtpRow(t); after != before {
				t.Errorf("smtp_settings changed under refused requests:\nbefore %s\nafter  %s", before, after)
			}
			requireCapability(t, e, sctx, false)
		})
	}

	// I3. Catches the constructor or the decision ignoring or inverting the
	// hosted flag.
	t.Run("hosted install refuses the install owner", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		sctx := s.session(t, installOwner, home)
		requireAllRefused(t, eHosted, sctx)
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, eHosted, sctx, false)
	})

	// I5. The flag catches removing IsSiteConstrained; the routes catch
	// removing RequireOrgScope.
	t.Run("install owner in a site-share session is refused", func(t *testing.T) {
		seedSiteShare(t, s.adminDB, tenantB, seedSite(t, s.pool, tenantB, "https://smtp-io-"+sfx+".example.test"), installOwner, "viewer")
		before, _ := s.smtpRow(t)
		sctx := s.session(t, installOwner, tenantB)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, smtpPath, ""},
			{http.MethodPut, smtpPath, smtpPutBody},
			{http.MethodPost, smtpTestPath, smtpTestBody},
		} {
			r := smtpIADo(e, sctx, c.method, c.path, c.body)
			if r.status != http.StatusForbidden || r.code != "org_scope_required" {
				t.Errorf("%s %s site-scoped: got %d %q (%s); want 403 org_scope_required", c.method, c.path, r.status, r.code, r.body)
			}
		}
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, e, sctx, false)
	})

	// I6. An API key carries no user today, so this proves the route refuses
	// the install owner's organisation's owner-role key; the type check that
	// would refuse a key naming the install owner is pinned by
	// internal/admingate's TestInstallOwnerArm_APIKeyRefused.
	t.Run("owner-role API key of the home organisation is refused", func(t *testing.T) {
		created, err := apikey.NewService(s.pool).Create(ctx, home, "smtp-io-key", authz.RoleOwner)
		if err != nil {
			t.Fatalf("create API key: %v", err)
		}
		before, _ := s.smtpRow(t)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, smtpPath, ""},
			{http.MethodPut, smtpPath, smtpPutBody},
			{http.MethodPost, smtpTestPath, smtpTestBody},
		} {
			r := smtpIADoKey(e, created.Token, c.method, c.path, c.body)
			if r.status != http.StatusForbidden || r.code != settings.InstanceAuthorityRequiredCode {
				t.Errorf("%s %s with an API key: got %d %q (%s); want 403 %q", c.method, c.path, r.status, r.code, r.body, settings.InstanceAuthorityRequiredCode)
			}
		}
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
	})

	// The agent-mirror check is not widened: the same install owner, against
	// the same rows, through the production stores, is refused by
	// CanRunAgentMirrorCheck even when that decision is handed the wider store.
	t.Run("agent-mirror check still refuses the install owner", func(t *testing.T) {
		pctx := domain.WithPrincipal(ctx, domain.Principal{Type: domain.PrincipalUser, UserID: installOwner, TenantID: home, Scope: domain.ScopeOrg})
		if !admingate.CanManageInstanceEmail(pctx, selfHosted) {
			t.Fatal("PREMISE FAILED: the install owner does not hold instance email authority here")
		}
		if admingate.CanRunAgentMirrorCheck(pctx, admingate.NewPoolStore(s.pool)) {
			t.Error("CanRunAgentMirrorCheck(PoolStore) admitted the install owner")
		}
		if admingate.CanRunAgentMirrorCheck(pctx, selfHosted) {
			t.Error("CanRunAgentMirrorCheck(InstanceEmailPoolStore) admitted the install owner")
		}
	})

	// I8. A failed earlier read does not fall through to the install-owner
	// arm, and a failed install-owner read refuses.
	failing := closedAppPool(t, s.pool)
	healthy := admingate.NewPoolStore(s.pool)
	for _, tc := range []struct {
		name  string
		store splitGateStore
	}{
		{"sole-owner read failing refuses the install owner", splitGateStore{superadmin: healthy, owner: admingate.NewPoolStore(failing), installOwner: selfHosted}},
		{"superadmin read failing refuses the install owner", splitGateStore{superadmin: admingate.NewPoolStore(failing), owner: healthy, installOwner: selfHosted}},
		{"install-owner read failing refuses", splitGateStore{superadmin: healthy, owner: healthy, installOwner: admingate.NewInstanceEmailPoolStore(failing, false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := s.smtpRow(t)
			ee := s.engine(t, tc.store)
			sctx := s.session(t, installOwner, home)
			requireAllRefused(t, ee, sctx)
			if after, _ := s.smtpRow(t); after != before {
				t.Errorf("smtp_settings changed under refused requests")
			}
			requireCapability(t, ee, sctx, false)
		})
	}

	// I7. Catches dropping the users.status join.
	t.Run("disabled install owner is refused", func(t *testing.T) {
		if _, err := adminRepo.SetStatus(ctx, installOwner, "disabled"); err != nil {
			t.Fatalf("disable the install owner: %v", err)
		}
		before, _ := s.smtpRow(t)
		sctx := s.session(t, installOwner, home)
		requireAllRefused(t, e, sctx)
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, e, sctx, false)
		if _, err := adminRepo.SetStatus(ctx, installOwner, "active"); err != nil {
			t.Fatalf("re-enable the install owner: %v", err)
		}
		requireCapability(t, e, sctx, true)
	})

	// Tamper-proofing as wpmgr_app: the row cannot be re-pointed, deleted,
	// emptied or joined by a second row.
	t.Run("wpmgr_app cannot change the install owner record", func(t *testing.T) {
		s.requireSQLState(t, "42501", `UPDATE install_owner SET user_id = $1`, ownerB)
		s.requireSQLState(t, "42501", `DELETE FROM install_owner`)
		s.requireSQLState(t, "42501", `TRUNCATE install_owner`)
		s.requireSQLState(t, "23505", `INSERT INTO install_owner (singleton, user_id, tenant_id, source) VALUES (true, $1, $2, 'bootstrap')`, ownerB, tenantB)
		if uid, tid, _, found := s.installOwnerRow(t); !found || uid != installOwner || tid != home {
			t.Errorf("install_owner changed: (%s, %s, found=%v); want (%s, %s)", uid, tid, found, installOwner, home)
		}
	})

	// I4. Runs last: it deletes accounts. Catches ON CONFLICT DO UPDATE and
	// any "earliest surviving user or owner" fallback.
	t.Run("deleting the install owner passes the authority to nobody, and a re-bootstrap does not take it", func(t *testing.T) {
		if err := adminRepo.DeleteUser(ctx, installOwner); err != nil {
			t.Fatalf("delete the install owner through admin.Repo: %v", err)
		}
		if uid, _, _, found := s.installOwnerRow(t); !found || uid != installOwner {
			t.Fatalf("install_owner after delete names %s (found=%v); want the deleted %s", uid, found, installOwner)
		}
		for name, c := range map[string]struct{ user, tenant uuid.UUID }{
			"remaining home owner": {homeOwner2, home},
			"second owner":         {ownerB, tenantB},
		} {
			t.Run(name, func(t *testing.T) {
				sctx := s.session(t, c.user, c.tenant)
				requireAllRefused(t, e, sctx)
				requireCapability(t, e, sctx, false)
			})
		}

		// Remove every owner, so first-run bootstrap is open again.
		for _, id := range []uuid.UUID{homeOwner2, ownerB} {
			if err := adminRepo.DeleteUser(ctx, id); err != nil {
				t.Fatalf("delete owner %s: %v", id, err)
			}
		}
		again, err := s.authSvc.Bootstrap(ctx, auth.RegisterInput{
			Email:      "smtp-io-again-" + sfx + "@example.com",
			Password:   "rebootstrap-strong-pass",
			TenantName: "Again " + sfx,
			TenantSlug: "smtp-io-again-" + sfx,
		}, testClaim)
		if err != nil {
			t.Fatalf("re-bootstrap with every owner gone: %v", err)
		}
		if uid, _, src, found := s.installOwnerRow(t); !found || uid != installOwner || src != "bootstrap" {
			t.Fatalf("install_owner after re-bootstrap = (%s, %q, found=%v); want the original %s", uid, src, found, installOwner)
		}
		if n := s.liveTenantCount(t, again.ActiveTenant); n < 2 {
			t.Fatalf("PREMISE FAILED: %d live organisations after re-bootstrap; the sole-owner arm would decide", n)
		}
		before, _ := s.smtpRow(t)
		sctx := s.session(t, again.User.ID, again.ActiveTenant)
		requireAllRefused(t, e, sctx)
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, e, sctx, false)
	})
}
