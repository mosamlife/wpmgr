package auth

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Service holds authentication business logic: password login, first-run
// bootstrap, invited registration, and OIDC user upsert. It records auth events
// to the audit log.
type Service struct {
	repo      *Repo
	audit     *audit.Recorder
	validator *domain.Validator
}

// NewService builds an auth Service.
func NewService(repo *Repo, rec *audit.Recorder, v *domain.Validator) *Service {
	return &Service{repo: repo, audit: rec, validator: v}
}

// LoginResult is the outcome of a successful login.
type LoginResult struct {
	User         User
	Memberships  []Membership
	ActiveTenant uuid.UUID
}

// loginInput validates the email/password login body.
type loginInput struct {
	Email    string `validate:"required,email"`
	Password string `validate:"required"`
}

// Login verifies an email+password and returns the user with their memberships.
// It records a login success/failure audit event against the user's first
// tenant (failures with no resolvable tenant are not chained to any tenant).
func (s *Service) Login(ctx context.Context, email, password string) (LoginResult, error) {
	email = normalizeEmail(email)
	if err := s.validator.Struct(loginInput{Email: email, Password: password}); err != nil {
		return LoginResult{}, err
	}

	u, err := s.repo.GetUserByEmail(ctx, email)
	if err != nil {
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
			// Do not reveal whether the email exists.
			return LoginResult{}, domain.Unauthorized("invalid_credentials", "invalid email or password")
		}
		return LoginResult{}, err
	}

	if u.PasswordHash == "" {
		return LoginResult{}, domain.Unauthorized("password_login_disabled", "this account has no password set; use SSO")
	}
	match, verr := VerifyPassword(password, u.PasswordHash)
	if verr != nil {
		return LoginResult{}, domain.Internal("password_verify_failed", "failed to verify password").WithCause(verr)
	}

	memberships, _ := s.repo.ListMembershipsForUser(ctx, u.ID)
	if !match {
		s.recordLogin(ctx, memberships, u.ID, audit.ActionLoginFailure)
		return LoginResult{}, domain.Unauthorized("invalid_credentials", "invalid email or password")
	}

	_ = s.repo.TouchLogin(ctx, u.ID)
	s.recordLogin(ctx, memberships, u.ID, audit.ActionLoginSuccess)

	res := LoginResult{User: u, Memberships: memberships}
	res.ActiveTenant = s.resolveActiveTenant(ctx, u.ID, memberships)
	return res, nil
}

// resolveActiveTenant picks the session's active tenant after authentication.
// Org members use their first membership; a user with NO membership but an
// active site_share (an outside collaborator) falls back to that share's
// tenant, so site-scoped access survives logout/login. Without the fallback the
// session would carry no tenant and every tenant-scoped request would 403 — even
// though accept-time access worked, because Accept() set the tenant directly.
func (s *Service) resolveActiveTenant(ctx context.Context, userID uuid.UUID, memberships []Membership) uuid.UUID {
	if len(memberships) > 0 {
		return memberships[0].TenantID
	}
	if tid, ok := s.repo.FirstActiveShareTenant(ctx, userID); ok {
		return tid
	}
	return uuid.Nil
}

func (s *Service) recordLogin(ctx context.Context, memberships []Membership, userID uuid.UUID, action string) {
	if len(memberships) == 0 {
		return
	}
	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   memberships[0].TenantID,
		ActorType:  audit.ActorUser,
		ActorID:    userID.String(),
		Action:     action,
		TargetType: "user",
		TargetID:   userID.String(),
	})
}

// RegisterInput is the registration request body.
type RegisterInput struct {
	Email      string `validate:"required,email"`
	Password   string `validate:"required,min=12,max=200"`
	Name       string `validate:"max=200"`
	TenantName string `validate:"max=200"`
	TenantSlug string `validate:"omitempty,slug,max=64"`
}

// Bootstrap creates the very first user together with their tenant and an owner
// membership. It is only valid when there are zero users; otherwise it returns
// a conflict directing the caller to the invitation flow. The tenant is created
// via the supplied createTenant callback (the tenant domain owns that table).
func (s *Service) Bootstrap(
	ctx context.Context,
	in RegisterInput,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
) (LoginResult, error) {
	in.Email = normalizeEmail(in.Email)
	if err := s.validator.Struct(in); err != nil {
		return LoginResult{}, err
	}

	count, err := s.repo.CountUsers(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	if count > 0 {
		return LoginResult{}, domain.Forbidden("registration_closed", "open registration is closed; ask a tenant owner or admin for an invitation")
	}

	hash, err := HashPassword(in.Password)
	if err != nil {
		return LoginResult{}, domain.Internal("password_hash_failed", "failed to hash password").WithCause(err)
	}

	tenantName := strings.TrimSpace(in.TenantName)
	if tenantName == "" {
		tenantName = "Default"
	}
	tenantSlug := in.TenantSlug
	if tenantSlug == "" {
		tenantSlug = "default"
	}
	tenantID, err := createTenant(ctx, tenantName, tenantSlug)
	if err != nil {
		return LoginResult{}, err
	}

	u, err := s.repo.CreateUser(ctx, in.Email, hash, in.Name, "", "")
	if err != nil {
		return LoginResult{}, err
	}
	m, err := s.repo.CreateMembership(ctx, u.ID, tenantID, authz.RoleOwner)
	if err != nil {
		return LoginResult{}, err
	}

	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    u.ID.String(),
		Action:     audit.ActionRegister,
		TargetType: "user",
		TargetID:   u.ID.String(),
		Metadata:   map[string]any{"bootstrap": true},
	})

	return LoginResult{User: u, Memberships: []Membership{m}, ActiveTenant: tenantID}, nil
}

