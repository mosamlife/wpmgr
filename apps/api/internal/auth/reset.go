package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const (
	// resetTokenTTL bounds how long a reset link is valid (OWASP 15-60 min).
	resetTokenTTL = 30 * time.Minute
	// minPasswordLen is the unified policy (ADR-045 §0.4; raised from the old 8).
	minPasswordLen = 12
	maxPasswordLen = 200
	// Rate-limit budgets (per minute).
	forgotPerMinute = 5
	resetPerMinute  = 10
)

// EmailEnqueuer enqueues a transactional email. Satisfied by *mailer.Enqueuer;
// declared here so the auth package does not import mailer.
type EmailEnqueuer interface {
	Enqueue(ctx context.Context, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error
}

// RateLimiter throttles by key. Satisfied by *autologin.MemoryLimiter.
type RateLimiter interface {
	Allow(ctx context.Context, key string, limitPerMinute int) (bool, time.Duration)
}

// SetMailer wires the email enqueuer, the public base URL (for links), and the
// rate limiter after River has started. Deferred so the auth service can be
// constructed before the River client exists.
func (s *Service) SetMailer(enq EmailEnqueuer, baseURL string, limiter RateLimiter) {
	s.email = enq
	s.baseURL = strings.TrimRight(baseURL, "/")
	s.limiter = limiter
}

// RequestPasswordReset issues a reset link for the email if it maps to a
// password account. It ALWAYS returns nil (generic, enumeration-safe): the
// caller's response must be identical whether or not the account exists. Timing
// is equalized with a dummy hash on the miss path.
func (s *Service) RequestPasswordReset(ctx context.Context, email string, ip netip.Addr) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil
	}
	// Per-account throttle: silently drop excess requests (still generic).
	if s.limiter != nil {
		if ok, _ := s.limiter.Allow(ctx, "pwreset:"+email, forgotPerMinute); !ok {
			return nil
		}
	}

	u, err := s.repo.GetUserByEmail(ctx, email)
	if err != nil || u.PasswordHash == "" {
		// Unknown account OR SSO-only (no password): equalize timing, send nothing.
		_, _ = HashPassword("timing-equalizer-not-stored")
		return nil
	}

	raw, hash, gerr := newResetToken()
	if gerr != nil {
		return nil // best-effort; never leak failure to the caller
	}
	var ipPtr *netip.Addr
	if ip.IsValid() {
		ipPtr = &ip
	}
	txErr := s.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if err := q.InvalidateUserPasswordResetTokens(ctx, u.ID); err != nil {
			return err
		}
		_, err := q.InsertPasswordResetToken(ctx, sqlc.InsertPasswordResetTokenParams{
			UserID:      u.ID,
			TokenHash:   hash,
			ExpiresAt:   time.Now().Add(resetTokenTTL),
			RequestedIp: ipPtr,
		})
		return err
	})
	if txErr != nil {
		return nil
	}

	if s.email != nil {
		link := s.baseURL + "/reset-password?token=" + raw
		_ = s.email.Enqueue(ctx, uuid.Nil, []string{u.Email}, "password_reset", map[string]any{
			"Name":           u.Name,
			"ResetURL":       link,
			"ExpiresMinutes": "30",
		})
	}
	return nil
}

