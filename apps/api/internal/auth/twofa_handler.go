package auth

// twofa_handler.go -- Phase 3 HTTP handlers for dashboard two-factor
// authentication (ADR-056). All routes are hand-written Gin matching the
// style of handler.go. No ogen router is used.
//
// Route groups:
//
//   Unauthenticated (challenge-completion, called by the login flow):
//     POST /auth/2fa/totp
//     POST /auth/2fa/recovery
//     POST /auth/2fa/webauthn/begin
//     POST /auth/2fa/webauthn/finish
//
//   Authenticated (management, require a logged-in session):
//     GET    /auth/2fa/status
//     POST   /auth/2fa/totp/begin
//     POST   /auth/2fa/totp/confirm
//     POST   /auth/2fa/totp/disable
//     POST   /auth/2fa/webauthn/begin-registration
//     POST   /auth/2fa/webauthn/finish-registration
//     GET    /auth/2fa/webauthn/credentials
//     DELETE /auth/2fa/webauthn/credentials/:id
//     POST   /auth/2fa/recovery-codes/regenerate
//     GET    /auth/2fa/trusted-devices
//     DELETE /auth/2fa/trusted-devices/:id
//     POST   /auth/2fa/trusted-devices/revoke-all
//
// INVARIANT (ADR-056 security invariant): a 2FA-enabled user can NEVER receive
// a full session without completing a factor challenge or presenting a valid
// trusted-device cookie. This is enforced in the login handler (handler.go):
// when user.TwoFactorEnabled is true the handler returns 202 + challenge
// instead of calling sessions.Login.

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Trusted-device cookie name and policy. The raw 256-bit token is stored as
// the cookie value; its SHA-256 hash is stored in trusted_devices.token_hash.
// HttpOnly + Secure (in prod) + SameSite=Lax follow the same policy as the
// session cookie, but Path=/auth so browsers only send it on /auth/* requests.
const (
	trustedDeviceCookieName = "wpmgr_2fa_device"
	trustedDeviceCookiePath = "/auth"
)

// RegisterTwoFactor mounts both the unauthenticated challenge-completion routes
// and the authenticated 2FA management routes on the supplied /auth group g.
// The authenticated sub-group adds RequireAuth middleware via the principal
// check inside each handler (same pattern as /auth/me, /auth/me/password).
// Called from handler.go Register.
func (h *Handler) RegisterTwoFactor(g gin.IRouter) {
	// --- unauthenticated challenge-completion ---
	g.POST("/2fa/totp", h.twoFATOTPComplete)
	g.POST("/2fa/recovery", h.twoFARecoveryComplete)
	g.POST("/2fa/webauthn/begin", h.twoFAWebAuthnBegin)
	g.POST("/2fa/webauthn/finish", h.twoFAWebAuthnFinish)

	// --- authenticated management ---
	g.GET("/2fa/status", h.twoFAStatus)
	g.POST("/2fa/totp/begin", h.twoFATOTPBegin)
	g.POST("/2fa/totp/confirm", h.twoFATOTPConfirm)
	g.POST("/2fa/totp/disable", h.twoFATOTPDisable)
	g.POST("/2fa/webauthn/begin-registration", h.twoFAWebAuthnBeginReg)
	g.POST("/2fa/webauthn/finish-registration", h.twoFAWebAuthnFinishReg)
	g.GET("/2fa/webauthn/credentials", h.twoFAWebAuthnListCreds)
	g.DELETE("/2fa/webauthn/credentials/:id", h.twoFAWebAuthnDeleteCred)
	g.POST("/2fa/recovery-codes/regenerate", h.twoFARegenRecoveryCodes)
	g.GET("/2fa/trusted-devices", h.twoFAListTrustedDevices)
	g.DELETE("/2fa/trusted-devices/:id", h.twoFARevokeTrustedDevice)
	g.POST("/2fa/trusted-devices/revoke-all", h.twoFARevokeAllTrustedDevices)
}

// ---------------------------------------------------------------------------
// Unauthenticated: challenge-completion endpoints
// The challenge nonce returned at login is the sole bearer credential.
// ---------------------------------------------------------------------------

type totpCompleteBody struct {
	Challenge      string `json:"challenge"`
	Code           string `json:"code"`
	RememberDevice bool   `json:"remember_device"`
	DeviceLabel    string `json:"device_label"`
}

