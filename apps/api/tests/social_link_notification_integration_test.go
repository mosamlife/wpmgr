// social_link_notification_integration_test.go — the visibility half of the
// social-linking rule, exercised end to end against the real schema.
//
// The product decision: attaching a provider-verified identity to a locally
// verified account keeps happening without anyone signing in first, because
// demanding an authenticated link would tax every returning user with a
// password reset to defend against an attacker who already needs BOTH halves.
// What changes is that it stops being silent. These tests hold the wiring that
// makes it visible, which a unit test on the notification alone cannot: that
// the LINK path calls it, that the create path does not, and that the account
// state WPMGR_SUPERADMIN_EMAILS now leaves behind is one that cannot be linked
// onto at all.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// sentEmail is one captured Enqueue call.
type sentEmail struct {
	Recipients []string
	Template   string
	Data       map[string]any
}

// recordingMailer is an auth.EmailEnqueuer double that records every send, so a
// test can assert both what WAS sent and what was NOT.
type recordingMailer struct{ sent []sentEmail }

func (m *recordingMailer) Enqueue(_ context.Context, _ uuid.UUID, recipients []string, template string, data map[string]any) error {
	m.sent = append(m.sent, sentEmail{Recipients: recipients, Template: template, Data: data})
	return nil
}

func (m *recordingMailer) withTemplate(name string) []sentEmail {
	var out []sentEmail
	for _, e := range m.sent {
		if e.Template == name {
			out = append(out, e)
		}
	}
	return out
}

// seedUser inserts a user directly. users carries no RLS (see db/schema.sql), so
// this needs no GUC. verified controls email_verified_at, which is the fact the
// whole linking rule turns on.
func seedUser(t *testing.T, pool *db.Pool, email, name string, verified bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	stmt := `INSERT INTO users (email, password_hash, name, status)
	         VALUES ($1, 'x', $2, 'active') RETURNING id`
	if verified {
		stmt = `INSERT INTO users (email, password_hash, name, status, email_verified_at)
		        VALUES ($1, 'x', $2, 'active', now()) RETURNING id`
	}
	if err := pool.QueryRow(context.Background(), stmt, email, name).Scan(&id); err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return id
}

func newSocialStack(t *testing.T, pool *db.Pool) (*auth.Service, *recordingMailer, func(ctx context.Context, name, slug string) (uuid.UUID, error)) {
	t.Helper()
	svc, _ := newAuthStack(pool)
	mail := &recordingMailer{}
	svc.SetMailer(mail, "https://manage.example.com", nil)
	createTenant := func(ctx context.Context, name, slug string) (uuid.UUID, error) {
		return seedTenant(t, pool, slug+"-"+uuid.NewString()[:8]), nil
	}
	return svc, mail, createTenant
}

// TestSocialLinkNotifiesTheLocalAddress is the fix. A Google identity attaches
// itself to an existing, locally verified account with nobody signed in, and
// the account holder is told at the address this install verified.
func TestSocialLinkNotifiesTheLocalAddress(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, mail, createTenant := newSocialStack(t, pool)

	seedUser(t, pool, "sarah@acme.com", "Sarah", true)

	if _, err := svc.SignInWithSocial(ctx, auth.SocialIdentity{
		Provider: "google", Subject: "google-sub-1",
		Email: "sarah@acme.com", EmailVerified: true, Name: "Sarah",
	}, createTenant); err != nil {
		t.Fatalf("linking a verified provider identity onto a verified account must still succeed: %v", err)
	}

	notices := mail.withTemplate("sign_in_method_added")
	if len(notices) != 1 {
		t.Fatalf("expected exactly one new-sign-in-method notice, got %d (sent: %+v)", len(notices), mail.sent)
	}
	got := notices[0]
	if len(got.Recipients) != 1 || got.Recipients[0] != "sarah@acme.com" {
		t.Errorf("notice went to %v, want the local account address", got.Recipients)
	}
	if got.Data["Provider"] != "Google" {
		t.Errorf("notice does not name the provider: %+v", got.Data)
	}
	if when, _ := got.Data["When"].(string); when == "" {
		t.Errorf("notice does not carry a time: %+v", got.Data)
	}
}

