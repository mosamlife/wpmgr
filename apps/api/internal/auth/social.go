package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

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
		// THE LINK IS AUTHORIZED HERE AND WRITTEN SOMEWHERE ELSE, ON PURPOSE.
		//
		// Linking binds a new way into an existing account, so it is a change to
		// that account's credentials, and it used to be committed right here,
		// before the caller had reached the second factor. Everything after this
		// point could then fail and the binding still stood: an attacker who got
		// no further than a provider consent screen for an address the provider
		// vouches for walked away with their identity permanently attached to a
		// 2FA-protected account they never authenticated to.
		//
		// So the decision travels as data and the write happens once the login is
		// genuinely complete. See CompleteSocialLink.
		res, err := s.finishSocialLogin(ctx, *emailUser, createTenant)
		if err != nil {
			return LoginResult{}, err
		}
		res.PendingSocialLink = &Identity{
			UserID: emailUser.ID, Provider: in.Provider, Subject: in.Subject,
			Issuer: in.Issuer, Email: in.Email, EmailVerified: in.EmailVerified,
		}
		return res, nil

	default:
		// No deferral on this branch: the account is being created by this very
		// sign-in, so it cannot already carry a second factor to skip past, and
		// the identity is written in the same transaction as the user it belongs
		// to (see createSocialUser).
		u, err := s.createSocialUser(ctx, in, createTenant)
		if err != nil {
			return LoginResult{}, err
		}
		// Audited after the login is resolved, not before: finishSocialLogin is
		// what settles which tenant this account belongs to, and recording first
		// would file every new account's registration against no tenant at all.
		res, err := s.finishSocialLogin(ctx, u, createTenant)
		if err != nil {
			return LoginResult{}, err
		}
		s.recordSocialAudit(ctx, u.ID, in.Provider, "register")
		return res, nil
	}
}

// CompleteSocialLink writes a link that SignInWithSocial authorized but
// deliberately left unwritten until the caller finished authenticating.
//
// The caller must have issued a session for userID first. The userID argument
// is the session's, not the pending link's, and the mismatch check below is the
// point of taking both: a pending link parked during one login must never be
// applied to whoever completes the next one.
func (s *Service) CompleteSocialLink(ctx context.Context, userID uuid.UUID, link Identity) error {
	if link.UserID == uuid.Nil || link.UserID != userID {
		return domain.Forbidden("social_link_user_mismatch", "this sign-in cannot complete that link")
	}
	if err := s.repo.CreateIdentity(ctx, link); err != nil {
		return err
	}
	s.recordSocialAudit(ctx, userID, link.Provider, "link")
	return nil
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

// recordSocialAudit records that a provider was linked to an account, or that
// an account was created from one. Both are credential changes, so neither may
// go unrecorded.
//
// IT USED TO GO UNRECORDED EXACTLY WHERE IT MATTERED MOST. The tenant came from
// memberships[0] and fell back to the zero UUID when the list was empty;
// audit_log.tenant_id references tenants, so the insert failed the foreign key
// and a best-effort caller threw the error away. Every user with no org
// membership therefore had their provider linked in total silence, and that set
// is not an edge case: it is a brand new social account, a site collaborator, a
// portal user, and anyone whose only org is inside its soft-delete grace
// window. The accounts with the least oversight got the least audit.
//
// So the tenant is resolved the same way the session's active tenant is (org
// membership, then site share, then client membership), and when there is
// genuinely no tenant the event goes to the tenant-independent sink instead of
// being dropped.
func (s *Service) recordSocialAudit(ctx context.Context, userID uuid.UUID, provider, action string) {
	memberships, _ := s.repo.ListMembershipsForUser(ctx, userID)
	tenantID := s.resolveActiveTenant(ctx, userID, memberships)

	if tenantID != uuid.Nil {
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     audit.ActionOIDCLogin,
			TargetType: "user",
			TargetID:   userID.String(),
			Metadata:   map[string]any{"provider": provider, "event": action},
		})
		return
	}

	meta, err := json.Marshal(map[string]any{
		"provider": provider,
		"event":    action,
		"user_id":  userID.String(),
	})
	if err != nil {
		meta = nil
	}
	if err := s.repo.RecordTenantlessAuthEvent(ctx, userID, audit.ActionOIDCLogin, meta); err != nil {
		slog.ErrorContext(ctx, "social audit record failed",
			slog.String("event", action), slog.String("provider", provider),
			slog.String("user_id", userID.String()), slog.Any("error", err))
	}
}

// createSocialUser creates a user and their identity in ONE transaction. The
// account is active because the provider verified the address; see
// SignInWithSocial.
//
// The atomicity is the whole point of the helper. These were three separate
// statements against the pool, and the address is unique, so a failure between
// any two of them left an account holding that address with nothing attached to
// it: no identity, no password, and no verification. See
// Repo.CreateSocialUserTx for why that row is so hard to get out of the way.
func (s *Service) createSocialUser(
	ctx context.Context,
	in SocialIdentity,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
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
	legacyIssuer, legacySubject := "", ""
	if operatorConfigured(in.Provider) {
		legacyIssuer, legacySubject = in.Issuer, in.Subject
	}

	// Verified means active immediately, ONLY when a provider actually vouched
	// for the address. Mailing our own verification link would otherwise ask the
	// user to prove a second time what a trusted provider just proved.
	u, err := s.repo.CreateSocialUserTx(ctx, email, in.Name, legacyIssuer, legacySubject, verified, Identity{
		Provider: in.Provider, Subject: in.Subject,
		Issuer: in.Issuer, Email: email, EmailVerified: verified,
	})
	if err != nil {
		return User{}, err
	}

	// Bootstrapping the organisation is left to finishSocialLogin, which owns
	// the first-user gate. Creating one here as well would grant an org to every
	// collaborator and portal user who signs in socially.
	return u, nil
}

