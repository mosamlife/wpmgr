// social_review_debt_integration_test.go: the four social/identity findings
// that can only be settled against the real schema.
//
// Each one is a case where the code did something plausible and the DATABASE is
// the only witness: an audit row that was never written, a column that was
// overwritten with nothing, a token whose intent was destroyed by the act of
// minting its successor, and a unique index nobody classified. None of these is
// observable from a pure function, which is exactly why they survived review.
package tests

import (
	"context"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// countAuthAudit counts an action across BOTH audit sinks. Which one an event
// lands in depends on whether the user has a tenant, and the point of every
// test below is that it lands somewhere.
//
// Takes the ADMIN pool: audit_log is RLS-scoped, so the app role reads nothing
// from it without a tenant GUC, and a helper whose job is to tell "written" from
// "not written" must not have a way of reporting zero for the wrong reason.
func countAuthAudit(t *testing.T, admin *db.Pool, action string, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM system_audit_log WHERE action = $1 AND actor_id = $2)
		      + (SELECT count(*) FROM audit_log        WHERE action = $1 AND actor_id = $3)`,
		action, userID, userID.String(),
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows for %s: %v", action, err)
	}
	return n
}

// seedUnrelatedOrg puts a second account with an organisation in the database,
// so the account under test is not the first user and never gets handed an org
// by the first-run bootstrap. Without it, "a user with no membership" is not
// the state being tested.
func seedUnrelatedOrg(t *testing.T, pool *db.Pool, slug, email string) {
	t.Helper()
	tenantID := seedTenant(t, pool, slug)
	other := seedVerifiedUser(t, pool, email)
	if _, err := auth.NewRepo(pool).CreateMembership(
		context.Background(), other, tenantID, authz.RoleOwner); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 1. The connected-accounts audit, for the accounts with the least oversight
// ---------------------------------------------------------------------------

// TestIdentityAuditSurvivesAUserWithNoMembership is the identities.go half of a
// defect that was fixed on the social path in the same pull request and left
// standing here.
//
// recordIdentityAudit resolved the tenant from memberships[0] and returned
// early when the list was empty, so the event was not written against a nil
// tenant, it was not written at all. "No org membership" is not an edge case:
// it is a site collaborator, a portal user, a brand new social account, and
// anyone whose only org is inside its soft-delete grace window. Both callers
// are credential changes, so the accounts with the least oversight were the
// ones whose credential changes went unrecorded.
//
// The sibling function's own comment calls this exact behaviour a defect. This
// pins that both functions now agree.
func TestIdentityAuditSurvivesAUserWithNoMembership(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	admin := connectAdmin(t, pool)
	seedUnrelatedOrg(t, pool, "identity-audit-org", "owner@identity-audit.test")

	// A social-only account: it has an identity, no password, and no org.
	res, err := svc.SignInWithSocial(ctx, googleIdentity("portal@identity-audit.test"),
		tenantCreator(t, pool, new(int)))
	if err != nil {
		t.Fatalf("social sign-up: %v", err)
	}
	if len(res.Memberships) != 0 {
		t.Fatalf("precondition: this user must have no membership, got %d", len(res.Memberships))
	}

	// Minting the first password is a credential change and must be recorded.
	if err := svc.SetInitialPassword(ctx, res.User.ID, "a-very-strong-password", netip.Addr{}); err != nil {
		t.Fatalf("set initial password: %v", err)
	}
	if n := countAuthAudit(t, admin, audit.ActionPasswordSet, res.User.ID); n == 0 {
		t.Error("setting a first password on an account with no organisation went completely unrecorded; that is the account that most needs the record")
	}

	// And so is removing a sign-in method. The account now has a password, so
	// the invariant permits unlinking its only identity.
	if err := svc.UnlinkIdentity(ctx, res.User.ID, "google"); err != nil {
		t.Fatalf("unlink identity: %v", err)
	}
	if n := countAuthAudit(t, admin, audit.ActionIdentityUnlinked, res.User.ID); n == 0 {
		t.Error("removing the only sign-in method from an account with no organisation went completely unrecorded")
	}
}

// TestIdentityAuditUsesTheResolvedTenantNotTheFirstMembership pins the other
// half: when a tenant CAN be resolved, the event goes to the per-tenant chain
// rather than to the tenantless sink.
func TestIdentityAuditUsesTheResolvedTenantNotTheFirstMembership(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	repo := auth.NewRepo(pool)

	tenantID := seedTenant(t, pool, "identity-audit-member")
	userID := seedVerifiedUser(t, pool, "member@identity-audit.test")
	if _, err := repo.CreateMembership(ctx, userID, tenantID, authz.RoleOwner); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if err := repo.CreateIdentity(ctx, auth.Identity{
		UserID: userID, Provider: "google", Subject: "sub-member",
		Email: "member@identity-audit.test", EmailVerified: true,
	}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	if err := svc.UnlinkIdentity(ctx, userID, "google"); err != nil {
		t.Fatalf("unlink identity: %v", err)
	}

	// Read as the bootstrap superuser: audit_log is RLS-scoped, so the app role
	// sees nothing without a tenant GUC, and "nothing" is what this test must be
	// able to tell apart from "the row was never written".
	admin := connectAdmin(t, pool)
	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND actor_id = $2 AND tenant_id = $3`,
		audit.ActionIdentityUnlinked, userID.String(), tenantID).Scan(&n); err != nil {
		t.Fatalf("count tenant audit rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("tenant audit rows = %d, want 1: a user WITH an org must be recorded on that org's chain", n)
	}
}

