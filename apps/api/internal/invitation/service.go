// Package invitation implements the tokenized invite-accept flow for both
// org membership invitations (scope=org) and per-site share invitations
// (scope=site). Tokens are single-use, 7-day-expiry, SHA-256-hashed-only in
// the DB, and bound to the invited email so they cannot be identity-swapped.
package invitation

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const maxAttempts = 10 // rate-limit: refuse after 10 failed attempts

// Mailer sends invitation emails.
type Mailer interface {
	Send(ctx context.Context, recipients []string, subject, body string) error
}

// InviteEnqueuer enqueues a branded transactional email (the "invite" template)
// via the ADR-045 mailer. Satisfied by *mailer.Enqueuer; declared here so the
// invitation package does not import mailer. When set, it supersedes the legacy
// plaintext Mailer for org invitations.
type InviteEnqueuer interface {
	Enqueue(ctx context.Context, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error
	// Enabled reports whether SMTP is currently configured. When false,
	// Enqueue may still succeed (the job is queued) but delivery will be
	// skipped, so callers should surface email_sent=false to the client.
	Enabled(ctx context.Context) bool
}

// SessionStarter establishes a session for a newly accepted invitation.
type SessionStarter interface {
	Login(ctx context.Context, userID, tenantID uuid.UUID) error
}

// Service implements the invitation token lifecycle.
type Service struct {
	pool     *db.Pool
	authRepo *auth.Repo
	audit    *audit.Recorder
	sessions SessionStarter
	mailer   Mailer         // legacy plaintext fallback; may be nil
	enqueuer InviteEnqueuer // ADR-045 branded mailer; preferred when set
	baseURL  string
}

// NewService builds an invitation Service.
func NewService(pool *db.Pool, authRepo *auth.Repo, rec *audit.Recorder, sessions SessionStarter, mailer Mailer, baseURL string) *Service {
	return &Service{pool: pool, authRepo: authRepo, audit: rec, sessions: sessions, mailer: mailer, baseURL: baseURL}
}

// SetInviteEnqueuer wires the ADR-045 branded mailer (post-River). When set, org
// invitations send the "invite" template instead of the legacy plaintext email.
func (s *Service) SetInviteEnqueuer(e InviteEnqueuer) { s.enqueuer = e }

// tenantName best-effort resolves an org's display name for the invite email.
func (s *Service) tenantName(ctx context.Context, tenantID uuid.UUID) string {
	var name string
	_ = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT name FROM tenants WHERE id = $1", tenantID).Scan(&name)
	})
	return name
}

