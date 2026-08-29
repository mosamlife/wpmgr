package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// TenantCreator creates a tenant and returns its ID. The auth domain depends on
// this narrow capability (provided by the tenant service) rather than importing
// the tenant package, keeping the dependency one-directional.
type TenantCreator func(ctx context.Context, name, slug string) (uuid.UUID, error)

// ManagedStorageResolver reports whether a tenant's CURRENT plan permits
// routing a new backup to CP-managed storage (M16 Phase B). Implemented by
// *internal/billing.Service (wired in cmd/wpmgr) via its ManagedStorageAllowed
// accessor; a nil value disables the check and every Me response reports true
// — mirrors billing.Service's own !enabled short-circuit (Unlimited().
// ManagedBackupStorage) so self-host and any test that does not wire this
// never see a spuriously-false signal. This local interface keeps the auth
// package free of a direct billing import (mirrors site.BillingGate /
// backup.BillingGate).
type ManagedStorageResolver interface {
	ManagedStorageAllowed(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

// Handler serves the authentication endpoints (/auth/*).
type Handler struct {
	svc       *Service
	sessions  *SessionManager
	oidc      *OIDCProvider
	social    *SocialProviders
	newTenant TenantCreator
	// secureCookies mirrors cfg.IsProduction(). Controls the Secure flag on
	// the trusted-device cookie. Set after construction via SetSecureCookies.
	secureCookies bool
	// hosted mirrors cfg.Hosted.Enabled (M16 Phase A). Surfaced on every Me
	// response so the frontend can gate billing/plan UI. Set after
	// construction via SetHosted.
	hosted bool
	// managedStorage resolves Me.managed_storage_allowed (M16 Phase B).
	// Optional: nil makes every Me response report true. Set after
	// construction via SetManagedStorageResolver.
	managedStorage ManagedStorageResolver
	// proxyHops is the deployment's appending-proxy count; see SetProxyHops.
	// proxyHopsSet distinguishes "explicitly 0" (nothing appends, use the peer
	// address) from "never configured", which must not silently become 0.
	proxyHops    int
	proxyHopsSet bool
	// logger receives the social sign-in failure lines (see social_log.go).
	// Optional: nil falls back to slog.Default(), which cmd/wpmgr configures.
	logger *slog.Logger
	// handshake seals the social sign-in handshake into a cookie instead of a
	// session record. NOT optional in effect: unset makes socialStart refuse
	// rather than issue a handshake nobody signed. See social_handshake.go.
	handshake *handshakeCodec
}

// NewHandler builds an auth Handler.
func NewHandler(svc *Service, sessions *SessionManager, oidc *OIDCProvider, newTenant TenantCreator) *Handler {
	return &Handler{svc: svc, sessions: sessions, oidc: oidc, newTenant: newTenant}
}

// SetSocialProviders wires the consumer identity providers after construction,
// mirroring how the other optional dependencies are injected. Leaving it unset
// keeps the routes present but reporting nothing enabled, so a self-hosted
// install that configures neither provider behaves exactly as before.
func (h *Handler) SetSocialProviders(sp *SocialProviders) { h.social = sp }

// SetLogger wires the logger the social sign-in path writes its failures to.
// Call this after NewHandler, before serving. Leaving it unset falls back to
// slog.Default(), so a test or an embedder that never calls it still gets the
// lines rather than silence.
func (h *Handler) SetLogger(l *slog.Logger) { h.logger = l }

// SetSecureCookies configures whether the Secure flag is set on the
// trusted-device cookie. Call this after NewHandler, before serving.
// Pass cfg.IsProduction() at startup.
func (h *Handler) SetSecureCookies(secure bool) {
	h.secureCookies = secure
}

// SetHosted configures the "hosted" flag returned on every Me response.
// Call this after NewHandler, before serving. Pass cfg.Hosted.Enabled at
// startup.
func (h *Handler) SetHosted(hosted bool) {
	h.hosted = hosted
}

// SetManagedStorageResolver wires the M16 Phase B managed_storage_allowed
// signal on every Me response. Call this after NewHandler, before serving.
// Optional: when never called, every Me response reports
// managed_storage_allowed=true (matches the resolver-nil / hosted-disabled
// default exactly).
func (h *Handler) SetManagedStorageResolver(r ManagedStorageResolver) {
	h.managedStorage = r
}

// managedStorageAllowed resolves Me.managed_storage_allowed for tenantID.
// Fails OPEN (true) on a nil resolver, a nil tenant (unauthenticated/no
// active org yet), or a resolution error — this is a coarse DISPLAY signal
// only (see ManagedStorageResolver's doc comment); the authoritative gate is
// internal/backup's server-side enforcement, which must never be bypassed by
// a Me-response hiccup, and conversely a hiccup here must never wrongly show
// an operator a blocked-storage banner they cannot act on correctly.
func (h *Handler) managedStorageAllowed(ctx context.Context, tenantID uuid.UUID) bool {
	if h.managedStorage == nil || tenantID == uuid.Nil {
		return true
	}
	allowed, err := h.managedStorage.ManagedStorageAllowed(ctx, tenantID)
	if err != nil {
		return true
	}
	return allowed
}

// Register mounts the auth routes on the root engine group.
func (h *Handler) Register(r gin.IRouter) {
	g := r.Group("/auth")
	g.POST("/register", h.register)
	g.POST("/login", h.login)
	g.POST("/logout", h.logout)
	g.GET("/me", h.me)
	g.PATCH("/me", h.updateProfile)
	g.POST("/me/password", h.changePassword)
	// ADR-045 Phase 2 — public, unauthenticated password reset.
	g.POST("/password/forgot", h.forgotPassword)
	g.POST("/password/reset", h.resetPassword)
	// ADR-045 Phase 3 — public email verification for self-serve signup.
	g.POST("/verify-email", h.verifyEmail)
	g.POST("/verification/resend", h.resendVerification)
	g.GET("/oidc/login", h.oidcLogin)
	g.GET("/oidc/callback", h.oidcCallback)
	// Social sign-in. The provider list is public so the sign-in page can
	// render exactly the buttons that will work on this install.
	g.GET("/social/providers", h.socialProviders)
	g.GET("/social/:provider/start", h.socialStart)
	g.GET("/social/:provider/callback", h.socialCallback)
	// ADR-056 Phase 3 — dashboard 2FA challenge-completion + management.
	h.RegisterTwoFactor(g)
	// Connected accounts: list/remove linked providers, and add a password to
	// an account that only has a provider.
	h.RegisterIdentities(g)
}

type loginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) login(c *gin.Context) {
	var body loginBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	res, err := h.svc.Login(c.Request.Context(), body.Email, body.Password)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// ADR-056 Phase 3: two-factor enforcement.
	// INVARIANT: a 2FA-enabled user must NEVER receive a full session without
	// either completing a factor challenge or presenting a valid trusted-device
	// cookie. The two paths are:
	//
	//   1. two_factor_enabled == false: issue session directly (unchanged path).
	//   2. two_factor_enabled == true:
	//      a. Valid trusted-device cookie present AND bound to this user: bypass
	//         challenge, issue session.
	//      b. No valid trusted device (or device bound to a different user):
	//         return 202 + challenge nonce; no session.
	if res.User.TwoFactorEnabled {
		// Check for a valid trusted-device cookie (bypass path).
		//
		// INVARIANT (B1): the device record's user_id MUST match the user who just
		// supplied the correct password (res.User.ID). A cookie issued for user A
		// must never bypass the 2FA challenge for user B, even if user B's password
		// is also correct. Without this binding check, an attacker who knows user
		// B's password and possesses user A's device cookie could sign in as B
		// without completing a factor challenge.
		//
		// On a mismatch we silently fall through to the normal 202-challenge path
		// (we do NOT error) so we neither reveal that the cookie belonged to a
		// different user nor break the login UX for the correct user.
		//
		// TouchTrustedDevice (last_used_at bump) is intentionally called AFTER the
		// ownership check so it only stamps the correct user's device record.
		if rawToken := readDeviceCookie(c); rawToken != "" {
			device, verr := h.svc.VerifyTrustedDeviceNoTouch(c.Request.Context(), rawToken)
			if verr == nil && device.ID != uuid.Nil && device.UserID == res.User.ID {
				// Ownership confirmed: bump last_used_at now, then issue session.
				_ = h.svc.TouchTrustedDevice(c.Request.Context(), device.ID)
				if err := h.sessions.Login(c.Request.Context(), res.User.ID, res.ActiveTenant); err != nil {
					httpx.Error(c, domain.Internal("session_failed", "failed to establish session").WithCause(err))
					return
				}
				out := toMe(res.User, res.Memberships, res.ActiveTenant, h.hosted, h.managedStorageAllowed(c.Request.Context(), res.ActiveTenant), res.DesiredPlan)
				c.JSON(http.StatusOK, &out)
				return
			}
			// Trusted-device check failed or owner mismatch: fall through to
			// challenge issuance. The cookie may be expired, revoked, tampered,
			// or bound to a different user; proceed normally without leaking why.
		}
	}

	// issueSessionOrChallenge enforces the B2 invariant for the remaining paths:
	// for a 2FA-enabled user it creates a challenge and returns 202; for a non-2FA
	// user it issues the session directly. The trusted-device bypass above already
	// handled the 2FA+valid-device case and returned early.
	if !h.issueSessionOrChallenge(c, res, "") {
		return
	}
	out := toMe(res.User, res.Memberships, res.ActiveTenant, h.hosted, h.managedStorageAllowed(c.Request.Context(), res.ActiveTenant), res.DesiredPlan)
	c.JSON(http.StatusOK, &out)
}

type registerBody struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	Name       string `json:"name"`
	TenantName string `json:"tenant_name"`
	TenantSlug string `json:"tenant_slug"`
	// Plan is an OPTIONAL M16 Phase 0 "sign up into a plan" hint. See
	// RegisterInput.Plan's doc comment: any non-paid-tier value (including
	// "free", empty, or unrecognized) is silently treated as no intent.
	Plan string `json:"plan"`
}

