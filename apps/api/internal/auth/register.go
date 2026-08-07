package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// verifyTokenTTL bounds how long an email-verification link is valid.
const verifyTokenTTL = 7 * 24 * time.Hour

// RegisterSelfServe creates a self-registered, UNVERIFIED account (status
// 'pending') with its own tenant + owner membership and emails a verification
// link. It is enumeration-safe: it ALWAYS returns nil, and the HTTP handler
// returns an identical generic response whether or not the email already
// exists (a duplicate is silently ignored — no account is created and no email
// is sent). No session is established; the user must verify first.
func (s *Service) RegisterSelfServe(
	ctx context.Context,
	in RegisterInput,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
) error {
	in.Email = normalizeEmail(in.Email)
	if err := s.validator.Struct(in); err != nil {
		// Surface validation (weak password / bad email) so the form can react;
		// this does not leak existence.
		return err
	}

	if existing, err := s.repo.GetUserByEmail(ctx, in.Email); err == nil {
		// Already registered: stay generic to the HTTP caller, but nudge the real
		// owner by email so an existing user (e.g. a former collaborator) knows to
		// sign in / reset instead of being silently stuck. Rate-limited per email
		// so register cannot be abused to spam a known address.
		s.sendAccountExists(ctx, existing.Email, existing.Name)
		return nil
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		return nil // unexpected error: stay generic, never leak
	}

	hash, err := HashPassword(in.Password)
	if err != nil {
		return domain.Internal("password_hash_failed", "failed to hash password").WithCause(err)
	}
	tenantName := strings.TrimSpace(in.TenantName)
	if tenantName == "" {
		tenantName = defaultTenantName(in.Email)
	}
	// Tenant slugs are GLOBALLY unique, so a fixed "default" collides after the
	// first self-serve signup (tenant_slug_exists 409). Derive a unique slug.
	tenantSlug := in.TenantSlug
	if tenantSlug == "" {
		tenantSlug = uniqueTenantSlug(in.Email)
	}
	tenantID, err := createTenant(ctx, tenantName, tenantSlug)
	if err != nil {
		return err
	}
	u, err := s.repo.CreateUser(ctx, in.Email, hash, in.Name, "", "")
	if err != nil {
		return err
	}
	if _, err := s.repo.CreateMembership(ctx, u.ID, tenantID, authz.RoleOwner); err != nil {
		return err
	}
	// Mark pending until the email is verified.
	if err := s.repo.SetUserPending(ctx, u.ID); err != nil {
		return err
	}
	intent := s.validatePlanIntent(in.Plan)
	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID: tenantID, ActorType: audit.ActorUser, ActorID: u.ID.String(),
		Action: audit.ActionRegister, TargetType: "user", TargetID: u.ID.String(),
		Metadata: map[string]any{"self_serve": true, "pending": true, "desired_plan": intent},
	})

	s.sendVerificationEmail(ctx, u.ID, u.Email, u.Name, intent)
	return nil
}

// consumerMailDomains are providers where the domain says nothing about the
// organization, so the local part is the better guess. Not exhaustive on
// purpose: an unlisted provider yields a slightly odd name, which is a cosmetic
// miss on a field the owner can rename, not a failure.
var consumerMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true,
	"hotmail.com": true, "live.com": true, "msn.com": true,
	"yahoo.com": true, "ymail.com": true, "icloud.com": true,
	"me.com": true, "mac.com": true, "aol.com": true,
	"proton.me": true, "protonmail.com": true, "pm.me": true,
	"gmx.com": true, "gmx.net": true, "mail.com": true, "zoho.com": true,
	"yandex.com": true, "fastmail.com": true, "hey.com": true,
}