// CreateOrgInvitation creates a scope=org invitation token and sends an accept
// email. actorRole is the actor's own role and is used to enforce the privilege
// ceiling. Returns the raw token (for tests / self-host without SMTP) and a
// boolean indicating whether the accept link is being returned (SMTP unconfigured).
func (s *Service) CreateOrgInvitation(ctx context.Context, tenantID, actorID uuid.UUID, actorRole authz.Role, email, role string) (acceptLink string, err error) {
	// Privilege ceiling.
	targetRole := authz.Role(role)
	if !targetRole.Valid() {
		return "", domain.Validation("role_invalid", "invalid role")
	}
	// RoleClient is portal-only; org invitations must never carry it.
	if targetRole == authz.RoleClient {
		return "", domain.Validation("role_invalid", "client role cannot be assigned via org invitation")
	}
	if !actorRole.AtLeast(targetRole) {
		return "", domain.Forbidden("role_grant_exceeds_actor", "you cannot grant a role higher than your own")
	}

	rawToken, tokenHash, err := generateSecureToken()
	if err != nil {
		return "", domain.Internal("token_gen_failed", "failed to generate invitation token").WithCause(err)
	}

	var inv sqlc.Invitation
	err = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateInvitation(ctx, sqlc.CreateInvitationParams{
			TenantID:  tenantID,
			Email:     email,
			Scope:     "org",
			SiteID:    pgtype.UUID{Valid: false},
			ClientID:  pgtype.UUID{Valid: false},
			Role:      role,
			TokenHash: tokenHash,
			InvitedBy: pgtype.UUID{Bytes: actorID, Valid: actorID != uuid.Nil},
			ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
		})
		if err != nil {
			return domain.Internal("invitation_create_failed", "failed to create invitation").WithCause(err)
		}
		inv = row
		return nil
	})
	if err != nil {
		return "", err
	}

	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    actorID.String(),
		Action:     "member.invited",
		TargetType: "user",
		TargetID:   email,
		Metadata:   map[string]any{"role": role, "invitation_id": inv.ID.String()},
	})

	link := s.baseURL + "/accept?token=" + rawToken

	// Send the branded invite email when the ADR-045 mailer is wired; otherwise
	// fall back to the legacy plaintext mailer. Either way we ALWAYS return the
	// accept link so an admin can hand-deliver it (e.g. before SMTP is
	// configured, or to copy it directly) — ADR-045 addendum G7.
	if s.enqueuer != nil {
		inviterName := "A teammate"
		if u, uerr := s.authRepo.GetUserByID(ctx, actorID); uerr == nil && strings.TrimSpace(u.Name) != "" {
			inviterName = u.Name
		}
		_ = s.enqueuer.Enqueue(ctx, uuid.Nil, []string{email}, "invite", map[string]any{
			"Name":         "there",
			"InviterName":  inviterName,
			"OrgName":      s.tenantName(ctx, tenantID),
			"Role":         role,
			"AcceptURL":    link,
			"ExpiresHours": "168",
		})
	} else if s.mailer != nil {
		body := "You have been invited to join an organisation.\n\nAccept your invitation here:\n" + link + "\n\nThis link expires in 7 days and is single-use."
		_ = s.mailer.Send(ctx, []string{email}, "You have been invited to an organisation", body)
	}
	return link, nil
}

// AcceptInput is the public accept request.
type AcceptInput struct {
	Token    string
	Email    string
	Name     string
	Password string // may be empty if user already exists
}

// AcceptResult is returned on success.
type AcceptResult struct {
	TenantID uuid.UUID
	SiteID   *uuid.UUID
	ClientID *uuid.UUID
	Scope    string
}

