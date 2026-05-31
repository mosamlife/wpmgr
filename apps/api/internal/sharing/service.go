package sharing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Mailer is a narrow interface for sending invitation emails.
type Mailer interface {
	Send(ctx context.Context, recipients []string, subject, body string) error
}

// Service implements site-share CRUD and share-invitation flow.
type Service struct {
	pool     *db.Pool
	authRepo *auth.Repo
	audit    *audit.Recorder
	mailer   Mailer // may be nil (SMTP unconfigured)
	baseURL  string // PUBLIC_BASE_URL for accept links
}

// NewService builds a sharing Service.
func NewService(pool *db.Pool, authRepo *auth.Repo, rec *audit.Recorder, mailer Mailer, baseURL string) *Service {
	return &Service{pool: pool, authRepo: authRepo, audit: rec, mailer: mailer, baseURL: baseURL}
}

// ListForSite returns all shares for a site (tenant-scoped), each enriched with
// the collaborator's email + name so the UI shows a human identity, not a UUID.
func (s *Service) ListForSite(ctx context.Context, tenantID, siteID uuid.UUID) ([]Share, error) {
	var out []Share
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSharesForSite(ctx, siteID)
		if err != nil {
			return domain.Internal("share_list_failed", "failed to list shares").WithCause(err)
		}
		out = make([]Share, 0, len(rows))
		for _, r := range rows {
			out = append(out, rowToShare(r))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.attachUserIdentities(ctx, out)
	return out, nil
}

// attachUserIdentities batch-resolves each share's user_id to email + name via
// the auth repo (users has no RLS, so this spans collaborators outside the org).
// Best-effort: on lookup failure the rows simply keep empty email/name and the
// UI falls back to the id.
func (s *Service) attachUserIdentities(ctx context.Context, shares []Share) {
	if len(shares) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(shares))
	for _, sh := range shares {
		ids = append(ids, sh.UserID)
	}
	briefs, err := s.authRepo.GetUsersByIDs(ctx, ids)
	if err != nil {
		return
	}
	byID := make(map[uuid.UUID]auth.UserBrief, len(briefs))
	for _, b := range briefs {
		byID[b.ID] = b
	}
	for i := range shares {
		if b, ok := byID[shares[i].UserID]; ok {
			shares[i].Email = b.Email
			shares[i].Name = b.Name
		}
	}
}

// GrantInput is the input for granting/inviting a share.
type GrantInput struct {
	Email     string
	Role      string
	ExpiresAt *time.Time
	ActorID   uuid.UUID
}

// GrantResult is returned by Grant.
type GrantResult struct {
	Share      *Share // set when user already exists
	AcceptLink string // set when invitation created and SMTP is not configured
	Invited    bool   // true when an invitation was created (email unknown)
}

// Grant grants site access to the given email. If the email maps to an existing
// user, a site_shares row is upserted immediately. Otherwise an invitation
// (scope=site) is created and an accept email is sent (best-effort).
func (s *Service) Grant(ctx context.Context, tenantID, siteID uuid.UUID, in GrantInput) (GrantResult, error) {
	// Validate role: site shares cannot be owner.
	switch in.Role {
	case "viewer", "operator", "admin":
	default:
		return GrantResult{}, domain.Validation("invalid_role", "site share role must be viewer, operator, or admin")
	}

	// Try to resolve an existing user by email.
	u, err := s.authRepo.GetUserByEmail(ctx, in.Email)
	if err != nil {
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindNotFound {
			return GrantResult{}, err
		}
		// Unknown user: create invitation.
		return s.createSiteInvitation(ctx, tenantID, siteID, in)
	}

	// Known user: upsert site_shares immediately.
	share, err := s.upsertShare(ctx, tenantID, siteID, u.ID, in.Role, in.ActorID, in.ExpiresAt)
	if err != nil {
		return GrantResult{}, err
	}

	// Audit: share.granted
	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    in.ActorID.String(),
		Action:     "share.granted",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"grantee_id": u.ID.String(), "role": in.Role},
	})

	return GrantResult{Share: &share}, nil
}

