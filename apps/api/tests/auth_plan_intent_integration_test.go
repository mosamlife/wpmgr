// auth_plan_intent_integration_test.go — M16 "sign up into a plan" Phase 0.
//
// Covers: a validated paid-tier hint survives self-serve registration on the
// email-verification token, is surfaced (and consumed) exactly once by
// VerifyEmail, a resend carries the same intent forward onto its new token,
// a non-paid-tier hint (free/unknown/empty, or no validator wired at all —
// the self-host shape) is never persisted, and the first-run bootstrap path
// echoes a validated hint straight back in its immediate-session response.
package tests

import (
	"context"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// newAuthStackWithBilling builds an auth.Service wired to a REAL hosted
// billing.Service as its PaidTierValidator (mirrors newAuthStack in
// auth_integration_test.go, plus the M16 Phase 0 wiring cmd/wpmgr performs).
func newAuthStackWithBilling(pool *db.Pool) *auth.Service {
	svc, _ := newAuthStack(pool)
	svc.SetPlanValidator(newHostedBillingService(pool, true))
	return svc
}

// capturingMailer is a minimal auth.EmailEnqueuer double that records the
// data map of the most recent Enqueue call, so a test can pull the raw
// verification token out of the rendered VerifyURL without needing River or
// a real mailer.
type capturingMailer struct {
	lastData map[string]any
}

func (m *capturingMailer) Enqueue(_ context.Context, _ uuid.UUID, _ []string, _ string, data map[string]any) error {
	m.lastData = data
	return nil
}

// rawTokenFromVerifyURL extracts the ?token= query parameter a captured
// verify_email VerifyURL carries.
func rawTokenFromVerifyURL(t *testing.T, verifyURL string) string {
	t.Helper()
	u, err := url.Parse(verifyURL)
	if err != nil {
		t.Fatalf("parse VerifyURL %q: %v", verifyURL, err)
	}
	tok := u.Query().Get("token")
	if tok == "" {
		t.Fatalf("VerifyURL %q carried no token", verifyURL)
	}
	return tok
}

// desiredPlanForUser reads back the most recent desired_plan captured across
// a user's verification tokens directly from Postgres — a white-box check
// that registration persisted the intent SERVER-SIDE (on the token row),
// independent of anything the Service surfaces back to a caller.
func desiredPlanForUser(t *testing.T, pool *db.Pool, userID uuid.UUID) *string {
	t.Helper()
	var plan *string
	err := pool.InAgentTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT desired_plan FROM email_verification_tokens
			 WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`,
			userID,
		).Scan(&plan)
	})
	if err != nil {
		t.Fatalf("read back desired_plan for user %s: %v", userID, err)
	}
	return plan
}

func makeCreateTenant(t *testing.T, pool *db.Pool) func(ctx context.Context, name, slug string) (uuid.UUID, error) {
	return func(ctx context.Context, name, slug string) (uuid.UUID, error) {
		return seedTenant(t, pool, slug), nil
	}
}

// claimInstall gives the install its first owner, which is the precondition for
// open self-serve registration: while an install is unclaimed, that path writes
// nothing, so the first-account slot stays available to whoever holds the
// provisioning claim. Every self-serve and verify-email test below is about
// what happens on a NORMAL install, so each one claims first.
func claimInstall(t *testing.T, svc *auth.Service) {
	t.Helper()
	svc.SetBootstrapClaimSecret(testClaim)
	if _, err := svc.Bootstrap(context.Background(), auth.RegisterInput{
		Email:    "install-owner@example.com",
		Password: "a-very-strong-password",
		Name:     "Install Owner",
	}, testClaim); err != nil {
		t.Fatalf("claim install: %v", err)
	}
}

// TestRegisterSelfServe_PersistsDesiredPlanOnToken proves a validated paid-tier
// hint lands on the verification-token row created by registration.
func TestRegisterSelfServe_PersistsDesiredPlanOnToken(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := newAuthStackWithBilling(pool)
	repo := auth.NewRepo(pool)
	createTenant := makeCreateTenant(t, pool)
	claimInstall(t, svc)

	const email = "plan-agency-signup@example.com"
	if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
		Email:    email,
		Password: "a-very-strong-password",
		Name:     "Agency Signup",
		Plan:     string(billing.TierAgency),
	}, createTenant); err != nil {
		t.Fatalf("register: %v", err)
	}

	u, err := repo.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u.Status != "pending" {
		t.Fatalf("status = %q, want pending", u.Status)
	}

	got := desiredPlanForUser(t, pool, u.ID)
	if got == nil {
		t.Fatal("desired_plan on the verification token is NULL, want the agency tier")
	}
	if *got != string(billing.TierAgency) {
		t.Fatalf("desired_plan = %q, want %q", *got, billing.TierAgency)
	}
}

// TestRegisterSelfServe_NonPaidPlanHintsAreIgnored proves free, unknown, and
// absent plan hints never persist a desired_plan — "no intent" is the only
// correct outcome for each.
func TestRegisterSelfServe_NonPaidPlanHintsAreIgnored(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := newAuthStackWithBilling(pool)
	repo := auth.NewRepo(pool)
	createTenant := makeCreateTenant(t, pool)
	claimInstall(t, svc)

	cases := []struct {
		name  string
		email string
		plan  string
	}{
		{"free tier is not an intent", "plan-free-signup@example.com", string(billing.TierFree)},
		{"unrecognized value", "plan-bogus-signup@example.com", "not-a-real-tier"},
		{"absent field", "plan-absent-signup@example.com", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
				Email:    tc.email,
				Password: "a-very-strong-password",
				Name:     "Signup",
				Plan:     tc.plan,
			}, createTenant); err != nil {
				t.Fatalf("register: %v", err)
			}
			u, err := repo.GetUserByEmail(ctx, tc.email)
			if err != nil {
				t.Fatalf("get user: %v", err)
			}
			if got := desiredPlanForUser(t, pool, u.ID); got != nil {
				t.Fatalf("desired_plan = %q, want NULL (no intent)", *got)
			}
		})
	}
}

// TestRegisterSelfServe_NoPlanValidatorMeansNoIntent proves the self-host
// shape (no PaidTierValidator ever wired, i.e. Service.SetPlanValidator never
// called) is exactly as safe as an explicit free/unknown hint: a plan value
// is silently dropped rather than persisted.
func TestRegisterSelfServe_NoPlanValidatorMeansNoIntent(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool) // no SetPlanValidator call — mirrors self-host boot
	repo := auth.NewRepo(pool)
	createTenant := makeCreateTenant(t, pool)
	claimInstall(t, svc)

	const email = "plan-selfhost-signup@example.com"
	if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
		Email:    email,
		Password: "a-very-strong-password",
		Name:     "Self-host Signup",
		Plan:     string(billing.TierAgency),
	}, createTenant); err != nil {
		t.Fatalf("register: %v", err)
	}
	u, err := repo.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got := desiredPlanForUser(t, pool, u.ID); got != nil {
		t.Fatalf("desired_plan = %q, want NULL (no validator wired => no intent)", *got)
	}
}

// TestVerifyEmail_SurfacesAndConsumesDesiredPlan proves VerifyEmail reads the
// desired_plan off the token it just consumed, returns it exactly once, and
// that the token cannot be replayed to read it (or anything else) again.
func TestVerifyEmail_SurfacesAndConsumesDesiredPlan(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := newAuthStackWithBilling(pool)
	createTenant := makeCreateTenant(t, pool)
	claimInstall(t, svc)

	mailer := &capturingMailer{}
	svc.SetMailer(mailer, "https://manage.example.test", nil)

	const email = "plan-starter-verify@example.com"
	if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
		Email:    email,
		Password: "a-very-strong-password",
		Name:     "Starter Signup",
		Plan:     string(billing.TierStarter),
	}, createTenant); err != nil {
		t.Fatalf("register: %v", err)
	}
	if mailer.lastData == nil {
		t.Fatal("no verification email was enqueued")
	}
	verifyURL, _ := mailer.lastData["VerifyURL"].(string)
	token := rawTokenFromVerifyURL(t, verifyURL)

	res, err := svc.VerifyEmail(ctx, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.DesiredPlan != string(billing.TierStarter) {
		t.Fatalf("VerifyEmail DesiredPlan = %q, want %q", res.DesiredPlan, billing.TierStarter)
	}
	if res.User.Email != email {
		t.Fatalf("verified the wrong user: %q", res.User.Email)
	}

	// Single-use: the SAME token must never verify (or surface a plan) again.
	if _, err := svc.VerifyEmail(ctx, token); err == nil {
		t.Fatal("replaying a consumed verification token should fail")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindGone {
		t.Fatalf("want Gone on replay, got %v", err)
	}

	// The intent is gone with the token: nothing left to look up for this user.
	if got := desiredPlanForUser(t, pool, res.User.ID); got == nil || *got != string(billing.TierStarter) {
		t.Fatalf("desired_plan on the (now-consumed) token = %v, want it to still read back %q (row persists, only used_at flips)", got, billing.TierStarter)
	}
}

// TestVerifyEmail_NoIntentReturnsEmpty proves an ordinary (no plan hint)
// registration's verification response carries no desired_plan.
func TestVerifyEmail_NoIntentReturnsEmpty(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := newAuthStackWithBilling(pool)
	createTenant := makeCreateTenant(t, pool)
	claimInstall(t, svc)

	mailer := &capturingMailer{}
	svc.SetMailer(mailer, "https://manage.example.test", nil)

	const email = "plan-none-verify@example.com"
	if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
		Email:    email,
		Password: "a-very-strong-password",
		Name:     "Ordinary Signup",
	}, createTenant); err != nil {
		t.Fatalf("register: %v", err)
	}
	token := rawTokenFromVerifyURL(t, mailer.lastData["VerifyURL"].(string))

	res, err := svc.VerifyEmail(ctx, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.DesiredPlan != "" {
		t.Fatalf("DesiredPlan = %q, want empty for an ordinary signup", res.DesiredPlan)
	}
}

// TestResendVerification_CarriesForwardDesiredPlan proves a resend (which
// invalidates the original token and mints a new one) does not silently
// lose the original plan intent.
func TestResendVerification_CarriesForwardDesiredPlan(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := newAuthStackWithBilling(pool)
	repo := auth.NewRepo(pool)
	createTenant := makeCreateTenant(t, pool)
	claimInstall(t, svc)

	mailer := &capturingMailer{}
	svc.SetMailer(mailer, "https://manage.example.test", nil)

	const email = "plan-scale-resend@example.com"
	if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
		Email:    email,
		Password: "a-very-strong-password",
		Name:     "Scale Signup",
		Plan:     string(billing.TierScale),
	}, createTenant); err != nil {
		t.Fatalf("register: %v", err)
	}
	firstToken := rawTokenFromVerifyURL(t, mailer.lastData["VerifyURL"].(string))

	if err := svc.ResendVerification(ctx, email); err != nil {
		t.Fatalf("resend: %v", err)
	}
	secondToken := rawTokenFromVerifyURL(t, mailer.lastData["VerifyURL"].(string))
	if secondToken == firstToken {
		t.Fatal("resend should mint a NEW token, not reuse the original")
	}

	// The original token was invalidated by the resend.
	if _, err := svc.VerifyEmail(ctx, firstToken); err == nil {
		t.Fatal("the pre-resend token should no longer verify")
	}

	res, err := svc.VerifyEmail(ctx, secondToken)
	if err != nil {
		t.Fatalf("verify with the resent token: %v", err)
	}
	if res.DesiredPlan != string(billing.TierScale) {
		t.Fatalf("DesiredPlan after resend = %q, want %q (carried forward)", res.DesiredPlan, billing.TierScale)
	}

	u, err := repo.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u.Status != "active" {
		t.Fatalf("status = %q, want active after verification", u.Status)
	}
}

// TestBootstrap_SurfacesDesiredPlanImmediately proves the first-run bootstrap
// path — which never touches the verification-token table at all, since it
// issues an immediate session in the same request — still echoes a validated
// plan hint straight back on its LoginResult.
func TestBootstrap_SurfacesDesiredPlanImmediately(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := newAuthStackWithBilling(pool)
	svc.SetBootstrapClaimSecret(testClaim)

	res, err := svc.Bootstrap(ctx, auth.RegisterInput{
		Email:    "first-admin@example.com",
		Password: "a-very-strong-password",
		Name:     "First Admin",
		Plan:     string(billing.TierScale),
	}, testClaim)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.DesiredPlan != string(billing.TierScale) {
		t.Fatalf("Bootstrap DesiredPlan = %q, want %q", res.DesiredPlan, billing.TierScale)
	}
	if res.ActiveTenant == uuid.Nil {
		t.Fatal("bootstrap did not set an active tenant")
	}
}

// TestBootstrap_NonPaidPlanHintSurfacesEmpty proves a non-paid bootstrap hint
// (free tier) resolves to no intent, same as self-serve registration — on a
// SEPARATE fresh instance, since Bootstrap only ever succeeds once (count==0).
func TestBootstrap_NonPaidPlanHintSurfacesEmpty(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc := newAuthStackWithBilling(pool)
	svc.SetBootstrapClaimSecret(testClaim)

	res, err := svc.Bootstrap(ctx, auth.RegisterInput{
		Email:    "first-admin@example.com",
		Password: "a-very-strong-password",
		Name:     "First Admin",
		Plan:     string(billing.TierFree),
	}, testClaim)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.DesiredPlan != "" {
		t.Fatalf("Bootstrap DesiredPlan for a free hint = %q, want empty", res.DesiredPlan)
	}
}