// Accept validates the token, creates/links the user, grants membership or
// share, marks the invitation accepted, and starts a session. It is intentionally
// public (no auth required).
func (s *Service) Accept(ctx context.Context, in AcceptInput) (AcceptResult, error) {
	// Hash the raw token for the DB lookup.
	tokenHash := hashToken(in.Token)

	// Look up the invitation under the special invite_lookup policy.
	var inv sqlc.Invitation
	err := s.pool.InInviteLookupTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetInvitationByTokenHash(ctx, tokenHash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("invitation_not_found", "invitation not found or already used")
			}
			return domain.Internal("invitation_lookup_failed", "failed to look up invitation").WithCause(err)
		}
		inv = row
		return nil
	})
	if err != nil {
		return AcceptResult{}, err
	}

	// Validate: single-use, revoked, expiry, email binding, rate-limit.
	if inv.AcceptedAt.Valid {
		return AcceptResult{}, domain.Conflict("invitation_already_used", "this invitation has already been accepted")
	}
	// A revoked invite is dead even to a holder of the original (un-rotated)
	// link — the sharing UI's "Revoke" action must be enforced here, not just at
	// list time. Return the same opaque not-found as an unknown token (no oracle
	// distinguishing "revoked" from "never existed"). Regenerate clears
	// revoked_at, so a re-issued invite is intentionally acceptable again.
	if inv.RevokedAt.Valid {
		return AcceptResult{}, domain.NotFound("invitation_not_found", "invitation not found or already used")
	}
	if time.Now().UTC().After(inv.ExpiresAt) {
		return AcceptResult{}, domain.Forbidden("invitation_expired", "this invitation has expired")
	}
	// FIX 6 (NIT): compare emails via subtle.ConstantTimeCompare on lowercased
	// strings to prevent timing-based email enumeration attacks.
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(inv.Email)), []byte(strings.ToLower(in.Email))) != 1 {
		// Increment attempts before returning (rate-limit enumeration).
		_ = s.incrementAttempts(ctx, inv.TenantID, inv.ID)
		return AcceptResult{}, domain.Forbidden("invitation_email_mismatch", "email does not match the invitation")
	}
	if inv.Attempts >= maxAttempts {
		return AcceptResult{}, domain.Forbidden("invitation_rate_limited", "too many failed attempts; request a new invitation")
	}

	// Resolve or create the user.
	u, err := s.authRepo.GetUserByEmail(ctx, in.Email)
	if err != nil {
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindNotFound {
			return AcceptResult{}, err
		}
		// New user: requires a password to create the account.
		if in.Password == "" {
			return AcceptResult{}, domain.Validation("password_required", "choose a password to create your account")
		}
		hash, herr := auth.HashPassword(in.Password)
		if herr != nil {
			return AcceptResult{}, domain.Internal("password_hash_failed", "failed to hash password").WithCause(herr)
		}
		u, err = s.authRepo.CreateUser(ctx, in.Email, hash, in.Name, "", "")
		if err != nil {
			return AcceptResult{}, err
		}
	} else {
		// EXISTING user: authenticate before granting access + starting a
		// session, otherwise possession of the invite link alone would log the
		// caller in as an existing account (the token is email-bound but the
		// link can still leak). A logged-in user simply re-enters their password.
		if u.PasswordHash == "" {
			return AcceptResult{}, domain.Validation("password_login_unavailable",
				"this account uses single sign-on — sign in first, then open the invite link again")
		}
		if in.Password == "" {
			return AcceptResult{}, domain.Validation("password_required", "enter your password to accept")
		}
		okPw, verr := auth.VerifyPassword(in.Password, u.PasswordHash)
		if verr != nil || !okPw {
			_ = s.incrementAttempts(ctx, inv.TenantID, inv.ID)
			return AcceptResult{}, domain.Unauthorized("invalid_credentials", "incorrect password")
		}
	}

	// FIX 5 (CRITICAL): claim the invitation FIRST using RETURNING to make the
	// single-use guarantee atomic. The UPDATE ... WHERE accepted_at IS NULL
	// RETURNING will return ErrNoRows if a concurrent Accept already claimed
	// this token. We must NOT start a session or grant access if the claim fails.
	tenantID := inv.TenantID
	claimed, err := s.claimInvitation(ctx, inv, u.ID)
	if err != nil {
		return AcceptResult{}, err
	}
	if !claimed {
		// Another request claimed this token between our lookup and this UPDATE.
		return AcceptResult{}, domain.Conflict("invitation_already_used", "this invitation has already been accepted")
	}

	// Grant the appropriate access (after a successful claim).
	if err := s.grantInvitation(ctx, inv, u.ID); err != nil {
		return AcceptResult{}, err
	}

	// Start a session (after grant, after claim).
	if err := s.sessions.Login(ctx, u.ID, tenantID); err != nil {
		return AcceptResult{}, domain.Internal("session_start_failed", "failed to start session").WithCause(err)
	}

	result := AcceptResult{TenantID: tenantID, Scope: inv.Scope}
	if inv.Scope == "site" && inv.SiteID.Valid {
		id := uuid.UUID(inv.SiteID.Bytes)
		result.SiteID = &id
	}
	if inv.Scope == "client" && inv.ClientID.Valid {
		id := uuid.UUID(inv.ClientID.Bytes)
		result.ClientID = &id
	}
	return result, nil
}

