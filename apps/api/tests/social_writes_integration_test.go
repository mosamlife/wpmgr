package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/invitation"
)

// These cover the four writes a social sign-in makes: the identity link, the
// account creation, the audit event, and the memberships the session ends up
// with. Each one is exercised against the real schema, because every defect
// here is about what survives a partial failure or an unfinished login, and
// that is not observable from a pure function.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// tenantCreator returns a createTenant callback that really inserts a tenant,
// and records how many times it was asked to. The count is the assertion for
// the bootstrap tests: "did signing in mint an organisation" is exactly the
// question.
func tenantCreator(t *testing.T, pool *db.Pool, created *int) func(context.Context, string, string) (uuid.UUID, error) {
	t.Helper()
	return func(ctx context.Context, name, slug string) (uuid.UUID, error) {
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			"INSERT INTO tenants (name, slug) VALUES ($1, $2) RETURNING id", name, slug,
		).Scan(&id); err != nil {
			return uuid.Nil, err
		}
		*created++
		return id, nil
	}
}

func countIdentities(t *testing.T, pool *db.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM user_identities WHERE user_id = $1", userID).Scan(&n); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	return n
}

func userExists(t *testing.T, pool *db.Pool, email string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM users WHERE email = $1", email).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n > 0
}

// seedVerifiedUser creates an account this install has verified: the state the
// linking policy requires before a new provider may be attached to it.
func seedVerifiedUser(t *testing.T, pool *db.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, name, status, email_verified_at, password_hash)
		 VALUES ($1, 'Seeded', 'active', now(), 'x') RETURNING id`, email,
	).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func enrollTwoFactor(t *testing.T, pool *db.Pool, userID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE users SET two_factor_enabled = true WHERE id = $1", userID); err != nil {
		t.Fatalf("enroll 2fa: %v", err)
	}
}

func googleIdentity(email string) auth.SocialIdentity {
	return auth.SocialIdentity{
		Provider: "google", Subject: "google-sub-" + email,
		Email: email, EmailVerified: true, Name: "Sarah",
	}
}

// ---------------------------------------------------------------------------
// 2.4: the link must not outlive an unfinished login
// ---------------------------------------------------------------------------

// TestSocialLinkIsNotWrittenBeforeTheSecondFactorIsProven pins the rule that
// binding a new provider to an existing account is a credential change, so it
// may not be committed on the strength of a provider handshake alone.
//
// The attack it closes: the victim has 2FA. An attacker who reaches only the
// provider consent screen for an address the provider vouches for used to walk
// away with their own Google identity permanently attached to the victim's
// account, because the link was written before the challenge was even issued
// and nothing rolled it back when the challenge went unanswered.
func TestSocialLinkIsNotWrittenBeforeTheSecondFactorIsProven(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)

	victim := seedVerifiedUser(t, pool, "sarah@acme.com")
	enrollTwoFactor(t, pool, victim)

	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("sign-in should be allowed to reach the challenge: %v", err)
	}

	// The policy approved the link and handed it back as data instead of
	// writing it.
	if res.PendingSocialLink == nil {
		t.Fatal("a link approved for an existing account must be returned for the caller to complete, not written here")
	}
	if res.PendingSocialLink.UserID != victim {
		t.Fatalf("pending link names user %s, want %s", res.PendingSocialLink.UserID, victim)
	}
	if n := countIdentities(t, pool, victim); n != 0 {
		t.Fatalf("the identity was written before any second factor was proven (%d rows); an unanswered 2FA challenge must leave no link behind", n)
	}

	// Completing it is what writes it, and only for the account it names.
	if err := svc.CompleteSocialLink(ctx, uuid.New(), *res.PendingSocialLink); err == nil {
		t.Fatal("a link parked for one account must never be applied to whoever completes the next login")
	}
	if n := countIdentities(t, pool, victim); n != 0 {
		t.Fatalf("a refused completion still wrote the link (%d rows)", n)
	}
	if err := svc.CompleteSocialLink(ctx, victim, *res.PendingSocialLink); err != nil {
		t.Fatalf("completing the link for the right account: %v", err)
	}
	if n := countIdentities(t, pool, victim); n != 1 {
		t.Fatalf("got %d identities after completion, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// 2.23: creating an account is one write or none
// ---------------------------------------------------------------------------

// TestSocialAccountCreationIsAtomic proves a failure part-way through leaves no
// account behind.
//
// It matters because the address is unique and an account with no identity, no
// password and no verification is almost impossible to get out of the way: the
// next social sign-in finds it, correctly refuses to link a verified identity
// onto an account this install never verified, and the only exit is a
// verification email, which is the one thing a fresh self-host install with no
// SMTP cannot send. The person is locked out of their own address by the ghost
// of their own first attempt.
//
// The failure is injected with a trigger rather than a mock so the transaction
// itself is what is under test.
func TestSocialAccountCreationIsAtomic(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)

	// Fail the identity insert, and only for this one subject, so every other
	// write in the sequence behaves exactly as it does in production. Installed
	// as the superuser: the application role deliberately cannot create objects.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, `
		CREATE FUNCTION reject_wedged_identity() RETURNS trigger AS $$
		BEGIN
			IF NEW.subject = 'wedge' THEN
				RAISE EXCEPTION 'injected identity failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_wedged_identity BEFORE INSERT ON user_identities
			FOR EACH ROW EXECUTE FUNCTION reject_wedged_identity();
	`); err != nil {
		t.Fatalf("install failure injection: %v", err)
	}

	created := 0
	_, err := svc.SignInWithSocial(ctx, auth.SocialIdentity{
		Provider: "google", Subject: "wedge",
		Email: "sarah@acme.com", EmailVerified: true, Name: "Sarah",
	}, tenantCreator(t, pool, &created))
	if err == nil {
		t.Fatal("expected the injected identity failure to surface")
	}

	if userExists(t, pool, "sarah@acme.com") {
		t.Fatal("a failed sign-up left an account holding the address: with no identity, no password and no verification, that row locks the real owner out of their own email")
	}
}

// ---------------------------------------------------------------------------
// 2.22: the credential change is recorded even with no organisation
// ---------------------------------------------------------------------------

// TestSocialAuditSurvivesAUserWithNoMembership pins that linking a provider, or
// creating an account from one, is recorded even when there is no organisation
// to record it against.
//
// It used to be dropped in exactly that case: the tenant came from the first
// membership and fell back to the zero UUID, audit_log.tenant_id references
// tenants, so the insert failed the foreign key and a best-effort caller threw
// the error away. The accounts with no membership are a brand new social
// account, a site collaborator, a portal user, and anyone whose only org is mid
// soft-delete, which is the population with the least oversight, not the most.
func TestSocialAuditSurvivesAUserWithNoMembership(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)

	// A second account, so this is not first-run and the new user is not handed
	// an organisation by the bootstrap.
	tenantID := seedTenant(t, pool, "existing-org")
	other := seedVerifiedUser(t, pool, "owner@acme.com")
	if _, err := auth.NewRepo(pool).CreateMembership(ctx, other, tenantID, authz.RoleOwner); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("social sign-up: %v", err)
	}
	if len(res.Memberships) != 0 {
		t.Fatalf("precondition: this user must have no membership, got %d", len(res.Memberships))
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM system_audit_log WHERE action = $1 AND actor_id = $2)
		      + (SELECT count(*) FROM audit_log        WHERE action = $1 AND actor_id = $3)`,
		// The registration action, not the login one. Creating an account out of
		// a provider assertion is a credential change and is recorded as such;
		// asserting on auth.oidc.login here would be asserting on the very
		// mislabelling that was fixed.
		audit.ActionSocialRegistered, res.User.ID, res.User.ID.String(),
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n == 0 {
		t.Fatal("an account created from an external identity went completely unaudited because it had no organisation; that is the case that most needs the record")
	}
}