// ResetPassword consumes a reset token and sets a new password. A bad/expired/
// used token is indistinguishable (all yield Gone/410). On success it stamps
// password_changed_at (invalidating other sessions), burns outstanding tokens,
// and enqueues the "password changed" notification. It does NOT establish a
// session — the user re-authenticates.
func (s *Service) ResetPassword(ctx context.Context, token, newPwd string, ip netip.Addr) error {
	if len(newPwd) < minPasswordLen {
		return domain.Validation("new_password_too_short", "password must be at least 12 characters")
	}
	if len(newPwd) > maxPasswordLen {
		return domain.Validation("new_password_too_long", "password must be 200 characters or fewer")
	}
	if strings.TrimSpace(token) == "" {
		return domain.Gone("reset_token_invalid", "this reset link is invalid or has expired")
	}
	if s.limiter != nil && ip.IsValid() {
		if ok, _ := s.limiter.Allow(ctx, "pwreset-consume:"+ip.String(), resetPerMinute); !ok {
			// Logged because the refusal is otherwise invisible: the caller sees an
			// ordinary 429 and operators see nothing at all. If the address this
			// keys on ever stops distinguishing callers, every user is refused and
			// this line is the only thing that says so.
			slog.WarnContext(ctx, "password reset consume rate limited",
				"key", "pwreset-consume", "addr", ip.String(), "limit_per_minute", resetPerMinute)
			return domain.RateLimited("too_many_attempts", "too many attempts; try again later")
		}
	}

	hash := sha256Sum(token)
	var userID uuid.UUID
	consumeErr := s.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).ConsumePasswordResetToken(ctx, hash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errResetTokenInvalid
			}
			return err
		}
		userID = row.UserID
		return nil
	})
	if consumeErr != nil {
		if errors.Is(consumeErr, errResetTokenInvalid) {
			return domain.Gone("reset_token_invalid", "this reset link is invalid or has expired")
		}
		return domain.Internal("reset_failed", "could not process the reset")
	}

	newHash, herr := HashPassword(newPwd)
	if herr != nil {
		return domain.Internal("password_hash_failed", "failed to hash new password").WithCause(herr)
	}
	// UpdatePasswordHash also stamps password_changed_at -> other sessions die.
	if err := s.repo.UpdatePasswordHash(ctx, userID, newHash); err != nil {
		return domain.Internal("password_write_failed", "failed to set new password").WithCause(err)
	}
	// S1: revoke all trusted devices after a password reset. This is the
	// post-compromise lever: an attacker who reset the password via a leaked
	// link cannot reuse "remember this device" bypass tokens to avoid 2FA.
	// Best-effort; a failure here does NOT block the reset.
	if s.twofa != nil {
		_ = s.twofa.repo.twoFA().RevokeAllTrustedDevices(ctx, userID)
	}
	// Burn any other outstanding reset tokens for this user.
	_ = s.repo.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).InvalidateUserPasswordResetTokens(ctx, userID)
	})
	s.sendPasswordChanged(ctx, userID, ip)
	return nil
}

// sendPasswordChanged enqueues the "your password was changed" notification.
// Best-effort: a mail failure never blocks the password change.
func (s *Service) sendPasswordChanged(ctx context.Context, userID uuid.UUID, ip netip.Addr) {
	if s.email == nil {
		return
	}
	u, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		return
	}
	ipStr := ""
	if ip.IsValid() {
		ipStr = ip.String()
	}
	_ = s.email.Enqueue(ctx, uuid.Nil, []string{u.Email}, "password_changed", map[string]any{
		"Name": u.Name,
		"When": time.Now().UTC().Format("2006-01-02 15:04 MST"),
		"IP":   ipStr,
	})
}

// PasswordChangedAt returns when the user last changed their password and
// whether it is set. The Authenticator rejects sessions older than this.
func (s *Service) PasswordChangedAt(ctx context.Context, userID uuid.UUID) (time.Time, bool) {
	t, ok, err := s.repo.GetUserPasswordChangedAt(ctx, userID)
	if err != nil {
		return time.Time{}, false
	}
	return t, ok
}

// errResetTokenInvalid is the sentinel returned inside the consume tx when the
// atomic UPDATE matched no row (unknown, expired, or already-used token).
var errResetTokenInvalid = errors.New("reset token invalid")

// newResetToken returns a high-entropy URL-safe token (raw, for the email link)
// and its sha256 digest (for storage). Only the digest is persisted.
func newResetToken() (raw string, digest []byte, err error) {
	buf := make([]byte, 32) // 256 bits
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	d := sha256.Sum256([]byte(raw))
	return raw, d[:], nil
}

// sha256Sum hashes a presented token for the unique-index lookup.
func sha256Sum(token string) []byte {
	d := sha256.Sum256([]byte(token))
	return d[:]
}