// bootstrapClaim reads the provisioning claim a first-run caller presented.
//
// A HEADER, NOT A BODY FIELD. The claim is a credential rather than user data,
// and the register body is the one payload on this route that gets echoed into
// validation errors and request logs. Keeping it in a header also keeps it out
// of the OpenAPI request schema, so it is never something a generated client
// offers to fill in: an installer sets it deliberately, once.
func bootstrapClaim(c *gin.Context) string {
	return strings.TrimSpace(c.GetHeader(BootstrapClaimHeader))
}

// bootstrapLog returns the logger for first-run ownership, tagged so an
// operator can select these lines out of a busy request log. Mirrors socialLog.
func (h *Handler) bootstrapLog() *slog.Logger {
	l := h.logger
	if l == nil {
		l = slog.Default()
	}
	return l.With(slog.String("component", "auth.bootstrap"))
}

func (h *Handler) register(c *gin.Context) {
	var body registerBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	in := RegisterInput{
		Email:      body.Email,
		Password:   body.Password,
		Name:       body.Name,
		TenantName: body.TenantName,
		TenantSlug: body.TenantSlug,
		Plan:       body.Plan,
	}

	// FIRST-RUN OWNERSHIP IS DECIDED BY THE CLAIM, NOT BY THE USER COUNT. A
	// caller presenting the provisioning claim is asking to own this install,
	// and Service.Bootstrap alone decides whether it may: it verifies the claim
	// in constant time and then takes the install lock, so the "is this install
	// unowned?" question is asked once, inside the transaction that acts on the
	// answer. This handler's job is to route the request, not to pre-judge it —
	// a count read here would be a second, unlockable opinion on the same
	// question, which is what a count read here used to be.
	//
	// Everything else is OPEN self-serve: a generic pending response, verify by
	// email before logging in (ADR-045 Phase 3).
	//
	// Bootstrap creates a brand-new user who cannot yet have 2FA enrolled, so
	// the 2FA gate here is defensive (makes the code correct uniformly) rather
	// than protecting against a real threat today.
	if claim := bootstrapClaim(c); claim != "" {
		res, err := h.svc.Bootstrap(c.Request.Context(), in, claim)
		if err != nil {
			if !h.svc.BootstrapClaimConfigured() {
				// Operator-facing only. The response stays the generic refusal;
				// the reason a correctly-provisioned installer is being turned
				// away belongs in the log, where the operator is the only
				// reader. The claim itself is never logged.
				h.bootstrapLog().Warn("first-run ownership refused: no provisioning claim is configured on this install",
					"remedy", "set "+BootstrapClaimEnvVar+" to a random secret and restart the control plane")
			}
			httpx.Error(c, err)
			return
		}
		if !h.issueSessionOrChallenge(c, res, "") {
			return
		}
		out := toMe(res.User, res.Memberships, res.ActiveTenant, h.hosted, h.managedStorageAllowed(c.Request.Context(), res.ActiveTenant), res.DesiredPlan)
		c.JSON(http.StatusCreated, &out)
		return
	}

	if err := h.svc.RegisterSelfServe(c.Request.Context(), in, h.newTenant); err != nil {
		// Only validation errors (weak password / bad email) surface; existence
		// is never leaked.
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "pending": true})
}