// ---------------------------------------------------------------------------
// 2.21: the two sign-in paths must agree about memberships
// ---------------------------------------------------------------------------

// TestSocialSignInDoesNotMintAnOrgDuringTheDeleteGraceWindow pins that signing
// in with a provider never resurrects an organisation the owner just deleted.
//
// A soft-deleted org is hidden from the membership list but the row is still
// there for the length of the grace window. Counting only visible memberships
// made a one-user install look like a fresh one, so social sign-in minted a
// brand new empty organisation and made the user its owner, while the very same
// person signing in with their password got nothing at all. Two paths
// disagreeing about what an account belongs to is the defect; undoing a
// deletion nobody asked to undo is the damage.
func TestSocialSignInDoesNotMintAnOrgDuringTheDeleteGraceWindow(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)

	created := 0
	newTenant := tenantCreator(t, pool, &created)

	// The install's only user, WITH an org. She gets it the way anyone gets
	// one: the claim-bearing first-run call, which is the only path that mints
	// an organisation. A social sign-in never does, so it cannot be used to
	// arrange this precondition — that is the whole subject of
	// TestFirstRunOwnership_SocialSignInNeverMints.
	svc.SetBootstrapClaimSecret(testClaim)
	first, err := svc.Bootstrap(ctx, auth.RegisterInput{
		Email:    "sarah@acme.com",
		Password: "a-very-strong-password",
		Name:     "Sarah",
	}, testClaim)
	if err != nil {
		t.Fatalf("claim the install: %v", err)
	}
	if len(first.Memberships) != 1 {
		t.Fatalf("first run must create exactly one organisation, got %d", len(first.Memberships))
	}
	orgID := first.Memberships[0].TenantID

	// The social linking policy refuses to attach a provider identity to an
	// address this install never verified (the account-takeover defence, not
	// what this test is about), so verify it out of band first.
	if _, err := pool.Exec(ctx,
		"UPDATE users SET email_verified_at = now() WHERE email = $1", "sarah@acme.com"); err != nil {
		t.Fatalf("mark the owner verified: %v", err)
	}
	if _, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), newTenant); err != nil {
		t.Fatalf("link the provider: %v", err)
	}

	// The owner deletes it. Soft delete: the row survives the grace window.
	if _, err := pool.Exec(ctx, "UPDATE tenants SET deleted_at = now() WHERE id = $1", orgID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	createdBefore := created

	second, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), newTenant)
	if err != nil {
		t.Fatalf("second social sign-in: %v", err)
	}
	if created != createdBefore {
		t.Fatalf("signing in with a provider minted %d organisation(s) mid delete grace window; the password path for the same user creates none", created-createdBefore)
	}
	if len(second.Memberships) != 0 {
		t.Fatalf("got %d visible memberships, want 0: a soft-deleted org must stay deleted until its owner restores it", len(second.Memberships))
	}

	// And the same login through the password path agrees. She has a password
	// now (the first-run call set one), so this is the stronger form of the
	// original check: the two paths are compared on the same account, and both
	// must report zero visible memberships rather than one merely failing.
	pw, lerr := svc.Login(ctx, "sarah@acme.com", "a-very-strong-password")
	if lerr != nil {
		t.Fatalf("password login: %v", lerr)
	}
	if len(pw.Memberships) != 0 {
		t.Fatalf("the password path reports %d visible membership(s); the social path reports 0", len(pw.Memberships))
	}
	if created != createdBefore {
		t.Fatal("the password path created an organisation")
	}
}

