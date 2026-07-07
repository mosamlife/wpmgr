// twofa_rls_integration_test.go — real, outcome-based replacements for the
// tautological "security-review gate" tests removed from
// internal/auth/twofa_security_test.go (GH #170 outcome-test-debt audit,
// Wave 3, 2026-07-07).
//
// The removed tests re-implemented the guard condition inline against two
// synthetic uuid.New() values (e.g. `wouldConsume := (codeOwner == caller)`)
// and never called the real handler, Service, or generated SQL. A stub that
// deleted the actual `AND user_id = $caller` scoping, the B1 device-ownership
// check in the login handler, or the S4 WebAuthn ownership assertion would
// have stayed green forever. Every test in this file drives the REAL code
// (the real /auth/login Gin handler, the real auth.Service, the real
// sqlc-generated queries) against a live, non-superuser Postgres (RLS
// enforced) via startPostgres/seedTenant from rls_integration_test.go.
//
// Per-guard stub-kill verification (performed by hand-editing the production
// file, confirming the corresponding test below went RED, then reverting —
// see the PR/session notes for the exact diff of each temporary edit):
//
//   - B1 (trusted-device ownership): internal/auth/handler.go's
//     `device.UserID == res.User.ID` condition. Dropping that clause turned
//     TestTwoFA_TrustedDeviceCookie_CrossUserBypassDenied red (B's login
//     returned 200 + a session cookie using A's device).
//   - Recovery-code consume scoping: db/query/two_factor.sql's
//     ConsumeRecoveryCode `AND user_id = @user_id`. Dropping it (hand-edited
//     into the generated internal/db/sqlc/two_factor.sql.go for the trial)
//     turned TestTwoFA_RecoveryCodeConsume_UserScoped red (B's call consumed
//     A's code; pgx.ErrNoRows was no longer returned).
//   - Trusted-device revoke scoping: same file's RevokeTrustedDevice
//     `AND user_id = @user_id`. Dropping it turned
//     TestTwoFA_TrustedDeviceRevoke_UserScoped red (B's revoke call removed
//     A's device).
//   - WebAuthn credential delete scoping: same file's DeleteWebAuthnCredential
//     `AND user_id = @user_id`. Dropping it turned
//     TestTwoFA_WebAuthnCredentialDelete_UserScoped red (B's delete call
//     removed A's credential).
//   - S4 WebAuthn ownership assertion: internal/auth/twofa.go's
//     `if credRow.UserID != userID { ... }` in finishWebAuthnLogin. See the
//     doc comment on TestTwoFA_WebAuthnFinishLogin_CrossUserCredentialRejected
//     below for why this specific guard could NOT be isolated with a single
//     stub, and what was actually verified.
package tests

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp/totp"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth/twofactor"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Shared fixtures
// ---------------------------------------------------------------------------

// twofaTestUser holds a seeded user + their plaintext password, needed to
// drive the real /auth/login handler.
type twofaTestUser struct {
	id       uuid.UUID
	email    string
	password string
}

// seedTwoFAUser creates a real user + owner membership (via the real Repo,
// same path production registration uses) so login/audit resolve normally.
func seedTwoFAUser(t *testing.T, pool *db.Pool, tenant uuid.UUID, email string) twofaTestUser {
	t.Helper()
	ctx := context.Background()
	repo := auth.NewRepo(pool)
	password := "a-very-strong-password-" + email
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password for %s: %v", email, err)
	}
	u, err := repo.CreateUser(ctx, email, hash, "Test User", "", "")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	if _, err := repo.CreateMembership(ctx, u.ID, tenant, authz.RoleOwner); err != nil {
		t.Fatalf("create membership %s: %v", email, err)
	}
	return twofaTestUser{id: u.ID, email: email, password: password}
}