// twoFATOTPComplete handles POST /auth/2fa/totp.
// Verifies the TOTP code against the active challenge and, on success, issues
// a full session. Honors remember_device to issue a trusted-device cookie.
func (h *Handler) twoFATOTPComplete(c *gin.Context) {
	var body totpCompleteBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	challengeID, err := uuid.Parse(body.Challenge)
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_challenge", "challenge must be a valid UUID"))
		return
	}

	ip := clientAddr(c)
	res, err := h.svc.VerifyTOTPChallenge(c.Request.Context(), challengeID, body.Code, &ip)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if err := h.sessions.Login(c.Request.Context(), res.User.ID, res.ActiveTenant); err != nil {
		httpx.Error(c, domain.Internal("session_failed", "failed to establish session").WithCause(err))
		return
	}

	// A social sign-in that reached this challenge left its approved identity
	// link unwritten. Both factors are now proven, so it can be written.
	h.completePendingSocialLink(c, res.User.ID, challengeID)

	label := body.DeviceLabel
	if label == "" {
		label = labelFromUserAgent(c.Request.UserAgent())
	}
	if body.RememberDevice {
		h.issueDeviceCookieWithLabel(c, res, label)
	}

	remaining, _ := h.svc.CountRecoveryCodes(c.Request.Context(), res.User.ID)
	out := toMe(res.User, res.Memberships, res.ActiveTenant, h.hosted, h.managedStorageAllowed(c.Request.Context(), res.ActiveTenant), res.DesiredPlan)
	c.JSON(http.StatusOK, gin.H{
		"me":                       out,
		"recovery_codes_remaining": remaining,
	})
}

type recoveryCompleteBody struct {
	Challenge      string `json:"challenge"`
	Code           string `json:"code"`
	RememberDevice bool   `json:"remember_device"`
	DeviceLabel    string `json:"device_label"`
}

// twoFARecoveryComplete handles POST /auth/2fa/recovery.
// Verifies a single-use recovery code and issues a full session on success.
func (h *Handler) twoFARecoveryComplete(c *gin.Context) {
	var body recoveryCompleteBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	challengeID, err := uuid.Parse(body.Challenge)
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_challenge", "challenge must be a valid UUID"))
		return
	}

	ip := clientAddr(c)
	res, remaining, err := h.svc.VerifyRecoveryCodeChallenge(c.Request.Context(), challengeID, body.Code, &ip)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if err := h.sessions.Login(c.Request.Context(), res.User.ID, res.ActiveTenant); err != nil {
		httpx.Error(c, domain.Internal("session_failed", "failed to establish session").WithCause(err))
		return
	}

	// A social sign-in that reached this challenge left its approved identity
	// link unwritten. Both factors are now proven, so it can be written.
	h.completePendingSocialLink(c, res.User.ID, challengeID)

	label := body.DeviceLabel
	if label == "" {
		label = labelFromUserAgent(c.Request.UserAgent())
	}
	if body.RememberDevice {
		h.issueDeviceCookieWithLabel(c, res, label)
	}

	out := toMe(res.User, res.Memberships, res.ActiveTenant, h.hosted, h.managedStorageAllowed(c.Request.Context(), res.ActiveTenant), res.DesiredPlan)
	c.JSON(http.StatusOK, gin.H{
		"me":                       out,
		"recovery_codes_remaining": remaining,
	})
}

type webAuthnBeginChallengeBody struct {
	Challenge string `json:"challenge"`
}

// twoFAWebAuthnBegin handles POST /auth/2fa/webauthn/begin.
// Starts the WebAuthn assertion ceremony; returns CredentialAssertion options.
func (h *Handler) twoFAWebAuthnBegin(c *gin.Context) {
	var body webAuthnBeginChallengeBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	challengeID, err := uuid.Parse(body.Challenge)
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_challenge", "challenge must be a valid UUID"))
		return
	}

	optionsJSON, err := h.svc.BeginWebAuthnChallenge(c.Request.Context(), challengeID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Return the raw JSON assertion options so the browser can pass them
	// directly to navigator.credentials.get().
	c.Data(http.StatusOK, "application/json", optionsJSON)
}

type webAuthnFinishChallengeBody struct {
	Challenge      string `json:"challenge"`
	Assertion      []byte `json:"assertion"`
	RememberDevice bool   `json:"remember_device"`
	DeviceLabel    string `json:"device_label"`
}