// TestSocialSignInStillBootstrapsTheFirstInstall IS GONE, AND ITS ABSENCE IS
// THE POINT. It asserted that a first social sign-in mints the install's first
// organisation. That is no longer true and is no longer wanted: minting the
// first organisation is an act of provisioning, authorised by the provisioning
// claim, and a social sign-in is a redirect shaped by the identity provider
// with nowhere to carry one. A path that cannot check the claim must not make
// the grant.
//
// The property that replaced it is asserted in
// auth_first_run_ownership_integration_test.go:
//   - TestFirstRunOwnership_SocialSignInNeverMints — an unclaimed install is
//     left with zero tenants and zero owner memberships.
//   - TestFirstRunOwnership_SocialSignInStillWorksOnAClaimedInstall — the
//     does-not-over-fire half: once the install has an owner, a social sign-in
//     resolves to the org that person already owns.
//
// The concern this test carried — that the first user must not land with
// nowhere to work — is met by the claim-bearing first-run call, which creates
// the org and issues the session in one request
// (TestFirstRunOwnership_CorrectClaimStillWorks).

// ---------------------------------------------------------------------------
// CompleteSocialLink re-checks the policy against fresh state
// ---------------------------------------------------------------------------

// TestParkedSocialLinkIsRefusedWhenTheAccountStateChanged pins that an approval
// made before a two-factor prompt is re-evaluated before it is written.
//
// The parked link can sit for the whole 2FA window, and the decision it carries
// is only as good as the facts it was made on. If the account is disabled in
// that window, replaying the approval from stale data would bind a new
// credential to an account that is no longer allowed to be signed into at all,
// which is exactly what deferring the write was supposed to prevent.
func TestParkedSocialLinkIsRefusedWhenTheAccountStateChanged(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)

	victim := seedVerifiedUser(t, pool, "sarah@acme.com")
	enrollTwoFactor(t, pool, victim)

	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("sign-in should be allowed to reach the challenge: %v", err)
	}
	if res.PendingSocialLink == nil {
		t.Fatal("precondition: the link must have been approved and parked")
	}

	// The window. An administrator disables the account before the challenge is
	// answered.
	if _, err := pool.Exec(ctx, "UPDATE users SET status = 'disabled' WHERE id = $1", victim); err != nil {
		t.Fatalf("disable account: %v", err)
	}

	if err := svc.CompleteSocialLink(ctx, victim, *res.PendingSocialLink); err == nil {
		t.Fatal("a link approved before the account was disabled was still written; the policy must be re-run against fresh state at write time")
	}
	if n := countIdentities(t, pool, victim); n != 0 {
		t.Fatalf("a refused completion still wrote the link (%d rows)", n)
	}
}