// defaultTenantName derives a readable organization name from the signup email.
//
// The signup form asks for two fields, email and password, so this default is
// what every self-serve account is actually named. It used to be the literal
// string "Default", which was tolerable while the form still offered an
// organization field and is not once that field is gone: every account on the
// instance would share one meaningless name, in the switcher, in client
// reports, and in the admin console.
//
// A work address carries the organization, so acme.com becomes "Acme". A
// consumer mailbox does not, so the local part is used instead and
// sarah.jones@gmail.com becomes "Sarah Jones". Both are guesses, and both are
// renameable in settings; the goal is a sensible starting point rather than a
// correct one.
func defaultTenantName(email string) string {
	local, domainPart, ok := strings.Cut(email, "@")
	if !ok || strings.TrimSpace(local) == "" {
		return "My organization"
	}
	source := local
	domainPart = strings.ToLower(strings.TrimSpace(domainPart))
	if domainPart != "" && !consumerMailDomains[domainPart] {
		source = registrableLabel(domainPart)
	}
	return titleizeName(source)
}

// secondLevelSuffixes are the common two-part public suffixes. Taking the
// second-to-last label of "acme.co.uk" yields "co", so these have to be
// stepped over. This is a short pragmatic list rather than the full public
// suffix list, which would mean vendoring and refreshing a large dataset to
// improve the default value of a renameable display name.
var secondLevelSuffixes = map[string]bool{
	"co": true, "com": true, "net": true, "org": true, "gov": true,
	"edu": true, "ac": true, "or": true, "ne": true, "in": true, "gen": true,
}

// registrableLabel picks the label that identifies the organization:
// "mail.acme.com" and "acme.co.uk" both yield "acme".
func registrableLabel(domainPart string) string {
	labels := strings.Split(domainPart, ".")
	if len(labels) < 2 {
		return labels[0]
	}
	i := len(labels) - 2
	// Step over a two-part suffix, but only when a label remains in front of
	// it: "co.uk" on its own must not walk off the start of the slice.
	if secondLevelSuffixes[labels[i]] && i > 0 {
		i--
	}
	return labels[i]
}

// titleizeName turns "sarah.jones" or "sarah_jones-01" into "Sarah Jones 01".
func titleizeName(raw string) string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '+' || r == ' '
	})
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		runes := []rune(strings.ToLower(f))
		if len(runes) == 0 {
			continue
		}
		runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
		parts = append(parts, string(runes))
	}
	out := strings.Join(parts, " ")
	if out == "" {
		return "My organization"
	}
	if len([]rune(out)) > 200 {
		out = string([]rune(out)[:200])
	}
	return out
}

// uniqueTenantSlug builds a globally-unique tenant slug from the email's local
// part plus a short random suffix, so self-serve signups never collide on the
// tenant slug (which has a UNIQUE constraint with no auto-uniquification).
func uniqueTenantSlug(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		local = email[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "org"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	suf := make([]byte, 4)
	_, _ = rand.Read(suf)
	return base + "-" + hex.EncodeToString(suf)
}

// VerifyEmail consumes a verification token, activates the account, and returns
// a LoginResult so the caller can establish a session (the user lands logged
// in). A bad/expired/used token yields Gone (410).
func (s *Service) VerifyEmail(ctx context.Context, token string) (LoginResult, error) {
	if strings.TrimSpace(token) == "" {
		return LoginResult{}, domain.Gone("verification_token_invalid", "this verification link is invalid or has expired")
	}
	hash := sha256Sum(token)
	var userID uuid.UUID
	var intent string
	consumeErr := s.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).ConsumeEmailVerificationToken(ctx, hash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errResetTokenInvalid
			}
			return err
		}
		userID = row.UserID
		if row.DesiredPlan != nil {
			intent = *row.DesiredPlan
		}
		return nil
	})
	if consumeErr != nil {
		if errors.Is(consumeErr, errResetTokenInvalid) {
			return LoginResult{}, domain.Gone("verification_token_invalid", "this verification link is invalid or has expired")
		}
		return LoginResult{}, domain.Internal("verify_failed", "could not verify the email")
	}

	if err := s.repo.MarkUserEmailVerified(ctx, userID); err != nil {
		return LoginResult{}, domain.Internal("verify_write_failed", "could not activate the account").WithCause(err)
	}
	_ = s.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).InvalidateUserEmailVerificationTokens(ctx, userID)
	})

	u, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		return LoginResult{}, err
	}
	memberships, _ := s.repo.ListMembershipsForUser(ctx, userID)
	_ = s.repo.TouchLogin(ctx, userID)
	res := LoginResult{User: u, Memberships: memberships, DesiredPlan: intent}
	res.ActiveTenant = s.resolveActiveTenant(ctx, userID, memberships)
	return res, nil
}