// twoFAWebAuthnFinish handles POST /auth/2fa/webauthn/finish.
// Validates the WebAuthn assertion and, on success, issues a full session.
func (h *Handler) twoFAWebAuthnFinish(c *gin.Context) {
	var body webAuthnFinishChallengeBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	challengeID, err := uuid.Parse(body.Challenge)
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_challenge", "challenge must be a valid UUID"))
		return
	}
	if len(body.Assertion) == 0 {
		httpx.Error(c, domain.Validation("missing_assertion", "assertion bytes are required"))
		return
	}

	ip := clientAddr(c)
	res, err := h.svc.FinishWebAuthnChallenge(c.Request.Context(), challengeID, body.Assertion, &ip)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if err := h.sessions.Login(c.Request.Context(), res.User.ID, res.ActiveTenant); err != nil {
		httpx.Error(c, domain.Internal("session_failed", "failed to establish session").WithCause(err))
		return
	}

	// A social sign-in that reached this challenge left its approved identity
	// link unwritten. Both factors are now proven, so it can be written.
	h.completePendingSocialLink(c, res.User.ID, challengeID)

	label := body.DeviceLabel
	if label == "" {
		label = labelFromUserAgent(c.Request.UserAgent())
	}
	if body.RememberDevice {
		h.issueDeviceCookieWithLabel(c, res, label)
	}

	out := toMe(res.User, res.Memberships, res.ActiveTenant, h.hosted, h.managedStorageAllowed(c.Request.Context(), res.ActiveTenant), res.DesiredPlan)
	c.JSON(http.StatusOK, &out)
}

// ---------------------------------------------------------------------------
// Authenticated: 2FA management endpoints
// Each handler checks that a valid session principal exists (same pattern
// as /auth/me and /auth/me/password). The session is the bearer credential.
// ---------------------------------------------------------------------------

// twoFAStatus handles GET /auth/2fa/status.
// Returns the current 2FA configuration summary for the Security settings screen.
func (h *Handler) twoFAStatus(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	state, err := h.svc.GetTwoFactorState(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	waCount, err := h.svc.GetWebAuthnCount(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	remaining, _ := h.svc.CountRecoveryCodes(c.Request.Context(), p.UserID)

	devices, err := h.svc.ListTrustedDevices(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	deviceOut := make([]gin.H, 0, len(devices))
	for _, d := range devices {
		entry := gin.H{
			"id":         d.ID,
			"label":      d.Label,
			"user_agent": d.UserAgent,
			"created_at": d.CreatedAt,
			"expires_at": d.ExpiresAt,
		}
		if d.LastUsedAt != nil {
			entry["last_used_at"] = d.LastUsedAt
		}
		if d.IP != nil {
			entry["ip"] = d.IP.String()
		}
		deviceOut = append(deviceOut, entry)
	}

	c.JSON(http.StatusOK, gin.H{
		"totp_enabled":             state.TOTPConfirmedAt != nil,
		"webauthn_count":           waCount,
		"recovery_codes_remaining": remaining,
		"two_factor_enabled":       state.TwoFactorEnabled,
		"trusted_devices":          deviceOut,
	})
}

// twoFATOTPBegin handles POST /auth/2fa/totp/begin.
// Starts TOTP enrollment. Returns the otpauth_uri and base32 secret for the
// enrollment wizard; this is the ONLY time the secret is returned.
func (h *Handler) twoFATOTPBegin(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	u, _, err := h.svc.Me(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	setup, err := h.svc.BeginTOTPEnrollment(c.Request.Context(), p.UserID, u.Email)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"otpauth_uri": setup.OtpAuthURI,
		"secret":      setup.Secret,
	})
}

type totpConfirmBody struct {
	Code string `json:"code"`
}

// twoFATOTPConfirm handles POST /auth/2fa/totp/confirm.
// Validates the live code against the provisional secret, persists the
// confirmed secret, and returns 10 recovery codes (shown once).
func (h *Handler) twoFATOTPConfirm(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	var body totpConfirmBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}

	codes, err := h.svc.ConfirmTOTPEnrollment(c.Request.Context(), p.UserID, body.Code)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"recovery_codes": codes})
}

type totpDisableBody struct {
	CurrentPassword string `json:"current_password"`
}

