package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth/twofactor"
	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const (
	// challengeTTL is how long a login 2FA challenge is valid.
	challengeTTL = 5 * time.Minute

	// maxChallengeAttempts is the number of failed factor checks that lock a
	// challenge. After this many failures the challenge is expired and a new
	// one must be issued (ADR-056 invariant 4).
	maxChallengeAttempts = 5

	// totpProvisionalTTL is how long a provisional TOTP secret is valid during
	// the enrollment wizard (BeginRegistration -> FinishRegistration).
	totpProvisionalTTL = 10 * time.Minute

	// webauthnRegistrationTTL is how long a WebAuthn registration session is
	// kept before the user must restart.
	webauthnRegistrationTTL = 10 * time.Minute

	// trustedDeviceDefaultTTL is the default "remember this device" window.
	trustedDeviceDefaultTTL = 30 * 24 * time.Hour

	// S2: cross-challenge rate-limit constants.
	// twoFAUserLockoutPerMinute is the number of failed factor attempts allowed
	// per user per rolling 60-second window ACROSS all challenges. A fresh
	// challenge per login without this cross-challenge limit means the per-
	// challenge cap of 5 is trivially bypassed by issuing a new challenge each
	// time. Safe default: 10 attempts/min covers legitimate clock-skew retries.
	twoFAUserLockoutPerMinute = 10
	// twoFAIPLockoutPerMinute caps total 2FA verification attempts from a
	// single IP across all users. Prevents a single attacker from cycling
	// through accounts. Safe default: 30/min.
	twoFAIPLockoutPerMinute = 30
)

// TwoFactorService holds all Phase 2 2FA business logic. It is embedded (via
// delegation) into the main auth Service rather than being a separate type so
// that existing callers do not need new constructor wiring. All DB writes go
// through pool.InAgentTx (these tables use app.agent='on' RLS).
//
// Fields are injected via SetTwoFactorDeps after the Service is built.
type TwoFactorService struct {
	repo       *Repo
	audit      *audit.Recorder
	totpFactor *twofactor.TOTPFactor
	waFactor   *twofactor.WebAuthnFactor
	cryptbox   *cryptbox.AgeIdentity
	// limiter is the shared rate limiter from the main auth Service (S2). It is
	// used to enforce a cross-challenge per-user and per-IP lockout on factor
	// verification, preventing brute-force via repeated challenge issuance.
	// nil = no rate limiting (non-production, or limiter not wired).
	limiter RateLimiter
}

// SetTwoFactorDeps injects the 2FA dependencies into the auth Service.
// Called once at startup after all components are constructed.
func (s *Service) SetTwoFactorDeps(totpFactor *twofactor.TOTPFactor, waFactor *twofactor.WebAuthnFactor, box *cryptbox.AgeIdentity) {
	s.twofa = &TwoFactorService{
		repo:       s.repo,
		audit:      s.audit,
		totpFactor: totpFactor,
		waFactor:   waFactor,
		cryptbox:   box,
		limiter:    s.limiter, // inherit from the parent service (set via SetMailer)
	}
}

// twofa is the 2FA service embedded in auth.Service. Accessed via s.twofa.
// It is nil until SetTwoFactorDeps is called; all 2FA methods guard against nil.

// AvailableFactors lists which second factors a user has enrolled.
type AvailableFactors struct {
	TOTP           bool
	WebAuthnCount  int
}

// TwoFactorChallengeResult is the outcome of RequestTwoFactorChallenge.
type TwoFactorChallengeResult struct {
	ChallengeID uuid.UUID
	Factors     AvailableFactors
}