// InviteInput is an admin/owner request to add a user to a tenant.
type InviteInput struct {
	Email    string     `validate:"required,email"`
	Password string     `validate:"required,min=12,max=200"`
	Name     string     `validate:"max=200"`
	Role     authz.Role `validate:"required"`
}

// Invite creates (or reuses) a user and grants them a membership in the given
// tenant with the requested role. The caller (handler) must have already
// authorized the actor as admin+ in that tenant. actorRole is the actor's own
// role in the tenant and is used to enforce a privilege ceiling: an actor can
// never grant a role more privileged than its own (so only an owner may grant
// owner). actorID is recorded for audit.
func (s *Service) Invite(ctx context.Context, tenantID, actorID uuid.UUID, actorRole authz.Role, in InviteInput) (User, Membership, error) {
	in.Email = normalizeEmail(in.Email)
	if !in.Role.Valid() {
		return User{}, Membership{}, domain.Validation("role_invalid", "invalid role")
	}
	if err := s.validator.Struct(in); err != nil {
		return User{}, Membership{}, err
	}
	// Privilege ceiling: the granted role must not exceed the actor's own role.
	// Without this, an admin could grant owner (privilege escalation).
	if !actorRole.AtLeast(in.Role) {
		return User{}, Membership{}, domain.Forbidden("role_grant_exceeds_actor", "you cannot grant a role higher than your own")
	}

	u, err := s.repo.GetUserByEmail(ctx, in.Email)
	if err != nil {
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindNotFound {
			return User{}, Membership{}, err
		}
		hash, herr := HashPassword(in.Password)
		if herr != nil {
			return User{}, Membership{}, domain.Internal("password_hash_failed", "failed to hash password").WithCause(herr)
		}
		u, err = s.repo.CreateUser(ctx, in.Email, hash, in.Name, "", "")
		if err != nil {
			return User{}, Membership{}, err
		}
	}

	m, err := s.repo.CreateMembership(ctx, u.ID, tenantID, in.Role)
	if err != nil {
		return User{}, Membership{}, err
	}

	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    actorID.String(),
		Action:     audit.ActionMemberAdd,
		TargetType: "user",
		TargetID:   u.ID.String(),
		Metadata:   map[string]any{"role": string(in.Role)},
	})

	return u, m, nil
}

// Me returns a user and their memberships for /auth/me.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (User, []Membership, error) {
	u, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		return User{}, nil, err
	}
	memberships, err := s.repo.ListMembershipsForUser(ctx, userID)
	if err != nil {
		return User{}, nil, err
	}
	return u, memberships, nil
}

// RoleInTenant returns the user's role in a tenant, or false if not a member.
// It reads the caller's own membership rows via the self-read policy.
func (s *Service) RoleInTenant(ctx context.Context, userID, tenantID uuid.UUID) (authz.Role, bool) {
	memberships, err := s.repo.ListMembershipsForUser(ctx, userID)
	if err != nil {
		return "", false
	}
	for _, m := range memberships {
		if m.TenantID == tenantID {
			return m.Role, true
		}
	}
	return "", false
}

