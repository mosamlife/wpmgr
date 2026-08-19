package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
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
	// EmailUnreachable marks an address the PROVIDER will not deliver mail to,
	// even though it is genuine and verified. GitHub's privacy address is the
	// one that exists in practice (see isGitHubNoreply). Verified and reachable
	// are separate questions and this install needs both: verified decides
	// whether the address may be acted on, reachable decides whether an account
	// built on it can ever be sent a verification link, a password reset or an
	// alert.
	EmailUnreachable bool
	Name             string
}

// usableEmail reports whether the provider handed us an address this install
// may both act on and reach the person at. Anything less is treated as no
// address at all, which is what keeps an unreachable address from becoming an
// account's permanent contact address.
func (i SocialIdentity) usableEmail() bool {
	return i.Email != "" && i.EmailVerified && !i.EmailUnreachable
}

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

// normalizeIssuer folds the differences between two spellings of the SAME
// issuer, and nothing else.
//
// Scheme and host are case-insensitive by definition (DNS and RFC 3986), and a
// trailing slash on the issuer URL denotes nothing: no operator runs two
// different identity providers on one hostname distinguished only by whether
// the URL ends in "/". So treating those as a changed issuer is a lockout with
// no security content on the other side of it.
//
// The PATH is deliberately left alone apart from that trailing slash. Two
// issuers on one host that differ by path are two issuers (Dex, Keycloak and
// Auth0 all use the path for the realm), and folding them together would merge
// two populations of users.
func normalizeIssuer(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		// Not a URL we can reason about. Compare it as an opaque string rather
		// than guess at its structure.
		return strings.TrimRight(s, "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// sameIssuer reports whether two issuer strings name the same issuer.
func sameIssuer(a, b string) bool {
	return normalizeIssuer(a) == normalizeIssuer(b)
}

// identityMatchKind says HOW a stored identity was matched, which decides both
// what has to be written and what has to be audited.
type identityMatchKind int

const (
	matchNone           identityMatchKind = iota
	matchExact                            // stored under the issuer that just signed the token
	matchIssuerMigrated                   // stored under the issuer the operator DECLARED as the previous one
)

// matchStoredIdentity decides which stored identity, if any, authenticates this
// sign-in. Pure, and separate from the database work, for the same reason
// decideSocial is: an account is won or lost here.
//
// (provider, subject, issuer) IS THE IDENTITY. A subject is only unique within
// the issuer that minted it, so matching on (provider, subject) alone would let
// two identity providers that happen to mint the same opaque string resolve to
// one account: a cross-IdP collision would become a silent, complete account
// takeover. That is why issuer is in the unique index and in the exact lookup.
//
// AN ISSUER CHANGE IS A MIGRATION, NOT A LOOKUP RULE. Keying on issuer has a
// real cost: an operator who repoints WPMGR_OIDC_ISSUER strands every
// generic-OIDC identity at once, and every SSO user on the install stops being
// recognised on the same deploy. Two narrow relaxations answer that without
// making the key ambiguous:
//
//   - A cosmetic difference is not a difference. See normalizeIssuer.
//
//   - A GENUINE change is accepted only when the operator declared the old
//     issuer in WPMGR_OIDC_PREVIOUS_ISSUER, only for the generic OIDC provider
//     (Google and GitHub mint a constant issuer, so a mismatch there could only
//     ever be a collision between two different people), and only when exactly
//     one stored row is a candidate. The caller then MOVES the row to the new
//     issuer and audits it, so the relaxation applies once per identity rather
//     than becoming a standing rule.
//
// Anything else, including any ambiguity at all, is matchNone. There is no
// tiebreak that is safe when two rows are candidates: picking one hands a
// person somebody else's account.
func matchStoredIdentity(in SocialIdentity, previousIssuer string, stored []Identity) (Identity, identityMatchKind) {
	var exact, previous []Identity
	for _, s := range stored {
		// Defensive: the query already scopes this, and a pure function that
		// trusts its caller for the identity halves of the key is one refactor
		// away from being wrong.
		if s.Provider != in.Provider || s.Subject != in.Subject {
			continue
		}
		switch {
		case sameIssuer(s.Issuer, in.Issuer):
			exact = append(exact, s)
		case operatorConfigured(in.Provider) && previousIssuer != "" && sameIssuer(s.Issuer, previousIssuer):
			previous = append(previous, s)
		}
	}
	if len(exact) == 1 {
		return exact[0], matchExact
	}
	if len(exact) > 1 {
		// Only reachable through a normalization fold (the unique index stops
		// exact duplicates), and still ambiguous. Refuse.
		return Identity{}, matchNone
	}
	if len(previous) == 1 {
		return previous[0], matchIssuerMigrated
	}
	return Identity{}, matchNone
}

// matchLegacyUser applies exactly the same rule to the pre-m110 identity still
// sitting on users.oidc_issuer / users.oidc_subject, for a user whose
// user_identities row was never written.
//
// It is a separate function only because the row shape differs. The policy must
// not: a repair path that matched more loosely than the authenticating lookup
// would simply move the takeover one query along.
func matchLegacyUser(in SocialIdentity, previousIssuer string, holders []User) (User, identityMatchKind) {
	if !operatorConfigured(in.Provider) {
		// The legacy columns were only ever written by the generic OIDC path, so
		// for a consumer provider a match could only be a subject collision
		// between two different people.
		return User{}, matchNone
	}
	stored := make([]Identity, 0, len(holders))
	byUser := make(map[uuid.UUID]User, len(holders))
	for _, u := range holders {
		if u.OIDCSubject == "" {
			continue
		}
		stored = append(stored, Identity{
			UserID: u.ID, Provider: in.Provider, Subject: u.OIDCSubject, Issuer: u.OIDCIssuer,
		})
		byUser[u.ID] = u
	}
	got, kind := matchStoredIdentity(in, previousIssuer, stored)
	if kind == matchNone {
		return User{}, matchNone
	}
	return byUser[got.UserID], kind
}

// legacySlotTaken reports whether some user already holds this exact
// (issuer, subject) pair on the legacy users columns.
//
// createSocialUser mirrors new generic-OIDC accounts into those columns so a
// rollback still recognises them. That mirror is a convenience; the sign-in
// itself is not. Writing it blind makes the convenience fatal:
// users_oidc_identity_key is unique, so creating an account whose pair was
// already taken fails the whole sign-in with a duplicate-key error rather than
// declining to mirror.
func legacySlotTaken(in SocialIdentity, holders []User) bool {
	for _, u := range holders {
		if u.OIDCSubject == in.Subject && sameIssuer(u.OIDCIssuer, in.Issuer) {
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
	if in.usableEmail() && emailUser != nil {
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
	if !in.usableEmail() {
		if !operatorConfigured(in.Provider) {
			if in.EmailUnreachable {
				// A DIFFERENT refusal, because it has a different fix. This
				// person's address is verified; it is the provider's own
				// outbound-only placeholder, so telling them to "verify your
				// email" would send them looking for a problem that is not
				// there. What resolves it is one setting on the provider.
				return 0, domain.Forbidden(
					"social_email_unreachable",
					"your "+providerLabel(in.Provider)+" account keeps its email address private, so this "+
						"install has no address it can reach you at. Make your address visible in your "+
						providerLabel(in.Provider)+" email settings and sign in again.",
				)
			}
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

	// NO SUBJECT, NO IDENTITY. Everything below keys on (provider, subject,
	// issuer), and an empty subject would make that key a shared bucket: the
	// first person through it would create the account and everyone after would
	// sign into it. A provider that returns no `sub` is broken, not anonymous.
	if in.Provider == "" || in.Subject == "" {
		return LoginResult{}, domain.Unauthorized(
			"social_subject_missing",
			"your identity provider did not return an account identifier",
		)
	}

	facts, err := s.loadSocialFacts(ctx, in)
	if err != nil {
		return LoginResult{}, err
	}
	identityUser, emailUser := facts.identityUser, facts.emailUser

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
		// AFTER THE GATE, NEVER BEFORE IT. The repair writes a permanent
		// identity binding, and doing it while loading the facts meant a
		// sign-in the policy went on to REFUSE (a disabled account, an
		// unverified one) still mutated authentication state on its way out.
		// A refusal must leave the database exactly as it found it.
		s.repairIdentity(ctx, action, in, facts)
		// Stamped under the issuer the row is stored with, not the one that
		// arrived. Those differ whenever the match came through the
		// normalization fold: the token said "https://idp.example.com/" and the
		// row still says "https://idp.example.com", the sign-in is correctly
		// recognised, and repairIdentity deliberately writes nothing for a
		// difference that denotes nothing. The stamp then went looking for the
		// inbound spelling under an exact predicate and updated no rows at all,
		// so last_login_at stayed null for the life of the identity and the
		// provider's current address was never recorded. See touchIssuer for why
		// this is not simply facts.storedIssuer.
		s.repo.TouchIdentityLogin(ctx, in.Provider, in.Subject, facts.touchIssuer(in), in.Email, in.EmailVerified)
		return s.finishSocialLogin(ctx, *identityUser)

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
		res, err := s.finishSocialLogin(ctx, *emailUser)
		if err != nil {
			return LoginResult{}, err
		}
		res.PendingSocialLink = &Identity{
			UserID: emailUser.ID, Provider: in.Provider, Subject: in.Subject,
			Issuer: in.Issuer, Email: in.Email, EmailVerified: in.EmailVerified,
		}
		// No notification here either. The account has not gained anything yet:
		// this is an authorization travelling as data, and it is CompleteSocialLink
		// that writes the identity and tells the holder about it. Mailing "a new
		// sign-in method was added" at this point would announce a link that the
		// second factor may still refuse, and would hand an attacker who only
		// reached a consent screen a way to mail the victim on demand.
		return res, nil

	default:
		// No deferral on this branch: the account is being created by this very
		// sign-in, so it cannot already carry a second factor to skip past, and
		// the identity is written in the same transaction as the user it belongs
		// to (see createSocialUser).
		//
		// No notification either: the account IS the new method, so there is no
		// prior holder to warn and the only address available is the one the
		// provider just asserted.
		// AN OWNERLESS INSTALL ACCEPTS NO NEW ACCOUNTS. This branch creates a
		// user from a provider handshake alone, and on an install whose first
		// organisation has not been claimed yet there is nobody who could grant
		// that user anything: the account would sit with zero memberships, on an
		// install with no owner, created by a caller nobody authorised. Worse,
		// the row itself is consequential — it is exactly the sort of artefact a
		// "has this install been claimed?" test can be fooled by, which is why
		// that question now asks about owner memberships and this branch refuses
		// outright rather than relying on it.
		//
		// The two sign-in paths therefore agree: neither can establish
		// ownership, and neither creates an account while ownership is
		// unestablished. Once the claim-bearing first-run call has run, this
		// branch behaves exactly as it always has.
		//
		// Fails closed on an unreadable answer, like every other caller.
		if owned, oerr := s.repo.OwnershipEstablished(ctx); oerr != nil || !owned {
			return LoginResult{}, domain.Forbidden(
				"registration_closed",
				"open registration is closed; ask a tenant owner or admin for an invitation",
			)
		}
		u, err := s.createSocialUser(ctx, in, legacySlotTaken(in, facts.legacyHolders))
		if err != nil {
			return LoginResult{}, err
		}
		// Audited after the login is resolved, not before: finishSocialLogin is
		// what settles which tenant this account belongs to, and recording first
		// would file every new account's registration against no tenant at all.
		res, err := s.finishSocialLogin(ctx, u)
		if err != nil {
			return LoginResult{}, err
		}
		s.recordSocialAudit(ctx, u.ID, in.Provider, "register")
		return res, nil
	}
}

// socialFacts is everything the database knows that the policy needs, plus the
// bookkeeping the sign-in will owe IF the policy allows it. Loading and
// deciding are kept apart on purpose: see repairIdentity.
type socialFacts struct {
	// identityUser is the account the identity resolved to, by whichever of the
	// three routes below matched.
	identityUser *User
	// emailUser is the account holding the provider's asserted address, looked
	// up only when it can matter.
	emailUser *User

	// match is how identityUser was found, and storedIssuer is the issuer it was
	// found under. Together they say what has to be repaired.
	match        identityMatchKind
	storedIssuer string
	// fromLegacy is true when the match came from the pre-m110 users.oidc_*
	// columns, which means no user_identities row exists for it yet.
	fromLegacy bool
	// legacyHolders is the raw legacy lookup, kept so account creation can see
	// whether the legacy mirror slot is already occupied.
	legacyHolders []User
}

// loadSocialFacts reads, and ONLY reads. Every write this sign-in might owe is
// recorded in the returned facts and executed later, once the policy has
// allowed the sign-in.
func (s *Service) loadSocialFacts(ctx context.Context, in SocialIdentity) (socialFacts, error) {
	f, err := s.resolveIdentity(ctx, in)
	if err != nil {
		return socialFacts{}, err
	}

	// The email is looked up only when it can matter, so an address the provider
	// will not vouch for never even probes for the existence of an account.
	// usableEmail, not a bare verified check: an address we cannot reach is no
	// use for linking either, and decideSocial refuses it a few lines later, so
	// probing for its owner would be a lookup whose answer can never be acted on.
	if f.identityUser == nil && in.usableEmail() {
		if u, err := s.repo.GetUserByEmail(ctx, in.Email); err == nil {
			f.emailUser = &u
		} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
			return socialFacts{}, err
		}
	}
	return f, nil
}

// resolveIdentity answers the only question that authenticates: which account,
// if any, does this external identity belong to? Three routes, in descending
// order of certainty, and none of them writes anything.
func (s *Service) resolveIdentity(ctx context.Context, in SocialIdentity) (socialFacts, error) {
	var f socialFacts

	// 1. The exact identity: (provider, subject, issuer). The ordinary path,
	//    and the only one that involves no relaxation of any kind.
	if u, err := s.repo.GetUserByIdentity(ctx, in.Provider, in.Subject, in.Issuer); err == nil {
		f.identityUser, f.match, f.storedIssuer = &u, matchExact, in.Issuer
		return f, nil
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		return socialFacts{}, err
	}

	// 2. THE SAME IDENTITY UNDER THE ISSUER IT WAS STORED WITH. Reached only on
	//    a miss, and matchStoredIdentity decides whether anything here may be
	//    used: a cosmetic difference in the issuer string, or an issuer the
	//    operator explicitly declared as this install's previous one. Ambiguity
	//    resolves to no match, on this lookup and not only on the legacy one.
	stored, err := s.repo.ListIdentitiesBySubject(ctx, in.Provider, in.Subject)
	if err != nil {
		return socialFacts{}, err
	}
	if got, kind := matchStoredIdentity(in, s.previousOIDCIssuer, stored); kind != matchNone {
		u, err := s.repo.GetUserByID(ctx, got.UserID)
		if err != nil {
			return socialFacts{}, err
		}
		f.identityUser, f.match, f.storedIssuer = &u, kind, got.Issuer
		return f, nil
	}

	// 3. THE PRE-m110 IDENTITY, WHICH A MIGRATION CANNOT BE TRUSTED TO HAVE
	//    MOVED. m110 copied users.oidc_subject into user_identities once, and a
	//    backfill runs once by construction: schema_migrations records it and it
	//    is never revisited. Anything the PREVIOUS release wrote afterwards,
	//    during a rollback window, has legacy columns and no identity row for
	//    good.
	//
	//    The consequence is not cosmetic. That release never wrote
	//    email_verified_at, so such an account looks never-verified, and the
	//    takeover defence correctly declines to link a social identity onto a
	//    never-verified account. A correct rule meeting the wrong population:
	//    people who have signed in through SSO for months are refused at the
	//    door, where the release before this one signed them straight in.
	//
	//    Only for an operator-configured issuer, and under exactly the issuer
	//    rule above: a repair path that matched more loosely than the
	//    authenticating lookup would just move the takeover one query along.
	if !operatorConfigured(in.Provider) {
		return f, nil
	}
	holders, err := s.repo.ListUsersByLegacyOIDCSubject(ctx, in.Subject)
	if err != nil {
		return socialFacts{}, err
	}
	f.legacyHolders = holders
	if u, kind := matchLegacyUser(in, s.previousOIDCIssuer, holders); kind != matchNone {
		f.identityUser, f.match, f.storedIssuer, f.fromLegacy = &u, kind, u.OIDCIssuer, true
	}
	return f, nil
}

// repairIdentity writes the one row this sign-in owes, once the policy has said
// the sign-in may proceed. Everything it does is best effort by design: the
// identity has already been established, so a bookkeeping failure must not turn
// into a failed login. The next sign-in repeats it.
//
// It takes the DECIDED action and re-checks it rather than trusting its call
// site, because the bug this guards against is precisely a repair that runs
// before, or instead of, a decision: a refused sign-in must not leave a
// permanent identity binding behind. Any action other than signing in owns no
// repair, and a refusal never reaches here at all.
func (s *Service) repairIdentity(ctx context.Context, action socialAction, in SocialIdentity, f socialFacts) {
	if action != socialSignIn || f.identityUser == nil {
		return
	}
	switch {
	case f.fromLegacy:
		// The missing user_identities row, written under the CURRENT issuer, so
		// the next sign-in is an ordinary exact hit that does not depend on the
		// legacy columns at all.
		written, err := s.repo.AdoptLegacyIdentity(ctx, Identity{
			UserID: f.identityUser.ID, Provider: in.Provider, Subject: in.Subject,
			Issuer: in.Issuer, Email: in.Email, EmailVerified: in.EmailVerified,
		})
		if err != nil || !written {
			// Nothing written means a concurrent sign-in healed it first, or the
			// user already holds an 'oidc' identity under another subject. Either
			// way there is no new fact to record.
			return
		}
		s.recordSocialAuditWith(ctx, f.identityUser.ID, in.Provider, "legacy_identity_adopted",
			issuerMoveMeta(f.storedIssuer, in.Issuer))

	case f.match == matchIssuerMigrated:
		// The operator declared the move; this is the row actually moving, once,
		// and it is recorded because an identity changing issuer is exactly the
		// kind of event somebody reading the audit log later needs to see.
		moved, err := s.repo.MigrateIdentityIssuer(ctx, in.Provider, in.Subject,
			f.storedIssuer, in.Issuer, in.Email, in.EmailVerified)
		if err != nil || !moved {
			// Not moved means a concurrent sign-in moved it first. One move, one
			// audit entry.
			return
		}
		s.recordSocialAuditWith(ctx, f.identityUser.ID, in.Provider, "identity_issuer_migrated",
			issuerMoveMeta(f.storedIssuer, in.Issuer))
	}
}

// touchIssuer is the issuer the identity row lives under once repairIdentity
// has run, which is the only value an exact-match stamp can find it by.
//
// It is not always the stored issuer, and that is the whole subtlety. Both
// repairs write the row under the issuer that just signed the token: the legacy
// adoption inserts it there, and the migration moves it there. Stamping those
// under the OLD issuer would miss for exactly the same reason stamping a fold
// match under the inbound one does. Only the fold match leaves the row where it
// was, because nothing repairs a difference that denotes nothing.
//
// On the ordinary path every one of these is the same string.
func (f socialFacts) touchIssuer(in SocialIdentity) string {
	if f.fromLegacy || f.match == matchIssuerMigrated {
		return in.Issuer
	}
	if f.storedIssuer == "" {
		return in.Issuer
	}
	return f.storedIssuer
}

// issuerMoveMeta records where an identity was and where it went, omitting the
// pair when nothing actually moved.
func issuerMoveMeta(from, to string) map[string]any {
	if sameIssuer(from, to) {
		return nil
	}
	return map[string]any{"from_issuer": from, "to_issuer": to}
}

// CompleteSocialLink writes a link that SignInWithSocial authorized but
// deliberately left unwritten until the caller finished authenticating.
//
// The caller must have issued a session for userID first. The userID argument
// is the session's, not the pending link's, and the mismatch check below is the
// point of taking both: a pending link parked during one login must never be
// applied to whoever completes the next one.
//
// THE POLICY IS RE-RUN HERE, AGAINST FRESHLY READ STATE. An approval made
// before a two-factor prompt can sit parked for up to pendingSocialLinkTTL, and
// the decision it encodes is only as good as the facts it was made on. In that
// window an administrator can disable the account, the address can be changed
// or its verification revoked, or the same provider identity can be linked
// somewhere else. Replaying the write from stale data would bind a new
// credential to an account whose current state forbids exactly that, which is
// the one thing deferring the write was supposed to prevent. So the account is
// re-read and decideSocial, the same function that authorized it, has to say
// "link" a second time.
func (s *Service) CompleteSocialLink(ctx context.Context, userID uuid.UUID, link Identity) error {
	if link.UserID == uuid.Nil || link.UserID != userID {
		return domain.Forbidden("social_link_user_mismatch", "this sign-in cannot complete that link")
	}

	// Somebody else may have taken this provider identity while the link was
	// parked. Already linked to this same account is success, not a conflict:
	// completion has to be safe to run twice.
	if owner, err := s.repo.GetUserByIdentity(ctx, link.Provider, link.Subject, link.Issuer); err == nil {
		if owner.ID == userID {
			return nil
		}
		return domain.Conflict("social_identity_taken", "this "+providerLabel(link.Provider)+" account is already linked to another user")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		return err
	}

	u, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	// decideSocial takes the account found BY the asserted address, so handing it
	// an account fetched by id is only equivalent while the two still agree. A
	// changed address must not inherit the old one's approval.
	if normalizeEmail(u.Email) != normalizeEmail(link.Email) {
		return domain.Forbidden("social_link_email_changed", "this account's email address changed; sign in with "+providerLabel(link.Provider)+" again")
	}
	action, err := decideSocial(SocialIdentity{
		Provider: link.Provider, Subject: link.Subject, Issuer: link.Issuer,
		Email: link.Email, EmailVerified: link.EmailVerified,
	}, nil, &u)
	if err != nil {
		return err
	}
	if action != socialLinkExisting {
		return domain.Forbidden("social_link_no_longer_permitted", "this account can no longer be linked to "+providerLabel(link.Provider))
	}

	if err := s.repo.CreateIdentity(ctx, link); err != nil {
		return err
	}
	s.recordSocialAudit(ctx, userID, link.Provider, "link")
	// THIS is the moment the account gained a way to sign in, so this is where
	// its holder is told. The notification used to sit at the point the link was
	// authorized, which stopped being the point it was written once the write
	// moved behind the second factor; leaving it there would have announced
	// links that were never made and stayed silent about the ones that were.
	// Addressed to u, re-read above, so it goes to the address the account holds
	// now rather than the one the parked link was approved against.
	s.sendSignInMethodAdded(ctx, u, SocialIdentity{
		Provider: link.Provider, Issuer: link.Issuer,
	})
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
	s.recordSocialAuditWith(ctx, userID, provider, action, nil)
}

// socialAuditAction maps what happened onto the action the audit log is keyed
// and filtered by.
//
// EVERY ONE OF THESE USED TO BE auth.oidc.login, which is the one thing none of
// them is. The log rendered "Signed in with SSO" for a provider being bound to
// an existing account, for an account being created out of a provider
// assertion, and for a stored identity changing issuer; the distinguishing fact
// existed only inside metadata.event, so nothing an owner could filter or scan
// on told these apart from an ordinary sign-in. Every entry recorded here is a
// credential change, which is exactly what recordSocialAuditWith's own
// documentation says it exists to capture.
//
// An unrecognised event keeps the old action rather than inventing one: this
// only ever runs on a genuine sign-in, so filing an unknown event as a login is
// the honest fallback, and a new event type is expected to add its own case.
func socialAuditAction(event string) string {
	switch event {
	case "link":
		return audit.ActionSocialLinked
	case "register":
		return audit.ActionSocialRegistered
	case "identity_issuer_migrated":
		return audit.ActionIdentityIssuerMoved
	case "legacy_identity_adopted":
		return audit.ActionIdentityAdopted
	default:
		return audit.ActionOIDCLogin
	}
}

// recordSocialAuditWith carries the extra facts that make an entry worth
// reading: which issuer an identity moved from and to, above all, because that
// is the one event here that changes what a stored credential means.
func (s *Service) recordSocialAuditWith(ctx context.Context, userID uuid.UUID, provider, action string, extra map[string]any) {
	memberships, _ := s.repo.ListMembershipsForUser(ctx, userID)
	tenantID := s.resolveActiveTenant(ctx, userID, memberships)
	auditAction := socialAuditAction(action)

	// Built once and used by whichever sink takes the event, so the issuer-move
	// facts survive on the tenantless path too. That path is exactly where they
	// matter most: a brand new social account has no org yet, and an identity
	// changing issuer is the event a reader most needs the before and after of.
	meta := map[string]any{"provider": provider, "event": action}
	for k, v := range extra {
		meta[k] = v
	}

	if tenantID != uuid.Nil {
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     auditAction,
			TargetType: "user",
			TargetID:   userID.String(),
			Metadata:   meta,
		})
		return
	}

	// The tenant-independent sink has no actor column of its own, so the subject
	// travels in the payload.
	meta["user_id"] = userID.String()
	raw, err := json.Marshal(meta)
	if err != nil {
		raw = nil
	}
	if err := s.repo.RecordTenantlessAuthEvent(ctx, userID, auditAction, raw); err != nil {
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
	legacyTaken bool,
) (User, error) {
	email := in.Email
	verified := in.EmailVerified
	if !in.usableEmail() {
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
//
// That scoping is what stops a subject collision between two IdPs from becoming
// a unique-email failure at account creation: the same opaque subject minted by
// two different issuers is two different people, and they get two different
// placeholders. The address is minted ONCE, at creation, and never recomputed,
// so an issuer change afterwards does not move anybody's address; continuity
// across such a change is the identity layer's job, not this function's.
//
// It cannot be used to claim or collide with a real address: .invalid is the
// reserved TLD that resolves nowhere, and the row is created with
// email_verified false, which the linking rules already refuse to act on.
func syntheticEmailDomain(in SocialIdentity) string {
	host := issuerHost(in.Issuer)
	if host == "" {
		host = in.Provider
	}
	return host + ".oidc.invalid"
}

// finishSocialLogin resolves memberships and the active tenant, mirroring the
// tail of the password login path.
func (s *Service) finishSocialLogin(ctx context.Context, u User) (LoginResult, error) {
	// NOTHING THAT GRANTS ACCESS BELONGS HERE. This runs before the second
	// factor is even issued, so any grant it made would be a grant made on the
	// strength of a provider handshake alone. An earlier version of this
	// function accepted the caller's pending invitations at this point, which
	// both spent them without the person ever asking and did so on the pre-2FA
	// path, defeating the deferral the link write goes to such lengths for. An
	// invitation is now accepted only by the affirmative act of opening the link
	// and submitting it, on an authenticated session; see
	// invitation.Service.Accept.
	//
	// NOR DOES FIRST-RUN OWNERSHIP BELONG HERE. Granting the install's first
	// organisation is an act of provisioning, and provisioning is authorised by
	// the claim the installer minted (Service.Bootstrap). A social sign-in has
	// nowhere to carry that claim: the request is a redirect the identity
	// provider decided the shape of, so a claim can be neither attached to it
	// nor demanded of it. There is therefore no version of this path that could
	// check the claim, which makes "never mint here" the only answer that keeps
	// the two paths agreeing about who may own an install.
	//
	// Nothing is lost. The person who holds the claim runs the claim-bearing
	// register call once, and every social sign-in afterwards resolves normally
	// against the memberships that exist. Someone who signs in socially before
	// that has happened lands with zero memberships — exactly what the password
	// path already gives an unattached account, and exactly what a site
	// collaborator, a client-portal user and a grace-window owner already get
	// here (see the git history of this function for what minting an org for
	// them cost).
	memberships, _ := s.repo.ListMembershipsForUser(ctx, u.ID)

	_ = s.repo.TouchLogin(ctx, u.ID)
	s.recordLogin(ctx, memberships, u.ID, audit.ActionLoginSuccess)

	res := LoginResult{User: u, Memberships: memberships}
	res.ActiveTenant = s.resolveActiveTenant(ctx, u.ID, memberships)
	return res, nil
}

// isFirstRunBootstrap is gone deliberately, and this note stands in its place
// so it is not reintroduced. It answered "should this social sign-in mint the
// install's first organisation?" from a user count, which is a question about
// the install's state and not about the caller's authority. First-run ownership
// is now granted only against the provisioning claim
// (WPMGR_BOOTSTRAP_CLAIM_SECRET), on the one path that can carry it — see
// Service.Bootstrap and finishSocialLogin's comment above.

// sendVerificationForSocialLink mails the link that unblocks a refused social
// link. Best effort and deliberately silent: whether an address received mail
// is not something an unauthenticated caller should be able to observe.
//
// It carries the prior plan intent forward, like every other resend, because
// minting a token DESTROYS the intent rather than merely omitting it:
// sendVerificationEmail invalidates the user's existing tokens and then inserts
// the new one, and priorDesiredPlan reads the most recent token. Passing "" here
// therefore made the empty token the latest, so the plan a paid registration
// chose was lost for good, including to any LATER resend that would otherwise
// have recovered it. Someone who signed up for a paid plan, never verified, then
// tried the social button landed on the free path after verifying, having done
// nothing wrong.
func (s *Service) sendVerificationForSocialLink(ctx context.Context, u User) {
	if s.limiter != nil {
		if ok, _ := s.limiter.Allow(ctx, "verify-social-link:"+u.Email, forgotPerMinute); !ok {
			return
		}
	}
	s.sendVerificationEmail(ctx, u.ID, u.Email, u.Name, s.priorDesiredPlan(ctx, u.ID))
}