// buildTwoFAService wires a real auth.Service with real TOTP + WebAuthn
// factors and a real age cryptbox — the same wiring cmd/wpmgr does at
// startup via SetTwoFactorDeps — backed by the test's real Postgres pool.
// No rate limiter is wired (matches self-host default: checkCrossChallengeLimits
// no-ops when the limiter is nil), so these tests exercise ownership scoping,
// not rate limiting.
func buildTwoFAService(t *testing.T, pool *db.Pool) *auth.Service {
	t.Helper()
	svc, _ := newAuthStack(pool)
	box, err := cryptbox.NewAgeIdentity("")
	if err != nil {
		t.Fatalf("cryptbox.NewAgeIdentity: %v", err)
	}
	totpFactor := twofactor.NewTOTPFactor("WPMgr")
	wa, err := twofactor.NewWebAuthn(twofactor.Config{
		RPID:          specVectorRPID,
		RPOrigins:     []string{specVectorOrigin},
		RPDisplayName: "WPMgr Test",
	})
	if err != nil {
		t.Fatalf("twofactor.NewWebAuthn: %v", err)
	}
	svc.SetTwoFactorDeps(totpFactor, twofactor.NewWebAuthnFactor(wa), box)
	return svc
}

// enrollTOTP drives the REAL enrollment ceremony (BeginTOTPEnrollment ->
// ConfirmTOTPEnrollment) so the user ends up with two_factor_enabled=true and
// 10 real, hashed recovery codes — exactly what the dashboard Security page
// does. Returns the plaintext recovery codes (shown once, per production).
func enrollTOTP(t *testing.T, svc *auth.Service, userID uuid.UUID, email string) []string {
	t.Helper()
	ctx := context.Background()
	setup, err := svc.BeginTOTPEnrollment(ctx, userID, email)
	if err != nil {
		t.Fatalf("BeginTOTPEnrollment(%s): %v", email, err)
	}
	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode(%s): %v", email, err)
	}
	codes, err := svc.ConfirmTOTPEnrollment(ctx, userID, code)
	if err != nil {
		t.Fatalf("ConfirmTOTPEnrollment(%s): %v", email, err)
	}
	return codes
}

// seedWebAuthnCredential inserts a webauthn_credentials row directly via the
// real generated SQL (sqlc.InsertWebAuthnCredential, under InAgentTx — the
// exact query the real registration ceremony uses to persist a credential).
// Bypassing the full go-webauthn attestation ceremony is fine here: these
// fixtures back the revoke/delete scoping tests, which never touch crypto.
func seedWebAuthnCredential(t *testing.T, pool *db.Pool, userID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	credentialID := make([]byte, 32)
	if _, err := rand.Read(credentialID); err != nil {
		t.Fatalf("rand credential id: %v", err)
	}
	publicKey := make([]byte, 32)
	if _, err := rand.Read(publicKey); err != nil {
		t.Fatalf("rand public key: %v", err)
	}
	var id uuid.UUID
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).InsertWebAuthnCredential(ctx, sqlc.InsertWebAuthnCredentialParams{
			UserID:          userID,
			CredentialID:    credentialID,
			PublicKey:       publicKey,
			AttestationType: "none",
			Aaguid:          make([]byte, 16),
			SignCount:       0,
			Transports:      nil,
			Name:            name,
			BackupEligible:  false,
			BackupState:     false,
		})
		if err != nil {
			return err
		}
		id = row.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed webauthn credential for %s: %v", name, err)
	}
	return id
}

// ---------------------------------------------------------------------------
// 1. B1 — stolen trusted-device cookie must not bypass another user's 2FA
// ---------------------------------------------------------------------------