// ---------------------------------------------------------------------------
// 2. A provider that stops reporting an address must not erase the last one
// ---------------------------------------------------------------------------

// identityEmail reads what user_identities currently holds for one identity.
func identityEmail(t *testing.T, pool *db.Pool, userID uuid.UUID, provider string) (string, bool) {
	t.Helper()
	var email string
	var verified bool
	if err := pool.QueryRow(context.Background(),
		`SELECT email, email_verified FROM user_identities WHERE user_id = $1 AND provider = $2`,
		userID, provider).Scan(&email, &verified); err != nil {
		t.Fatalf("read identity email: %v", err)
	}
	return email, verified
}

// TestSignInWithNoReportedEmailKeepsTheLastSeenAddress pins that an empty
// report is not an address of "".
//
// TouchIdentityLogin wrote the provider's current email on every sign-in,
// unconditionally. GitHub reports an empty address whenever /user/emails
// carries no primary verified entry, which is exactly what a user who makes
// their address private looks like on their next sign-in, and a known identity
// is signed in without the policy ever consulting email. So the sign-in that
// was meant to keep the last-seen address current destroyed it instead: the
// connected-accounts page lost the address it renders, and the row lost its
// forensic value.
func TestSignInWithNoReportedEmailKeepsTheLastSeenAddress(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	seedUnrelatedOrg(t, pool, "touch-email-org", "owner@touch-email.test")

	first := googleIdentity("private@touch-email.test")
	res, err := svc.SignInWithSocial(ctx, first, tenantCreator(t, pool, new(int)))
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if got, verified := identityEmail(t, pool, res.User.ID, "google"); got != first.Email || !verified {
		t.Fatalf("after the first sign-in the identity holds (%q, %v), want (%q, true)", got, verified, first.Email)
	}

	// The same identity, signing in again after making their address private.
	quiet := first
	quiet.Email = ""
	quiet.EmailVerified = false
	if _, err := svc.SignInWithSocial(ctx, quiet, tenantCreator(t, pool, new(int))); err != nil {
		t.Fatalf("second sign-in with no reported address: %v", err)
	}

	got, verified := identityEmail(t, pool, res.User.ID, "google")
	if got != first.Email {
		t.Errorf("identity email = %q, want the last-seen %q: a provider declining to report an address must not erase the one it reported before", got, first.Email)
	}
	if !verified {
		t.Error("email_verified was cleared alongside an address that was not replaced; the flag must move with the address it describes")
	}

	// The stamp itself still has to happen: this is a real sign-in.
	var stamped bool
	if err := pool.QueryRow(ctx,
		`SELECT last_login_at IS NOT NULL FROM user_identities WHERE user_id = $1 AND provider = 'google'`,
		res.User.ID).Scan(&stamped); err != nil {
		t.Fatalf("read last_login_at: %v", err)
	}
	if !stamped {
		t.Error("last_login_at was not stamped; the login is the one fact this sign-in did establish")
	}
}

// TestSignInWithAReportedEmailStillRecordsIt is the guard on the test above: a
// provider that DOES report a changed address must still be recorded, or the
// preservation rule would have quietly turned the column read-only.
func TestSignInWithAReportedEmailStillRecordsIt(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	seedUnrelatedOrg(t, pool, "touch-email-org-2", "owner@touch-email-2.test")

	first := googleIdentity("before@touch-email-2.test")
	res, err := svc.SignInWithSocial(ctx, first, tenantCreator(t, pool, new(int)))
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}

	changed := first
	changed.Email = "after@touch-email-2.test"
	if _, err := svc.SignInWithSocial(ctx, changed, tenantCreator(t, pool, new(int))); err != nil {
		t.Fatalf("second sign-in with a changed address: %v", err)
	}

	if got, _ := identityEmail(t, pool, res.User.ID, "google"); got != changed.Email {
		t.Errorf("identity email = %q, want the newly reported %q", got, changed.Email)
	}
}

// ---------------------------------------------------------------------------
// 3. Minting a verification token must not destroy the plan intent
// ---------------------------------------------------------------------------