func (s *Service) upsertShare(ctx context.Context, tenantID, siteID, userID uuid.UUID, role string, actorID uuid.UUID, expiresAt *time.Time) (Share, error) {
	var out Share
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var expiresAtPg pgtype.Timestamptz
		if expiresAt != nil {
			expiresAtPg = pgtype.Timestamptz{Time: *expiresAt, Valid: true}
		}
		row, err := sqlc.New(tx).CreateShare(ctx, sqlc.CreateShareParams{
			TenantID:  tenantID,
			SiteID:    siteID,
			UserID:    userID,
			Role:      role,
			GrantedBy: pgtype.UUID{Bytes: actorID, Valid: actorID != uuid.Nil},
			ExpiresAt: expiresAtPg,
		})
		if err != nil {
			return domain.Internal("share_create_failed", "failed to create share").WithCause(err)
		}
		out = rowToShare(row)
		return nil
	})
	return out, err
}

func (s *Service) createSiteInvitation(ctx context.Context, tenantID, siteID uuid.UUID, in GrantInput) (GrantResult, error) {
	// Generate a token.
	rawToken, tokenHash, err := generateToken()
	if err != nil {
		return GrantResult{}, domain.Internal("token_gen_failed", "failed to generate invitation token").WithCause(err)
	}

	var inv sqlc.Invitation
	err = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var siteIDPg pgtype.UUID
		siteIDPg.Bytes = siteID
		siteIDPg.Valid = true

		row, err := sqlc.New(tx).CreateInvitation(ctx, sqlc.CreateInvitationParams{
			TenantID:  tenantID,
			Email:     in.Email,
			Scope:     "site",
			SiteID:    siteIDPg,
			Role:      in.Role,
			TokenHash: tokenHash,
			InvitedBy: pgtype.UUID{Bytes: in.ActorID, Valid: in.ActorID != uuid.Nil},
			ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
		})
		if err != nil {
			return domain.Internal("invitation_create_failed", "failed to create invitation").WithCause(err)
		}
		inv = row
		return nil
	})
	if err != nil {
		return GrantResult{}, err
	}

	acceptLink := s.baseURL + "/accept?token=" + rawToken

	// Audit: share.invited
	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    in.ActorID.String(),
		Action:     "share.invited",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"email": in.Email, "role": in.Role, "invitation_id": inv.ID.String()},
	})

	// Send email best-effort.
	var returnLink string
	if s.mailer != nil {
		body := "You have been invited to collaborate on a site.\n\nAccept your invitation here:\n" + acceptLink + "\n\nThis link expires in 7 days and is single-use."
		if err := s.mailer.Send(ctx, []string{in.Email}, "You have been invited to a site", body); err != nil {
			// Email failed but SMTP is configured: return link as fallback.
			returnLink = acceptLink
		}
	} else {
		// SMTP not configured: return link directly.
		returnLink = acceptLink
	}

	return GrantResult{Invited: true, AcceptLink: returnLink}, nil
}

// Revoke removes a site share (immediate).
func (s *Service) Revoke(ctx context.Context, tenantID, siteID, userID, actorID uuid.UUID) error {
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).DeleteShare(ctx, sqlc.DeleteShareParams{
			SiteID: siteID,
			UserID: userID,
		})
		if err != nil {
			return domain.Internal("share_delete_failed", "failed to delete share").WithCause(err)
		}
		if n == 0 {
			return domain.NotFound("share_not_found", "share not found")
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Audit: share.revoked
	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    actorID.String(),
		Action:     "share.revoked",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"grantee_id": userID.String()},
	})
	return nil
}

