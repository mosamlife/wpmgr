package auth

import (
	"context"
	"net/netip"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// PaidTierValidator answers whether a caller-supplied string names one of the
// hosted PAID tiers (never the free tier) recognized by internal/billing's
// plan ladder (M16 "sign up into a plan" Phase 0). This narrow interface
// keeps internal/auth free of a direct internal/billing import — billing
// already imports auth, so the reverse import would cycle — and keeps
// internal/billing the SOLE owner of paid-tier vocabulary (see
// billing/grep_guard_test.go): auth only ever asks the question, it never
// spells out a tier name itself. Implemented by *internal/billing.Service
// (wired in cmd/wpmgr via Service.SetPlanValidator), mirroring
// ManagedStorageResolver above. A nil validator (never wired, e.g. in a test
// that doesn't need this) or a validator that answers false for everything
// (self-host / hosted disabled) both correctly resolve to "no intent" — there
// is no checkout to route a plan to.
type PaidTierValidator interface {
	ValidPaidTier(plan string) bool
}

// Service holds authentication business logic: password login, first-run
// bootstrap, invited registration, and OIDC user upsert. It records auth events
// to the audit log.
type Service struct {
	repo      *Repo
	audit     *audit.Recorder
	validator *domain.Validator
	// Wired post-River via SetMailer (ADR-045 Phase 2). nil-safe.
	email   EmailEnqueuer
	baseURL string
	limiter RateLimiter
	// twofa holds the Phase 2 two-factor service logic. Injected via
	// SetTwoFactorDeps after startup. nil when 2FA is not configured.
	twofa *TwoFactorService
	// planValidator resolves a "sign up into a plan" hint (M16 Phase 0).
	// Injected via SetPlanValidator after startup; nil treats every plan hint
	// as no intent.
	planValidator PaidTierValidator
	// previousOIDCIssuer is the issuer this install used BEFORE the configured
	// one, declared by the operator. Empty on every install that never moved.
	// See SetPreviousOIDCIssuer.
	previousOIDCIssuer string
	// bootstrapClaim is the provisioning claim first-run ownership requires.
	// Injected via SetBootstrapClaimSecret. Empty means no caller can claim
	// this install — see bootstrapClaimAccepted.
	bootstrapClaim string
}

// SetPreviousOIDCIssuer declares the generic-OIDC issuer this install used
// before the current one, so identities stored under it can be migrated forward
// on their owners' next sign-in.
//
// IT IS AN AUTHORISATION, NOT A HINT, AND IT NEVER VERIFIES A TOKEN. An
// identity is (provider, subject, issuer) and subject is unique only within its
// issuer, so nothing may cross an issuer boundary on its own. Only the operator
// knows that the people arriving from the new issuer are the same people the old
// one vouched for, so only the operator can say so, by setting
// WPMGR_OIDC_PREVIOUS_ISSUER. Each identity is then moved once, and the move is
// audited.
//
// Leaving it unset is always safe: an unset value means no identity can ever be
// matched across an issuer change, which is the pre-existing behaviour.
func (s *Service) SetPreviousOIDCIssuer(issuer string) {
	s.previousOIDCIssuer = strings.TrimSpace(issuer)
}

// NewService builds an auth Service.
func NewService(repo *Repo, rec *audit.Recorder, v *domain.Validator) *Service {
	return &Service{repo: repo, audit: rec, validator: v}
}

// SetPlanValidator wires the M16 Phase 0 paid-tier validator. Call this after
// NewService, before serving. Pass the hosted *billing.Service at startup;
// leaving it unset (self-host, or a test that does not exercise plan intent)
// makes every registration's plan hint resolve to "no intent" — always safe.
func (s *Service) SetPlanValidator(v PaidTierValidator) {
	s.planValidator = v
}

// validatePlanIntent normalizes a caller-supplied "sign up into a plan" hint
// and validates it against the wired PaidTierValidator. Returns "" — meaning
// "no intent, nothing to persist" — for an empty string, an unrecognized or
// free-tier value, or when no validator is wired. Case/whitespace-insensitive.
func (s *Service) validatePlanIntent(raw string) string {
	norm := strings.ToLower(strings.TrimSpace(raw))
	if norm == "" || s.planValidator == nil || !s.planValidator.ValidPaidTier(norm) {
		return ""
	}
	return norm
}

// LoginResult is the outcome of a successful login.
type LoginResult struct {
	User         User
	Memberships  []Membership
	ActiveTenant uuid.UUID
	// DesiredPlan is the M16 Phase 0 "sign up into a plan" intent surfaced on
	// the two paths that can carry one: Bootstrap (immediate session, echoes
	// the just-validated request field) and VerifyEmail (read off the
	// just-consumed verification token). Every other path (Login,
	// UpsertOIDCUser, Me/UpdateProfile) leaves this at its zero value "".
	DesiredPlan string
	// PendingSocialLink is an external identity that the sign-in policy has
	// APPROVED for linking to User but that has deliberately NOT been written
	// yet. Only the social paths ever set it, and only on the branch that
	// attaches a new provider to an account that already exists.
	//
	// Linking changes how an account can be authenticated to, so it must not
	// outlive a login that never completed. The caller writes it with
	// CompleteSocialLink once, and only once, a session actually exists for
	// User: immediately for an account with no second factor, or after the
	// factor is proven for one that has.
	PendingSocialLink *Identity
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

	// Account-status gate (ADR-045 Phase 3). Only reached after the password is
	// verified correct, so it does not leak account existence to an attacker.
	switch u.Status {
	case "pending":
		return LoginResult{}, domain.Forbidden("email_not_verified", "please verify your email address before signing in")
	case "disabled":
		return LoginResult{}, domain.Forbidden("account_disabled", "this account is disabled")
	}

	_ = s.repo.TouchLogin(ctx, u.ID)
	s.recordLogin(ctx, memberships, u.ID, audit.ActionLoginSuccess)

	res := LoginResult{User: u, Memberships: memberships}
	res.ActiveTenant = s.resolveActiveTenant(ctx, u.ID, memberships)
	return res, nil
}

// resolveActiveTenant picks the session's active tenant after authentication.
// Org members use their first membership; a user with NO membership but an
// active site_share falls back to that share's tenant; a portal-only user
// (no membership, no share) falls back to their earliest client_member tenant.
func (s *Service) resolveActiveTenant(ctx context.Context, userID uuid.UUID, memberships []Membership) uuid.UUID {
	if len(memberships) > 0 {
		return memberships[0].TenantID
	}
	if tid, ok := s.repo.FirstActiveShareTenant(ctx, userID); ok {
		return tid
	}
	if tid, ok := s.repo.FirstClientMemberTenant(ctx, userID); ok {
		return tid
	}
	return uuid.Nil
}

func (s *Service) recordLogin(ctx context.Context, memberships []Membership, userID uuid.UUID, action string) {
	if len(memberships) > 0 {
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   memberships[0].TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     action,
			TargetType: "user",
			TargetID:   userID.String(),
		})
		return
	}
	// Portal-only users have no org membership. Best-effort record the login
	// event under their client member tenant so it reaches the audit log.
	if tid, ok := s.repo.FirstClientMemberTenant(ctx, userID); ok {
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tid,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     action,
			TargetType: "user",
			TargetID:   userID.String(),
		})
	}
}