// TestRefusedSocialLinkCarriesForwardTheDesiredPlan pins that the verification
// mail sent by a refused social link keeps the plan the user registered into.
//
// This is worse than an omission, and that is the whole point. Minting a token
// invalidates the previous one and then inserts the new row, and the intent is
// read from the MOST RECENT token. Passing "" therefore made the empty token
// the latest, so the plan was not merely absent from this mail: it was gone
// for good, including from any later resend that would otherwise have
// recovered it. Someone who signed up for a paid plan, never verified, then
// tried the social button landed on the free path after verifying.
func TestRefusedSocialLinkCarriesForwardTheDesiredPlan(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := newAuthStackWithBilling(pool)
	svc.SetMailer(&recordingMailer{}, "https://manage.example.com", nil)
	repo := auth.NewRepo(pool)
	// Open self-serve registration is a property of a NORMAL install, so give
	// this one its owner first: while an install is unclaimed the self-serve
	// path writes nothing, keeping the first-account slot for whoever holds the
	// provisioning claim.
	claimInstall(t, svc)

	const email = "paid-then-social@example.com"
	if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
		Email:    email,
		Password: "a-very-strong-password",
		Name:     "Paid Signup",
		Plan:     string(billing.TierAgency),
	}, makeCreateTenant(t, pool)); err != nil {
		t.Fatalf("register: %v", err)
	}
	u, err := repo.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got := desiredPlanForUser(t, pool, u.ID); got == nil || *got != string(billing.TierAgency) {
		t.Fatalf("precondition: registration must have captured the agency intent, got %v", got)
	}

	// ACTIVE BUT UNVERIFIED is the state that reaches this refusal, and it is a
	// real population rather than a contrivance: users.status defaults to
	// 'active' while email_verified_at defaults NULL, so the first user on an
	// install, everyone created by invitation and every pre-existing SSO user
	// looks like this. ResendVerification's own comment names the same set. A
	// status of 'pending' is refused earlier, by socialStatusGate, and never
	// reaches the branch under test.
	if _, err := pool.Exec(ctx, "UPDATE users SET status = 'active' WHERE id = $1", u.ID); err != nil {
		t.Fatalf("activate the unverified account: %v", err)
	}

	// The account is unverified, so linking a provider to it is refused, and
	// the refusal sends the verification mail that unblocks it.
	if _, err := svc.SignInWithSocial(ctx, googleIdentity(email), makeCreateTenant(t, pool)); err == nil {
		t.Fatal("precondition: linking onto a never-verified account must be refused")
	} else if de, ok := domain.AsDomain(err); !ok || de.Code != "social_link_requires_verification" {
		t.Fatalf("precondition: want social_link_requires_verification, got %v", err)
	}

	got := desiredPlanForUser(t, pool, u.ID)
	if got == nil {
		t.Fatal("the newest verification token carries no plan: minting it destroyed the intent the paid registration captured, and no later resend can recover it")
	}
	if *got != string(billing.TierAgency) {
		t.Fatalf("desired_plan = %q, want %q carried forward", *got, billing.TierAgency)
	}
}

// ---------------------------------------------------------------------------
// 4. A unique violation is a conflict, not an internal error
// ---------------------------------------------------------------------------

// TestLinkingASecondAccountFromTheSameProviderIsAConflict pins the error CLASS
// on the unique index nobody was classifying.
//
// The obvious duplicate, the same (provider, subject, issuer) already linked
// elsewhere, is pre-checked by the caller. The one that reaches CreateIdentity
// is user_identities_user_provider_key on (user_id, provider): a user who
// already has one GitHub identity linking a second GitHub account that carries
// the same verified address. That is a person doing something specific and
// answerable, and it was reported as an internal "failed to link identity",
// which describes nothing and suggests a broken server.
func TestLinkingASecondAccountFromTheSameProviderIsAConflict(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := auth.NewRepo(pool)

	userID := seedVerifiedUser(t, pool, "two-githubs@example.com")
	if err := repo.CreateIdentity(ctx, auth.Identity{
		UserID: userID, Provider: "github", Subject: "gh-1",
		Email: "two-githubs@example.com", EmailVerified: true,
	}); err != nil {
		t.Fatalf("first link: %v", err)
	}

	err := repo.CreateIdentity(ctx, auth.Identity{
		UserID: userID, Provider: "github", Subject: "gh-2",
		Email: "two-githubs@example.com", EmailVerified: true,
	})
	if err == nil {
		t.Fatal("a second identity for the same provider must not be accepted: the unique index refuses it")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("error is not a domain error: %v", err)
	}
	if de.Kind != domain.KindConflict {
		t.Fatalf("kind = %v (code %q), want a conflict: a unique violation is the caller's answer, not a server fault", de.Kind, de.Code)
	}
}

// TestCreateIdentityStillReportsRealFailuresAsInternal is the guard on the test
// above: only 23505 becomes a conflict. Anything else must keep its internal
// class, or a genuine fault would be reported to the caller as their mistake.
func TestCreateIdentityStillReportsRealFailuresAsInternal(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := auth.NewRepo(pool)

	// A user id that does not exist: a foreign key violation (23503), not a
	// unique one.
	err := repo.CreateIdentity(ctx, auth.Identity{
		UserID: uuid.New(), Provider: "github", Subject: "gh-orphan",
		Email: "orphan@example.com", EmailVerified: true,
	})
	if err == nil {
		t.Fatal("an identity for a nonexistent user must not be accepted")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("error is not a domain error: %v", err)
	}
	if de.Kind != domain.KindInternal {
		t.Fatalf("kind = %v (code %q), want internal: only a unique violation is the caller's answer", de.Kind, de.Code)
	}
}