// claimInvitation marks one invitation accepted, atomically. It reports
// claimed=false (with no error) when the row was already taken between the
// caller's read and this UPDATE, which is what makes the single-use guarantee
// hold against a concurrent accept rather than merely usually holding.
func (s *Service) claimInvitation(ctx context.Context, inv sqlc.Invitation, userID uuid.UUID) (bool, error) {
	var claimed bool
	err := s.pool.InTenantTx(ctx, inv.TenantID, func(tx pgx.Tx) error {
		_, txErr := sqlc.New(tx).MarkInvitationAccepted(ctx, sqlc.MarkInvitationAcceptedParams{
			ID:             inv.ID,
			AcceptedUserID: pgtype.UUID{Bytes: userID, Valid: true},
		})
		if txErr != nil {
			if errors.Is(txErr, pgx.ErrNoRows) {
				claimed = false
				return nil
			}
			return domain.Internal("invitation_claim_failed", "failed to claim invitation").WithCause(txErr)
		}
		claimed = true
		return nil
	})
	return claimed, err
}

// grantInvitation performs the scope-specific grant for an invitation that has
// ALREADY been claimed, and records it.
//
// Shared by the two ways an invitation can be accepted: opening the link
// (Accept, which proves the holder with a password) and signing in with a
// provider that has verified the invited address (ClaimForVerifiedEmail). One
// implementation because the grant is the same act either way, and a second
// copy of a three-scope switch is a second place for the scopes to drift.
func (s *Service) grantInvitation(ctx context.Context, inv sqlc.Invitation, userID uuid.UUID) error {
	tenantID := inv.TenantID
	switch inv.Scope {
	case "org":
		if _, err := s.authRepo.CreateMembership(ctx, userID, tenantID, authz.Role(inv.Role)); err != nil {
			// Conflict = already a member → still OK.
			de, ok := domain.AsDomain(err)
			if !ok || de.Kind != domain.KindConflict {
				return err
			}
		}
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     "share.accepted",
			TargetType: "tenant",
			TargetID:   tenantID.String(),
			Metadata:   map[string]any{"invitation_id": inv.ID.String(), "role": inv.Role},
		})

	case "site":
		if !inv.SiteID.Valid {
			return domain.Internal("invitation_site_missing", "site invitation has no site_id")
		}
		siteID := uuid.UUID(inv.SiteID.Bytes)
		err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := sqlc.New(tx).CreateShare(ctx, sqlc.CreateShareParams{
				TenantID:  tenantID,
				SiteID:    siteID,
				UserID:    userID,
				Role:      inv.Role,
				GrantedBy: inv.InvitedBy,
				ExpiresAt: pgtype.Timestamptz{Valid: false},
			})
			return err
		})
		if err != nil {
			return domain.Internal("share_create_failed", "failed to grant site access").WithCause(err)
		}
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     "share.accepted",
			TargetType: "site",
			TargetID:   siteID.String(),
			Metadata:   map[string]any{"invitation_id": inv.ID.String(), "role": inv.Role},
		})

	case "client":
		if !inv.ClientID.Valid {
			return domain.Internal("invitation_client_missing", "client invitation has no client_id")
		}
		clientID := uuid.UUID(inv.ClientID.Bytes)
		err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			_, qerr := sqlc.New(tx).CreateClientMember(ctx, sqlc.CreateClientMemberParams{
				TenantID:  tenantID,
				ClientID:  clientID,
				UserID:    userID,
				InvitedBy: inv.InvitedBy,
			})
			return qerr
		})
		if err != nil {
			return domain.Internal("client_member_create_failed", "failed to grant portal access").WithCause(err)
		}
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     "client_member.accepted",
			TargetType: "client",
			TargetID:   clientID.String(),
			Metadata:   map[string]any{"invitation_id": inv.ID.String(), "client_id": clientID.String()},
		})

	default:
		return domain.Internal("invitation_scope_unknown", "unknown invitation scope: "+inv.Scope)
	}
	return nil
}