// SharedWithMe returns the non-expired shares for the given user across all
// tenants (site_shares_self_read policy).
func (s *Service) SharedWithMe(ctx context.Context, userID uuid.UUID) ([]Share, error) {
	var out []Share
	err := s.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		// Enriched read: site_shares_self_read + the M22 sites_shared_read policy
		// let this JOIN surface the (cross-tenant) site url/name + owning-org name.
		rows, err := sqlc.New(tx).ListSharedSitesForUser(ctx, userID)
		if err != nil {
			return domain.Internal("share_list_failed", "failed to list shares").WithCause(err)
		}
		out = make([]Share, 0, len(rows))
		for _, r := range rows {
			sh := Share{
				ID:        r.ID,
				TenantID:  r.TenantID,
				SiteID:    r.SiteID,
				UserID:    r.UserID,
				Role:      r.Role,
				CreatedAt: r.CreatedAt,
				SiteURL:   r.SiteUrl,
				SiteName:  r.SiteName,
				OrgName:   r.OrgName,
			}
			if r.GrantedBy.Valid {
				id := uuid.UUID(r.GrantedBy.Bytes)
				sh.GrantedBy = &id
			}
			if r.ExpiresAt.Valid {
				t := r.ExpiresAt.Time
				sh.ExpiresAt = &t
			}
			out = append(out, sh)
		}
		return nil
	})
	return out, err
}

// ListInvitationsForSite returns the full invitation history (pending +
// accepted + expired + revoked) for a site, newest first. Tenant-scoped by RLS.
func (s *Service) ListInvitationsForSite(ctx context.Context, tenantID, siteID uuid.UUID) ([]Invitation, error) {
	var out []Invitation
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListInvitationsForSite(ctx, sqlc.ListInvitationsForSiteParams{
			TenantID: tenantID,
			SiteID:   pgtype.UUID{Bytes: siteID, Valid: true},
		})
		if err != nil {
			return domain.Internal("invitation_list_failed", "failed to list invitations").WithCause(err)
		}
		out = make([]Invitation, 0, len(rows))
		for _, r := range rows {
			out = append(out, rowToInvitation(r))
		}
		return nil
	})
	return out, err
}

// loadSiteInvitationTx loads an invitation by id inside an open tenant tx and
// verifies it is a site-scoped invite bound to the expected site. Tenant
// isolation is enforced by RLS (a cross-tenant id yields ErrNoRows -> NotFound);
// the scope + site_id checks block managing an org invite or another site's
// invite through this site's route. Returns NotFound (never Forbidden) on
// mismatch to avoid leaking invitation existence across sites.
func loadSiteInvitationTx(ctx context.Context, tx pgx.Tx, invID, siteID uuid.UUID) (sqlc.Invitation, error) {
	row, err := sqlc.New(tx).GetInvitationByID(ctx, invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.Invitation{}, domain.NotFound("invitation_not_found", "invitation not found")
		}
		return sqlc.Invitation{}, domain.Internal("invitation_load_failed", "failed to load invitation").WithCause(err)
	}
	if row.Scope != "site" || !row.SiteID.Valid || uuid.UUID(row.SiteID.Bytes) != siteID {
		return sqlc.Invitation{}, domain.NotFound("invitation_not_found", "invitation not found")
	}
	return row, nil
}

// RevokeInvitation soft-revokes a still-pending invitation for a site. Race-safe:
// an invite accepted between load and update is left untouched (returns Conflict).
func (s *Service) RevokeInvitation(ctx context.Context, tenantID, siteID, invID, actorID uuid.UUID) error {
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := loadSiteInvitationTx(ctx, tx, invID, siteID); err != nil {
			return err
		}
		if _, err := sqlc.New(tx).RevokeInvitation(ctx, sqlc.RevokeInvitationParams{
			ID:        invID,
			RevokedBy: pgtype.UUID{Bytes: actorID, Valid: actorID != uuid.Nil},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Accepted or already revoked between load and update.
				return domain.Conflict("invitation_not_pending", "this invite was already accepted, expired, or revoked")
			}
			return domain.Internal("invitation_revoke_failed", "failed to revoke invitation").WithCause(err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    actorID.String(),
		Action:     "share.invitation_revoked",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"invitation_id": invID.String()},
	})
	return nil
}

