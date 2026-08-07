package auth

import (
	"context"
	"strings"
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

// operatorConfigured reports whether the issuer was chosen by whoever runs this
// install, rather than being a provider anyone can open an account with.
//
// This distinction is narrow and load-bearing. Google and GitHub are consumer
// providers: anybody can create an account there, so an address they will not
// vouch for is worth nothing to us. The generic "oidc" provider is an issuer the
// operator configured in their own environment variables, usually a corporate
// IdP, and some of those do not return an email claim at all. Refusing those
// outright would break every existing SSO install for no security gain, because
// the operator already decided to trust that issuer.
//
// What it does NOT relax: linking. An operator-configured issuer still cannot
// attach an identity to a local account that has never been verified, and the
// status gate still applies. Trusting an issuer to say who its own users are is
// not the same as trusting it to claim an address on this install.
func operatorConfigured(provider string) bool { return provider == "oidc" }

// adoptableLegacyUser decides whether a pre-m110 users.oidc_subject row may be
// treated as the person signing in. Pure, and separate from the database work,
// for the same reason decideSocial is: the whole safety of the repair is this
// one rule.
//
// EXACTLY ONE HOLDER, OR NOBODY. The old unique index was on (oidc_issuer,
// oidc_subject), so a subject can legitimately be held by two users under two
// issuers. Once issuer is out of the key that subject identifies neither of
// them, and adopting the first row, or the newest, or any other tiebreak, is a
// coin flip that hands one person the other's account. There is no tiebreak
// that is safe here, so ambiguity resolves to "no match" and the sign-in falls
// through to the ordinary policy, which will refuse or create but never guess.
func adoptableLegacyUser(holders []User) (User, bool) {
	if len(holders) != 1 {
		return User{}, false
	}
	return holders[0], true
}

// legacySlotTaken reports whether some OTHER user already holds this exact
// (issuer, subject) pair on the legacy users columns.
//
// createSocialUser mirrors new generic-OIDC accounts into those columns so a
// rollback still recognises them. That mirror is a convenience; the sign-in
// itself is not. Writing it blind made the convenience fatal: users_oidc_identity_key
// is unique, so creating an account whose pair was already taken failed the
// whole sign-in with a duplicate-key error rather than declining to mirror.
// Reachable exactly in the ambiguous case above, where an account genuinely has
// to be created and the legacy slot genuinely is occupied.
func legacySlotTaken(in SocialIdentity, holders []User) bool {
	for _, u := range holders {
		if u.OIDCIssuer == in.Issuer {
			return true
		}
	}
	return false
}

// decideSocial IS THE POLICY, for every provider including the generic OIDC one.
// It is a pure function of the facts, deliberately separated from the database
// work, because everything that makes social sign-in safe or unsafe is decided
// here and this is what the tests exercise exhaustively. Plumbing bugs cost a
// request; a bug in this function costs an account.
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

	// LINKING. Only ever considered when the provider vouches for the address,
	// and only ever permitted when this install has verified it too.
	if in.Email != "" && in.EmailVerified && emailUser != nil {
		if err := socialStatusGate(*emailUser); err != nil {
			return 0, err
		}
		if !emailUser.EmailVerified() {
			// The takeover. See the comment on SignInWithSocial.
			return 0, domain.Forbidden(
				"social_link_requires_verification",
				"an account already exists for this email address but it has never been verified. "+
					"We have sent a verification link to that address; open it, then sign in with "+
					providerLabel(in.Provider)+" again.",
			)
		}
		return socialLinkExisting, nil
	}

	// CREATING. An address the provider will not vouch for cannot seed an
	// account on a consumer provider: somebody could otherwise squat an address
	// they do not own and collect the real owner's later sign-in.
	if in.Email == "" || !in.EmailVerified {
		if !operatorConfigured(in.Provider) {
			return 0, domain.Forbidden(
				"social_email_unverified",
				"your "+providerLabel(in.Provider)+" account has no verified email address. "+
					"Verify your email with "+providerLabel(in.Provider)+" and try again.",
			)
		}
		// Operator-configured issuer with no usable address: a distinct account
		// under a synthesised address. It cannot collide with or squat a real
		// one, which is what makes creating here safe when linking is not.
		return socialCreate, nil
	}

	// Verified address, and nobody holds it.
	return socialCreate, nil
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
	if u, err := s.repo.GetUserByIdentity(ctx, in.Provider, in.Subject); err == nil {
		identityUser = &u
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		return LoginResult{}, err
	}

	// THE PRE-m110 IDENTITY, WHICH A MIGRATION CANNOT BE TRUSTED TO HAVE MOVED.
	//
	// m110 copied users.oidc_subject into user_identities once. A backfill runs
	// once by construction: schema_migrations records it and it is never
	// revisited. So it could only ever see the rows that existed at that
	// instant, and anything the PREVIOUS release wrote afterwards, during a
	// rollback window, has legacy columns and no identity row for good.
	//
	// The consequence is not a cosmetic gap. That release never wrote
	// email_verified_at, so such an account looks never-verified, and the
	// takeover defence correctly declines to link a social identity onto a
	// never-verified account. A correct rule applied to the wrong population:
	// people who have signed in with SSO for months are refused at the door,
	// where the release before this one signed them straight in.
	//
	// So the legacy columns are consulted here and the missing row is written,
	// which fixes the gap whatever order the deploy and the rollback happened
	// in. Only for an operator-configured issuer: Google and GitHub never wrote
	// these columns, so for them a match could only ever be a subject collision
	// between two different people.
	var legacySubjectHolders []User
	if identityUser == nil && operatorConfigured(in.Provider) {
		holders, err := s.repo.ListUsersByLegacyOIDCSubject(ctx, in.Subject)
		if err != nil {
			return LoginResult{}, err
		}
		legacySubjectHolders = holders
		if u, ok := adoptableLegacyUser(holders); ok {
			// Best effort by design. If this write loses a race it is a no-op
			// and the next sign-in repeats it; what must not happen is a
			// bookkeeping failure turning into a failed login, because the
			// legacy columns have already told us who this is.
			_ = s.repo.AdoptLegacyIdentity(ctx, Identity{
				UserID: u.ID, Provider: in.Provider, Subject: in.Subject,
				Issuer: in.Issuer, Email: in.Email, EmailVerified: in.EmailVerified,
			})
			identityUser = &u
		}
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
		// THE REFUSAL HAS TO LEAVE A WAY OUT. Declining to link onto an
		// unverified account is correct, but on its own it is a dead end:
		// email_verified_at is written by exactly one query, reachable only by
		// opening a verification link, and neither signing in with a password
		// nor resetting one touches it. Without this the refusal repeats
		// forever, and it would hit the first user on every install, everyone
		// who arrived by invitation, and every pre-existing SSO user, because
		// users.status defaults to 'active' while email_verified_at defaults
		// NULL.
		//
		// So the refusal SENDS the link that resolves it. Rate limited on the
		// address, because reaching this point costs an attacker nothing.
		if de, ok := domain.AsDomain(err); ok && de.Code == "social_link_requires_verification" && emailUser != nil {
			s.sendVerificationForSocialLink(ctx, *emailUser)
		}
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
		u, err := s.createSocialUser(ctx, in, legacySlotTaken(in, legacySubjectHolders))
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
	legacyTaken bool,
) (User, error) {
	email := in.Email
	verified := in.EmailVerified
	if email == "" || !verified {
		// Only reachable for an operator-configured issuer (decideSocial refuses
		// this for consumer providers). A synthesised address satisfies the
		// unique-email constraint without claiming an address the holder has not
		// proven, so it can neither collide with a real account nor squat one.
		email = normalizeEmail(in.Subject + "@" + syntheticEmailDomain(in))
		verified = false
	}

	// The legacy users.oidc_* columns are written for the generic OIDC provider
	// only, so a rollback to the previous release still recognises anyone who
	// first signed in during the rollout. Google and GitHub have no
	// representation there and deliberately get none: inventing one would put
	// two providers into a single slot that only holds one.
	//
	// Skipped when another user already holds the pair, because that column pair
	// is UNIQUE and this mirror is a courtesy to a rollback that may never
	// happen. Insisting on it turned a creatable account into a duplicate-key
	// failure and a broken sign-in.
	legacyIssuer, legacySubject := "", ""
	if operatorConfigured(in.Provider) && !legacyTaken {
		legacyIssuer, legacySubject = in.Issuer, in.Subject
	}

	u, err := s.repo.CreateUser(ctx, email, "", in.Name, legacyIssuer, legacySubject)
	if err != nil {
		return User{}, err
	}
	// Active immediately ONLY when a provider actually vouched for the address.
	// Mailing our own verification link would otherwise ask the user to prove a
	// second time what a trusted provider just proved.
	if verified {
		if err := s.repo.MarkUserEmailVerified(ctx, u.ID); err != nil {
			return User{}, err
		}
		now := time.Now().UTC()
		u.EmailVerifiedAt = &now
	}
	u.Status = "active"

	if err := s.repo.CreateIdentity(ctx, Identity{
		UserID: u.ID, Provider: in.Provider, Subject: in.Subject,
		Issuer: in.Issuer, Email: email, EmailVerified: verified,
	}); err != nil {
		return User{}, err
	}

	// Bootstrapping the organisation is left to finishSocialLogin, which owns
	// the first-user gate. Creating one here as well would grant an org to every
	// collaborator and portal user who signs in socially.
	return u, nil
}

// syntheticEmailDomain keeps a placeholder address obviously non-routable, for
// the one case that needs one: an operator-configured IdP that returns no email
// claim, where the unique-email constraint still has to be satisfied by
// something.
//
// IT IS SCOPED TO THE PROVIDER, NOT THE ISSUER, AND THAT IS THE WHOLE POINT.
// Deriving the domain from the issuer host made the address a function of
// configuration: repointing WPMGR_OIDC_ISSUER changed the address the same
// subject would be given, so the same human came back and got a second account,
// silently, with nothing to reconcile the two on. An account's own address is
// not something an environment variable should be able to move.
//
// This cannot be used to claim or collide with a real address: .invalid is the
// reserved TLD that resolves nowhere, and the row is created with
// email_verified false, which the linking rules already refuse to act on.
//
// The remaining fork is genuinely not solvable here. If an IdP asserts no
// address, a person arriving on a SECOND provider brings nothing that could tie
// them to the first account, and inventing a link would mean guessing. That is
// what an explicit "link another sign-in method" flow on an already
// authenticated session is for, not the sign-in path.
func syntheticEmailDomain(in SocialIdentity) string {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = "sso"
	}
	return provider + ".invalid"
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
		// ZERO MEMBERSHIPS IS A NORMAL STATE, NOT A FAULT TO REPAIR. It is what
		// a site collaborator has (an active site_share and nothing else), what
		// a client-portal user has (a client_member row and nothing else), and
		// what anyone whose only organisation is inside its soft-delete grace
		// window has.
		//
		// An earlier version of this function created a tenant and granted
		// RoleOwner whenever the list came back empty, which would have handed a
		// portal user org-level capabilities the product deliberately withholds,
		// activated that new empty org instead of their portal tenant (see
		// resolveActiveTenant, which takes memberships[0] first) and, on a
		// hosted install, minted an unbilled organisation on demand.
		//
		// So the bootstrap is gated exactly as UpsertOIDCUser gated it: only
		// when this is genuinely the only user on the install.
		if count, cerr := s.repo.CountUsers(ctx); cerr == nil && count <= 1 {
			tenantID, terr := createTenant(ctx, defaultTenantName(u.Email), uniqueTenantSlug(u.Email))
			if terr == nil {
				if m, merr := s.repo.CreateMembership(ctx, u.ID, tenantID, authz.RoleOwner); merr == nil {
					memberships = []Membership{m}
				}
			}
		}
	}

	_ = s.repo.TouchLogin(ctx, u.ID)
	s.recordLogin(ctx, memberships, u.ID, audit.ActionLoginSuccess)

	res := LoginResult{User: u, Memberships: memberships}
	res.ActiveTenant = s.resolveActiveTenant(ctx, u.ID, memberships)
	return res, nil
}

// sendVerificationForSocialLink mails the link that unblocks a refused social
// link. Best effort and deliberately silent: whether an address received mail
// is not something an unauthenticated caller should be able to observe.
func (s *Service) sendVerificationForSocialLink(ctx context.Context, u User) {
	if s.limiter != nil {
		if ok, _ := s.limiter.Allow(ctx, "verify-social-link:"+u.Email, forgotPerMinute); !ok {
			return
		}
	}
	s.sendVerificationEmail(ctx, u.ID, u.Email, u.Name, "")
}