// TestTwoFA_TrustedDeviceCookie_CrossUserBypassDenied drives the REAL
// /auth/login Gin handler (gin.Engine with the real SessionManager +
// auth.Service mounted, exactly like production) for two enrolled users A and
// B. It proves:
//   - Control: A presenting A's OWN trusted-device cookie bypasses the 2FA
//     challenge and receives a full session (200 + wpmgr_session cookie).
//   - Attack: B presenting A's stolen trusted-device cookie is STILL
//     challenged (202 + two_factor_required, no session cookie) — the real
//     VerifyTrustedDeviceNoTouch + handler ownership check reject it because
//     sm.Current has no authenticated user for that request.
func TestTwoFA_TrustedDeviceCookie_CrossUserBypassDenied(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "twofa-b1")
	svc := buildTwoFAService(t, pool)

	userA := seedTwoFAUser(t, pool, tenant, "b1-a@example.com")
	userB := seedTwoFAUser(t, pool, tenant, "b1-b@example.com")
	enrollTOTP(t, svc, userA.id, userA.email)
	enrollTOTP(t, svc, userB.id, userB.email)

	membershipsA, err := svc.GetMemberships(ctx, userA.id)
	if err != nil {
		t.Fatalf("memberships A: %v", err)
	}
	rawTokenA, deviceA, err := svc.IssueTrustedDevice(ctx, userA.id, "A's laptop", "go-test-ua", nil, membershipsA)
	if err != nil {
		t.Fatalf("issue trusted device for A: %v", err)
	}
	if deviceA.UserID != userA.id {
		t.Fatalf("seeded device owner = %s, want %s", deviceA.UserID, userA.id)
	}

	sm := auth.NewSessionManagerWithStore(scs.New(), false)
	h := auth.NewHandler(svc, sm, nil, nil)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sm.LoadAndSave())
	h.Register(r)

	doLogin := func(email, password, deviceCookie string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)
		req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if deviceCookie != "" {
			req.AddCookie(&http.Cookie{Name: "wpmgr_2fa_device", Value: deviceCookie})
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	hasSessionCookie := func(rec *httptest.ResponseRecorder) bool {
		for _, c := range rec.Result().Cookies() {
			if c.Name == "wpmgr_session" && c.Value != "" {
				return true
			}
		}
		return false
	}

	// Control: A presenting A's own device cookie bypasses the challenge.
	recOwn := doLogin(userA.email, userA.password, rawTokenA)
	if recOwn.Code != http.StatusOK {
		t.Fatalf("A + own device cookie: status = %d, want 200; body = %s", recOwn.Code, recOwn.Body.String())
	}
	if strings.Contains(recOwn.Body.String(), "two_factor_required") {
		t.Fatalf("A + own device cookie: still challenged: %s", recOwn.Body.String())
	}
	if !hasSessionCookie(recOwn) {
		t.Fatal("A + own device cookie: no wpmgr_session cookie set on the legitimate bypass")
	}

	// Attack: B presenting A's device cookie must NOT bypass B's 2FA.
	recCross := doLogin(userB.email, userB.password, rawTokenA)
	if recCross.Code != http.StatusAccepted {
		t.Fatalf("B + A's stolen device cookie: status = %d, want 202 (challenge); body = %s", recCross.Code, recCross.Body.String())
	}
	if !strings.Contains(recCross.Body.String(), `"two_factor_required":true`) {
		t.Fatalf("B + A's stolen device cookie: response did not require a challenge: %s", recCross.Body.String())
	}
	if hasSessionCookie(recCross) {
		t.Fatal("B + A's stolen device cookie: a wpmgr_session cookie was set — B1 cross-user bypass succeeded")
	}
}

// ---------------------------------------------------------------------------
// 2. Recovery-code consume is user-scoped
// ---------------------------------------------------------------------------