// UpsertOIDCUser finds-or-creates a user for an OIDC identity and ensures they
// have at least one tenant (bootstrapping a personal tenant on first OIDC login
// when no users exist yet, mirroring password bootstrap). Returns the resolved
// login result.
func (s *Service) UpsertOIDCUser(
	ctx context.Context,
	issuer, subject, email string,
	emailVerified bool,
	name string,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
) (LoginResult, error) {
	email = normalizeEmail(email)

	u, err := s.repo.GetUserByOIDC(ctx, issuer, subject)
	if err != nil {
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindNotFound {
			return LoginResult{}, err
		}
		// No user by OIDC identity. We may link this identity to a pre-existing
		// account that shares the same email ONLY when the IdP asserts the email
		// is verified. Linking on an unverified email would let an attacker who
		// controls an external IdP claim an arbitrary address and take over the
		// matching password account, so on an unverified email we fall through to
		// creating/keeping a distinct OIDC account instead.
		if email != "" && emailVerified {
			if existing, eerr := s.repo.GetUserByEmail(ctx, email); eerr == nil {
				u, err = s.repo.LinkOIDC(ctx, existing.ID, issuer, subject)
				if err != nil {
					return LoginResult{}, err
				}
			}
		}
		if u.ID == uuid.Nil {
			// Brand new user. Bootstrap a tenant only if this is the first user.
			u, err = s.createOIDCUser(ctx, issuer, subject, email, name)
			if err != nil {
				return LoginResult{}, err
			}
		}
	}

	memberships, _ := s.repo.ListMembershipsForUser(ctx, u.ID)
	if len(memberships) == 0 {
		// First OIDC user with no membership: bootstrap a tenant + owner.
		count, cerr := s.repo.CountUsers(ctx)
		if cerr == nil && count <= 1 {
			tenantID, terr := createTenant(ctx, "Default", "default")
			if terr == nil {
				if m, merr := s.repo.CreateMembership(ctx, u.ID, tenantID, authz.RoleOwner); merr == nil {
					memberships = []Membership{m}
				}
			}
		}
	}

	_ = s.repo.TouchLogin(ctx, u.ID)
	if len(memberships) > 0 {
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   memberships[0].TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    u.ID.String(),
			Action:     audit.ActionOIDCLogin,
			TargetType: "user",
			TargetID:   u.ID.String(),
		})
	}

	res := LoginResult{User: u, Memberships: memberships}
	res.ActiveTenant = s.resolveActiveTenant(ctx, u.ID, memberships)
	return res, nil
}

func (s *Service) createOIDCUser(ctx context.Context, issuer, subject, email, name string) (User, error) {
	if email == "" {
		// Synthesize a stable placeholder email from the subject so the unique
		// email constraint is satisfied for providers that omit email.
		email = normalizeEmail(subject + "@oidc.local")
	}
	return s.repo.CreateUser(ctx, email, "", name, issuer, subject)
}

// CountOwners returns how many owner-role memberships exist for the tenant.
// Used for last-owner protection in the members handler.
func (s *Service) CountOwners(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return s.repo.CountOwners(ctx, tenantID)
}

// RecordAudit delegates to the underlying audit Recorder. Exposed so handlers
// can record events without importing the audit package's internal Recorder.
func (s *Service) RecordAudit(ctx context.Context, e audit.Event) {
	_, _ = s.audit.Record(ctx, e)
}

// CountUsers exposes the user count (used to gate registration in handlers).
func (s *Service) CountUsers(ctx context.Context) (int64, error) {
	return s.repo.CountUsers(ctx)
}

// UpdateProfile sets the user's display name. The name is trimmed and capped at
// 120 characters. Email is intentionally not editable here (it is the login
// identity). Returns the updated user + their current memberships.
func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, name string) (User, []Membership, error) {
	name = strings.TrimSpace(name)
	if len(name) > 120 {
		return User{}, nil, domain.Validation("name_too_long", "name must be 120 characters or fewer")
	}
	u, err := s.repo.UpdateName(ctx, userID, name)
	if err != nil {
		return User{}, nil, err
	}
	memberships, err := s.repo.ListMembershipsForUser(ctx, userID)
	if err != nil {
		return User{}, nil, err
	}
	return u, memberships, nil
}

// ChangePassword verifies current against the stored hash, then replaces it
// with a new argon2id hash of newPwd. OIDC-only accounts (empty password_hash)
// are rejected with a clear 400 so the caller knows to redirect to SSO settings.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, currentPwd, newPwd string) error {
	u, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.PasswordHash == "" {
		return domain.Validation("sso_account_no_password", "password change is not available for SSO sign-in")
	}
	match, verr := VerifyPassword(currentPwd, u.PasswordHash)
	if verr != nil {
		return domain.Internal("password_verify_failed", "failed to verify password").WithCause(verr)
	}
	if !match {
		return domain.Unauthorized("invalid_current_password", "current password is incorrect")
	}
	if len(newPwd) < 8 {
		return domain.Validation("new_password_too_short", "new password must be at least 8 characters")
	}
	if len(newPwd) > 200 {
		return domain.Validation("new_password_too_long", "new password must be 200 characters or fewer")
	}
	hash, err := HashPassword(newPwd)
	if err != nil {
		return domain.Internal("password_hash_failed", "failed to hash new password").WithCause(err)
	}
	return s.repo.UpdatePasswordHash(ctx, userID, hash)
}

// ActorInfo holds the resolved identity fields for a triggered_by actor.
type ActorInfo struct {
	Email string
	Name  string
}

// ResolveActors returns a map of user UUID → ActorInfo for the provided IDs.
// Unresolvable IDs (unparseable, unknown) are silently omitted from the result.
// This is a tenant-agnostic lookup since users is not RLS-scoped.
func (s *Service) ResolveActors(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ActorInfo, error) {
	briefs, err := s.repo.GetUsersByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]ActorInfo, len(briefs))
	for _, b := range briefs {
		out[b.ID] = ActorInfo{Email: b.Email, Name: b.Name}
	}
	return out, nil
}

func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}