// ---------------------------------------------------------------------------
// invitations: accepting is an affirmative act, and it is one transaction
// ---------------------------------------------------------------------------

type noopSessions struct{}

func (noopSessions) Login(ctx context.Context, userID, tenantID uuid.UUID) error { return nil }

// seedInvitation creates an org invitation for email and returns the raw token.
func seedInvitation(t *testing.T, ctx context.Context, inviteSvc *invitation.Service, tenantID, inviter uuid.UUID, email string) string {
	t.Helper()
	link, err := inviteSvc.CreateOrgInvitation(ctx, tenantID, inviter, authz.RoleOwner, email, string(authz.RoleAdmin))
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	return link[len("/accept?token="):]
}

func invitationAcceptedBy(t *testing.T, pool *db.Pool, tenantID uuid.UUID, email string) *uuid.UUID {
	t.Helper()
	// Read inside the tenant's RLS scope; invitations are tenant-isolated.
	var acceptedBy *uuid.UUID
	if err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			"SELECT accepted_user_id FROM invitations WHERE email = $1", email).Scan(&acceptedBy)
	}); err != nil {
		t.Fatalf("read invitation: %v", err)
	}
	return acceptedBy
}

// TestSigningInSociallyDoesNotAcceptInvitations is the consent invariant.
//
// An invitation names an organisation the person has not agreed to join.
// Accepting it on their behalf because a provider vouched for their address
// takes a decision that is theirs to take, silently, across every organisation
// on the install at once, and lands them in one of those orgs as their active
// tenant. It also happens on the pre-2FA path, so it would be a grant made on
// the strength of a provider handshake alone. Sign-in reads; it does not join.
func TestSigningInSociallyDoesNotAcceptInvitations(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, auditRec := newAuthStack(pool)
	inviteSvc := invitation.NewService(pool, auth.NewRepo(pool), auditRec, noopSessions{}, nil, "")

	tenantID := seedTenant(t, pool, "acme")
	owner := seedVerifiedUser(t, pool, "owner@acme.com")
	seedInvitation(t, ctx, inviteSvc, tenantID, owner, "sarah@acme.com")

	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("social sign-in: %v", err)
	}

	if len(res.Memberships) != 0 {
		t.Fatalf("signing in joined %d organisation(s) nobody agreed to join", len(res.Memberships))
	}
	if res.ActiveTenant != uuid.Nil {
		t.Fatalf("signing in activated org %s off an unaccepted invitation", res.ActiveTenant)
	}
	if by := invitationAcceptedBy(t, pool, tenantID, "sarah@acme.com"); by != nil {
		t.Fatalf("the invitation was spent by a sign-in, accepted_user_id=%s; it must still be there for the person to accept", by)
	}
}

