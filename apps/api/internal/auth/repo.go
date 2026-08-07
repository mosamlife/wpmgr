package auth

import (
	"context"
	"errors"
	"time"

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
		Status:       u.Status,
		IsSuperadmin: u.IsSuperadmin,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
		// m73: two-factor authentication fields (ADR-056).
		TwoFactorEnabled:    u.TwoFactorEnabled,
		TOTPSecretEncrypted: u.TotpSecretEncrypted,
	}
	if u.LastLoginAt.Valid {
		t := u.LastLoginAt.Time
		m.LastLoginAt = &t
	}
	if u.EmailVerifiedAt.Valid {
		t := u.EmailVerifiedAt.Time
		m.EmailVerifiedAt = &t
	}
	if u.TotpConfirmedAt.Valid {
		t := u.TotpConfirmedAt.Time
		m.TOTPConfirmedAt = &t
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
		 RETURNING id, email, password_hash, oidc_subject, oidc_issuer, name, created_at, updated_at,
		           last_login_at, password_changed_at, status, email_verified_at, is_superadmin,
		           two_factor_enabled, totp_secret_encrypted, totp_confirmed_at`,
		name, userID,
	)
	var u sqlc.User
	if err := row.Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.OidcSubject, &u.OidcIssuer,
		&u.Name, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt, &u.PasswordChangedAt,
		&u.Status, &u.EmailVerifiedAt, &u.IsSuperadmin,
		&u.TwoFactorEnabled, &u.TotpSecretEncrypted, &u.TotpConfirmedAt,
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

// SetUserPending marks a self-registered account unverified (ADR-045 Phase 3).
func (r *Repo) SetUserPending(ctx context.Context, userID uuid.UUID) error {
	return r.q.SetUserPending(ctx, userID)
}

// MarkUserEmailVerified activates + verifies an account on activation/bootstrap.
func (r *Repo) MarkUserEmailVerified(ctx context.Context, userID uuid.UUID) error {
	return r.q.MarkUserEmailVerified(ctx, userID)
}

// GetUserPasswordChangedAt returns the user's last password-change time and
// whether it is set. Used by the Authenticator to reject stale sessions
// (ADR-045 Phase 2). users is not RLS-scoped, so this runs on the bare pool.
func (r *Repo) GetUserPasswordChangedAt(ctx context.Context, userID uuid.UUID) (time.Time, bool, error) {
	ts, err := r.q.GetUserPasswordChangedAt(ctx, userID)
	if err != nil {
		return time.Time{}, false, err
	}
	if !ts.Valid {
		return time.Time{}, false, nil
	}
	return ts.Time, true, nil
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

// FirstClientMemberTenant returns the tenant of the user's earliest client
// membership, used at login to pick an active tenant for portal-only users who
// have no org membership and no site_share. Mirrors FirstActiveShareTenant.
// Runs under InUserTx so client_members_self_read (user_id = app.user_id)
// allows the SELECT. Returns ok=false on no memberships or error.
func (r *Repo) FirstClientMemberTenant(ctx context.Context, userID uuid.UUID) (uuid.UUID, bool) {
	var tenantID uuid.UUID
	var found bool
	err := r.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		tid, qerr := sqlc.New(tx).FirstClientMemberTenant(ctx, userID)
		if qerr != nil {
			return qerr
		}
		tenantID = tid
		found = true
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

// ---------------------------------------------------------------------------
// Social identities (m110)
// ---------------------------------------------------------------------------

// Identity is a linked external sign-in method.
type Identity struct {
	UserID        uuid.UUID
	Provider      string
	Subject       string
	Issuer        string
	Email         string
	EmailVerified bool
	// CreatedAt and LastLoginAt are read-only display facts for the connected
	// accounts list. LastLoginAt is nil until the identity has been used to
	// sign in at least once, which is the normal state for one just linked.
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

func identityToModel(row sqlc.UserIdentity) Identity {
	out := Identity{
		UserID:        row.UserID,
		Provider:      row.Provider,
		Subject:       row.Subject,
		Issuer:        row.Issuer,
		Email:         row.Email,
		EmailVerified: row.EmailVerified,
		CreatedAt:     row.CreatedAt,
	}
	if row.LastLoginAt.Valid {
		t := row.LastLoginAt.Time
		out.LastLoginAt = &t
	}
	return out
}

// GetUserByIdentity resolves the local user behind an external identity. This
// is the ONLY function that authenticates a social sign-in: (provider, subject,
// issuer) is the provider's immutable id for the person, and no email is
// consulted. Email is used elsewhere to decide whether a NEW identity may be
// linked, never to decide who is signing in.
func (r *Repo) GetUserByIdentity(ctx context.Context, provider, subject, issuer string) (User, error) {
	row, err := r.q.GetUserByIdentity(ctx, sqlc.GetUserByIdentityParams{
		Provider: provider, Subject: subject, Issuer: issuer,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, domain.NotFound("identity_not_found", "no user for this identity")
		}
		return User{}, domain.Internal("identity_get_failed", "failed to load identity").WithCause(err)
	}
	return userToModel(row), nil
}

// CreateIdentity links an external identity to an existing user.
func (r *Repo) CreateIdentity(ctx context.Context, in Identity) error {
	_, err := r.q.CreateIdentity(ctx, sqlc.CreateIdentityParams{
		UserID:        in.UserID,
		Provider:      in.Provider,
		Subject:       in.Subject,
		Issuer:        in.Issuer,
		Email:         in.Email,
		EmailVerified: in.EmailVerified,
	})
	if err != nil {
		return domain.Internal("identity_create_failed", "failed to link identity").WithCause(err)
	}
	return nil
}

// TouchIdentityLogin stamps the login and records the address the provider
// reported this time, which may differ from last time.
func (r *Repo) TouchIdentityLogin(ctx context.Context, provider, subject, issuer, email string, verified bool) {
	_ = r.q.TouchIdentityLogin(ctx, sqlc.TouchIdentityLoginParams{
		Provider: provider, Subject: subject, Issuer: issuer,
		Email: email, EmailVerified: verified,
	})
}

// ListIdentitiesForUser powers the account settings list.
func (r *Repo) ListIdentitiesForUser(ctx context.Context, userID uuid.UUID) ([]Identity, error) {
	rows, err := r.q.ListIdentitiesForUser(ctx, userID)
	if err != nil {
		return nil, domain.Internal("identity_list_failed", "failed to list identities").WithCause(err)
	}
	out := make([]Identity, 0, len(rows))
	for _, row := range rows {
		out = append(out, identityToModel(row))
	}
	return out, nil
}

// UnlinkIdentity removes one external sign-in method, and only when the account
// keeps at least one other way in. decideUnlink holds the rule; this function
// exists to make the rule's inputs and the delete ATOMIC.
//
// WHY A TRANSACTION AND A ROW LOCK. Read-then-delete across two statements has
// a gap, and the gap is reachable without an attacker: someone with two linked
// providers and no password, disconnecting both from two browser tabs, has each
// request read "two identities, removing one is safe" and then delete a
// different row. The account ends with no password and no identity.
//
// Nothing recovers from that state. Password reset deliberately refuses to mint
// a set-password link for an account with no password (that would turn reset
// into account creation for anyone who knows the address), so there is no way
// back in and no way to prove ownership. That is why this is a locked
// transaction rather than a count followed by a delete.
//
// The lock is taken on the users row, which is also where password_hash lives,
// so one statement both serialises concurrent unlinks for this user and reads
// the password half of the rule on the same snapshot as the identity list. Any
// concurrent unlink waits and then sees this one's result; SetInitialPassword
// locks the same row, so a password being added mid-unlink cannot be missed
// either way round.
func (r *Repo) UnlinkIdentity(ctx context.Context, userID uuid.UUID, provider string) error {
	return r.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		var hash *string
		err := tx.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&hash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("user_not_found", "user not found")
			}
			return domain.Internal("identity_unlink_failed", "failed to unlink identity").WithCause(err)
		}

		q := sqlc.New(tx)
		// One read answers both halves of the rule: is this provider actually
		// linked, and how many methods would be left. Counting separately would
		// reintroduce the gap the lock exists to close.
		rows, err := q.ListIdentitiesForUser(ctx, userID)
		if err != nil {
			return domain.Internal("identity_list_failed", "failed to list identities").WithCause(err)
		}
		linked := false
		for _, row := range rows {
			if row.Provider == provider {
				linked = true
				break
			}
		}
		if err := decideUnlink(linked, len(rows), deref(hash) != "", provider); err != nil {
			return err
		}
		if err := q.DeleteIdentity(ctx, sqlc.DeleteIdentityParams{UserID: userID, Provider: provider}); err != nil {
			return domain.Internal("identity_delete_failed", "failed to unlink identity").WithCause(err)
		}
		return nil
	})
}

// SetInitialPasswordHash writes a first password for an account that has none.
// The "has none" half is decided by the UPDATE's own WHERE clause rather than by
// a preceding read, so this can never overwrite a password that already exists:
// see SetUserInitialPassword in db/query/users.sql. Zero rows affected is that
// case, and it is a conflict, not a failure.
func (r *Repo) SetInitialPasswordHash(ctx context.Context, userID uuid.UUID, hash string) error {
	n, err := r.q.SetUserInitialPassword(ctx, sqlc.SetUserInitialPasswordParams{
		ID:           userID,
		PasswordHash: strPtr(hash),
	})
	if err != nil {
		return domain.Internal("password_update_failed", "failed to set password").WithCause(err)
	}
	if n == 0 {
		return domain.Conflict("password_already_set",
			"this account already has a password. Change it from the change password form, which asks for the current one.")
	}
	return nil
}