// TestTwoFA_RecoveryCodeConsume_UserScoped calls the REAL sqlc-generated
// ConsumeRecoveryCode query (the exact query twoFARepo.ConsumeRecoveryCode
// wraps) directly, proving `AND user_id = @user_id` is load-bearing: B's
// caller ID against A's code affects zero rows (pgx.ErrNoRows) and leaves A's
// code untouched, while A consuming A's own code succeeds.
func TestTwoFA_RecoveryCodeConsume_UserScoped(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "twofa-recovery")
	svc := buildTwoFAService(t, pool)

	userA := seedTwoFAUser(t, pool, tenant, "rec-a@example.com")
	userB := seedTwoFAUser(t, pool, tenant, "rec-b@example.com")
	enrollTOTP(t, svc, userA.id, userA.email)
	enrollTOTP(t, svc, userB.id, userB.email)

	var codeAID uuid.UUID
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListActiveRecoveryCodes(ctx, userA.id)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("no active recovery codes seeded for A")
		}
		codeAID = rows[0].ID
		return nil
	}); err != nil {
		t.Fatalf("list A's recovery codes: %v", err)
	}

	// Attack: B's caller ID against A's code row.
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).ConsumeRecoveryCode(ctx, sqlc.ConsumeRecoveryCodeParams{ID: codeAID, UserID: userB.id})
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("B consuming A's recovery code: err = %v, want pgx.ErrNoRows (0 rows affected)", err)
	}

	remainingA, err := svc.CountRecoveryCodes(ctx, userA.id)
	if err != nil {
		t.Fatalf("count A's remaining codes: %v", err)
	}
	if remainingA != int64(twofactor.RecoveryCodeCount) {
		t.Fatalf("A's recovery codes: %d remain, want all %d untouched by B's cross-user attempt", remainingA, twofactor.RecoveryCodeCount)
	}

	// Control: A consumes A's own code successfully.
	err = pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).ConsumeRecoveryCode(ctx, sqlc.ConsumeRecoveryCodeParams{ID: codeAID, UserID: userA.id})
		return err
	})
	if err != nil {
		t.Fatalf("A consuming A's own recovery code: %v", err)
	}
	remainingAfter, err := svc.CountRecoveryCodes(ctx, userA.id)
	if err != nil {
		t.Fatalf("count A's remaining codes after self-consume: %v", err)
	}
	if remainingAfter != int64(twofactor.RecoveryCodeCount)-1 {
		t.Fatalf("A's recovery codes after self-consume: %d, want %d", remainingAfter, twofactor.RecoveryCodeCount-1)
	}
}

// ---------------------------------------------------------------------------
// 3(a). Trusted-device revoke is user-scoped
// ---------------------------------------------------------------------------

// TestTwoFA_TrustedDeviceRevoke_UserScoped calls the REAL
// Service.RevokeTrustedDevice (which wraps the real RevokeTrustedDevice SQL,
// `WHERE id=$1 AND user_id=$2`) with B's caller ID against A's device, and
// proves it silently affects zero rows (A's device survives), while A
// revoking A's own device succeeds.
func TestTwoFA_TrustedDeviceRevoke_UserScoped(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "twofa-revoke")
	svc := buildTwoFAService(t, pool)

	userA := seedTwoFAUser(t, pool, tenant, "revoke-a@example.com")
	userB := seedTwoFAUser(t, pool, tenant, "revoke-b@example.com")
	membershipsA, err := svc.GetMemberships(ctx, userA.id)
	if err != nil {
		t.Fatalf("memberships A: %v", err)
	}
	membershipsB, err := svc.GetMemberships(ctx, userB.id)
	if err != nil {
		t.Fatalf("memberships B: %v", err)
	}

	_, deviceA, err := svc.IssueTrustedDevice(ctx, userA.id, "A's phone", "go-test-ua", nil, membershipsA)
	if err != nil {
		t.Fatalf("issue device for A: %v", err)
	}

	// Attack: B calls the real revoke on A's device.
	if err := svc.RevokeTrustedDevice(ctx, deviceA.ID, userB.id, membershipsB); err != nil {
		t.Fatalf("RevokeTrustedDevice(B, A's device) returned an error (production swallows a 0-row update): %v", err)
	}

	devicesA, err := svc.ListTrustedDevices(ctx, userA.id)
	if err != nil {
		t.Fatalf("list A's devices: %v", err)
	}
	found := false
	for _, d := range devicesA {
		if d.ID == deviceA.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("B's revoke call removed A's trusted device — cross-user revoke succeeded")
	}

	// Control: A revokes A's own device successfully.
	if err := svc.RevokeTrustedDevice(ctx, deviceA.ID, userA.id, membershipsA); err != nil {
		t.Fatalf("RevokeTrustedDevice(A, A's own device): %v", err)
	}
	devicesAfter, err := svc.ListTrustedDevices(ctx, userA.id)
	if err != nil {
		t.Fatalf("list A's devices after self-revoke: %v", err)
	}
	for _, d := range devicesAfter {
		if d.ID == deviceA.ID {
			t.Fatal("A's own revoke did not remove the device")
		}
	}
}