// RequestTwoFactorChallenge creates a challenge row and returns the challenge
// ID + which factors the user has available. Called by the login handler when
// user.two_factor_enabled is true.
func (s *Service) RequestTwoFactorChallenge(ctx context.Context, userID uuid.UUID, ip *netip.Addr) (TwoFactorChallengeResult, error) {
	if s.twofa == nil {
		return TwoFactorChallengeResult{}, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.requestChallenge(ctx, userID, ip)
}

// VerifyTOTPChallenge validates a TOTP code against the active challenge. On
// success it returns the user + their memberships for session issuance.
// ip is the caller's IP address, used for the S2 cross-challenge rate limit.
func (s *Service) VerifyTOTPChallenge(ctx context.Context, challengeID uuid.UUID, code string, ip *netip.Addr) (LoginResult, error) {
	if s.twofa == nil {
		return LoginResult{}, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.verifyTOTP(ctx, challengeID, code, ip)
}

// VerifyRecoveryCodeChallenge validates a recovery code against the active
// challenge. On success it returns the user + memberships for session issuance
// and the remaining code count. ip is used for the S2 cross-challenge rate limit.
func (s *Service) VerifyRecoveryCodeChallenge(ctx context.Context, challengeID uuid.UUID, code string, ip *netip.Addr) (LoginResult, int64, error) {
	if s.twofa == nil {
		return LoginResult{}, 0, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.verifyRecoveryCode(ctx, challengeID, code, ip)
}

// BeginWebAuthnChallenge starts the WebAuthn assertion for an active challenge.
// Returns the CredentialAssertion JSON for the browser.
func (s *Service) BeginWebAuthnChallenge(ctx context.Context, challengeID uuid.UUID) ([]byte, error) {
	if s.twofa == nil {
		return nil, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.beginWebAuthnLogin(ctx, challengeID)
}

// FinishWebAuthnChallenge validates the WebAuthn assertion. On success it
// returns the user + memberships. ip is used for the S2 cross-challenge rate limit.
func (s *Service) FinishWebAuthnChallenge(ctx context.Context, challengeID uuid.UUID, assertionJSON []byte, ip *netip.Addr) (LoginResult, error) {
	if s.twofa == nil {
		return LoginResult{}, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.finishWebAuthnLogin(ctx, challengeID, assertionJSON, ip)
}

// --- TOTP Enrollment ---

// BeginTOTPEnrollment generates a new TOTP secret and stores it as a
// provisional secret. Returns the TOTPSetup for the UI.
func (s *Service) BeginTOTPEnrollment(ctx context.Context, userID uuid.UUID, userEmail string) (*twofactor.TOTPSetup, error) {
	if s.twofa == nil {
		return nil, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.beginTOTPEnroll(ctx, userID, userEmail)
}

// ConfirmTOTPEnrollment validates the live code against the provisional secret,
// persists the confirmed secret, and generates + returns 10 recovery codes.
// The plaintext codes are returned ONCE; the caller must show them to the user.
func (s *Service) ConfirmTOTPEnrollment(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	if s.twofa == nil {
		return nil, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.confirmTOTPEnroll(ctx, userID, code)
}

// DisableTOTP clears the TOTP secret and recomputes two_factor_enabled. The
// caller (handler) must have already verified the current password (Phase 3
// enforcement). Also revokes all trusted devices per the ADR invariant.
func (s *Service) DisableTOTP(ctx context.Context, userID uuid.UUID, memberships []Membership) error {
	if s.twofa == nil {
		return domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.disableTOTP(ctx, userID, memberships)
}

// --- Recovery Codes ---

// RegenerateRecoveryCodes replaces the existing recovery code batch with 10
// new ones. The plaintext codes are returned ONCE.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID uuid.UUID, memberships []Membership) ([]string, error) {
	if s.twofa == nil {
		return nil, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.regenerateRecoveryCodes(ctx, userID, memberships)
}

// CountRecoveryCodes returns the number of unused recovery codes remaining.
func (s *Service) CountRecoveryCodes(ctx context.Context, userID uuid.UUID) (int64, error) {
	if s.twofa == nil {
		return 0, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.repo.twoFA().CountActiveRecoveryCodes(ctx, userID)
}

// --- WebAuthn Enrollment ---

// BeginWebAuthnEnrollment starts the registration ceremony. Returns the
// CredentialCreation JSON for the browser.
func (s *Service) BeginWebAuthnEnrollment(ctx context.Context, userID uuid.UUID, userEmail, displayName string) ([]byte, error) {
	if s.twofa == nil {
		return nil, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.beginWebAuthnEnroll(ctx, userID, userEmail, displayName)
}

// FinishWebAuthnEnrollment validates the attestation and persists the credential.
// Returns the row ID of the new credential.
func (s *Service) FinishWebAuthnEnrollment(ctx context.Context, userID uuid.UUID, name string, attestationJSON []byte, memberships []Membership) (WebAuthnCredentialRow, error) {
	if s.twofa == nil {
		return WebAuthnCredentialRow{}, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.finishWebAuthnEnroll(ctx, userID, name, attestationJSON, memberships)
}

// ListWebAuthnCredentials returns all registered credentials for the UI.
func (s *Service) ListWebAuthnCredentials(ctx context.Context, userID uuid.UUID) ([]WebAuthnCredentialRow, error) {
	if s.twofa == nil {
		return nil, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.repo.twoFA().ListWebAuthnCredentials(ctx, userID)
}

// DeleteWebAuthnCredential removes one credential and recomputes
// two_factor_enabled. The caller (handler) must authorize the actor.
func (s *Service) DeleteWebAuthnCredential(ctx context.Context, credID, userID uuid.UUID, memberships []Membership) error {
	if s.twofa == nil {
		return domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.deleteWebAuthnCredential(ctx, credID, userID, memberships)
}

// --- Trusted Devices ---

// IssueTrustedDevice creates a new trusted-device token. Returns the raw token
// (Phase 3 sets it as a signed HttpOnly cookie).
func (s *Service) IssueTrustedDevice(ctx context.Context, userID uuid.UUID, label, userAgent string, ip *netip.Addr, memberships []Membership) (string, TrustedDevice, error) {
	if s.twofa == nil {
		return "", TrustedDevice{}, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.issueTrustedDevice(ctx, userID, label, userAgent, ip, memberships)
}

// VerifyTrustedDevice checks whether a presented device token is valid and
// bumps its last_used_at. On success the caller may skip the 2FA challenge.
func (s *Service) VerifyTrustedDevice(ctx context.Context, rawToken string) (TrustedDevice, error) {
	if s.twofa == nil {
		return TrustedDevice{}, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.verifyTrustedDevice(ctx, rawToken)
}

// VerifyTrustedDeviceNoTouch looks up the device by token hash WITHOUT bumping
// last_used_at. Used by the login handler to perform the B1 user-binding check
// before committing the touch. Call TouchTrustedDevice after confirming ownership.
func (s *Service) VerifyTrustedDeviceNoTouch(ctx context.Context, rawToken string) (TrustedDevice, error) {
	if s.twofa == nil {
		return TrustedDevice{}, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.verifyTrustedDeviceNoTouch(ctx, rawToken)
}

// TouchTrustedDevice bumps last_used_at for a verified device. Called by the
// login handler after confirming device ownership (B1 invariant).
func (s *Service) TouchTrustedDevice(ctx context.Context, deviceID uuid.UUID) error {
	if s.twofa == nil {
		return nil // no-op; 2FA not configured
	}
	return s.twofa.repo.twoFA().TouchTrustedDevice(ctx, deviceID)
}

// ListTrustedDevices returns all active trusted devices for the Security UI.
func (s *Service) ListTrustedDevices(ctx context.Context, userID uuid.UUID) ([]TrustedDevice, error) {
	if s.twofa == nil {
		return nil, domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.repo.twoFA().ListTrustedDevices(ctx, userID)
}

// RevokeTrustedDevice soft-deletes one device.
func (s *Service) RevokeTrustedDevice(ctx context.Context, deviceID, userID uuid.UUID, memberships []Membership) error {
	if s.twofa == nil {
		return domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.revokeTrustedDevice(ctx, deviceID, userID, memberships)
}

// RevokeAllTrustedDevices revokes every device trust for a user.
func (s *Service) RevokeAllTrustedDevices(ctx context.Context, userID uuid.UUID, memberships []Membership) error {
	if s.twofa == nil {
		return domain.ServiceUnavailable("2fa_not_configured", "two-factor authentication is not configured")
	}
	return s.twofa.revokeAllTrustedDevices(ctx, userID, memberships)
}

// ---------------------------------------------------------------------------
// internal TwoFactorService method implementations
// ---------------------------------------------------------------------------

func (t *TwoFactorService) requestChallenge(ctx context.Context, userID uuid.UUID, ip *netip.Addr) (TwoFactorChallengeResult, error) {
	// Read available factors.
	state, err := t.repo.twoFA().GetTwoFactorState(ctx, userID)
	if err != nil {
		return TwoFactorChallengeResult{}, err
	}

	waCount, err := t.repo.twoFA().CountWebAuthnCredentials(ctx, userID)
	if err != nil {
		return TwoFactorChallengeResult{}, err
	}

	factors := AvailableFactors{
		TOTP:          state.TOTPConfirmedAt != nil,
		WebAuthnCount: int(waCount),
	}

	// Generate a high-entropy nonce (32 bytes URL-safe base64, same as reset token).
	nonce, err := generate32ByteToken()
	if err != nil {
		return TwoFactorChallengeResult{}, domain.Internal("challenge_nonce_failed", "failed to generate challenge nonce").WithCause(err)
	}

	id, err := t.repo.twoFA().CreateChallenge(ctx, userID, nonce, "login", nil, ip, challengeTTL)
	if err != nil {
		return TwoFactorChallengeResult{}, err
	}

	// Audit: challenge issued (best-effort; no tenant context).
	t.recordUserAudit(ctx, userID, audit.Action2FAChallengeIssued, map[string]any{
		"challenge_id":      id.String(),
		"factors_available": factorList(factors),
	})

	return TwoFactorChallengeResult{ChallengeID: id, Factors: factors}, nil
}

func (t *TwoFactorService) verifyTOTP(ctx context.Context, challengeID uuid.UUID, code string, ip *netip.Addr) (LoginResult, error) {
	// Load and validate challenge, then run all TOTP checks + consume inside
	// a single InAgentTx so the step stamp and challenge consume are atomic.
	var result LoginResult
	var challengeUserID uuid.UUID

	// Phase 1: load and validate the challenge outside a long tx.
	challenge, err := t.repo.twoFA().GetActiveChallenge(ctx, challengeID)
	if err != nil {
		return LoginResult{}, err
	}
	challengeUserID = challenge.UserID

	// S2: enforce cross-challenge per-user + per-IP rate limits BEFORE doing any
	// crypto work, so a locked-out user gets a fast rejection. The IP comes from
	// the calling handler (current request IP).
	if err := t.checkCrossChallengeLimits(ctx, challengeUserID, ip); err != nil {
		return LoginResult{}, err
	}

	// Phase 2: load the user's TOTP secret.
	state, err := t.repo.twoFA().GetTwoFactorState(ctx, challengeUserID)
	if err != nil {
		return LoginResult{}, err
	}
	if state.TOTPConfirmedAt == nil || len(state.TOTPSecretEncrypted) == 0 {
		return LoginResult{}, domain.Validation("totp_not_enrolled", "TOTP is not enrolled for this user")
	}

	// Decrypt the secret.
	secretBytes, err := t.cryptbox.Decrypt(state.TOTPSecretEncrypted)
	if err != nil {
		return LoginResult{}, domain.Internal("totp_decrypt_failed", "failed to decrypt TOTP secret").WithCause(err)
	}
	secret := string(secretBytes)

	// Phase 3: validate the code (no DB involved yet).
	valid, step, valErr := t.totpFactor.ValidateCode(code, secret)
	if valErr != nil {
		// Malformed code (not 6 digits, etc.) counts as an attempt.
		_ = t.bumpAttemptAndMaybeExpire(ctx, challengeID, challengeUserID, "invalid_code")
		return LoginResult{}, domain.Unauthorized("invalid_totp_code", "invalid TOTP code")
	}
	if !valid {
		_ = t.bumpAttemptAndMaybeExpire(ctx, challengeID, challengeUserID, "invalid_code")
		return LoginResult{}, domain.Unauthorized("invalid_totp_code", "invalid TOTP code")
	}

	// Phase 4: replay protection and atomic commit inside InAgentTx.
	txErr := t.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		// Re-check: load totp_last_step inside the tx to prevent TOCTOU.
		lastStep, stepErr := q.GetUserTOTPLastStep(ctx, challengeUserID)
		if stepErr != nil && !errors.Is(stepErr, pgx.ErrNoRows) {
			return domain.Internal("totp_step_failed", "failed to read TOTP step").WithCause(stepErr)
		}
		if lastStep != nil && *lastStep == step {
			// This exact step was already accepted: reject as replay.
			return domain.Unauthorized("totp_code_already_used", "this TOTP code has already been used; wait for the next time step")
		}

		// Stamp the step.
		if err := t.repo.twoFA().SetTOTPLastStep(ctx, tx, challengeUserID, step); err != nil {
			return err
		}
		// Consume the challenge.
		return t.repo.twoFA().ConsumeChallenge(ctx, tx, challengeID)
	})
	if txErr != nil {
		// If the error is a replay, count as an attempt too.
		if de, ok := domain.AsDomain(txErr); ok && de.Code == "totp_code_already_used" {
			_ = t.bumpAttemptAndMaybeExpire(ctx, challengeID, challengeUserID, "replay")
		}
		return LoginResult{}, txErr
	}

	// Load user + memberships for session issuance.
	u, err := t.repo.GetUserByID(ctx, challengeUserID)
	if err != nil {
		return LoginResult{}, err
	}
	memberships, _ := t.repo.ListMembershipsForUser(ctx, challengeUserID)

	// Audit success.
	t.recordUserAuditFromMemberships(ctx, challengeUserID, memberships, audit.ActionTOTPVerified, map[string]any{
		"challenge_id": challengeID.String(),
	})

	result = LoginResult{User: u, Memberships: memberships}
	result.ActiveTenant = resolveActiveTenantFromMemberships(memberships)
	return result, nil
}

func (t *TwoFactorService) verifyRecoveryCode(ctx context.Context, challengeID uuid.UUID, rawCode string, ip *netip.Addr) (LoginResult, int64, error) {
	challenge, err := t.repo.twoFA().GetActiveChallenge(ctx, challengeID)
	if err != nil {
		return LoginResult{}, 0, err
	}
	userID := challenge.UserID

	// S2: enforce cross-challenge per-user + per-IP rate limits.
	if err := t.checkCrossChallengeLimits(ctx, userID, ip); err != nil {
		return LoginResult{}, 0, err
	}

	// Normalize the submitted code (strip hyphens, upper-case).
	normalized := twofactor.NormalizeRecoveryCode(rawCode)

	// Load all unused codes and find a constant-time match.
	codes, err := t.repo.twoFA().ListActiveRecoveryCodes(ctx, userID)
	if err != nil {
		return LoginResult{}, 0, err
	}
	if len(codes) == 0 {
		return LoginResult{}, 0, domain.Gone("codes_exhausted", "all recovery codes have been used; please regenerate")
	}

	var matchedID uuid.UUID
	var matched bool
	for _, c := range codes {
		// VerifyPassword uses argon2id constant-time comparison.
		ok, _ := VerifyPassword(normalized, c.CodeHash)
		if ok {
			matchedID = c.ID
			matched = true
			// Do NOT break: continue comparing all codes to avoid timing leaks
			// that would reveal which position in the list matched.
		}
	}

	if !matched {
		_ = t.bumpAttemptAndMaybeExpire(ctx, challengeID, userID, "invalid_recovery_code")
		return LoginResult{}, 0, domain.Unauthorized("invalid_recovery_code", "invalid recovery code")
	}

	// Atomically consume the code and the challenge.
	var remaining int64
	txErr := t.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		if err := t.repo.twoFA().ConsumeRecoveryCode(ctx, tx, matchedID, userID); err != nil {
			return err
		}
		if err := t.repo.twoFA().ConsumeChallenge(ctx, tx, challengeID); err != nil {
			return err
		}
		r, err := sqlc.New(tx).CountActiveRecoveryCodes(ctx, userID)
		if err != nil {
			return domain.Internal("recovery_count_failed", "failed to count remaining codes").WithCause(err)
		}
		remaining = r
		return nil
	})
	if txErr != nil {
		return LoginResult{}, 0, txErr
	}

	u, err := t.repo.GetUserByID(ctx, userID)
	if err != nil {
		return LoginResult{}, 0, err
	}
	memberships, _ := t.repo.ListMembershipsForUser(ctx, userID)

	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionRecoveryCodeUsed, map[string]any{
		"remaining": remaining,
	})

	result := LoginResult{User: u, Memberships: memberships}
	result.ActiveTenant = resolveActiveTenantFromMemberships(memberships)
	return result, remaining, nil
}

func (t *TwoFactorService) beginWebAuthnLogin(ctx context.Context, challengeID uuid.UUID) ([]byte, error) {
	challenge, err := t.repo.twoFA().GetActiveChallenge(ctx, challengeID)
	if err != nil {
		return nil, err
	}
	userID := challenge.UserID

	// Build the webauthn.User adapter from the user's credentials.
	user, err := t.buildWebAuthnUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	assertionJSON, sessionData, err := t.waFactor.BeginLoginForUser(user)
	if err != nil {
		return nil, domain.Internal("webauthn_begin_failed", "failed to begin WebAuthn login").WithCause(err)
	}

	// Persist the SessionData into the challenge row (encoded as JSONB).
	sessionBytes, err := twofactor.MarshalSessionData(sessionData)
	if err != nil {
		return nil, err
	}
	// Update the challenge's webauthn_session column. We do this via a direct
	// UPDATE to avoid creating a new challenge row.
	err = t.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE two_factor_challenges SET webauthn_session = $1 WHERE id = $2 AND used_at IS NULL`,
			sessionBytes, challengeID,
		)
		return err
	})
	if err != nil {
		return nil, domain.Internal("webauthn_session_store_failed", "failed to store WebAuthn session").WithCause(err)
	}

	return assertionJSON, nil
}

func (t *TwoFactorService) finishWebAuthnLogin(ctx context.Context, challengeID uuid.UUID, assertionJSON []byte, ip *netip.Addr) (LoginResult, error) {
	challenge, err := t.repo.twoFA().GetActiveChallenge(ctx, challengeID)
	if err != nil {
		return LoginResult{}, err
	}
	userID := challenge.UserID

	// S2: enforce cross-challenge per-user + per-IP rate limits.
	if err := t.checkCrossChallengeLimits(ctx, userID, ip); err != nil {
		return LoginResult{}, err
	}

	if len(challenge.WebAuthnSession) == 0 {
		return LoginResult{}, domain.Validation("webauthn_session_missing", "WebAuthn session not found; call BeginWebAuthnChallenge first")
	}
	sessionData, err := twofactor.UnmarshalSessionData(challenge.WebAuthnSession)
	if err != nil {
		return LoginResult{}, err
	}

	// Build the webauthn.User adapter.
	waUser, err := t.buildWebAuthnUser(ctx, userID)
	if err != nil {
		return LoginResult{}, err
	}

	cred, _, validateErr := t.waFactor.ValidateLoginBytes(waUser, sessionData, assertionJSON)

	if errors.Is(validateErr, twofactor.ErrClonedAuthenticator) {
		// Audit the security event.
		t.recordUserAudit(ctx, userID, audit.ActionClonedAuthenticatorDetected, map[string]any{
			"challenge_id":     challengeID.String(),
			"credential_id":    hex.EncodeToString(cred.ID),
			"stored_count":     cred.Authenticator.SignCount,
			"presented_count":  cred.Authenticator.SignCount, // UpdateCounter was called by the lib
		})
		_ = t.bumpAttemptAndMaybeExpire(ctx, challengeID, userID, "cloned_authenticator")
		return LoginResult{}, domain.Unauthorized("cloned_authenticator", "authenticator clone detected; login rejected")
	}
	if validateErr != nil {
		_ = t.bumpAttemptAndMaybeExpire(ctx, challengeID, userID, "webauthn_failed")
		return LoginResult{}, domain.Unauthorized("webauthn_verify_failed", "WebAuthn verification failed")
	}

	// Find the matching stored row to update sign_count.
	credRow, err := t.repo.twoFA().GetWebAuthnCredentialByCredentialID(ctx, cred.ID)
	if err != nil {
		return LoginResult{}, err
	}
	// S4: assert that the matched credential belongs to the user who issued the
	// challenge. GetWebAuthnCredentialByCredentialID queries by binary credential
	// ID only (no user filter), so a compromised/cloned credential that shares a
	// binary ID with a different user could otherwise pass the assertion and be
	// attributed to the wrong user. The RLS isolation test in B3 covers this.
	if credRow.UserID != userID {
		// Credential ID exists but is owned by a different user. Treat this as a
		// failed assertion (not an internal error) to avoid leaking information.
		_ = t.bumpAttemptAndMaybeExpire(ctx, challengeID, userID, "credential_user_mismatch")
		return LoginResult{}, domain.Unauthorized("webauthn_verify_failed", "WebAuthn verification failed")
	}

	// Atomically bump sign_count and consume the challenge.
	txErr := t.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		if err := t.repo.twoFA().UpdateWebAuthnCredentialSignCount(ctx, tx, credRow.ID, int64(cred.Authenticator.SignCount), cred.Flags.BackupState); err != nil {
			return err
		}
		return t.repo.twoFA().ConsumeChallenge(ctx, tx, challengeID)
	})
	if txErr != nil {
		return LoginResult{}, txErr
	}

	u, err := t.repo.GetUserByID(ctx, userID)
	if err != nil {
		return LoginResult{}, err
	}
	memberships, _ := t.repo.ListMembershipsForUser(ctx, userID)

	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionWebAuthnVerified, map[string]any{
		"challenge_id":  challengeID.String(),
		"credential_id": hex.EncodeToString(cred.ID),
	})

	result := LoginResult{User: u, Memberships: memberships}
	result.ActiveTenant = resolveActiveTenantFromMemberships(memberships)
	return result, nil
}

// --- TOTP enrollment ---

func (t *TwoFactorService) beginTOTPEnroll(ctx context.Context, userID uuid.UUID, userEmail string) (*twofactor.TOTPSetup, error) {
	setupAny, err := t.totpFactor.BeginRegistration(ctx, userID, userEmail)
	if err != nil {
		return nil, domain.Internal("totp_begin_failed", "failed to generate TOTP key").WithCause(err)
	}
	setup, ok := setupAny.(*twofactor.TOTPSetup)
	if !ok {
		return nil, domain.Internal("totp_setup_type", "unexpected TOTP setup type")
	}

	// Encrypt the provisional secret before storing it.
	encrypted, err := t.cryptbox.Encrypt([]byte(setup.Secret))
	if err != nil {
		return nil, domain.Internal("totp_encrypt_failed", "failed to encrypt provisional TOTP secret").WithCause(err)
	}
	if err := t.repo.twoFA().SetTOTPProvisional(ctx, userID, encrypted, time.Now().Add(totpProvisionalTTL)); err != nil {
		return nil, err
	}

	// Return the setup (including plaintext secret) to the caller for display.
	// This is the ONLY time the plaintext secret is returned.
	return setup, nil
}

func (t *TwoFactorService) confirmTOTPEnroll(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	// Load the provisional encrypted secret.
	encProvisional, err := t.repo.twoFA().GetTOTPProvisional(ctx, userID)
	if err != nil {
		return nil, err
	}

	// Decrypt it.
	secretBytes, err := t.cryptbox.Decrypt(encProvisional)
	if err != nil {
		return nil, domain.Internal("totp_decrypt_failed", "failed to decrypt provisional TOTP secret").WithCause(err)
	}
	secret := string(secretBytes)

	// Validate the code.
	valid, _, valErr := t.totpFactor.ValidateCode(code, secret)
	if valErr != nil || !valid {
		return nil, domain.Unauthorized("invalid_totp_code", "invalid TOTP verification code; please check your authenticator app and try again")
	}

	// Encrypt the confirmed secret for permanent storage.
	encConfirmed, err := t.cryptbox.Encrypt(secretBytes)
	if err != nil {
		return nil, domain.Internal("totp_encrypt_failed", "failed to encrypt TOTP secret").WithCause(err)
	}

	// Generate recovery codes.
	plainCodes, err := twofactor.GenerateRecoveryCodes()
	if err != nil {
		return nil, domain.Internal("recovery_code_gen_failed", "failed to generate recovery codes").WithCause(err)
	}
	hashes := make([]string, len(plainCodes))
	for i, c := range plainCodes {
		// Hash the normalized form (no hyphen, uppercase).
		h, herr := HashPassword(twofactor.NormalizeRecoveryCode(c))
		if herr != nil {
			return nil, domain.Internal("recovery_code_hash_failed", "failed to hash recovery code").WithCause(herr)
		}
		hashes[i] = h
	}

	// Atomically: store confirmed secret, clear provisional, insert recovery codes.
	txErr := t.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if err := q.SetUserTOTPSecret(ctx, sqlc.SetUserTOTPSecretParams{
			UserID:              userID,
			TotpSecretEncrypted: encConfirmed,
		}); err != nil {
			return domain.Internal("totp_secret_store_failed", "failed to store TOTP secret").WithCause(err)
		}
		if err := q.ClearUserTOTPProvisional(ctx, userID); err != nil {
			return domain.Internal("totp_provisional_clear_failed", "failed to clear provisional secret").WithCause(err)
		}
		// Replace any existing recovery codes (user may be re-enrolling).
		if err := q.DeleteAllRecoveryCodes(ctx, userID); err != nil {
			return domain.Internal("recovery_code_delete_failed", "failed to delete old recovery codes").WithCause(err)
		}
		for _, h := range hashes {
			if err := q.InsertRecoveryCode(ctx, sqlc.InsertRecoveryCodeParams{
				UserID:   userID,
				CodeHash: h,
			}); err != nil {
				return domain.Internal("recovery_code_insert_failed", "failed to store recovery code").WithCause(err)
			}
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	// Audit the enrollment. Use the user's first membership as the tenant context.
	u, _ := t.repo.GetUserByID(ctx, userID)
	memberships, _ := t.repo.ListMembershipsForUser(ctx, userID)
	_ = u
	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionTOTPEnrolled, map[string]any{
		"confirmed_at": time.Now().UTC().Format(time.RFC3339),
	})

	return plainCodes, nil
}

func (t *TwoFactorService) disableTOTP(ctx context.Context, userID uuid.UUID, memberships []Membership) error {
	if err := t.repo.twoFA().ClearTOTPSecret(ctx, userID); err != nil {
		return err
	}

	// Recompute two_factor_enabled: still true if WebAuthn credentials exist.
	waCount, err := t.repo.twoFA().CountWebAuthnCredentials(ctx, userID)
	if err != nil {
		return err
	}
	if err := t.repo.twoFA().SetTwoFactorEnabled(ctx, userID, waCount > 0); err != nil {
		return err
	}

	// Revoke all trusted devices on TOTP disable (invariant: new second factor
	// state = no longer trusted without re-authentication).
	_ = t.repo.twoFA().RevokeAllTrustedDevices(ctx, userID)

	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionTOTPDisabled, map[string]any{
		"reason": "user_request",
	})
	return nil
}

// --- Recovery codes ---

func (t *TwoFactorService) regenerateRecoveryCodes(ctx context.Context, userID uuid.UUID, memberships []Membership) ([]string, error) {
	plainCodes, err := twofactor.GenerateRecoveryCodes()
	if err != nil {
		return nil, domain.Internal("recovery_code_gen_failed", "failed to generate recovery codes").WithCause(err)
	}
	hashes := make([]string, len(plainCodes))
	for i, c := range plainCodes {
		h, herr := HashPassword(twofactor.NormalizeRecoveryCode(c))
		if herr != nil {
			return nil, domain.Internal("recovery_code_hash_failed", "failed to hash recovery code").WithCause(herr)
		}
		hashes[i] = h
	}
	if err := t.repo.twoFA().ReplaceRecoveryCodes(ctx, userID, hashes); err != nil {
		return nil, err
	}
	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionTOTPCodesRegenerated, map[string]any{
		"count": twofactor.RecoveryCodeCount,
	})
	return plainCodes, nil
}

// --- WebAuthn enrollment ---

func (t *TwoFactorService) beginWebAuthnEnroll(ctx context.Context, userID uuid.UUID, userEmail, displayName string) ([]byte, error) {
	// Build a webauthn.User with the user's existing credentials (so go-webauthn
	// can populate excludeCredentials and prevent re-registration).
	waUser, err := t.buildWebAuthnUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	waUser.Email = userEmail
	if displayName != "" {
		waUser.DisplayName = displayName
	}

	creationJSON, sessionData, err := t.waFactor.BeginRegistrationForUser(waUser)
	if err != nil {
		return nil, domain.Internal("webauthn_begin_reg_failed", "failed to begin WebAuthn registration").WithCause(err)
	}

	if _, err := t.repo.twoFA().StoreWebAuthnRegistrationSession(ctx, userID, sessionData, webauthnRegistrationTTL); err != nil {
		return nil, err
	}

	return creationJSON, nil
}

func (t *TwoFactorService) finishWebAuthnEnroll(ctx context.Context, userID uuid.UUID, credName string, attestationJSON []byte, memberships []Membership) (WebAuthnCredentialRow, error) {
	sessionID, sessionData, err := t.repo.twoFA().LoadWebAuthnRegistrationSession(ctx, userID)
	if err != nil {
		return WebAuthnCredentialRow{}, err
	}

	waUser, err := t.buildWebAuthnUser(ctx, userID)
	if err != nil {
		return WebAuthnCredentialRow{}, err
	}

	cred, err := t.waFactor.CreateCredentialFromBytes(waUser, sessionData, attestationJSON)
	if err != nil {
		_ = t.repo.twoFA().DeleteWebAuthnRegistrationSession(ctx, sessionID)
		return WebAuthnCredentialRow{}, domain.Validation("webauthn_attestation_failed", "WebAuthn registration verification failed").WithCause(err)
	}

	// Persist credential and clean up the registration session atomically.
	var credRow WebAuthnCredentialRow
	txErr := t.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		transports := make([]string, len(cred.Transport))
		for i, tr := range cred.Transport {
			transports[i] = string(tr)
		}
		row, err := sqlc.New(tx).InsertWebAuthnCredential(ctx, sqlc.InsertWebAuthnCredentialParams{
			UserID:          userID,
			CredentialID:    cred.ID,
			PublicKey:       cred.PublicKey,
			AttestationType: cred.AttestationType,
			Aaguid:          cred.Authenticator.AAGUID,
			SignCount:       int64(cred.Authenticator.SignCount),
			Transports:      transports,
			Name:            credName,
			BackupEligible:  cred.Flags.BackupEligible,
			BackupState:     cred.Flags.BackupState,
		})
		if err != nil {
			return domain.Internal("webauthn_cred_store_failed", "failed to store WebAuthn credential").WithCause(err)
		}
		credRow = credRowFromSQLC(row)

		// Set two_factor_enabled = true.
		if err := sqlc.New(tx).SetUserTwoFactorEnabled(ctx, sqlc.SetUserTwoFactorEnabledParams{
			UserID:           userID,
			TwoFactorEnabled: true,
		}); err != nil {
			return domain.Internal("2fa_enable_failed", "failed to set two_factor_enabled").WithCause(err)
		}

		// Delete the registration session.
		return sqlc.New(tx).DeleteWebAuthnRegistrationSession(ctx, sessionID)
	})
	if txErr != nil {
		return WebAuthnCredentialRow{}, txErr
	}

	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionWebAuthnCredentialAdded, map[string]any{
		"credential_id": hex.EncodeToString(cred.ID),
		"label":         credName,
	})

	return credRow, nil
}

func (t *TwoFactorService) deleteWebAuthnCredential(ctx context.Context, credID, userID uuid.UUID, memberships []Membership) error {
	// Load before delete so we can audit the label.
	creds, err := t.repo.twoFA().ListWebAuthnCredentials(ctx, userID)
	if err != nil {
		return err
	}
	var label string
	var credBinaryID []byte
	for _, c := range creds {
		if c.ID == credID {
			label = c.Name
			credBinaryID = c.CredentialID
		}
	}

	if err := t.repo.twoFA().DeleteWebAuthnCredential(ctx, credID, userID); err != nil {
		return err
	}

	// Recompute two_factor_enabled.
	waCount, err := t.repo.twoFA().CountWebAuthnCredentials(ctx, userID)
	if err != nil {
		return err
	}
	state, _ := t.repo.twoFA().GetTwoFactorState(ctx, userID)
	totpActive := state.TOTPConfirmedAt != nil
	if err := t.repo.twoFA().SetTwoFactorEnabled(ctx, userID, waCount > 0 || totpActive); err != nil {
		return err
	}

	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionWebAuthnCredentialRemoved, map[string]any{
		"credential_id": hex.EncodeToString(credBinaryID),
		"label":         label,
	})
	return nil
}

// --- Trusted devices ---

func (t *TwoFactorService) issueTrustedDevice(ctx context.Context, userID uuid.UUID, label, userAgent string, ip *netip.Addr, memberships []Membership) (string, TrustedDevice, error) {
	// Generate 256-bit raw token.
	rawToken, err := generate32ByteToken()
	if err != nil {
		return "", TrustedDevice{}, domain.Internal("trusted_device_token_failed", "failed to generate device token").WithCause(err)
	}

	// Store SHA-256 of the raw token.
	tokenHash := sha256HexToken(rawToken)

	device, err := t.repo.twoFA().InsertTrustedDevice(ctx, userID, tokenHash, label, userAgent, ip, time.Now().Add(trustedDeviceDefaultTTL))
	if err != nil {
		return "", TrustedDevice{}, err
	}

	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionTrustedDeviceAdded, map[string]any{
		"device_id":  device.ID.String(),
		"label":      label,
		"expires_at": device.ExpiresAt.UTC().Format(time.RFC3339),
	})

	return rawToken, device, nil
}

// verifyTrustedDevice looks up the device by token hash and bumps last_used_at
// atomically. Callers that need to check ownership BEFORE touching should use
// verifyTrustedDeviceNoTouch + touchTrustedDevice separately.
func (t *TwoFactorService) verifyTrustedDevice(ctx context.Context, rawToken string) (TrustedDevice, error) {
	device, err := t.verifyTrustedDeviceNoTouch(ctx, rawToken)
	if err != nil {
		return TrustedDevice{}, err
	}
	_ = t.repo.twoFA().TouchTrustedDevice(ctx, device.ID)
	return device, nil
}

// verifyTrustedDeviceNoTouch looks up the device by token hash WITHOUT bumping
// last_used_at. Used by the login handler to perform the B1 ownership check
// before deciding whether to touch the record.
func (t *TwoFactorService) verifyTrustedDeviceNoTouch(ctx context.Context, rawToken string) (TrustedDevice, error) {
	tokenHash := sha256HexToken(rawToken)
	return t.repo.twoFA().GetTrustedDeviceByTokenHash(ctx, tokenHash)
}

func (t *TwoFactorService) revokeTrustedDevice(ctx context.Context, deviceID, userID uuid.UUID, memberships []Membership) error {
	if err := t.repo.twoFA().RevokeTrustedDevice(ctx, deviceID, userID); err != nil {
		return err
	}
	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionTrustedDeviceRevoked, map[string]any{
		"device_id": deviceID.String(),
	})
	return nil
}

func (t *TwoFactorService) revokeAllTrustedDevices(ctx context.Context, userID uuid.UUID, memberships []Membership) error {
	count, _ := t.repo.twoFA().CountTrustedDevices(ctx, userID)
	if err := t.repo.twoFA().RevokeAllTrustedDevices(ctx, userID); err != nil {
		return err
	}
	t.recordUserAuditFromMemberships(ctx, userID, memberships, audit.ActionTrustedDevicesRevokedAll, map[string]any{
		"count": count,
	})
	return nil
}

// --- helpers ---

// checkCrossChallengeLimits enforces the S2 per-user and per-IP rate limits on
// 2FA verification across challenges. Returns a domain.RateLimited error when
// either limit is exceeded. ip may be a zero netip.Addr (limit skipped for IP).
//
// The per-user limit prevents an attacker from bypassing the per-challenge cap
// of 5 by simply issuing a new challenge on each login attempt. The per-IP
// limit prevents a single host from cycling through accounts.
func (t *TwoFactorService) checkCrossChallengeLimits(ctx context.Context, userID uuid.UUID, ip *netip.Addr) error {
	if t.limiter == nil {
		return nil
	}
	// Per-user lockout.
	if ok, retryAfter := t.limiter.Allow(ctx, "2fa-user:"+userID.String(), twoFAUserLockoutPerMinute); !ok {
		_ = retryAfter
		return domain.RateLimited("too_many_attempts", "too many 2FA verification attempts; wait before trying again")
	}
	// Per-IP lockout.
	if ip != nil && ip.IsValid() {
		if ok, retryAfter := t.limiter.Allow(ctx, "2fa-ip:"+ip.String(), twoFAIPLockoutPerMinute); !ok {
			_ = retryAfter
			// Logged because the refusal is otherwise invisible: the caller sees an
			// ordinary 429 and operators see nothing at all. If the address this
			// keys on ever stops distinguishing callers, every 2FA verification in
			// the fleet is refused and this line is the only thing that says so.
			slog.WarnContext(ctx, "2fa per-address rate limited",
				"key", "2fa-ip", "addr", ip.String(), "limit_per_minute", twoFAIPLockoutPerMinute)
			return domain.RateLimited("too_many_attempts", "too many requests from this address; wait before trying again")
		}
	}
	return nil
}

// bumpAttemptAndMaybeExpire increments the attempt counter. If the limit is
// reached it locks the challenge via ExpireTwoFactorChallenge.
func (t *TwoFactorService) bumpAttemptAndMaybeExpire(ctx context.Context, challengeID, userID uuid.UUID, reason string) error {
	return t.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		attempts, err := t.repo.twoFA().IncrementChallengeAttempts(ctx, tx, challengeID)
		if err != nil {
			return err
		}
		if attempts >= maxChallengeAttempts {
			_ = t.repo.twoFA().ExpireChallenge(ctx, tx, challengeID)
			t.recordUserAudit(ctx, userID, audit.Action2FAChallengeExpired, map[string]any{
				"challenge_id": challengeID.String(),
				"attempts":     attempts,
				"reason":       reason,
			})
		}
		return nil
	})
}

// buildWebAuthnUser loads the user's WebAuthn credentials and builds the
// webauthn.User adapter for go-webauthn ceremonies.
func (t *TwoFactorService) buildWebAuthnUser(ctx context.Context, userID uuid.UUID) (*twofactor.WebAuthnUser, error) {
	u, err := t.repo.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	credRows, err := t.repo.twoFA().ListWebAuthnCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	waCreds := make([]webauthn.Credential, len(credRows))
	for i, cr := range credRows {
		waCreds[i] = twofactor.BuildCredentialFromRow(
			cr.CredentialID, cr.PublicKey, cr.AttestationType,
			cr.AAGUID, cr.SignCount, cr.Transports,
			cr.BackupEligible, cr.BackupState,
		)
	}
	return &twofactor.WebAuthnUser{
		ID:          u.ID,
		Email:       u.Email,
		DisplayName: u.Name,
		Credentials: waCreds,
	}, nil
}

// recordUserAudit emits an audit event. Uses the user's first membership as the
// tenant context; falls back to first client-member tenant; silently skips if
// neither is available.
func (t *TwoFactorService) recordUserAudit(ctx context.Context, userID uuid.UUID, action string, meta map[string]any) {
	memberships, _ := t.repo.ListMembershipsForUser(ctx, userID)
	t.recordUserAuditFromMemberships(ctx, userID, memberships, action, meta)
}

func (t *TwoFactorService) recordUserAuditFromMemberships(ctx context.Context, userID uuid.UUID, memberships []Membership, action string, meta map[string]any) {
	if len(memberships) > 0 {
		_, _ = t.audit.Record(ctx, audit.Event{
			TenantID:   memberships[0].TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    userID.String(),
			Action:     action,
			TargetType: "user",
			TargetID:   userID.String(),
			Metadata:   meta,
		})
	}
}

// resolveActiveTenantFromMemberships picks the first tenant from a membership
// list. Used to set ActiveTenant on LoginResult after a 2FA success.
func resolveActiveTenantFromMemberships(memberships []Membership) uuid.UUID {
	if len(memberships) > 0 {
		return memberships[0].TenantID
	}
	return uuid.Nil
}

// generate32ByteToken generates a 32-byte URL-safe base64-encoded random string.
func generate32ByteToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	// Encode as hex (64 chars) to avoid URL-encoding concerns.
	return hex.EncodeToString(buf), nil
}

// sha256HexToken returns the lower-hex SHA-256 of a raw token string.
func sha256HexToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// factorList returns the available factors as a string slice for audit metadata.
func factorList(f AvailableFactors) []string {
	var out []string
	if f.TOTP {
		out = append(out, "totp")
	}
	if f.WebAuthnCount > 0 {
		out = append(out, "webauthn")
	}
	return out
}