// RegenerateInvite rotates the token of a still-pending invitation: it mints a
// fresh token (invalidating the old link), resets expiry + attempts, and clears
// any prior soft-revoke. Returns the new one-time accept link (and re-sends the
// invite email best-effort, mirroring the create path). Race-safe: an accepted
// invite is left untouched (returns Conflict).
func (s *Service) RegenerateInvite(ctx context.Context, tenantID, siteID, invID, actorID uuid.UUID) (string, error) {
	rawToken, tokenHash, err := generateToken()
	if err != nil {
		return "", domain.Internal("token_gen_failed", "failed to generate invitation token").WithCause(err)
	}

	var email string
	err = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		existing, lerr := loadSiteInvitationTx(ctx, tx, invID, siteID)
		if lerr != nil {
			return lerr
		}
		email = existing.Email
		if _, uerr := sqlc.New(tx).RegenerateInvitationToken(ctx, sqlc.RegenerateInvitationTokenParams{
			ID:        invID,
			TokenHash: tokenHash,
			ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
		}); uerr != nil {
			if errors.Is(uerr, pgx.ErrNoRows) {
				return domain.Conflict("invitation_not_pending", "this invite was already accepted")
			}
			return domain.Internal("invitation_regenerate_failed", "failed to regenerate invitation").WithCause(uerr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	acceptLink := s.baseURL + "/accept?token=" + rawToken

	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    actorID.String(),
		Action:     "share.invitation_regenerated",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"invitation_id": invID.String(), "email": email},
	})

	// Best-effort re-send (mirrors createSiteInvitation). The link is always
	// returned to the caller for the copy-link flow.
	if s.mailer != nil && email != "" {
		body := "Your invitation link was refreshed.\n\nAccept your invitation here:\n" + acceptLink + "\n\nThe previous link no longer works. This link expires in 7 days and is single-use."
		_ = s.mailer.Send(ctx, []string{email}, "Your site invitation link was refreshed", body)
	}

	return acceptLink, nil
}

func rowToInvitation(r sqlc.Invitation) Invitation {
	inv := Invitation{
		ID:        r.ID,
		TenantID:  r.TenantID,
		Email:     r.Email,
		Role:      r.Role,
		ExpiresAt: r.ExpiresAt,
		Attempts:  int(r.Attempts),
		CreatedAt: r.CreatedAt,
	}
	if r.SiteID.Valid {
		id := uuid.UUID(r.SiteID.Bytes)
		inv.SiteID = &id
	}
	if r.InvitedBy.Valid {
		id := uuid.UUID(r.InvitedBy.Bytes)
		inv.InvitedBy = &id
	}
	if r.AcceptedAt.Valid {
		t := r.AcceptedAt.Time
		inv.AcceptedAt = &t
	}
	if r.RevokedAt.Valid {
		t := r.RevokedAt.Time
		inv.RevokedAt = &t
	}
	if r.RevokedBy.Valid {
		id := uuid.UUID(r.RevokedBy.Bytes)
		inv.RevokedBy = &id
	}
	return inv
}

// CountOwners returns the number of owners in a tenant (for last-owner protection).
func (s *Service) CountOwners(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var count int
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var n int64
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner'`,
			tenantID,
		).Scan(&n); err != nil {
			return domain.Internal("owner_count_failed", "failed to count owners").WithCause(err)
		}
		count = int(n)
		return nil
	})
	return count, err
}

func rowToShare(r sqlc.SiteShare) Share {
	s := Share{
		ID:        r.ID,
		TenantID:  r.TenantID,
		SiteID:    r.SiteID,
		UserID:    r.UserID,
		Role:      r.Role,
		CreatedAt: r.CreatedAt,
	}
	if r.GrantedBy.Valid {
		id := uuid.UUID(r.GrantedBy.Bytes)
		s.GrantedBy = &id
	}
	if r.ExpiresAt.Valid {
		t := r.ExpiresAt.Time
		s.ExpiresAt = &t
	}
	return s
}

// generateToken creates a cryptographically random token and returns both the
// raw token (returned to the caller once) and its SHA-256 hex hash (stored).
func generateToken() (raw, hash string, err error) {
	return generateSecureToken()
}