// issueSessionOrChallenge is the SINGLE session-issuing helper used by every
// human login path. It enforces the core 2FA invariant:
//
//	INVARIANT (B2): no human login path may issue a full session for a
//	2FA-enrolled user without a completed factor challenge.
//
// For non-2FA users it calls sessions.Login and writes the 200+Me response.
//
// For 2FA-enabled users it creates a challenge and returns 202+{challenge,factors}
// instead of calling sessions.Login. It returns issued=false in that case so
// the caller knows NOT to write any further response.
//
// For the OIDC callback path (a browser redirect, not JSON) the caller must
// pass oidcRedirectBase as a non-empty string; the handler then redirects to
// <oidcRedirectBase>/login?two_factor_challenge=<challengeID> instead of
// writing a JSON 202. For all other paths pass oidcRedirectBase="".
func (h *Handler) issueSessionOrChallenge(c *gin.Context, res LoginResult, oidcRedirectBase string) (issued bool) {
	return h.issueSessionOrChallengeThen(c, res, oidcRedirectBase, nil)
}

// issueSessionOrChallengeThen is issueSessionOrChallenge with one addition: a
// hook that runs the moment a challenge exists and BEFORE the response carrying
// it is written.
//
// Both halves of that timing are load-bearing. A caller that needs to remember
// something ABOUT this challenge (the provider paths park an approved identity
// link against it) can only bind to the challenge id once it has been minted,
// and can only put anything in the session before the response is written,
// because the session middleware saves on write and nothing after it lands.
// Passing a nil hook is exactly the old behaviour.
func (h *Handler) issueSessionOrChallengeThen(
	c *gin.Context,
	res LoginResult,
	oidcRedirectBase string,
	onChallengeIssued func(challengeID uuid.UUID),
) (issued bool) {
	if res.User.TwoFactorEnabled {
		ip := clientAddr(c)
		result, cherr := h.svc.RequestTwoFactorChallenge(c.Request.Context(), res.User.ID, &ip)
		if cherr != nil {
			httpx.Error(c, cherr)
			return false
		}
		if onChallengeIssued != nil {
			onChallengeIssued(result.ChallengeID)
		}
		if oidcRedirectBase != "" {
			// Browser redirect flow (OIDC callback): redirect to the SPA 2FA
			// challenge route carrying the challenge ID AND the available factors as
			// query params, matching the /2fa-challenge route's search schema so the
			// page renders the right methods without a separate lookup.
			// Contract documented in docs/2fa-api-contract.md §OIDC redirect.
			q := url.Values{}
			q.Set("challenge", result.ChallengeID.String())
			q.Set("totp", strconv.FormatBool(result.Factors.TOTP))
			q.Set("webauthn", strconv.FormatBool(result.Factors.WebAuthnCount > 0))
			q.Set("recovery_factor", "true")
			target := oidcRedirectBase + "/2fa-challenge?" + q.Encode()
			c.Redirect(http.StatusFound, target)
		} else {
			c.JSON(http.StatusAccepted, gin.H{
				"two_factor_required": true,
				"challenge":           result.ChallengeID,
				"factors": gin.H{
					"totp":     result.Factors.TOTP,
					"webauthn": result.Factors.WebAuthnCount > 0,
					"recovery": true,
				},
			})
		}
		return false
	}

	// No second factor: issue the session directly.
	if err := h.sessions.Login(c.Request.Context(), res.User.ID, res.ActiveTenant); err != nil {
		httpx.Error(c, domain.Internal("session_failed", "failed to establish session").WithCause(err))
		return false
	}
	return true
}

