package auth

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// sendSignInMethodAdded tells the account holder, at the address THIS install
// verified, that a new way of signing in now works on their account.
//
// WHY IT EXISTS. Attaching a provider-verified identity to a locally verified
// account happens without anyone signing in first, and that stays. Demanding an
// authenticated link instead would tax every returning user with a password
// reset in order to defend against an attacker who already needs BOTH a
// provider that vouches for the address AND an account this install has
// verified at that same address.
//
// The defect was never the linking. It was the SILENCE. A new sign-in method
// appearing on an account nobody was told about is how a quiet takeover stays
// quiet, and it is the one thing the owner could have caught. Naming the
// provider and the time turns it into something they can act on.
//
// The recipient is the LOCAL account's address, never the address the provider
// asserted. At link time the two are equal by construction, because the account
// was found BY that address, but only the local row is one this install
// verified and only the local row is beyond a provider's reach. Reading it off
// the identity would hand the choice of recipient to the party the notice is
// about.
//
// No throttle is needed: linking is once per (provider, subject), because every
// later sign-in with the same identity takes the sign-in path and never reaches
// here.
//
// Best effort, exactly like sendPasswordChanged. A mail failure never fails the
// sign-in the person is in the middle of.
func (s *Service) sendSignInMethodAdded(ctx context.Context, u User, in SocialIdentity) {
	if s.email == nil || u.Email == "" {
		return
	}
	name := u.Name
	if strings.TrimSpace(name) == "" {
		// Social accounts often carry no name, and "Hi ," reads like a broken
		// mail merge on a message whose whole job is to be taken seriously.
		name = "there"
	}
	_ = s.email.Enqueue(ctx, uuid.Nil, []string{u.Email}, "sign_in_method_added", map[string]any{
		"Name":        name,
		"Provider":    signInMethodLabel(in),
		"When":        time.Now().UTC().Format("2006-01-02 15:04 MST"),
		"SecurityURL": s.baseURL + "/settings/security",
	})
}

// NotifySignInMethodAdded is the same notification addressed by user id, for a
// caller outside this package that attaches a sign-in method from an
// authenticated session (the connected-accounts surface in settings). One
// implementation, so a second way to gain a sign-in method cannot ship without
// the message that makes it visible.
func (s *Service) NotifySignInMethodAdded(ctx context.Context, userID uuid.UUID, provider, issuer string) {
	if s.email == nil {
		return
	}
	u, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		return
	}
	s.sendSignInMethodAdded(ctx, u, SocialIdentity{Provider: provider, Issuer: issuer})
}

// signInMethodLabel names the method in terms the recipient can check.
//
// providerLabel answers "your identity provider" for the generic OIDC provider,
// which is the wrong thing to put in a security notice: it tells the reader
// nothing they can compare against what they actually did. When the operator
// configured an issuer, its host is a real answer, so name that instead.
func signInMethodLabel(in SocialIdentity) string {
	if operatorConfigured(in.Provider) {
		if host := issuerHost(in.Issuer); host != "" {
			return host
		}
	}
	return providerLabel(in.Provider)
}

// issuerHost reduces an issuer URL to its bare host.
func issuerHost(issuer string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(issuer, "https://"), "http://")
	if i := strings.IndexAny(host, "/:"); i > 0 {
		host = host[:i]
	}
	return host
}
