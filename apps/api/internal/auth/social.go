package auth

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// SocialIdentity is what a provider told us about the person signing in, after
// the provider's response has been cryptographically verified (an OIDC ID token
// checked for signature, audience, expiry and nonce; or, for a non-OIDC
// provider like GitHub, a token exchanged over TLS and used against the
// provider's own API).
//
// EmailVerified is the PROVIDER's assertion. It is not our own, and the two are
// not interchangeable: see SignInWithSocial.
type SocialIdentity struct {
	Provider      string // "google" | "github" | "oidc"
	Subject       string // the provider's immutable id for this person
	Issuer        string // only meaningful for the generic "oidc" provider
	Email         string
	EmailVerified bool
	Name          string
}

// SignInWithSocial resolves an external identity to a session.
//
// THE RULES, AND WHY EACH ONE EXISTS.
//
// An external identity is (provider, subject). That pair, and only that pair,
// authenticates. Email never does. Emails change, get reassigned inside a
// Workspace when an employee leaves, and repeat across providers, so treating a
// matching address as proof of the same person is the mistake that account
// takeovers are built on.
//
// Email decides one narrower question: may this NEW identity be attached to an
// account that already exists? The answer needs BOTH sides to be verified, and
// the two verifications mean different things:
//
//   - identity.EmailVerified is the provider saying it controls the address.
//     Google asserts this as a claim. GitHub has no such claim, so the adapter
//     must read /user/emails and take the entry that is both primary and
//     verified. Without this check anyone who can obtain a validly signed token
//     carrying an arbitrary address, which a Workspace administrator can, could
//     claim any account on this instance.
//
//   - user.EmailVerified() is US having seen the human open a link we sent to
//     that address.
//
// The four cases, which follow the pattern the identity industry converged on:
//
//	provider verified + local verified   -> link, sign in
//	provider unverified                  -> refuse, whatever the local state
//	provider verified + local UNVERIFIED -> REFUSE, and this is the subtle one
//	no local account at all              -> create, if the provider verified it
//
// The third case is the attack worth spelling out. An attacker registers
// victim@company.com with a password. Registration deliberately does not
// require proving control of the address, so the row exists in 'pending'. The
// victim later clicks "Sign in with Google", Google truthfully asserts the
// address is verified, and a naive implementation links the victim's Google
// identity onto the attacker's row. The attacker knows that account's password.
// Refusing here costs a legitimate user one password reset and closes the hole.
//
// The status gate at the top applies to EVERY path. Its absence was a real bug:
// the password path has always refused 'pending' and 'disabled' accounts, and
// this path refused neither, so an administrator could disable a user and that
// user could still sign in through SSO.
// socialAction is what the policy decided to do with an inbound identity.
type socialAction int

const (
	socialSignIn       socialAction = iota // known identity, just sign in
	socialLinkExisting                     // attach to the account found by email
	socialCreate                           // no account here yet
)

// decideSocial IS THE POLICY. It is a pure function of the facts, deliberately
// separated from the database work, because everything that makes social
// sign-in safe or unsafe is decided here and this is what the tests exercise
// exhaustively. Plumbing bugs cost a request; a bug in this function costs an
// account.
//
// identityUser is the account already bound to (provider, subject), if any.
// emailUser is the account holding the provider's asserted email, if any.
func decideSocial(in SocialIdentity, identityUser, emailUser *User) (socialAction, error) {
	// A known identity authenticates on its own. Email is not consulted, so a
	// provider changing the address on an existing identity can never move the
	// session to a different account.
	if identityUser != nil {
		if err := socialStatusGate(*identityUser); err != nil {
			return 0, err
		}
		return socialSignIn, nil
	}

	// Unknown identity. From here the only question is whether it may attach to
	// something that exists, and the provider must vouch for the address before
	// that question can even be asked.
	if in.Email == "" || !in.EmailVerified {
		return 0, domain.Forbidden(
			"social_email_unverified",
			"your "+providerLabel(in.Provider)+" account has no verified email address. Verify your email with "+providerLabel(in.Provider)+" and try again.",
		)
	}

	if emailUser == nil {
		return socialCreate, nil
	}
	if err := socialStatusGate(*emailUser); err != nil {
		return 0, err
	}
	if !emailUser.EmailVerified() {
		// The takeover case. See the type comment above SignInWithSocial.
		return 0, domain.Forbidden(
			"social_link_requires_verification",
			"an account already exists for this email address but has never been verified. Sign in with your password, or reset it, to confirm the account is yours before connecting "+providerLabel(in.Provider)+".",
		)
	}
	return socialLinkExisting, nil
}

