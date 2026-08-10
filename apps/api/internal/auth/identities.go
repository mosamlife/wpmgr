package auth

// identities.go -- the connected accounts surface: what an account can sign in
// with, removing one of those, and adding the one a social-only account is
// missing.
//
// THE INVARIANT THIS FILE EXISTS FOR: an account must never reach zero sign-in
// methods. A method is either a password or a linked external identity.
//
// It is worth being explicit about why zero is unrecoverable here, rather than
// merely inconvenient. Every other lockout has a way back: a forgotten password
// is reset by email, a lost second factor is bypassed with a recovery code. An
// account with no password and no identity has neither, because password reset
// deliberately refuses to mint a set-password link for an account with no
// password -- doing that would make "reset" an account-creation primitive for
// anyone who knows the address. So the refusal below is not a nicety, and it is
// not something the UI can be trusted to enforce on its own.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// SignInMethods is everything the settings page needs to describe how an
// account can be signed in to, and to decide what it may offer to remove.
type SignInMethods struct {
	// HasPassword reports only whether a password exists. The hash itself never
	// leaves the repo.
	HasPassword bool
	Identities  []Identity
}

// CanUnlink reports whether removing ONE identity would still leave a way in.
// The server re-decides this per request in decideUnlink; this exists so the
// response can carry the same answer and the UI does not have to reimplement
// the rule (and get it subtly wrong) to know whether to show a Disconnect
// button.
func (m SignInMethods) CanUnlink() bool {
	return m.HasPassword || len(m.Identities) > 1
}

// SignInMethods lists how this user can sign in.
func (s *Service) SignInMethods(ctx context.Context, userID uuid.UUID) (SignInMethods, error) {
	u, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		return SignInMethods{}, err
	}
	identities, err := s.repo.ListIdentitiesForUser(ctx, userID)
	if err != nil {
		return SignInMethods{}, err
	}
	return SignInMethods{HasPassword: u.PasswordHash != "", Identities: identities}, nil
}

// decideUnlink IS THE INVARIANT, as a pure function of the three facts that
// decide it, deliberately separated from the transaction that gathers them.
// This mirrors decideSocial in social.go and for the same reason: a plumbing
// bug costs a request, a bug in this function costs somebody their account, so
// this is the part that gets tested exhaustively rather than reasoned about.
//
// linked        -- is `provider` actually attached to this account
// identityCount -- how many identities the account has right now
// hasPassword   -- whether a password is set
//
// The order matters. "You are not connected to this" is checked first, so
// someone asking to remove a provider they never had is told that, rather than
// being told they cannot remove their last sign-in method.
func decideUnlink(linked bool, identityCount int, hasPassword bool, provider string) error {
	if !linked {
		return domain.NotFound("identity_not_linked",
			"that sign-in method is not connected to this account")
	}
	if hasPassword || identityCount > 1 {
		return nil
	}
	// THE REFUSAL HAS TO SAY WHAT TO DO INSTEAD. Without the second sentence
	// this reads as an arbitrary block on a button the page just offered; with
	// it, the person has the exact next step, and it is one they can take from
	// the same page.
	return domain.Conflict("last_sign_in_method",
		"disconnecting "+providerLabel(provider)+" would leave you no way to sign in, because this account "+
			"has no password. Set a password first, then disconnect "+providerLabel(provider)+".")
}

// UnlinkIdentity removes one external sign-in method from the caller's own
// account. The invariant is enforced in the repo, inside the transaction that
// reads the facts, because deciding out here would leave a gap between the
// decision and the delete: see Repo.UnlinkIdentity.
func (s *Service) UnlinkIdentity(ctx context.Context, userID uuid.UUID, provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return domain.Validation("provider_required", "a provider is required")
	}
	if err := s.repo.UnlinkIdentity(ctx, userID, provider); err != nil {
		return err
	}
	s.recordIdentityAudit(ctx, userID, audit.ActionIdentityUnlinked, map[string]any{"provider": provider})
	return nil
}

// SetInitialPassword gives a social-only account a password.
//
// THE SESSION IS THE AUTHORISATION, and it is the only one available: the
// account has no password to re-enter, so there is nothing to prove beyond
// already being signed in. That is exactly why this cannot live on the
// forgot-password flow, which is reachable by anyone who knows the address.
//
// It refuses outright when a password already exists. Not because overwriting
// would be wrong in principle, but because the path that overwrites must first
// ask for the current password (ChangePassword), and quietly accepting the
// request here would turn a stolen session into a password change without that
// check.
func (s *Service) SetInitialPassword(ctx context.Context, userID uuid.UUID, newPwd string, ip netip.Addr) error {
	if len(newPwd) < minPasswordLen {
		return domain.Validation("new_password_too_short", "password must be at least 12 characters")
	}
	if len(newPwd) > maxPasswordLen {
		return domain.Validation("new_password_too_long", "password must be 200 characters or fewer")
	}
	hash, err := HashPassword(newPwd)
	if err != nil {
		return domain.Internal("password_hash_failed", "failed to hash password").WithCause(err)
	}
	// Conflicts with an existing password are decided by the UPDATE itself.
	if err := s.repo.SetInitialPasswordHash(ctx, userID, hash); err != nil {
		return err
	}

	// Trusted devices are deliberately NOT revoked here, unlike ChangePassword
	// and ResetPassword. Those two are the post-compromise lever: a password
	// that changed may have changed because somebody else changed it, so the
	// "remember this device" bypasses have to go. Adding a first password from
	// a session that is already authenticated adds a way in, it does not
	// replace one, and there is no compromise signalled to respond to.

	// The account address hears about it. A new way to sign in appearing on an
	// account is exactly the kind of change that must not happen silently, and
	// the account owner is the only one who can tell us it was not them.
	s.sendPasswordChanged(ctx, userID, ip)
	s.recordIdentityAudit(ctx, userID, audit.ActionPasswordSet, nil)
	return nil
}

// recordIdentityAudit records a sign-in-method change: removing an identity, or
// setting the first password on a social-only account. Both callers are
// credential changes, so neither may go unrecorded.
//
// IT USED TO BE DROPPED EXACTLY WHERE IT MATTERED MOST. The tenant came from
// memberships[0] and the event was skipped outright when that list was empty,
// which is not an edge case: it is a site collaborator, a portal user, a brand
// new social account, and anyone whose only org is inside its soft-delete grace
// window. The accounts with the least oversight got the least audit, and the
// event skipped is the one that removes a way in.
//
// So this mirrors recordSocialAuditWith: the tenant is resolved the same way
// the session's active tenant is (org membership, then site share, then client
// membership), and when there is genuinely no tenant the event goes to the
// tenant-independent sink rather than being thrown away.
func (s *Service) recordIdentityAudit(ctx context.Context, userID uuid.UUID, action string, meta map[string]any) {
	memberships, _ := s.repo.ListMembershipsForUser(ctx, userID)
	tenantID := s.resolveActiveTenant(ctx, userID, memberships)

	if tenantID != uuid.Nil {
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     action,
			TargetType: "user",
			TargetID:   userID.String(),
			Metadata:   meta,
		})
		return
	}

	// The tenant-independent sink has no actor column of its own, so the subject
	// travels in the payload, alongside whatever the caller supplied.
	payload := map[string]any{"event": action, "user_id": userID.String()}
	for k, v := range meta {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = nil
	}
	if err := s.repo.RecordTenantlessAuthEvent(ctx, userID, action, raw); err != nil {
		slog.ErrorContext(ctx, "identity audit record failed",
			slog.String("event", action),
			slog.String("user_id", userID.String()), slog.Any("error", err))
	}
}