// twoFATOTPDisable handles POST /auth/2fa/totp/disable.
// Requires current_password (re-auth guard per ADR-056 invariant 5). On
// success: clears TOTP, recomputes two_factor_enabled, revokes all trusted
// devices, and clears the trusted-device cookie on this response.
func (h *Handler) twoFATOTPDisable(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	var body totpDisableBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}

	// Re-auth: verify the current password before allowing 2FA disable.
	if err := h.svc.VerifyCurrentPassword(c.Request.Context(), p.UserID, body.CurrentPassword); err != nil {
		httpx.Error(c, err)
		return
	}

	memberships, err := h.svc.GetMemberships(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if err := h.svc.DisableTOTP(c.Request.Context(), p.UserID, memberships); err != nil {
		httpx.Error(c, err)
		return
	}

	// Clear the trusted-device cookie for this client.
	h.clearDeviceCookie(c)

	c.Status(http.StatusNoContent)
}

// twoFAWebAuthnBeginReg handles POST /auth/2fa/webauthn/begin-registration.
// Starts the WebAuthn registration ceremony for the logged-in user.
func (h *Handler) twoFAWebAuthnBeginReg(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	u, _, err := h.svc.Me(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	optionsJSON, err := h.svc.BeginWebAuthnEnrollment(c.Request.Context(), p.UserID, u.Email, u.Name)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	c.Data(http.StatusOK, "application/json", optionsJSON)
}

type webAuthnFinishRegBody struct {
	Name        string `json:"name"`
	Attestation []byte `json:"attestation"`
}

// twoFAWebAuthnFinishReg handles POST /auth/2fa/webauthn/finish-registration.
// Validates the attestation and persists the new WebAuthn credential.
func (h *Handler) twoFAWebAuthnFinishReg(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	var body webAuthnFinishRegBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if len(body.Attestation) == 0 {
		httpx.Error(c, domain.Validation("missing_attestation", "attestation bytes are required"))
		return
	}

	memberships, err := h.svc.GetMemberships(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	cred, err := h.svc.FinishWebAuthnEnrollment(c.Request.Context(), p.UserID, body.Name, body.Attestation, memberships)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	out := gin.H{
		"id":         cred.ID,
		"name":       cred.Name,
		"created_at": cred.CreatedAt,
	}
	c.JSON(http.StatusCreated, out)
}

// twoFAWebAuthnListCreds handles GET /auth/2fa/webauthn/credentials.
func (h *Handler) twoFAWebAuthnListCreds(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	creds, err := h.svc.ListWebAuthnCredentials(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	items := make([]gin.H, 0, len(creds))
	for _, cr := range creds {
		entry := gin.H{
			"id":         cr.ID,
			"name":       cr.Name,
			"created_at": cr.CreatedAt,
		}
		if cr.LastUsedAt != nil {
			entry["last_used_at"] = cr.LastUsedAt
		}
		items = append(items, entry)
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}

type webAuthnDeleteCredBody struct {
	CurrentPassword string `json:"current_password"`
}

// twoFAWebAuthnDeleteCred handles DELETE /auth/2fa/webauthn/credentials/:id.
// Requires current_password re-auth to prevent session-theft-driven key removal.
func (h *Handler) twoFAWebAuthnDeleteCred(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	credID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_id", "credential ID must be a valid UUID"))
		return
	}

	var body webAuthnDeleteCredBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}

	// Re-auth: verify current password.
	if err := h.svc.VerifyCurrentPassword(c.Request.Context(), p.UserID, body.CurrentPassword); err != nil {
		httpx.Error(c, err)
		return
	}

	memberships, err := h.svc.GetMemberships(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if err := h.svc.DeleteWebAuthnCredential(c.Request.Context(), credID, p.UserID, memberships); err != nil {
		httpx.Error(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

type regenRecoveryCodesBody struct {
	CurrentPassword string `json:"current_password"`
}

// twoFARegenRecoveryCodes handles POST /auth/2fa/recovery-codes/regenerate.
// Replaces the existing recovery code batch with 10 new ones. Returns codes once.
func (h *Handler) twoFARegenRecoveryCodes(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	var body regenRecoveryCodesBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}

	// Re-auth: verify current password.
	if err := h.svc.VerifyCurrentPassword(c.Request.Context(), p.UserID, body.CurrentPassword); err != nil {
		httpx.Error(c, err)
		return
	}

	memberships, err := h.svc.GetMemberships(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	codes, err := h.svc.RegenerateRecoveryCodes(c.Request.Context(), p.UserID, memberships)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"recovery_codes": codes})
}

// twoFAListTrustedDevices handles GET /auth/2fa/trusted-devices.
func (h *Handler) twoFAListTrustedDevices(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	devices, err := h.svc.ListTrustedDevices(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	items := make([]gin.H, 0, len(devices))
	for _, d := range devices {
		entry := gin.H{
			"id":         d.ID,
			"label":      d.Label,
			"user_agent": d.UserAgent,
			"created_at": d.CreatedAt,
			"expires_at": d.ExpiresAt,
		}
		if d.LastUsedAt != nil {
			entry["last_used_at"] = d.LastUsedAt
		}
		if d.IP != nil {
			entry["ip"] = d.IP.String()
		}
		items = append(items, entry)
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}

// twoFARevokeTrustedDevice handles DELETE /auth/2fa/trusted-devices/:id.
func (h *Handler) twoFARevokeTrustedDevice(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	deviceID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_id", "device ID must be a valid UUID"))
		return
	}

	memberships, err := h.svc.GetMemberships(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if err := h.svc.RevokeTrustedDevice(c.Request.Context(), deviceID, p.UserID, memberships); err != nil {
		httpx.Error(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// twoFARevokeAllTrustedDevices handles POST /auth/2fa/trusted-devices/revoke-all.
// Clears the device cookie on this response to avoid an inconsistent state.
func (h *Handler) twoFARevokeAllTrustedDevices(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	memberships, err := h.svc.GetMemberships(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if err := h.svc.RevokeAllTrustedDevices(c.Request.Context(), p.UserID, memberships); err != nil {
		httpx.Error(c, err)
		return
	}

	h.clearDeviceCookie(c)
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Trusted-device cookie helpers
// ---------------------------------------------------------------------------

// labelFromUserAgent derives a human-readable device label from the User-Agent
// header. It returns a short browser/OS description. Falls back to "browser"
// when the UA is empty or unrecognised.
//
// N5: callers use this when the client does not supply an explicit device_label.
func labelFromUserAgent(ua string) string {
	if ua == "" {
		return "browser"
	}
	// Minimal heuristic: identify the most common browser families and OS.
	// This avoids a third-party UA parser dependency.
	switch {
	case containsAny(ua, "Firefox/"):
		return "Firefox"
	case containsAny(ua, "Edg/", "Edge/"):
		return "Edge"
	case containsAny(ua, "Chrome/", "CriOS/"):
		return "Chrome"
	case containsAny(ua, "Safari/") && !containsAny(ua, "Chrome/"):
		return "Safari"
	default:
		return "browser"
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(sub) > 0 {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// issueDeviceCookieWithLabel calls IssueTrustedDevice with the given label and
// sets the wpmgr_2fa_device cookie. Failures are silently ignored: the user is
// already authenticated; a failed device-trust write must not break the response.
//
// N5: callers derive the label from the client-supplied device_label field,
// falling back to labelFromUserAgent(). The old hardcoded "browser" label is gone.
func (h *Handler) issueDeviceCookieWithLabel(c *gin.Context, res LoginResult, label string) {
	ip := clientAddr(c)
	ua := c.Request.UserAgent()

	memberships := res.Memberships
	rawToken, _, err := h.svc.IssueTrustedDevice(c.Request.Context(), res.User.ID, label, ua, &ip, memberships)
	if err != nil {
		return // best-effort; do not fail the authenticated response
	}

	http.SetCookie(c.Writer, &http.Cookie{
		Name:     trustedDeviceCookieName,
		Value:    rawToken,
		Path:     trustedDeviceCookiePath,
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(trustedDeviceDefaultTTL / time.Second),
	})
}

// issueDeviceCookie is a convenience wrapper that derives the label from the
// User-Agent. Kept for any future callers that do not have an explicit label.
func (h *Handler) issueDeviceCookie(c *gin.Context, res LoginResult) {
	h.issueDeviceCookieWithLabel(c, res, labelFromUserAgent(c.Request.UserAgent()))
}

// clearDeviceCookie expires the trusted-device cookie immediately.
func (h *Handler) clearDeviceCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     trustedDeviceCookieName,
		Value:    "",
		Path:     trustedDeviceCookiePath,
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

// readDeviceCookie returns the raw token from the trusted-device cookie, or ""
// if the cookie is absent or empty.
func readDeviceCookie(c *gin.Context) string {
	ck, err := c.Request.Cookie(trustedDeviceCookieName)
	if err != nil || ck == nil {
		return ""
	}
	return ck.Value
}