// ClaimForVerifiedEmail accepts every live invitation addressed to an address
// that an identity provider has just verified for userID, and returns how many
// were claimed. It satisfies auth.SocialInviteClaimer.
//
// WHY THIS EXISTS. Accept is the only other way in, and it authenticates an
// existing account with a password. An account created by signing in with
// Google or GitHub has no password hash and can never be given one, so Accept
// answers it with password_login_unavailable and the advice to sign in first
// and open the link again, which lands on the identical refusal. Anyone who
// signed in socially before opening their invite had destroyed it: the
// invitation was addressed to them, still valid, and unacceptable by them or by
// anybody else, permanently.
//
// WHY IT IS SAFE WITHOUT A TOKEN. The token exists to prove that whoever holds
// the link was sent it, and the invitation is bound to the address it was sent
// to. This path proves control of that same address by a stronger route: an
// identity provider verified it during the sign-in that led here, and the
// caller (auth.finishSocialLogin) additionally requires that this install has
// its own record of the address being verified. Link-based accept asks for less
// than that.
//
// Each invitation is claimed and granted independently: one malformed row must
// not strand the rest, and a claim that loses the race to a concurrent accept
// is a normal outcome, not an error.
func (s *Service) ClaimForVerifiedEmail(ctx context.Context, userID uuid.UUID, email string) (int, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if userID == uuid.Nil || email == "" {
		return 0, nil
	}

	// Pre-auth, cross-tenant lookup: the invitations are from orgs this user is
	// not a member of yet, so only the invite_lookup policy can see them.
	var pending []sqlc.Invitation
	err := s.pool.InInviteLookupTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListPendingInvitationsForEmail(ctx, email)
		if qerr != nil {
			return domain.Internal("invitation_lookup_failed", "failed to look up invitations").WithCause(qerr)
		}
		pending = rows
		return nil
	})
	if err != nil {
		return 0, err
	}

	var claimedCount int
	var firstErr error
	for _, inv := range pending {
		claimed, cerr := s.claimInvitation(ctx, inv, userID)
		if cerr != nil {
			if firstErr == nil {
				firstErr = cerr
			}
			continue
		}
		if !claimed {
			continue
		}
		if gerr := s.grantInvitation(ctx, inv, userID); gerr != nil {
			slog.ErrorContext(ctx, "invitation granted to social sign-in failed after claim",
				slog.String("invitation_id", inv.ID.String()),
				slog.String("user_id", userID.String()), slog.Any("error", gerr))
			if firstErr == nil {
				firstErr = gerr
			}
			continue
		}
		claimedCount++
	}
	return claimedCount, firstErr
}

func (s *Service) incrementAttempts(ctx context.Context, tenantID, invID uuid.UUID) error {
	return s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).IncrementInviteAttempts(ctx, invID)
		return err
	})
}