// ---------------------------------------------------------------------------
// 3(b). WebAuthn credential delete is user-scoped
// ---------------------------------------------------------------------------

// TestTwoFA_WebAuthnCredentialDelete_UserScoped calls the REAL
// Service.DeleteWebAuthnCredential (wrapping DeleteWebAuthnCredential SQL,
// `WHERE id=$1 AND user_id=$2 :execrows`) with B's caller ID against A's
// credential, and proves it returns domain.NotFound (0 rows affected) leaving
// A's credential intact, while A deleting A's own credential succeeds.
func TestTwoFA_WebAuthnCredentialDelete_UserScoped(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "twofa-cred-delete")
	svc := buildTwoFAService(t, pool)

	userA := seedTwoFAUser(t, pool, tenant, "cred-a@example.com")
	userB := seedTwoFAUser(t, pool, tenant, "cred-b@example.com")
	membershipsA, err := svc.GetMemberships(ctx, userA.id)
	if err != nil {
		t.Fatalf("memberships A: %v", err)
	}
	membershipsB, err := svc.GetMemberships(ctx, userB.id)
	if err != nil {
		t.Fatalf("memberships B: %v", err)
	}

	credA := seedWebAuthnCredential(t, pool, userA.id, "A's key")

	// Attack: B calls the real delete on A's credential.
	err = svc.DeleteWebAuthnCredential(ctx, credA, userB.id, membershipsB)
	if err == nil {
		t.Fatal("B deleted A's WebAuthn credential — cross-user delete succeeded")
	}
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("B deleting A's credential: want NotFound, got %v", err)
	}

	credsA, err := svc.ListWebAuthnCredentials(ctx, userA.id)
	if err != nil {
		t.Fatalf("list A's credentials: %v", err)
	}
	found := false
	for _, c := range credsA {
		if c.ID == credA {
			found = true
		}
	}
	if !found {
		t.Fatal("A's credential was removed despite B's delete call returning NotFound")
	}

	// Control: A deletes A's own credential successfully.
	if err := svc.DeleteWebAuthnCredential(ctx, credA, userA.id, membershipsA); err != nil {
		t.Fatalf("A deleting A's own credential: %v", err)
	}
	credsAfter, err := svc.ListWebAuthnCredentials(ctx, userA.id)
	if err != nil {
		t.Fatalf("list A's credentials after self-delete: %v", err)
	}
	for _, c := range credsAfter {
		if c.ID == credA {
			t.Fatal("A's own delete did not remove the credential")
		}
	}
}

// ---------------------------------------------------------------------------
// 3(c). WebAuthn FinishLogin cross-user credential rejection
// ---------------------------------------------------------------------------
//
// These two tests use the real, fixed W3C WebAuthn Level 3 "NoneES256"
// authentication test vector — the same fixed clientDataJSON/authenticatorData/
// signature/credential bytes go-webauthn's own test suite
// (webauthn/login_test.go, testLoginSpecVectorNoneES256) uses to drive a
// genuine ECDSA P-256 signature verification through go-webauthn's real
// ValidateLogin. Reusing it lets these tests present a REAL, independently
// verifiable cryptographic WebAuthn assertion without a virtual authenticator
// library dependency. See https://www.w3.org/TR/webauthn-3/#sctn-test-vectors-none-es256.
const (
	specVectorRPID              = "example.org"
	specVectorOrigin            = "https://example.org"
	specVectorAuthDataHex       = "bfabc37432958b063360d3ad6461c9c4735ae7f8edd46592a5e0f01452b2e4b51900000000"
	specVectorClientDataJSONHex = "7b2274797065223a22776562617574686e2e676574222c226368616c6c656e6765223a224f63446e55685158756c5455506f334a5558543049393770767a7a59425039745a63685879617630314167222c226f726967696e223a2268747470733a2f2f6578616d706c652e6f7267222c2263726f73734f726967696e223a66616c73657d"
	specVectorSignatureHex      = "3046022100f50a4e2e4409249c4a853ba361282f09841df4dd4547a13a87780218deffcd380221008480ac0f0b93538174f575bf11a1dd5d78c6e486013f937295ea13653e331e87"
	specVectorCredentialIDHex   = "f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4"
	specVectorChallengeHex      = "39c0e7521417ba54d43e8dc95174f423dee9bf3cd804ff6d65c857c9abf4d408"
	specVectorPubKeyHex         = "a5010203262001215820afefa16f97ca9b2d23eb86ccb64098d20db90856062eb249c33a9b672f26df61225820930a56b87a2fca66334b03458abf879717c12cc68ed73290af2e2664796b9220"
)

func decodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

// buildSpecVectorAssertionJSON returns the fixed spec-vector assertion body
// (no userHandle, matching go-webauthn's own "ShouldSucceedNoAllowedCredentials"
// success case).
func buildSpecVectorAssertionJSON(t *testing.T) []byte {
	t.Helper()
	credentialID := decodeHex(t, specVectorCredentialIDHex)
	id := base64.RawURLEncoding.EncodeToString(credentialID)
	body := map[string]any{
		"id":    id,
		"rawId": id,
		"type":  "public-key",
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(decodeHex(t, specVectorAuthDataHex)),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(decodeHex(t, specVectorClientDataJSONHex)),
			"signature":         base64.RawURLEncoding.EncodeToString(decodeHex(t, specVectorSignatureHex)),
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal assertion json: %v", err)
	}
	return b
}

// seedSpecVectorCredential inserts the spec vector's credential_id + public_key
// via the real InsertWebAuthnCredential SQL, owned by the given user.
func seedSpecVectorCredential(t *testing.T, pool *db.Pool, owner uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).InsertWebAuthnCredential(ctx, sqlc.InsertWebAuthnCredentialParams{
			UserID:          owner,
			CredentialID:    decodeHex(t, specVectorCredentialIDHex),
			PublicKey:       decodeHex(t, specVectorPubKeyHex),
			AttestationType: "none",
			Aaguid:          make([]byte, 16),
			SignCount:       0,
			Transports:      nil,
			Name:            "spec-vector-key",
			BackupEligible:  true,
			BackupState:     false,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed spec-vector credential for %s: %v", owner, err)
	}
}

// seedWebAuthnChallengeSession persists a real webauthn_session directly onto
// an already-created challenge row (the same column beginWebAuthnLogin writes),
// bypassing Service.BeginWebAuthnChallenge only because that call would itself
// fail ("Found no credentials for user") when the challenge-holder legitimately
// owns none — exactly the victim's position in the cross-user test below.
func seedWebAuthnChallengeSession(t *testing.T, pool *db.Pool, challengeID, sessionUser uuid.UUID, allowedCredentialIDs [][]byte) {
	t.Helper()
	ctx := context.Background()
	sd := &webauthn.SessionData{
		Challenge:            base64.RawURLEncoding.EncodeToString(decodeHex(t, specVectorChallengeHex)),
		RelyingPartyID:       specVectorRPID,
		UserID:               sessionUser[:],
		AllowedCredentialIDs: allowedCredentialIDs,
	}
	b, err := twofactor.MarshalSessionData(sd)
	if err != nil {
		t.Fatalf("marshal session data: %v", err)
	}
	err = pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE two_factor_challenges SET webauthn_session = $1 WHERE id = $2", b, challengeID)
		return err
	})
	if err != nil {
		t.Fatalf("seed challenge session: %v", err)
	}
}