// TestASocialAccountCanAcceptAnInvitationOnceSignedIn closes the dead end that
// made the silent claim look necessary.
//
// A social account has no password hash and can never be given one, and this
// endpoint authenticated an existing account with a password, so it answered
// them with "sign in first, then open the invite link again", advice that led
// back to the identical refusal, because the check could not see the session.
// The invitation was addressed to them, still valid, and unacceptable by them
// or anyone else, permanently. A live session for that exact account is now
// accepted in place of the password. The token and the deliberate act of
// submitting it are unchanged.
func TestASocialAccountCanAcceptAnInvitationOnceSignedIn(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, auditRec := newAuthStack(pool)
	inviteSvc := invitation.NewService(pool, auth.NewRepo(pool), auditRec, noopSessions{}, nil, "")

	tenantID := seedTenant(t, pool, "acme")
	owner := seedVerifiedUser(t, pool, "owner@acme.com")
	token := seedInvitation(t, ctx, inviteSvc, tenantID, owner, "sarah@acme.com")

	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("social sign-in: %v", err)
	}

	// No password anywhere in this call. The session is the proof of identity.
	out, err := inviteSvc.Accept(ctx, invitation.AcceptInput{
		Token: token, Email: "sarah@acme.com", SessionUserID: res.User.ID,
	})
	if err != nil {
		t.Fatalf("a signed-in social account must be able to accept its own invitation: %v", err)
	}
	if out.TenantID != tenantID {
		t.Fatalf("accepted into tenant %s, want %s", out.TenantID, tenantID)
	}

	memberships, err := auth.NewRepo(pool).ListMembershipsForUser(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships) != 1 || memberships[0].TenantID != tenantID || memberships[0].Role != authz.RoleAdmin {
		t.Fatalf("accept did not grant the invited role: %+v", memberships)
	}
	if by := invitationAcceptedBy(t, pool, tenantID, "sarah@acme.com"); by == nil || *by != res.User.ID {
		t.Fatalf("invitation not marked accepted by the acceptor: %v", by)
	}
}

// TestAcceptStillRefusesAPasswordlessAccountWithoutASession guards the relaxation
// from overshooting: the session substitutes for the password, and only for the
// account the invitation is addressed to. An anonymous holder of a leaked link
// still gets nothing.
func TestAcceptStillRefusesAPasswordlessAccountWithoutASession(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, auditRec := newAuthStack(pool)
	inviteSvc := invitation.NewService(pool, auth.NewRepo(pool), auditRec, noopSessions{}, nil, "")

	tenantID := seedTenant(t, pool, "acme")
	owner := seedVerifiedUser(t, pool, "owner@acme.com")
	token := seedInvitation(t, ctx, inviteSvc, tenantID, owner, "sarah@acme.com")

	created := 0
	if _, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created)); err != nil {
		t.Fatalf("social sign-in: %v", err)
	}

	// Anonymous: SessionUserID is the zero UUID.
	_, err := inviteSvc.Accept(ctx, invitation.AcceptInput{
		Token: token, Email: "sarah@acme.com", Password: "anything-at-all",
	})
	de, _ := domain.AsDomain(err)
	if de == nil || de.Code != "password_login_unavailable" {
		t.Fatalf("got %v, want password_login_unavailable: possession of the link alone must not grant access", err)
	}

	// And someone else's session is not a substitute either.
	_, err = inviteSvc.Accept(ctx, invitation.AcceptInput{
		Token: token, Email: "sarah@acme.com", SessionUserID: owner,
	})
	if err == nil {
		t.Fatal("a session for a DIFFERENT account accepted an invitation addressed to sarah@acme.com")
	}
	if by := invitationAcceptedBy(t, pool, tenantID, "sarah@acme.com"); by != nil {
		t.Fatalf("a refused accept still spent the invitation: %v", by)
	}
}

