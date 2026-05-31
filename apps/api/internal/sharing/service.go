package sharing

import (
	"context"
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

// ListForSite returns all shares for a site (tenant-scoped).
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
	return out, err
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
		rows, err := sqlc.New(tx).ListSharesForUser(ctx, userID)
		if err != nil {
			return domain.Internal("share_list_failed", "failed to list shares").WithCause(err)
		}
		out = make([]Share, 0, len(rows))
		for _, r := range rows {
			out = append(out, rowToShare(r))
		}
		return nil
	})
	return out, err
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
