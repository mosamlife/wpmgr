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
		audit.ActionOIDCLogin, res.User.ID, res.User.ID.String(),
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

	// The install's only user, signed in socially once, with an org.
	first, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), newTenant)
	if err != nil {
		t.Fatalf("first social sign-in: %v", err)
	}
	if len(first.Memberships) != 1 {
		t.Fatalf("first run must bootstrap exactly one organisation, got %d", len(first.Memberships))
	}
	orgID := first.Memberships[0].TenantID

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

	// And the same login through the password path agrees.
	if _, lerr := svc.Login(ctx, "sarah@acme.com", "irrelevant"); lerr == nil {
		t.Fatal("precondition: a social account has no password")
	}
	if created != createdBefore {
		t.Fatal("the password path created an organisation")
	}
}

// TestSocialSignInStillBootstrapsTheFirstInstall guards the correction from
// overshooting: a genuinely new install must still get its first organisation
// from its first social sign-in, or the first user lands with nowhere to work.
func TestSocialSignInStillBootstrapsTheFirstInstall(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)

	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("first social sign-in: %v", err)
	}
	if created != 1 || len(res.Memberships) != 1 || res.Memberships[0].Role != authz.RoleOwner {
		t.Fatalf("first run must create one org and one owner membership: created=%d memberships=%+v", created, res.Memberships)
	}
	if res.ActiveTenant == uuid.Nil {
		t.Fatal("first run left the session with no active tenant")
	}
}

// ---------------------------------------------------------------------------
// 2.12: an invitation must survive its recipient signing in socially
// ---------------------------------------------------------------------------

type noopSessions struct{}

func (noopSessions) Login(ctx context.Context, userID, tenantID uuid.UUID) error { return nil }

// TestSocialSignInClaimsAPendingInvitation pins that being invited and then
// signing in with a provider lands the person in the organisation that invited
// them.
//
// Before this, that order of events destroyed the invitation. A social account
// has no password hash and can never be given one, and the accept endpoint
// authenticates an existing account with a password, so it answered with
// "this account uses single sign-on, sign in first, then open the invite link
// again", advice that leads straight back to the identical refusal, because
// being signed in changes nothing about that check. The invitation was valid,
// addressed to them, and unacceptable by anyone, permanently.
func TestSocialSignInClaimsAPendingInvitation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, auditRec := newAuthStack(pool)

	inviteSvc := invitation.NewService(pool, auth.NewRepo(pool), auditRec, noopSessions{}, nil, "")
	svc.SetInviteClaimer(inviteSvc)

	tenantID := seedTenant(t, pool, "acme")
	owner := seedVerifiedUser(t, pool, "owner@acme.com")
	if _, err := inviteSvc.CreateOrgInvitation(ctx, tenantID, owner, authz.RoleOwner, "sarah@acme.com", string(authz.RoleAdmin)); err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	created := 0
	res, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created))
	if err != nil {
		t.Fatalf("social sign-in: %v", err)
	}

	if len(res.Memberships) != 1 {
		t.Fatalf("the invitation was not claimed: got %d memberships, want 1. Signing in socially before opening the invite link makes that invitation permanently unacceptable", len(res.Memberships))
	}
	if res.Memberships[0].TenantID != tenantID {
		t.Fatalf("joined tenant %s, want the inviting tenant %s", res.Memberships[0].TenantID, tenantID)
	}
	if res.Memberships[0].Role != authz.RoleAdmin {
		t.Fatalf("granted role %q, want the invited role %q", res.Memberships[0].Role, authz.RoleAdmin)
	}
	if res.ActiveTenant != tenantID {
		t.Fatalf("active tenant %s, want %s: the claim must land before the session picks a tenant", res.ActiveTenant, tenantID)
	}

	// Single use still holds: the invitation is spent, not merely granted.
	// Read inside the tenant's RLS scope; invitations are tenant-isolated.
	var acceptedBy *uuid.UUID
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT accepted_user_id FROM invitations WHERE email = 'sarah@acme.com'").Scan(&acceptedBy)
	}); err != nil {
		t.Fatalf("read invitation: %v", err)
	}
	if acceptedBy == nil || *acceptedBy != res.User.ID {
		t.Fatalf("invitation not marked accepted by the signer: %v", acceptedBy)
	}

	// Signing in again must not re-grant anything or fail.
	if _, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created)); err != nil {
		t.Fatalf("second social sign-in: %v", err)
	}
}

// TestSocialAccountCannotAcceptAnInvitationByLink documents WHY the claim above
// has to happen at sign-in: the link route is closed to these accounts, and the
// error it returns advises a step that changes nothing.
func TestSocialAccountCannotAcceptAnInvitationByLink(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, auditRec := newAuthStack(pool)

	// Deliberately NOT wired as a claimer: this asserts the state of the world
	// without the sign-in claim, which is what makes that claim necessary.
	inviteSvc := invitation.NewService(pool, auth.NewRepo(pool), auditRec, noopSessions{}, nil, "")

	tenantID := seedTenant(t, pool, "acme")
	owner := seedVerifiedUser(t, pool, "owner@acme.com")
	link, err := inviteSvc.CreateOrgInvitation(ctx, tenantID, owner, authz.RoleOwner, "sarah@acme.com", string(authz.RoleAdmin))
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	token := link[len("/accept?token="):]

	created := 0
	if _, err := svc.SignInWithSocial(ctx, googleIdentity("sarah@acme.com"), tenantCreator(t, pool, &created)); err != nil {
		t.Fatalf("social sign-in: %v", err)
	}

	_, err = inviteSvc.Accept(ctx, invitation.AcceptInput{
		Token: token, Email: "sarah@acme.com", Password: "anything-at-all",
	})
	if err == nil {
		t.Skip("the accept route now admits password-less accounts; the sign-in claim is no longer the only way in")
	}
	de, _ := domain.AsDomain(err)
	if de == nil || de.Code != "password_login_unavailable" {
		t.Fatalf("got %v, want password_login_unavailable: this is the dead end the sign-in claim exists to avoid", err)
	}
}