// ResendVerification re-issues a verification link if the email maps to a
// pending account. Generic + rate-limited; always returns nil.
func (s *Service) ResendVerification(ctx context.Context, email string) error {
	email = normalizeEmail(email)
	if email == "" {
		return nil
	}
	if s.limiter != nil {
		if ok, _ := s.limiter.Allow(ctx, "verify-resend:"+email, forgotPerMinute); !ok {
			return nil
		}
	}
	u, err := s.repo.GetUserByEmail(ctx, email)
	// Gated on "not yet verified", NOT on status == 'pending'. Those are not the
	// same set: users.status defaults to 'active' while email_verified_at
	// defaults NULL, so the first user on an install, everyone created by
	// invitation and every pre-existing SSO user is active and unverified. The
	// old condition left all of them with no way to ever verify, which social
	// account linking then refused them for.
	if err != nil || u.Status == "disabled" || u.EmailVerified() {
		return nil
	}
	s.sendVerificationEmail(ctx, u.ID, u.Email, u.Name, s.priorDesiredPlan(ctx, u.ID))
	return nil
}

// priorDesiredPlan looks up the most recent desired_plan captured across this
// user's verification tokens (active or already consumed/invalidated), so a
// resent verification link (ResendVerification, above) carries the SAME
// intent forward onto its freshly minted token instead of losing it when the
// prior token is invalidated. Best-effort: any lookup failure — including
// "no token exists yet" — resolves to "", always a safe default.
func (s *Service) priorDesiredPlan(ctx context.Context, userID uuid.UUID) string {
	var intent string
	_ = s.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		got, err := sqlc.New(tx).GetLatestDesiredPlanForUser(ctx, userID)
		if err != nil {
			return nil // no rows yet: intent stays ""
		}
		if got != nil {
			intent = *got
		}
		return nil
	})
	return intent
}

// sendAccountExists nudges an existing user who tried to re-register: sign in or
// reset, never "set up". Rate-limited per email so register cannot be turned
// into an email bomb against a known address. Best-effort.
func (s *Service) sendAccountExists(ctx context.Context, email, name string) {
	if s.email == nil {
		return
	}
	if s.limiter != nil {
		if ok, _ := s.limiter.Allow(ctx, "register-exists:"+email, forgotPerMinute); !ok {
			return
		}
	}
	_ = s.email.Enqueue(ctx, uuid.Nil, []string{email}, "account_exists", map[string]any{
		"Name":     name,
		"LoginURL": s.baseURL + "/login",
		"ResetURL": s.baseURL + "/forgot-password",
	})
}

// sendVerificationEmail mints a verification token + enqueues the verify_email
// template. desiredPlan is the M16 Phase 0 "sign up into a plan" hint to carry
// on the new token ("" for none); the caller has already resolved it (either
// freshly, via validatePlanIntent, or carried forward, via priorDesiredPlan).
// Best-effort.
func (s *Service) sendVerificationEmail(ctx context.Context, userID uuid.UUID, email, name, desiredPlan string) {
	raw, hash, gerr := newResetToken()
	if gerr != nil {
		return
	}
	var desiredPlanArg *string
	if desiredPlan != "" {
		desiredPlanArg = &desiredPlan
	}
	txErr := s.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if err := q.InvalidateUserEmailVerificationTokens(ctx, userID); err != nil {
			return err
		}
		_, err := q.InsertEmailVerificationToken(ctx, sqlc.InsertEmailVerificationTokenParams{
			UserID:      userID,
			TokenHash:   hash,
			ExpiresAt:   time.Now().Add(verifyTokenTTL),
			DesiredPlan: desiredPlanArg,
		})
		return err
	})
	if txErr != nil || s.email == nil {
		return
	}
	link := s.baseURL + "/verify-email?token=" + raw
	_ = s.email.Enqueue(ctx, uuid.Nil, []string{email}, "verify_email", map[string]any{
		"Name":         name,
		"VerifyURL":    link,
		"ExpiresHours": "168",
	})
}