// TestTwoFA_WebAuthnFinishLogin_CrossUserCredentialRejected drives the REAL
// Service.FinishWebAuthnChallenge for B's own login challenge, presenting a
// genuine, cryptographically valid WebAuthn assertion for a credential that
// belongs to A, not B. It must be rejected.
//
// Reachability note (documented rather than glossed over): go-webauthn's own
// ValidateLogin requires the presented credential ID to already be a member of
// the CALLER's own credential list (built fresh, per-request, by
// buildWebAuthnUser -> ListWebAuthnCredentialsForUser scoped to
// `WHERE user_id = @user_id`) before it will even attempt signature
// verification — so in the current schema (credential_id is globally UNIQUE)
// this exact scenario is caught by that per-user scoping BEFORE the service's
// separate S4 post-lookup assertion
// (`if credRow.UserID != userID { reject }` in finishWebAuthnLogin) is ever
// reached. Reaching S4 specifically requires the credential's owning row to
// change between finishWebAuthnLogin's two internal reads — an intra-function
// TOCTOU window with no test hook, not deterministically triggerable without
// a flaky live race. I verified by hand (temporarily removing each guard in
// turn, then reverting) that removing the S4 assertion ALONE does not turn
// this test red (the per-user scoping still catches it); removing the
// ListWebAuthnCredentialsForUser user_id filter ALONE also does not turn it
// red (S4 still catches it, since the row's owner hasn't actually changed —
// only the query became too permissive); removing BOTH together does turn it
// red (FinishWebAuthnChallenge returns a successful LoginResult for B using
// A's credential). This test therefore pins the reachable, load-bearing first
// line of defense (the per-user credential scoping) plus a control proving
// the harness's cryptographic assertion is genuinely valid, not merely always
// rejected.
func TestTwoFA_WebAuthnFinishLogin_CrossUserCredentialRejected(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "twofa-s4")
	svc := buildTwoFAService(t, pool)

	userA := seedTwoFAUser(t, pool, tenant, "s4-a@example.com")
	userB := seedTwoFAUser(t, pool, tenant, "s4-b@example.com")

	// The real, valid credential belongs to A.
	seedSpecVectorCredential(t, pool, userA.id)

	// B requests their own real login challenge.
	challengeRes, err := svc.RequestTwoFactorChallenge(ctx, userB.id, nil)
	if err != nil {
		t.Fatalf("RequestTwoFactorChallenge(B): %v", err)
	}
	seedWebAuthnChallengeSession(t, pool, challengeRes.ChallengeID, userB.id, nil)

	assertion := buildSpecVectorAssertionJSON(t)
	if _, err := svc.FinishWebAuthnChallenge(ctx, challengeRes.ChallengeID, assertion, nil); err == nil {
		t.Fatal("B's challenge accepted A's WebAuthn credential — cross-user WebAuthn login succeeded")
	} else if de, ok := domain.AsDomain(err); !ok || de.Code != "webauthn_verify_failed" {
		t.Fatalf("want webauthn_verify_failed, got %v", err)
	}
}

// TestTwoFA_WebAuthnFinishLogin_OwnCredentialSucceeds is the control for the
// cross-user test above: the identical real spec-vector assertion, presented
// for the user who actually owns the credential, must validate successfully.
// Without this control, a harness bug (e.g. a malformed assertion, wrong RPID)
// could make the cross-user test above pass for the wrong reason — everything
// rejected, always, regardless of ownership.
func TestTwoFA_WebAuthnFinishLogin_OwnCredentialSucceeds(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "twofa-s4-ctrl")
	svc := buildTwoFAService(t, pool)

	userB := seedTwoFAUser(t, pool, tenant, "s4-ctrl-b@example.com")
	seedSpecVectorCredential(t, pool, userB.id)

	challengeRes, err := svc.RequestTwoFactorChallenge(ctx, userB.id, nil)
	if err != nil {
		t.Fatalf("RequestTwoFactorChallenge(B): %v", err)
	}
	credentialID := decodeHex(t, specVectorCredentialIDHex)
	seedWebAuthnChallengeSession(t, pool, challengeRes.ChallengeID, userB.id, [][]byte{credentialID})

	assertion := buildSpecVectorAssertionJSON(t)
	res, err := svc.FinishWebAuthnChallenge(ctx, challengeRes.ChallengeID, assertion, nil)
	if err != nil {
		t.Fatalf("FinishWebAuthnChallenge(B, B's own credential): %v", err)
	}
	if res.User.ID != userB.id {
		t.Fatalf("logged in as %s, want %s", res.User.ID, userB.id)
	}
}