// verifyEmail handles POST /auth/verify-email. Consumes the token, activates the
// account, and establishes a session so the user lands logged in.
//
// A user normally cannot have 2FA already enrolled before email verification
// (enrollment requires a full session), but we run the gate defensively via
// issueSessionOrChallenge to make this path future-proof and to satisfy the
// B2 invariant uniformly across all session-issuing paths.
func (h *Handler) verifyEmail(c *gin.Context) {
	var body struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	res, err := h.svc.VerifyEmail(c.Request.Context(), body.Token)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if !h.issueSessionOrChallenge(c, res, "") {
		return
	}
	out := toMe(res.User, res.Memberships, res.ActiveTenant, h.hosted, h.managedStorageAllowed(c.Request.Context(), res.ActiveTenant), res.DesiredPlan)
	c.JSON(http.StatusOK, &out)
}

// resendVerification handles POST /auth/verification/resend. ALWAYS 200 (generic).
func (h *Handler) resendVerification(c *gin.Context) {
	var body struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	_ = h.svc.ResendVerification(c.Request.Context(), body.Email)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) logout(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	if p.TenantID != uuid.Nil {
		_, _ = h.svc.audit.Record(c.Request.Context(), audit.Event{
			TenantID:   p.TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    p.UserID.String(),
			Action:     audit.ActionLogout,
			TargetType: "user",
			TargetID:   p.UserID.String(),
		})
	}
	if err := h.sessions.Destroy(c.Request.Context()); err != nil {
		httpx.Error(c, domain.Internal("logout_failed", "failed to destroy session").WithCause(err))
		return
	}
	// ADR-056: do NOT clear the trusted-device cookie on logout. "Remember this
	// device for 30 days" is meant to survive sign-out: it lets the user skip the
	// SECOND factor (never the password) on a known device. Clearing it here made
	// every subsequent sign-in re-prompt for 2FA and mint a duplicate trusted
	// device. Trusted devices are invalidated instead on password change/reset
	// (the compromise lever) and on explicit revoke or 2FA-disable.
	c.Status(http.StatusNoContent)
}