// TestInvitationIsNotSpentWhenTheGrantFails pins that claiming and granting are
// one transaction.
//
// They used to be two, and the invitation is single-use, so anything that went
// wrong in the gap marked the token accepted and granted nothing. The person it
// was addressed to could not retry, because the database now considered it
// used, and no amount of re-opening the link would help. The failure did not
// need to be exotic: a dropped connection between the two statements is enough.
func TestInvitationIsNotSpentWhenTheGrantFails(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, auditRec := newAuthStack(pool)
	inviteSvc := invitation.NewService(pool, auth.NewRepo(pool), auditRec, noopSessions{}, nil, "")

	tenantID := seedTenant(t, pool, "acme")
	owner := seedVerifiedUser(t, pool, "owner@acme.com")
	token := seedInvitation(t, ctx, inviteSvc, tenantID, owner, "sarah@acme.com")

	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("social sign-in: %v", err)
	}

	// Fail the grant, and only the grant. Installed as the superuser: the
	// application role deliberately cannot create objects.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, `
		CREATE FUNCTION reject_membership() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'injected membership failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_membership BEFORE INSERT ON memberships
			FOR EACH ROW EXECUTE FUNCTION reject_membership();
	`); err != nil {
		t.Fatalf("install failure injection: %v", err)
	}

	if _, err := inviteSvc.Accept(ctx, invitation.AcceptInput{
		Token: token, Email: "sarah@acme.com", SessionUserID: res.User.ID,
	}); err == nil {
		t.Fatal("expected the injected grant failure to surface")
	}

	if by := invitationAcceptedBy(t, pool, tenantID, "sarah@acme.com"); by != nil {
		t.Fatalf("a failed grant still spent the invitation (accepted_user_id=%s); the person it was sent to can never retry", by)
	}
}

func invitationAttempts(t *testing.T, pool *db.Pool, tenantID uuid.UUID, email string) int32 {
	t.Helper()
	var attempts int32
	if err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			"SELECT attempts FROM invitations WHERE email = $1", email).Scan(&attempts)
	}); err != nil {
		t.Fatalf("read invitation attempts: %v", err)
	}
	return attempts
}

// TestTheSessionsOwnAddressCostsNoAttempt guards a valid invitation against
// being destroyed by the person it was sent to.
//
// The accept page offers a signed-in caller their session's address, which is
// the right default and the wrong answer for anyone invited at a different
// address of their own: work and home, an old employer and a new one. Every
// such submission mismatches, an invitation dies after ten mismatches, and the
// caller has no way to tell that patiently retrying is what is killing it.
//
// The counter is there to stop a link-holder walking a list of addresses to
// discover who an invitation names. A caller signed in as the address they
// submitted is not walking anything: they have one address, they already knew
// it, and learning "not this one" tells them nothing they could not have
// guessed. So the refusal stands and the penalty does not. An anonymous guess
// still pays, which is the half that does the security work.
func TestTheSessionsOwnAddressCostsNoAttempt(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, auditRec := newAuthStack(pool)
	inviteSvc := invitation.NewService(pool, auth.NewRepo(pool), auditRec, noopSessions{}, nil, "")

	tenantID := seedTenant(t, pool, "acme")
	owner := seedVerifiedUser(t, pool, "owner@acme.com")
	token := seedInvitation(t, ctx, inviteSvc, tenantID, owner, "sarah@work.example")

	// Signed in at her personal address; the invitation went to her work one.
	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@home.example"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("social sign-in: %v", err)
	}

	for i := 0; i < 12; i++ {
		_, aerr := inviteSvc.Accept(ctx, invitation.AcceptInput{
			Token: token, Email: "sarah@home.example", SessionUserID: res.User.ID,
		})
		de, _ := domain.AsDomain(aerr)
		if de == nil || de.Code != "invitation_other_recipient" {
			t.Fatalf("attempt %d: got %v, want invitation_other_recipient", i+1, aerr)
		}
	}
	if n := invitationAttempts(t, pool, tenantID, "sarah@work.example"); n != 0 {
		t.Fatalf("the session's own address spent %d attempts; twelve of those kill an invitation nobody did anything wrong with", n)
	}

	// The invitation is still alive, and the person it names can still take it.
	sarah := seedVerifiedUser(t, pool, "sarah@work.example")
	if _, err := inviteSvc.Accept(ctx, invitation.AcceptInput{
		Token: token, Email: "sarah@work.example", SessionUserID: sarah,
	}); err != nil {
		t.Fatalf("the invitation did not survive the mismatches: %v", err)
	}
}

// The other half: an anonymous guess is exactly the enumeration the counter
// exists for, and still costs.
func TestAnAnonymousAddressGuessStillCostsAnAttempt(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	_, auditRec := newAuthStack(pool)
	inviteSvc := invitation.NewService(pool, auth.NewRepo(pool), auditRec, noopSessions{}, nil, "")

	tenantID := seedTenant(t, pool, "acme")
	owner := seedVerifiedUser(t, pool, "owner@acme.com")
	token := seedInvitation(t, ctx, inviteSvc, tenantID, owner, "sarah@work.example")

	_, err := inviteSvc.Accept(ctx, invitation.AcceptInput{
		Token: token, Email: "someone.else@acme.com", Password: "guessing",
	})
	de, _ := domain.AsDomain(err)
	if de == nil || de.Code != "invitation_email_mismatch" {
		t.Fatalf("got %v, want invitation_email_mismatch", err)
	}
	if n := invitationAttempts(t, pool, tenantID, "sarah@work.example"); n != 1 {
		t.Fatalf("a guess cost %d attempts, want 1; the rate limit is what stops a link-holder discovering who an invitation names", n)
	}
}