// syntheticEmailDomain keeps a placeholder address obviously non-routable and
// scoped to the issuer, so two issuers cannot generate the same address for the
// same subject.
func syntheticEmailDomain(in SocialIdentity) string {
	host := strings.TrimPrefix(strings.TrimPrefix(in.Issuer, "https://"), "http://")
	if i := strings.IndexAny(host, "/:"); i > 0 {
		host = host[:i]
	}
	if host == "" {
		host = in.Provider
	}
	return host + ".oidc.invalid"
}

// finishSocialLogin resolves memberships and the active tenant, mirroring the
// tail of the password login path.
func (s *Service) finishSocialLogin(
	ctx context.Context,
	u User,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
) (LoginResult, error) {
	// Claim first, so a just-granted membership is in the list below and becomes
	// the session's active tenant instead of the user landing nowhere.
	s.claimInvitationsForSocialUser(ctx, u)

	memberships, _ := s.repo.ListMembershipsForUser(ctx, u.ID)
	if len(memberships) == 0 && s.isFirstRunBootstrap(ctx, u) {
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

// isFirstRunBootstrap reports whether this login should mint the install's
// first organisation.
//
// ZERO VISIBLE MEMBERSHIPS IS A NORMAL STATE, NOT A FAULT TO REPAIR. It is what
// a site collaborator has (an active site_share and nothing else), what a
// client-portal user has (a client_member row and nothing else), and what
// anyone whose only organisation is inside its soft-delete grace window has.
// Creating a tenant for all of them would hand a portal user org-level
// capabilities the product deliberately withholds, activate that new empty org
// instead of their portal tenant (resolveActiveTenant takes memberships[0]
// first) and, on a hosted install, mint an unbilled organisation on demand.
//
// The user count alone is not enough to tell first run from the grace window.
// An install with one user whose only org has just been deleted still counts
// one user, and its owner signing in with Google was handed a fresh empty
// organisation while the very same person signing in with their password got
// nothing. Two sign-in paths disagreeing about what an account is a member of
// is the bug, and undoing a deletion nobody asked to undo is the damage.
//
// So it asks the question it actually means: is this user new, or merely
// currently unattached? CountAllMembershipsForUser counts soft-deleted orgs
// too, so a grace-window user answers "not new" and gets the same nothing the
// password path gives them. Restoring the org is the org owner's decision, made
// on the restore route, not a side effect of signing in.
func (s *Service) isFirstRunBootstrap(ctx context.Context, u User) bool {
	count, err := s.repo.CountUsers(ctx)
	if err != nil || count > 1 {
		return false
	}
	ever, err := s.repo.CountAllMembershipsForUser(ctx, u.ID)
	if err != nil {
		// Unreadable is not the same as zero. Refusing to bootstrap on an error
		// costs a first user one visit to the org page; guessing costs a
		// grace-window user a resurrected organisation.
		return false
	}
	return ever == 0
}

// SocialInviteClaimer accepts invitations addressed to an email address that an
// identity provider has just verified. Implemented by
// *internal/invitation.Service and wired via SetInviteClaimer.
//
// It is an interface for the same reason PaidTierValidator is one: internal
// /invitation already imports internal/auth, so the direct import would be a
// cycle. The claim mechanics stay in the package that owns invitations, which
// is also the package that already knows how to grant each of the three scopes.
type SocialInviteClaimer interface {
	ClaimForVerifiedEmail(ctx context.Context, userID uuid.UUID, email string) (int, error)
}

// SetInviteClaimer wires the invitation service. Leaving it unset (a test that
// does not exercise invitations) simply means nothing is claimed.
func (s *Service) SetInviteClaimer(c SocialInviteClaimer) { s.inviteClaimer = c }

// claimInvitationsForSocialUser accepts any invitation waiting for this
// account's address.
//
// WITHOUT IT, SIGNING IN SOCIALLY DESTROYS THE INVITATION. An invited person
// who clicks "Sign in with Google" before opening their invite link gets an
// account with no password hash, because a social account never has one. The
// accept endpoint then refuses them forever: it requires an existing user to
// prove themselves with a password, and answers a password-less account with
// "this account uses single sign-on, sign in first, then open the invite link
// again", advice that leads straight back to the same refusal, because being
// signed in changes nothing about that check. The invitation cannot be
// accepted by the person it was sent to, by any route, ever.
//
// Claiming here is safe on exactly the ground the invitation itself stands on:
// invitations are email-bound, and this runs only for an address this account
// has proven it controls (a provider verified it at sign-in, or we verified it
// ourselves). That is a higher bar than the link path, which asks only for the
// token plus a matching address plus a password.
//
// The password path is deliberately NOT changed to match. It has no dead end:
// a password user can always open the link and accept it.
func (s *Service) claimInvitationsForSocialUser(ctx context.Context, u User) {
	if s.inviteClaimer == nil || u.Email == "" || !u.EmailVerified() {
		return
	}
	if _, err := s.inviteClaimer.ClaimForVerifiedEmail(ctx, u.ID, u.Email); err != nil {
		// Best effort. A failed claim must not cost the user their sign-in; the
		// invitation stays pending and the next sign-in tries again.
		slog.ErrorContext(ctx, "social invitation claim failed",
			slog.String("user_id", u.ID.String()), slog.Any("error", err))
	}
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