// RegisterInput is the registration request body.
type RegisterInput struct {
	Email      string `validate:"required,email"`
	Password   string `validate:"required,min=12,max=200"`
	Name       string `validate:"max=200"`
	TenantName string `validate:"max=200"`
	TenantSlug string `validate:"omitempty,slug,max=64"`
	// Plan is an OPTIONAL "sign up into a plan" hint (M16 Phase 0). Any value
	// that is not a real hosted paid tier — including "free", empty, or
	// unrecognized — is silently treated as no intent; this field never
	// fails validation on its own. Resolved via Service.validatePlanIntent,
	// which is the ONLY place in this package allowed to ask
	// internal/billing whether a value is a real tier.
	Plan string `validate:"omitempty,max=32"`
}

// Bootstrap grants first-run ownership: the install's first organisation, its
// first user, and the owner membership binding them.
//
// IT REQUIRES THE PROVISIONING CLAIM. claim must be the value the operator
// configured as WPMGR_BOOTSTRAP_CLAIM_SECRET and handed to whoever is entitled
// to own this install. Ownership of a control plane is not a race to be first
// through the door: the installer decides who owns the install, and the claim
// is how that decision travels from the installer to this function.
//
// Every refusal — no claim configured, wrong claim, install already owned —
// returns errRegistrationClosed(), one indistinguishable answer. See its
// comment for why three answers would be worse than one.
//
// The count and the writes are one transaction under one advisory lock, in
// Repo.BootstrapInstall. Nothing is checked here that is acted on there.
func (s *Service) Bootstrap(ctx context.Context, in RegisterInput, claim string) (LoginResult, error) {
	// The claim is checked BEFORE the input is validated, so a caller without
	// it cannot use validation feedback to probe this path at all.
	if !s.bootstrapClaimAccepted(claim) {
		return LoginResult{}, errRegistrationClosed()
	}

	in.Email = normalizeEmail(in.Email)
	if err := s.validator.Struct(in); err != nil {
		return LoginResult{}, err
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

	u, m, tenantID, err := s.repo.BootstrapInstall(ctx, in.Email, hash, in.Name, tenantName, tenantSlug)
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

	// Bootstrap issues an immediate session in this same request, so there is
	// no verify-email gap to survive — the validated hint is simply echoed
	// back for the caller to act on right away (no server-side persistence
	// needed, unlike RegisterSelfServe).
	intent := s.validatePlanIntent(in.Plan)
	return LoginResult{User: u, Memberships: []Membership{m}, ActiveTenant: tenantID, DesiredPlan: intent}, nil
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

// UpsertOIDCUser resolves a generic-OIDC identity to a session.
//
// IT IS A THIN WRAPPER OVER SignInWithSocial, AND THAT IS THE WHOLE POINT. This
// function used to carry its own copy of the linking rules, and the copies
// drifted: when the account-takeover defence and the account-status gate were
// added for Google and GitHub, this path kept neither. The result was a route
// that still linked a verified external identity onto a local account nobody
// had ever proven they owned, and still let a DISABLED user sign in, while a
// comment one file over claimed both were fixed everywhere.
//
// Two implementations of one security policy is the bug. There is now one
// decideSocial, and every provider goes through it. The differences that are
// genuinely real for an operator-configured issuer, chiefly that some corporate
// IdPs return no email claim at all, live in operatorConfigured() inside that
// single policy rather than in a second copy of it here.
func (s *Service) UpsertOIDCUser(
	ctx context.Context,
	issuer, subject, email string,
	emailVerified bool,
	name string,
	createTenant func(ctx context.Context, name, slug string) (uuid.UUID, error),
) (LoginResult, error) {
	return s.SignInWithSocial(ctx, SocialIdentity{
		Provider:      "oidc",
		Subject:       subject,
		Issuer:        issuer,
		Email:         email,
		EmailVerified: emailVerified,
		Name:          name,
	}, createTenant)
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
	if len(newPwd) < minPasswordLen {
		return domain.Validation("new_password_too_short", "new password must be at least 12 characters")
	}
	if len(newPwd) > maxPasswordLen {
		return domain.Validation("new_password_too_long", "new password must be 200 characters or fewer")
	}
	hash, err := HashPassword(newPwd)
	if err != nil {
		return domain.Internal("password_hash_failed", "failed to hash new password").WithCause(err)
	}
	// UpdatePasswordHash stamps password_changed_at, which invalidates this
	// user's OTHER sessions on their next request (ADR-045 Phase 2). The current
	// session keeps working (its auth_at is refreshed below).
	if err := s.repo.UpdatePasswordHash(ctx, userID, hash); err != nil {
		return err
	}
	// S1: revoke all trusted devices so "remember this device" bypass tokens
	// cannot outlive a password change. Best-effort; a failure here does NOT
	// block the password change (the user is still protected by the changed
	// password and by session invalidation via password_changed_at).
	if s.twofa != nil {
		_ = s.twofa.repo.twoFA().RevokeAllTrustedDevices(ctx, userID)
	}
	// Best-effort: burn any outstanding reset tokens + notify the account owner.
	_ = s.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).InvalidateUserPasswordResetTokens(ctx, userID)
	})
	s.sendPasswordChanged(ctx, userID, netip.Addr{})
	return nil
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