// A SECOND sign-in with the same identity is not a new sign-in method. It takes
// the known-identity path, and sending the notice again would train the reader
// to ignore it.
func TestRepeatSocialSignInDoesNotRepeatTheNotice(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, mail, createTenant := newSocialStack(t, pool)

	seedUser(t, pool, "sarah@acme.com", "Sarah", true)
	id := auth.SocialIdentity{
		Provider: "google", Subject: "google-sub-1",
		Email: "sarah@acme.com", EmailVerified: true, Name: "Sarah",
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.SignInWithSocial(ctx, id, createTenant); err != nil {
			t.Fatalf("sign-in %d: %v", i, err)
		}
	}
	if n := len(mail.withTemplate("sign_in_method_added")); n != 1 {
		t.Fatalf("sent %d notices across three sign-ins; only the first attached a method", n)
	}
}

// Creating an account is not attaching a method to an existing one: there is no
// prior holder to warn, and the only address available is the one the provider
// just asserted.
func TestSocialAccountCreationSendsNoLinkNotice(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, mail, createTenant := newSocialStack(t, pool)

	if _, err := svc.SignInWithSocial(ctx, auth.SocialIdentity{
		Provider: "github", Subject: "gh-1",
		Email: "newcomer@acme.com", EmailVerified: true, Name: "Newcomer",
	}, createTenant); err != nil {
		t.Fatalf("creating an account from a verified provider identity: %v", err)
	}

	if n := len(mail.withTemplate("sign_in_method_added")); n != 0 {
		t.Fatalf("account creation sent %d new-sign-in-method notices; it should send none", n)
	}
}

// A refused link must not send the notice either: nothing was attached. The
// verification link that unblocks the refusal is what goes out instead.
func TestRefusedSocialLinkSendsVerificationNotTheNotice(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, mail, createTenant := newSocialStack(t, pool)

	seedUser(t, pool, "sarah@acme.com", "Sarah", false)

	_, err := svc.SignInWithSocial(ctx, auth.SocialIdentity{
		Provider: "google", Subject: "google-sub-1",
		Email: "sarah@acme.com", EmailVerified: true, Name: "Sarah",
	}, createTenant)
	if err == nil {
		t.Fatal("linking onto a never-verified account must be refused")
	}
	if n := len(mail.withTemplate("sign_in_method_added")); n != 0 {
		t.Errorf("a refused link sent %d notices; nothing was attached", n)
	}
	if n := len(mail.withTemplate("verify_email")); n != 1 {
		t.Errorf("a refused link must send the verification link that resolves it, sent %d", n)
	}
}

// TestSuperadminSeededAccountCannotAbsorbASocialIdentity is why
// WPMGR_SUPERADMIN_EMAILS stopped stamping email_verified_at, stated as the
// property that actually matters rather than as SQL.
//
// The seeder now leaves an allowlisted operator ACTIVE (they can still sign in
// with a password, which is the only reason it activates at all) but NOT
// verified. In that state the takeover defence applies to them like anyone
// else: a provider that asserts their address cannot attach an identity to the
// highest privilege account on the install. While the seeder stamped
// verification, it could.
func TestSuperadminSeededAccountCannotAbsorbASocialIdentity(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, mail, createTenant := newSocialStack(t, pool)

	// The row the seeder produces after this change: active, superadmin, and
	// never verified by this install.
	id := seedUser(t, pool, "ops@acme.com", "Ops", false)
	if _, err := pool.Exec(ctx, `UPDATE users SET is_superadmin = true WHERE id = $1`, id); err != nil {
		t.Fatalf("mark superadmin: %v", err)
	}

	_, err := svc.SignInWithSocial(ctx, auth.SocialIdentity{
		Provider: "google", Subject: "attacker-google-sub",
		Email: "ops@acme.com", EmailVerified: true, Name: "Ops",
	}, createTenant)
	if err == nil {
		t.Fatal("a provider-asserted address must not attach an identity to an unverified superadmin account")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "social_link_requires_verification" {
		t.Fatalf("got %v, want social_link_requires_verification", err)
	}
	if n := len(mail.withTemplate("sign_in_method_added")); n != 0 {
		t.Errorf("nothing was attached, so nothing should have been announced; sent %d", n)
	}
}
