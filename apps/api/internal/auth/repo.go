package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo persists users and memberships. Users are not tenant-scoped; membership
// writes/reads are RLS-scoped per tenant, while a user's own cross-tenant
// membership listing uses the memberships_self_read policy (InUserTx).
type Repo struct {
	pool *db.Pool
	q    *sqlc.Queries
}

// NewRepo builds an auth Repo over the pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool, q: sqlc.New(pool.Pool)}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func userToModel(u sqlc.User) User {
	m := User{
		ID:           u.ID,
		Email:        u.Email,
		PasswordHash: deref(u.PasswordHash),
		OIDCSubject:  deref(u.OidcSubject),
		OIDCIssuer:   deref(u.OidcIssuer),
		Name:         u.Name,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
	}
	if u.LastLoginAt.Valid {
		t := u.LastLoginAt.Time
		m.LastLoginAt = &t
	}
	return m
}

func membershipToModel(m sqlc.Membership) Membership {
	return Membership{
		UserID:    m.UserID,
		TenantID:  m.TenantID,
		Role:      authz.Role(m.Role),
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}

// CountUsers returns the total number of users (used for first-run bootstrap).
func (r *Repo) CountUsers(ctx context.Context) (int64, error) {
	n, err := r.q.CountUsers(ctx)
	if err != nil {
		return 0, domain.Internal("user_count_failed", "failed to count users").WithCause(err)
	}
	return n, nil
}

// CreateUser inserts a new user.
func (r *Repo) CreateUser(ctx context.Context, email, passwordHash, name, oidcIssuer, oidcSubject string) (User, error) {
	row, err := r.q.CreateUser(ctx, sqlc.CreateUserParams{
		Email:        email,
		PasswordHash: strPtr(passwordHash),
		OidcSubject:  strPtr(oidcSubject),
		OidcIssuer:   strPtr(oidcIssuer),
		Name:         name,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return User{}, domain.Conflict("user_exists", "a user with this email already exists").WithCause(err)
		}
		return User{}, domain.Internal("user_create_failed", "failed to create user").WithCause(err)
	}
	return userToModel(row), nil
}

// GetUserByEmail loads a user by email.
func (r *Repo) GetUserByEmail(ctx context.Context, email string) (User, error) {
	row, err := r.q.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, domain.NotFound("user_not_found", "user not found")
		}
		return User{}, domain.Internal("user_get_failed", "failed to load user").WithCause(err)
	}
	return userToModel(row), nil
}

// GetUserByID loads a user by id.
func (r *Repo) GetUserByID(ctx context.Context, id uuid.UUID) (User, error) {
	row, err := r.q.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, domain.NotFound("user_not_found", "user not found")
		}
		return User{}, domain.Internal("user_get_failed", "failed to load user").WithCause(err)
	}
	return userToModel(row), nil
}

// GetUserByOIDC loads a user by their OIDC identity.
func (r *Repo) GetUserByOIDC(ctx context.Context, issuer, subject string) (User, error) {
	row, err := r.q.GetUserByOIDC(ctx, sqlc.GetUserByOIDCParams{OidcIssuer: strPtr(issuer), OidcSubject: strPtr(subject)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, domain.NotFound("user_not_found", "user not found")
		}
		return User{}, domain.Internal("user_get_failed", "failed to load user").WithCause(err)
	}
	return userToModel(row), nil
}

// LinkOIDC attaches an OIDC identity to an existing user.
func (r *Repo) LinkOIDC(ctx context.Context, userID uuid.UUID, issuer, subject string) (User, error) {
	row, err := r.q.LinkUserOIDC(ctx, sqlc.LinkUserOIDCParams{ID: userID, OidcIssuer: strPtr(issuer), OidcSubject: strPtr(subject)})
	if err != nil {
		return User{}, domain.Internal("user_link_failed", "failed to link OIDC identity").WithCause(err)
	}
	return userToModel(row), nil
}

// TouchLogin updates the user's last_login_at.
func (r *Repo) TouchLogin(ctx context.Context, userID uuid.UUID) error {
	if err := r.q.TouchUserLogin(ctx, userID); err != nil {
		return domain.Internal("user_touch_failed", "failed to record login").WithCause(err)
	}
	return nil
}

// UpdateName sets the user's display name and bumps updated_at. Returns the
// updated User. The users table has no RLS, so no tenant transaction is needed.
func (r *Repo) UpdateName(ctx context.Context, userID uuid.UUID, name string) (User, error) {
	row := r.pool.QueryRow(ctx,
		`UPDATE users SET name = $1, updated_at = now() WHERE id = $2
		 RETURNING id, email, password_hash, oidc_subject, oidc_issuer, name, created_at, updated_at, last_login_at`,
		name, userID,
	)
	var u sqlc.User
	if err := row.Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.OidcSubject, &u.OidcIssuer,
		&u.Name, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, domain.NotFound("user_not_found", "user not found")
		}
		return User{}, domain.Internal("user_update_failed", "failed to update user name").WithCause(err)
	}
	return userToModel(u), nil
}

// UpdatePasswordHash replaces the stored argon2id hash for the given user.
// SetUserPasswordHash in sqlc already exists; we call it directly.
func (r *Repo) UpdatePasswordHash(ctx context.Context, userID uuid.UUID, hash string) error {
	if err := r.q.SetUserPasswordHash(ctx, sqlc.SetUserPasswordHashParams{
		ID:           userID,
		PasswordHash: strPtr(hash),
	}); err != nil {
		return domain.Internal("password_update_failed", "failed to update password").WithCause(err)
	}
	return nil
}