// SignInWithSocial resolves an external identity to a session by loading the
// facts, asking decideSocial what to do, and doing it.
//
// THE RULES, AND WHY EACH ONE EXISTS.
//
// An external identity is (provider, subject). That pair, and only that pair,
// authenticates. Email never does. Emails change, get reassigned inside a
// Workspace when an employee leaves, and repeat across providers, so treating a
// matching address as proof of the same person is the mistake that account
// takeovers are built on.
//
// Email decides one narrower question: may this NEW identity be attached to an
// account that already exists? The answer needs BOTH sides verified, and the
// two verifications mean different things:
//
//   - in.EmailVerified is the PROVIDER saying it controls the address. Google
//     asserts this as a claim. GitHub has no such claim, so its adapter must
//     read /user/emails and take the entry that is both primary and verified.
//     Without the check, anyone able to obtain a validly signed token carrying
//     an arbitrary address, which a Workspace administrator can, could claim
//     any account on this instance.
//
//   - user.EmailVerified() is US having seen the human open a link we sent.
//
// The cases:
//
//	provider verified + local verified   -> link, sign in
//	provider unverified                  -> refuse, whatever the local state
//	provider verified + local UNVERIFIED -> REFUSE, and this is the subtle one
//	no local account at all              -> create
//
// The third case is the attack worth spelling out. An attacker registers
// victim@company.com with a password. Registration deliberately does not
// require proving control of the address, so the row exists in 'pending'. The
// victim later clicks "Sign in with Google", Google truthfully asserts the
// address is verified, and a naive implementation links the victim's Google
// identity onto the attacker's row, which the attacker knows the password to.
// Refusing costs a legitimate user one password reset and closes the hole.
//
// The status gate applies on EVERY path. Its absence was a real bug: the
// password path has always refused 'pending' and 'disabled' accounts and this
// path refused neither, so an administrator could disable a user and that user
// could still sign in through SSO.
func (s *Service) SignInWithSocial(
	ctx context.Context,
	in SocialIdentity,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
) (LoginResult, error) {
	in.Email = normalizeEmail(in.Email)

	var identityUser *User
	if u, err := s.repo.GetUserByIdentity(ctx, in.Provider, in.Subject, in.Issuer); err == nil {
		identityUser = &u
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		return LoginResult{}, err
	}

	// Only looked up when it can matter, so an unverified provider email never
	// even probes for the existence of an account.
	var emailUser *User
	if identityUser == nil && in.Email != "" && in.EmailVerified {
		if u, err := s.repo.GetUserByEmail(ctx, in.Email); err == nil {
			emailUser = &u
		} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
			return LoginResult{}, err
		}
	}

	action, err := decideSocial(in, identityUser, emailUser)
	if err != nil {
		return LoginResult{}, err
	}

	switch action {
	case socialSignIn:
		s.repo.TouchIdentityLogin(ctx, in.Provider, in.Subject, in.Issuer, in.Email, in.EmailVerified)
		return s.finishSocialLogin(ctx, *identityUser, createTenant)

	case socialLinkExisting:
		if err := s.repo.CreateIdentity(ctx, Identity{
			UserID: emailUser.ID, Provider: in.Provider, Subject: in.Subject,
			Issuer: in.Issuer, Email: in.Email, EmailVerified: in.EmailVerified,
		}); err != nil {
			return LoginResult{}, err
		}
		s.recordSocialAudit(ctx, emailUser.ID, in.Provider, "link")
		return s.finishSocialLogin(ctx, *emailUser, createTenant)

	default:
		u, err := s.createSocialUser(ctx, in, createTenant)
		if err != nil {
			return LoginResult{}, err
		}
		s.recordSocialAudit(ctx, u.ID, in.Provider, "register")
		return s.finishSocialLogin(ctx, u, createTenant)
	}
}