func (h *Handler) me(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	u, memberships, err := h.svc.Me(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toMe(u, memberships, p.TenantID, h.hosted, h.managedStorageAllowed(c.Request.Context(), p.TenantID), "")
	// m66 — portal principal: enrich Me with scope, role, and portal branding.
	enrichMePortal(c.Request.Context(), &out, p, h.svc.repo)
	c.JSON(http.StatusOK, &out)
}

// updateProfileBody is the request body for PATCH /auth/me.
type updateProfileBody struct {
	Name string `json:"name"`
}

// updateProfile handles PATCH /auth/me — update the authenticated user's
// display name. Email is intentionally not editable here.
func (h *Handler) updateProfile(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body updateProfileBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	u, memberships, err := h.svc.UpdateProfile(c.Request.Context(), p.UserID, body.Name)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toMe(u, memberships, p.TenantID, h.hosted, h.managedStorageAllowed(c.Request.Context(), p.TenantID), "")
	// Enrich exactly as me() does. PATCH /auth/me returns the SAME Me resource
	// as GET /auth/me, and clients cache it under the same key — apps/web does
	// setQueryData(authKeys.me, <patch response>). Omitting scope/role here
	// does not return a smaller Me, it returns a WRONG one: a site-scoped
	// collaborator who saves their display name would have me.scope silently
	// become undefined, canWriteSiteContext would flip to false, and the
	// context editor would go read-only until a hard reload. An absent field
	// must never read as a denied permission.
	enrichMePortal(c.Request.Context(), &out, p, h.svc.repo)
	c.JSON(http.StatusOK, &out)
}

// changePasswordBody is the request body for POST /auth/me/password.
type changePasswordBody struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword handles POST /auth/me/password — verify current, set new.
func (h *Handler) changePassword(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body changePasswordBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := h.svc.ChangePassword(c.Request.Context(), p.UserID, body.CurrentPassword, body.NewPassword); err != nil {
		httpx.Error(c, err)
		return
	}
	// Keep THIS session alive (its auth_at now predates password_changed_at,
	// which would otherwise log it out); other sessions are invalidated.
	h.sessions.RefreshAuthAt(c.Request.Context())
	c.Status(http.StatusNoContent)
}

// forgotPasswordBody is the request body for POST /auth/password/forgot.
type forgotPasswordBody struct {
	Email string `json:"email"`
}

// forgotPassword handles POST /auth/password/forgot. ALWAYS returns 200 {ok:true}
// (enumeration-safe) whether or not the email maps to an account.
func (h *Handler) forgotPassword(c *gin.Context) {
	var body forgotPasswordBody
	if err := c.ShouldBindJSON(&body); err != nil {
		// Even a malformed body returns the generic OK shape (no oracle).
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	_ = h.svc.RequestPasswordReset(c.Request.Context(), body.Email, clientAddr(c))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// resetPasswordBody is the request body for POST /auth/password/reset.
type resetPasswordBody struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// resetPassword handles POST /auth/password/reset. Consumes the token + sets the
// new password; never establishes a session. Bad/expired/used tokens → 410.
func (h *Handler) resetPassword(c *gin.Context) {
	var body resetPasswordBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	// limiterAddr, not clientAddr: ResetPassword rate-limits on this value
	// ("pwreset-consume:"), and it is the only limit on reset-token guessing.
	if err := h.svc.ResetPassword(c.Request.Context(), body.Token, body.Password, h.limiterAddr(c)); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// clientAddr parses gin's resolved client IP into a netip.Addr (invalid when
// unparseable). Used where the requesting address is RECORDED — audit rows,
// notification bodies, trusted-device rows. See limiterAddr for the address a
// rate-limit decision may be keyed on; the two are deliberately different.
func clientAddr(c *gin.Context) netip.Addr {
	addr, err := netip.ParseAddr(c.ClientIP())
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

// defaultProxyHops is the fallback for a Handler that was never told its
// deployment shape. It matches config's default and the hosted topology.
const defaultProxyHops = 2

// SetProxyHops declares how many X-Forwarded-For entries the infrastructure in
// front of this process appends. See config.AuthConfig.ProxyHops for the
// meaning of each value; the effective value is logged at startup.
//
// A negative value is refused by config.Validate before this is reached, so it
// is coerced here only to keep a zero-value Handler in tests from selecting
// nonsense.
func (h *Handler) SetProxyHops(n int) {
	if n < 0 {
		n = defaultProxyHops
	}
	h.proxyHops = n
	h.proxyHopsSet = true
}

// effectiveProxyHops treats an unset field as the default, so a Handler built
// without SetProxyHops behaves as the hosted deployment rather than as though
// nothing were in front of it.
func (h *Handler) effectiveProxyHops() int {
	if h.proxyHops == 0 && !h.proxyHopsSet {
		return defaultProxyHops
	}
	return h.proxyHops
}

// limiterAddr returns the address that a rate-limit DECISION may be keyed on.
//
// It is separate from clientAddr on purpose. An address that is merely recorded
// can tolerate being caller-influenced; an address that decides whether to
// refuse a request cannot, because then the caller selects its own bucket and
// the limit stops binding.
//
// The number of entries the infrastructure appends is configuration, not a
// property this process can observe — see config.AuthConfig.ProxyHops. A count
// of 0 means nothing appends, so the forwarded header is ignored entirely and
// the peer address is used.
//
// When the chain is shorter than the configured count, this falls back to the
// peer address rather than reaching further left. That fallback is NOT a
// security guard and must not be described as one: it catches honest short
// chains only. A caller wanting control of its key supplies the configured
// number of entries itself, which skips the fallback entirely. So if this
// process is reachable without the proxies it is configured for, the fallback
// protects nobody — honest clients collapse onto the peer key while a caller
// keeps a key per request. Only a correct hop count, matching what actually sits
// in front, makes this sound.
func (h *Handler) limiterAddr(c *gin.Context) netip.Addr {
	hops := h.effectiveProxyHops()
	if hops == 0 {
		// Nothing in front appends, so every forwarded entry is caller-supplied
		// and none of it is evidence. The peer address is the client.
		if addr, ok := parseForwardedAddr(c.RemoteIP()); ok {
			return addr
		}
		return netip.Addr{}
	}
	// Values, not Get. Get returns only the FIRST header line, so a caller that
	// sends X-Forwarded-For twice would have the appended entries land on a line
	// this never reads, handing back a value it chose. Joining every line is the
	// chain as the invariant above describes it, and removes any dependence on
	// whether the proxy in front coalesces repeated lines.
	joined := strings.Join(c.Request.Header.Values("X-Forwarded-For"), ",")
	parts := strings.Split(joined, ",")
	if len(parts) >= hops {
		if addr, ok := parseForwardedAddr(parts[len(parts)-hops]); ok {
			return addr
		}
	}
	if addr, ok := parseForwardedAddr(c.RemoteIP()); ok {
		return addr
	}
	return netip.Addr{}
}

// parseForwardedAddr parses one forwarded-chain entry. Entries are usually bare
// addresses, but the bracketed and port-suffixed forms are legal in the wild and
// are what IPv6 tends to arrive as. Treating those as unparseable would drop
// every v6 client onto the shared fallback key, so they are handled explicitly.
func parseForwardedAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr.Unmap(), true
	}
	// "[2001:db8::1]:443" and "198.51.100.7:443".
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	// "[2001:db8::1]" with no port.
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		if addr, err := netip.ParseAddr(s[1 : len(s)-1]); err == nil {
			return addr.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

func (h *Handler) oidcLogin(c *gin.Context) {
	if !h.oidc.Enabled() {
		httpx.Error(c, domain.Unavailable("oidc_disabled", "OIDC is not configured"))
		return
	}
	url, state, nonce, verifier, err := h.oidc.AuthCodeURL(c.Request.Context())
	if err != nil {
		// Unavailable, not Internal: since discovery moved off the boot path this
		// is where an unreachable identity provider first shows up, and that is
		// somebody else's server being down rather than a fault in this one.
		// Getting that wrong turns "your IdP is not answering" into a 500 that
		// reads as a control-plane bug and gets escalated as one.
		slog.ErrorContext(c.Request.Context(), "oidc authorization URL failed",
			slog.String("issuer", h.oidc.cfg.Issuer), slog.Any("error", err))
		httpx.Error(c, domain.Unavailable("oidc_url_failed", "the identity provider could not be reached").WithCause(err))
		return
	}
	h.sessions.putOAuth(c.Request.Context(), state, nonce, verifier)
	c.Redirect(http.StatusFound, url)
}

func (h *Handler) oidcCallback(c *gin.Context) {
	if !h.oidc.Enabled() {
		httpx.Error(c, domain.Unavailable("oidc_disabled", "OIDC is not configured"))
		return
	}
	state, nonce, verifier := h.sessions.takeOAuth(c.Request.Context())
	if state == "" || c.Query("state") != state {
		httpx.Error(c, domain.Unauthorized("oidc_state_mismatch", "OIDC state mismatch or expired"))
		return
	}
	code := c.Query("code")
	if code == "" {
		httpx.Error(c, domain.Unauthorized("oidc_no_code", "OIDC callback missing code"))
		return
	}
	claims, err := h.oidc.Exchange(c.Request.Context(), code, verifier, nonce)
	if err != nil {
		httpx.Error(c, domain.Unauthorized("oidc_exchange_failed", "OIDC verification failed"))
		return
	}
	res, err := h.svc.UpsertOIDCUser(c.Request.Context(), claims.Issuer, claims.Subject, claims.Email, claims.EmailVerified, claims.Name, h.newTenant)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// B2 INVARIANT: if the OIDC user has 2FA enrolled, we must NOT issue a full
	// session here. The OIDC callback is a browser redirect, so we redirect the
	// browser to the SPA challenge route instead of writing a JSON 202. The SPA
	// reads the two_factor_challenge query parameter and enters the challenge UI.
	// See docs/2fa-api-contract.md §"OIDC callback redirect".
	//
	// h.svc.baseURL is the public base URL (WPMGR_PUBLIC_BASE_URL). When 2FA is
	// not enrolled issueSessionOrChallenge issues the session and returns true;
	// we then redirect to the SPA home (the callback was always a browser redirect).
	// Routed through the shared provider helper so this callback also defers an
	// approved identity link until the second factor is proven, and writes it
	// once it is. The generic OIDC path goes through the same policy as the
	// consumer providers, so it must go through the same completion too.
	if !h.issueProviderSessionOrChallenge(c, res) {
		// 2FA challenge redirect was already written by issueSessionOrChallenge.
		return
	}
	// Non-2FA path: session was issued; redirect the browser to the SPA home.
	out := toMe(res.User, res.Memberships, res.ActiveTenant, h.hosted, h.managedStorageAllowed(c.Request.Context(), res.ActiveTenant), res.DesiredPlan)
	c.JSON(http.StatusOK, &out)
}

// toMe builds the wire Me response. desiredPlan is the M16 Phase 0 "sign up
// into a plan" intent (LoginResult.DesiredPlan) — non-empty ONLY right after
// Bootstrap or VerifyEmail; every other caller passes "" (no such field
// exists on that path's result).
func toMe(u User, memberships []Membership, active uuid.UUID, hosted, managedStorageAllowed bool, desiredPlan string) gen.Me {
	me := gen.Me{
		User:                  toAPIUser(u),
		Memberships:           toAPIMemberships(memberships),
		Hosted:                gen.NewOptBool(hosted),
		ManagedStorageAllowed: gen.NewOptBool(managedStorageAllowed),
	}
	if active != uuid.Nil {
		me.ActiveTenantID = gen.NewOptUUID(active)
	}
	if desiredPlan != "" {
		me.DesiredPlan = gen.NewOptMeDesiredPlan(gen.MeDesiredPlan(desiredPlan))
	}
	return me
}

// enrichMePortal sets the Me.Scope, Me.Role, and Me.Portal fields from the
// resolved principal. Portal branding (client name, logo, color, agency name)
// is fetched via a best-effort DB query; failures leave the fields empty.
//
// Called from every handler that returns a Me built from a principal the auth
// middleware already resolved — me() and updateProfile(). It is deliberately
// NOT called from login/register/verifyEmail/oidcCallback or the 2FA
// completions: those mint the session themselves via issueSessionOrChallenge,
// so the middleware ran before any cookie existed and there is no resolved
// principal to read scope/role from. Those clients refetch GET /auth/me once
// the cookie is set (see apps/web/src/routes/login.tsx).
func enrichMePortal(ctx context.Context, me *gen.Me, p domain.Principal, repo *Repo) {
	if p.Scope != "" {
		me.Scope = gen.NewOptMeScope(gen.MeScope(p.Scope))
	}
	if p.Role != "" {
		me.Role = gen.NewOptPrincipalRole(gen.PrincipalRole(p.Role))
	}

	if authz.Role(p.Role) != authz.RoleClient || len(p.ClientIDs) == 0 || p.TenantID == uuid.Nil {
		return
	}

	// Resolve client branding: earliest-created client by the order in ClientIDs
	// (which is ordered by client_members.created_at ASC in resolveClientAccess).
	// Fetch all client brands and pick the first one that appears in ClientIDs
	// (preserves created_at ASC order since GetClientBrandsByIDs orders the same way).
	var clientName, logoURL, color, agencyName string

	_ = repo.pool.InUserTx(ctx, p.UserID, func(tx pgx.Tx) error {
		// GetClientBrandsByIDs runs under InUserTx; the client_members_self_read
		// + sites_client_read policies admit the rows. We use a direct query here
		// because sqlc.Queries is tied to the tx.
		rows, qerr := tx.Query(ctx,
			`SELECT c.id, c.name, c.color, c.logo_url, t.name AS tenant_name
			 FROM clients c
			 JOIN tenants t ON t.id = c.tenant_id
			 WHERE c.id = ANY($1::uuid[]) AND c.tenant_id = $2 AND c.archived_at IS NULL
			 ORDER BY c.created_at ASC, c.id ASC
			 LIMIT 1`,
			p.ClientIDs, p.TenantID,
		)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		if rows.Next() {
			var clientID uuid.UUID
			var color_, logoURL_ *string
			if err := rows.Scan(&clientID, &clientName, &color_, &logoURL_, &agencyName); err == nil {
				if color_ != nil {
					color = *color_
				}
				if logoURL_ != nil {
					logoURL = *logoURL_
				}
			}
		}
		return rows.Err()
	})

	portal := gen.MePortal{
		ClientID:   p.ClientIDs[0], // earliest; same as query result
		ClientName: clientName,
		AgencyName: agencyName,
	}
	if logoURL != "" {
		if u, err := parseLogoURI(logoURL); err == nil {
			portal.LogoURL = gen.NewOptURI(u)
		}
	}
	if color != "" {
		portal.Color = gen.NewOptString(color)
	}
	me.Portal = gen.NewOptMePortal(portal)
}

func toAPIUser(u User) gen.User {
	out := gen.User{
		ID:               u.ID,
		Email:            u.Email,
		Name:             u.Name,
		CreatedAt:        u.CreatedAt,
		UpdatedAt:        u.UpdatedAt,
		IsSuperadmin:     gen.NewOptBool(u.IsSuperadmin),
		TwoFactorEnabled: gen.NewOptBool(u.TwoFactorEnabled),
	}
	if u.LastLoginAt != nil {
		out.LastLoginAt = gen.NewOptDateTime(*u.LastLoginAt)
	}
	return out
}

func toAPIMembership(m Membership) gen.Membership {
	return gen.Membership{UserID: m.UserID, TenantID: m.TenantID, Role: gen.Role(m.Role)}
}

// parseLogoURI parses a logo URL string into a url.URL. Used by enrichMePortal.
func parseLogoURI(raw string) (url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return url.URL{}, err
	}
	return *u, nil
}

func toAPIMemberships(ms []Membership) []gen.Membership {
	out := make([]gen.Membership, 0, len(ms))
	for _, m := range ms {
		out = append(out, toAPIMembership(m))
	}
	return out
}

// roleOrDefault parses a role string, defaulting to viewer when empty.
func roleOrDefault(s string) authz.Role {
	if s == "" {
		return authz.RoleViewer
	}
	return authz.Role(s)
}
