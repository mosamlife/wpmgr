// Package invitation implements the tokenized invite-accept flow for both
// org membership invitations (scope=org) and per-site share invitations
// (scope=site). Tokens are single-use, 7-day-expiry, SHA-256-hashed-only in
// the DB, and bound to the invited email so they cannot be identity-swapped.
package invitation

import (
	"context"
	"crypto/subtle"
	"errors"
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
	mailer   Mailer // may be nil
	baseURL  string
}

// NewService builds an invitation Service.
func NewService(pool *db.Pool, authRepo *auth.Repo, rec *audit.Recorder, sessions SessionStarter, mailer Mailer, baseURL string) *Service {
	return &Service{pool: pool, authRepo: authRepo, audit: rec, sessions: sessions, mailer: mailer, baseURL: baseURL}
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
	if s.mailer != nil {
		body := "You have been invited to join an organisation.\n\nAccept your invitation here:\n" + link + "\n\nThis link expires in 7 days and is single-use."
		if err := s.mailer.Send(ctx, []string{email}, "You have been invited to an organisation", body); err != nil {
			// SMTP configured but failed: return link as fallback.
			return link, nil
		}
		return "", nil // email sent; do not leak link in the response
	}
	// SMTP unconfigured: return link directly (self-host mode).
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
	var claimed bool
	err = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, txErr := sqlc.New(tx).MarkInvitationAccepted(ctx, sqlc.MarkInvitationAcceptedParams{
			ID:             inv.ID,
			AcceptedUserID: pgtype.UUID{Bytes: u.ID, Valid: true},
		})
		if txErr != nil {
			if errors.Is(txErr, pgx.ErrNoRows) {
				// 0 rows updated: already accepted by a concurrent request.
				claimed = false
				return nil
			}
			return domain.Internal("invitation_claim_failed", "failed to claim invitation").WithCause(txErr)
		}
		claimed = true
		return nil
	})
	if err != nil {
		return AcceptResult{}, err
	}
	if !claimed {
		// Another request claimed this token between our lookup and this UPDATE.
		return AcceptResult{}, domain.Conflict("invitation_already_used", "this invitation has already been accepted")
	}

	// Grant the appropriate access (after a successful claim).
	switch inv.Scope {
	case "org":
		if _, err := s.authRepo.CreateMembership(ctx, u.ID, tenantID, authz.Role(inv.Role)); err != nil {
			// Conflict = already a member → still OK.
			de, ok := domain.AsDomain(err)
			if !ok || de.Kind != domain.KindConflict {
				return AcceptResult{}, err
			}
		}
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    u.ID.String(),
			Action:     "share.accepted",
			TargetType: "tenant",
			TargetID:   tenantID.String(),
			Metadata:   map[string]any{"invitation_id": inv.ID.String(), "role": inv.Role},
		})

	case "site":
		if !inv.SiteID.Valid {
			return AcceptResult{}, domain.Internal("invitation_site_missing", "site invitation has no site_id")
		}
		siteID := uuid.UUID(inv.SiteID.Bytes)
		err = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := sqlc.New(tx).CreateShare(ctx, sqlc.CreateShareParams{
				TenantID:  tenantID,
				SiteID:    siteID,
				UserID:    u.ID,
				Role:      inv.Role,
				GrantedBy: inv.InvitedBy,
				ExpiresAt: pgtype.Timestamptz{Valid: false},
			})
			return err
		})
		if err != nil {
			return AcceptResult{}, domain.Internal("share_create_failed", "failed to grant site access").WithCause(err)
		}
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    u.ID.String(),
			Action:     "share.accepted",
			TargetType: "site",
			TargetID:   siteID.String(),
			Metadata:   map[string]any{"invitation_id": inv.ID.String(), "role": inv.Role},
		})

	default:
		return AcceptResult{}, domain.Internal("invitation_scope_unknown", "unknown invitation scope: "+inv.Scope)
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
	return result, nil
}

func (s *Service) incrementAttempts(ctx context.Context, tenantID, invID uuid.UUID) error {
	return s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).IncrementInviteAttempts(ctx, invID)
		return err
	})
}