// UserBrief carries just the identity fields needed for actor resolution.
type UserBrief struct {
	ID    uuid.UUID
	Email string
	Name  string
}

// GetUsersByIDs returns brief identity records for the given IDs (batch). The
// users table has no RLS. Unknown IDs are silently omitted from the result.
func (r *Repo) GetUsersByIDs(ctx context.Context, ids []uuid.UUID) ([]UserBrief, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id, email, name FROM users WHERE id = ANY($1)`, ids,
	)
	if err != nil {
		return nil, domain.Internal("user_batch_get_failed", "failed to batch-load users").WithCause(err)
	}
	defer rows.Close()
	var out []UserBrief
	for rows.Next() {
		var b UserBrief
		if err := rows.Scan(&b.ID, &b.Email, &b.Name); err != nil {
			return nil, domain.Internal("user_batch_scan_failed", "failed to scan user row").WithCause(err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateMembership inserts a membership inside the tenant's RLS scope.
func (r *Repo) CreateMembership(ctx context.Context, userID, tenantID uuid.UUID, role authz.Role) (Membership, error) {
	var out Membership
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateMembership(ctx, sqlc.CreateMembershipParams{
			UserID:   userID,
			TenantID: tenantID,
			Role:     string(role),
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.Conflict("membership_exists", "user is already a member of this tenant").WithCause(err)
			}
			return domain.Internal("membership_create_failed", "failed to create membership").WithCause(err)
		}
		out = membershipToModel(row)
		return nil
	})
	return out, err
}

// GetMembership loads a single membership within the tenant's RLS scope.
func (r *Repo) GetMembership(ctx context.Context, userID, tenantID uuid.UUID) (Membership, error) {
	var out Membership
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetMembership(ctx, sqlc.GetMembershipParams{UserID: userID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("membership_not_found", "membership not found")
			}
			return domain.Internal("membership_get_failed", "failed to load membership").WithCause(err)
		}
		out = membershipToModel(row)
		return nil
	})
	return out, err
}

// ListMembershipsForUser returns the user's memberships across all tenants,
// using the self-read policy (no active tenant required).
func (r *Repo) ListMembershipsForUser(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	var out []Membership
	err := r.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListMembershipsForUser(ctx, userID)
		if err != nil {
			return domain.Internal("membership_list_failed", "failed to list memberships").WithCause(err)
		}
		out = make([]Membership, 0, len(rows))
		for _, row := range rows {
			out = append(out, membershipToModel(row))
		}
		return nil
	})
	return out, err
}

// FirstActiveShareTenant returns the tenant of the user's earliest non-expired
// site_share, used at login to pick an active tenant for a site-scoped
// collaborator who has NO org membership. Without this the session would carry
// no active tenant and the auth middleware's site-scope branch (gated on
// activeTenant != Nil) would never run, 403ing every request after re-login.
// Runs under InUserTx so the site_shares_self_read RLS policy (user_id =
// app.user_id) permits the SELECT. Returns ok=false on no shares or error.
func (r *Repo) FirstActiveShareTenant(ctx context.Context, userID uuid.UUID) (uuid.UUID, bool) {
	var tenantID uuid.UUID
	var found bool
	err := r.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		shares, err := sqlc.New(tx).ListSharesForUser(ctx, userID)
		if err != nil {
			return err
		}
		if len(shares) > 0 {
			tenantID = shares[0].TenantID // ordered by created_at ASC
			found = true
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, false
	}
	return tenantID, found
}

// ListMembershipsForTenant returns a tenant's members (RLS-scoped).
func (r *Repo) ListMembershipsForTenant(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Membership, error) {
	var out []Membership
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListMembershipsForTenant(ctx, sqlc.ListMembershipsForTenantParams{TenantID: tenantID, Limit: limit, Offset: offset})
		if err != nil {
			return domain.Internal("membership_list_failed", "failed to list memberships").WithCause(err)
		}
		out = make([]Membership, 0, len(rows))
		for _, row := range rows {
			out = append(out, membershipToModel(row))
		}
		return nil
	})
	return out, err
}

// UpdateMembershipRole changes a member's role (RLS-scoped).
func (r *Repo) UpdateMembershipRole(ctx context.Context, userID, tenantID uuid.UUID, role authz.Role) (Membership, error) {
	var out Membership
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).UpdateMembershipRole(ctx, sqlc.UpdateMembershipRoleParams{UserID: userID, TenantID: tenantID, Role: string(role)})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("membership_not_found", "membership not found")
			}
			return domain.Internal("membership_update_failed", "failed to update membership").WithCause(err)
		}
		out = membershipToModel(row)
		return nil
	})
	return out, err
}

// CountOwners returns the number of owner-role members in a tenant (RLS-scoped).
func (r *Repo) CountOwners(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var count int
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
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

// DeleteMembership removes a member from a tenant (RLS-scoped).
func (r *Repo) DeleteMembership(ctx context.Context, userID, tenantID uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).DeleteMembership(ctx, sqlc.DeleteMembershipParams{UserID: userID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("membership_delete_failed", "failed to delete membership").WithCause(err)
		}
		if n == 0 {
			return domain.NotFound("membership_not_found", "membership not found")
		}
		return nil
	})
}
