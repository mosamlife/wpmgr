package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// The production ResolveSetter, with no database behind it. The membership,
// share, client and account reads each fail, which is the case a decision
// must treat as "could not check", never as a pass. The proofs that it reads
// a member, a removed member, a site collaborator and a disabled or deleted
// account correctly are in tests/approval_tiers_setter_integration_test.go,
// as wpmgr_app.

// unreachablePool is a pool with nothing listening at its address. It opens
// no connection until a read asks for one, and then every read fails.
func unreachablePool(t *testing.T) *db.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://wpmgr:wpmgr@127.0.0.1:1/wpmgr?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatalf("parse the pool config: %v", err)
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("build the pool: %v", err)
	}
	t.Cleanup(p.Close)
	return &db.Pool{Pool: p}
}

func setterAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	pool := unreachablePool(t)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	return NewAuthenticator(nil, auth.NewService(auth.NewRepo(pool), rec, domain.NewValidator()), nil, pool)
}

// TestResolveSetterLookupFailureIsAFailedCheck proves a failed read makes
// the setter checks fail as not checked, grants no access, and reads no
// account status, so a decision on it waits for a person with not_checked.
//
// Mutation: ignore the lookup errors in ResolveSetter (never set
// LookupFailed); the setter then reads as "not a member" instead of "could
// not check", and the decision's reason is no longer not_checked.
func TestResolveSetterLookupFailureIsAFailedCheck(t *testing.T) {
	a := setterAuthenticator(t)
	tenant, site, user := uuid.New(), uuid.New(), uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s := a.ResolveSetter(ctx, tenant, user)
	if !s.LookupFailed {
		t.Fatalf("ResolveSetter with every read failing: LookupFailed = false (%+v)", s)
	}
	if s.UserID != user {
		t.Fatalf("UserID %s, want %s", s.UserID, user)
	}
	if p := s.Principal; p.TenantID != uuid.Nil || p.Role != "" || p.Scope != "" || len(p.AllowedSiteIDs) != 0 {
		t.Fatalf("a failed read granted access: %+v", p)
	}
	if s.AccountStatus != "" {
		t.Fatalf("AccountStatus %q from a failed read, want none", s.AccountStatus)
	}

	siteCheck := aipolicy.CheckSiteSetter(s, tenant, site, aipolicy.ModeAIDrafts, string(authz.PermSiteContentEdit))
	connCheck := aipolicy.CheckConnectionSetter(s, tenant)
	for name, c := range map[string]aipolicy.SetterCheck{"site setter": siteCheck, "connection setter": connCheck} {
		if c.OK || !c.LookupFailed || c.Failed != aipolicy.FailedLookup {
			t.Fatalf("%s check %+v, want failed as %s", name, c, aipolicy.FailedLookup)
		}
	}
	d := aipolicy.Evaluate(aipolicy.Inputs{
		Class:            aipolicy.Classify(aipolicy.ClassFacts{Stored: aipolicy.ClassAIDraft, Undo: aipolicy.UndoByAbility}),
		SiteMode:         aipolicy.ModeAIDrafts,
		SiteSetter:       siteCheck,
		ConnectionAuto:   aipolicy.AutoSiteSetting,
		ConnectionSetter: connCheck,
		Usage:            aipolicy.Usage{Checked: true},
	})
	if d.Outcome != aipolicy.OutcomeAsk || d.Ask != aipolicy.AskNotChecked {
		t.Fatalf("decision %+v, want ask %s", d, aipolicy.AskNotChecked)
	}
	t.Logf("every read failed: LookupFailed=%v site=%s connection=%s decision=%s/%s",
		s.LookupFailed, siteCheck.Failed, connCheck.Failed, d.Outcome, d.Ask)
}

// TestResolveSetterWithNoPersonResolvesNoOne proves a setting with no
// person on record, or no organisation, stands for no one: no read is made
// (a read would fail against this pool and set LookupFailed), and both
// setter checks fail.
func TestResolveSetterWithNoPersonResolvesNoOne(t *testing.T) {
	a := setterAuthenticator(t)
	tenant, user := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name         string
		tenant, user uuid.UUID
		want         string
	}{
		{"no person on record", tenant, uuid.Nil, aipolicy.FailedNoSetter},
		{"no organisation", uuid.Nil, user, aipolicy.FailedNotMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := a.ResolveSetter(context.Background(), tc.tenant, tc.user)
			if s.LookupFailed || s.AccountStatus != "" || s.Principal.TenantID != uuid.Nil || s.UserID != tc.user {
				t.Fatalf("setter %+v, want no one resolved and no read made", s)
			}
			if c := aipolicy.CheckConnectionSetter(s, tenant); c.OK || c.Failed != tc.want {
				t.Fatalf("connection setter check %+v, want failed as %s", c, tc.want)
			}
		})
	}
}