// socialStatusGate refuses the same account states the password path refuses.
// Its absence on this path meant a disabled user could still sign in via SSO.
func socialStatusGate(u User) error {
	switch u.Status {
	case "pending":
		// Reachable when a password account was created but never verified and
		// the identity was linked under an earlier release.
		return domain.Forbidden("email_not_verified", "please verify your email address before signing in")
	case "disabled":
		return domain.Forbidden("account_disabled", "this account is disabled")
	}
	return nil
}

func providerLabel(p string) string {
	switch p {
	case "google":
		return "Google"
	case "github":
		return "GitHub"
	default:
		return "your identity provider"
	}
}

func (s *Service) recordSocialAudit(ctx context.Context, userID uuid.UUID, provider, action string) {
	memberships, _ := s.repo.ListMembershipsForUser(ctx, userID)
	var tenantID uuid.UUID
	if len(memberships) > 0 {
		tenantID = memberships[0].TenantID
	}
	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    userID.String(),
		Action:     audit.ActionOIDCLogin,
		TargetType: "user",
		TargetID:   userID.String(),
		Metadata:   map[string]any{"provider": provider, "event": action},
	})
}

// createSocialUser creates an ACTIVE user plus their identity, and bootstraps a
// tenant. The account is active because the provider verified the address; see
// SignInWithSocial.
func (s *Service) createSocialUser(
	ctx context.Context,
	in SocialIdentity,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
) (User, error) {
	// Empty password hash: this account has no password until the user sets
	// one. The old (issuer, subject) columns are left empty on purpose; this
	// release writes identities to user_identities, and populating both would
	// create two sources of truth that can disagree.
	u, err := s.repo.CreateUser(ctx, in.Email, "", in.Name, "", "")
	if err != nil {
		return User{}, err
	}
	// Active immediately. The provider has already proven control of the
	// address, so mailing our own verification link would ask the user to prove
	// a second time what a trusted provider just proved.
	if err := s.repo.MarkUserEmailVerified(ctx, u.ID); err != nil {
		return User{}, err
	}
	u.Status = "active"
	now := time.Now().UTC()
	u.EmailVerifiedAt = &now
	if err := s.repo.CreateIdentity(ctx, Identity{
		UserID: u.ID, Provider: in.Provider, Subject: in.Subject,
		Issuer: in.Issuer, Email: in.Email, EmailVerified: in.EmailVerified,
	}); err != nil {
		return User{}, err
	}

	// The organisation name is derived the same way the password signup derives
	// it, so an account's name does not depend on how the person got in. A
	// provider that gave us a display name is a better source than the email,
	// so it wins when present.
	tenantName := defaultTenantName(in.Email)
	tenantSlug := uniqueTenantSlug(in.Email)
	tenantID, err := createTenant(ctx, tenantName, tenantSlug)
	if err != nil {
		return User{}, err
	}
	if _, err := s.repo.CreateMembership(ctx, u.ID, tenantID, authz.RoleOwner); err != nil {
		return User{}, err
	}
	return u, nil
}

// finishSocialLogin resolves memberships and the active tenant, mirroring the
// tail of the password login path.
func (s *Service) finishSocialLogin(
	ctx context.Context,
	u User,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
) (LoginResult, error) {
	memberships, _ := s.repo.ListMembershipsForUser(ctx, u.ID)
	if len(memberships) == 0 {
		// A user with no membership cannot do anything. This mirrors the
		// existing OIDC bootstrap, but is no longer gated on being the first
		// user on the instance: a social signup that created a tenant above
		// already has one, and anything reaching here without one is a repair.
		tenantID, terr := createTenant(ctx, defaultTenantName(u.Email), uniqueTenantSlug(u.Email))
		if terr == nil {
			if m, merr := s.repo.CreateMembership(ctx, u.ID, tenantID, authz.RoleOwner); merr == nil {
				memberships = []Membership{m}
			}
		}
	}

	_ = s.repo.TouchLogin(ctx, u.ID)
	s.recordLogin(ctx, memberships, u.ID, audit.ActionLoginSuccess)

	res := LoginResult{User: u, Memberships: memberships}
	res.ActiveTenant = s.resolveActiveTenant(ctx, u.ID, memberships)
	return res, nil
}