// CreateClientInvitation creates a scope='client' invitation token and sends a
// portal-invite email. Returns the raw accept link (always; copyable-link
// fallback for unconfigured SMTP), the invitation ID, the expiry time, and a
// boolean indicating whether the invitation email was successfully enqueued.
// emailSent is false when SMTP is unconfigured, the enqueuer is nil, or
// enqueue fails — but in all those cases the accept link is still returned and
// invitation creation is never aborted. This satisfies client.InviteService.
func (s *Service) CreateClientInvitation(ctx context.Context, tenantID, clientID, actorID uuid.UUID, email string) (acceptLink string, invitationID uuid.UUID, expiresAt time.Time, emailSent bool, err error) {
	rawToken, tokenHash, genErr := generateSecureToken()
	if genErr != nil {
		return "", uuid.Nil, time.Time{}, false, domain.Internal("token_gen_failed", "failed to generate invitation token").WithCause(genErr)
	}

	expiry := time.Now().UTC().Add(7 * 24 * time.Hour)
	var inv sqlc.Invitation
	txErr := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).CreateInvitation(ctx, sqlc.CreateInvitationParams{
			TenantID:  tenantID,
			Email:     email,
			Scope:     "client",
			SiteID:    pgtype.UUID{Valid: false},
			ClientID:  pgtype.UUID{Bytes: clientID, Valid: true},
			Role:      string(authz.RoleClient),
			TokenHash: tokenHash,
			InvitedBy: pgtype.UUID{Bytes: actorID, Valid: actorID != uuid.Nil},
			ExpiresAt: expiry,
		})
		if qerr != nil {
			return domain.Internal("invitation_create_failed", "failed to create client invitation").WithCause(qerr)
		}
		inv = row
		return nil
	})
	if txErr != nil {
		return "", uuid.Nil, time.Time{}, false, txErr
	}

	link := s.baseURL + "/accept?token=" + rawToken

	// Send the portal invite email when the ADR-045 mailer is wired.
	// Resolve client and agency names best-effort.
	var clientName, agencyName, inviterName string
	inviterName = "Your agency"
	agencyName = s.tenantName(ctx, tenantID)
	if agencyName == "" {
		agencyName = "Your agency"
	}
	_ = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT name FROM clients WHERE id = $1 AND tenant_id = $2", clientID, tenantID).Scan(&clientName)
	})
	if clientName == "" {
		clientName = agencyName
	}
	if actorID != uuid.Nil {
		if u, uerr := s.authRepo.GetUserByID(ctx, actorID); uerr == nil && strings.TrimSpace(u.Name) != "" {
			inviterName = u.Name
		}
	}

	emailSent = sendClientPortalInvite(ctx, s.enqueuer, s.mailer, email, link, inviterName, clientName, agencyName)

	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    actorID.String(),
		Action:     "client_member.invited",
		TargetType: "client",
		TargetID:   clientID.String(),
		Metadata:   map[string]any{"email": email, "invitation_id": inv.ID.String(), "client_id": clientID.String()},
	})

	return link, inv.ID, expiry, emailSent, nil
}

// sendClientPortalInvite dispatches the portal invite email and returns whether
// the email was successfully enqueued with SMTP configured. It is extracted so
// the dispatch logic can be unit-tested without a live DB connection.
//
// Rules (per contract §1.3 / §1.4):
//   - emailSent=true only when enqueuer.Enabled()==true AND Enqueue returns nil.
//   - An enqueue failure logs a warning but NEVER aborts invitation creation.
//   - When Enabled()==false, enqueue is still attempted (so the email_log row is
//     created and Deliver marks it "smtp not configured"), but emailSent=false.
//   - A nil enqueuer falls back to the legacy Mailer (emailSent = Send==nil).
//   - A nil enqueuer AND nil mailer: emailSent=false, no side-effects.
func sendClientPortalInvite(ctx context.Context, enq InviteEnqueuer, mailer Mailer,
	email, link, inviterName, clientName, agencyName string) bool {
	data := map[string]any{
		"Name":         "there",
		"InviterName":  inviterName,
		"ClientName":   clientName,
		"AgencyName":   agencyName,
		"AcceptURL":    link,
		"ExpiresHours": "168",
	}
	if enq != nil {
		if enq.Enabled(ctx) {
			if err := enq.Enqueue(ctx, uuid.Nil, []string{email}, "client_portal_invite", data); err != nil {
				slog.Warn("client portal invite: enqueue failed; invitation still created",
					slog.String("email", email), slog.Any("error", err))
				return false
			}
			return true
		}
		// SMTP unconfigured: still enqueue so the log row is created and Deliver
		// marks it "smtp not configured" — but email_sent is false.
		_ = enq.Enqueue(ctx, uuid.Nil, []string{email}, "client_portal_invite", data)
		return false
	}
	if mailer != nil {
		body := "You have been invited to access the " + clientName + " portal, managed by " + agencyName + ".\n\nAccept your invitation here:\n" + link + "\n\nThis link expires in 7 days and is single-use."
		return mailer.Send(ctx, []string{email}, "You have been invited to a client portal", body) == nil
	}
	return false
}
