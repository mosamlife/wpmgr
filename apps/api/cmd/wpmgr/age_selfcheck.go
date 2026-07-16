package main

// age_selfcheck.go — GH #215 diagnosability follow-up. The secrets-at-rest
// age identity derivation itself is correct (see resolveAgeIdentity); the gap
// this file closes is that a ROTATED key was previously silent at boot and
// only surfaced later as a generic, unrelated-looking failure (e.g. "invalid
// 2FA code" at login, or a mail-send failure). Two independent, non-secret
// boot-time signals close that gap:
//
//  1. ageRecipientFingerprint — a short, non-secret fingerprint of the
//     resolved identity's PUBLIC recipient, logged alongside the existing
//     "source" line. Two boots that log a DIFFERENT fingerprint used a
//     different key, full stop — a trivial, definitive comparison an
//     operator can make across a restart/redeploy.
//  2. ageIdentitySelfCheck — a best-effort boot probe that samples a bounded
//     set of ALREADY-STORED at-rest ciphertexts and attempts to decrypt them
//     with the resolved identity. If every sampled secret fails specifically
//     with age's "wrong identity" signature, the key has rotated since those
//     secrets were written, and we say so loudly with the exact remediation.
//
// Neither signal changes any crypto behavior, any resolution precedence, or
// any request-path error mapping (twofa.go's totp_decrypt_failed domain error
// is unchanged). Both are pure, additive diagnostics and both are designed to
// never delay or block boot: bounded sample size, a short context timeout,
// and a recover() so even a bug in this file degrades to a debug log line
// rather than taking the process down.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// ageFingerprintHexLen is the number of hex characters (4 bytes) of the
// recipient's sha256 digest logged as the fingerprint — enough to make two
// different keys distinguishable at a glance, short enough to stay a log
// scalar rather than a wall of hex.
const ageFingerprintHexLen = 8

// ageRecipientFingerprint returns the first ageFingerprintHexLen hex
// characters of sha256(recipient string) for the resolved identity's PUBLIC
// age recipient ("age1..."). It is NEVER the secret key — RecipientString()
// already only ever returns the public recipient — so this value is always
// safe to log. Comparing it across two boots (e.g. before/after a redeploy
// that may have regenerated WPMGR_SESSION_SECRET) is a definitive signal of
// whether the resolved secrets-at-rest key changed.
//
// Returns "" for a nil identity or one with no recipient (defensive; never
// happens for a successfully-resolved identity from resolveAgeIdentity).
func ageRecipientFingerprint(id *cryptbox.AgeIdentity) string {
	if id == nil {
		return ""
	}
	recipient := id.RecipientString()
	if recipient == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(sum[:])[:ageFingerprintHexLen]
}

// ageSelfCheckSampleLimit bounds how many existing at-rest ciphertexts the
// boot self-check will ever fetch or attempt to decrypt, across all sources
// combined. Kept small: this is a diagnostic sanity check, not a migration or
// audit — it only needs enough samples to distinguish "one corrupted row"
// from "every sampled secret fails the same way" (a rotated key).
const ageSelfCheckSampleLimit = 5

// ageSelfCheckTimeout bounds the total wall-clock time the self-check's DB
// sampling is allowed to take. It is intentionally short: on a healthy
// instance these are a handful of indexed/singleton-row reads, and a slow or
// unreachable DB should degrade this diagnostic to a skipped debug line, not
// add meaningful latency to boot.
const ageSelfCheckTimeout = 3 * time.Second

// ageSecretSample is one existing at-rest ciphertext blob sampled for the
// boot-time decrypt self-check, tagged with a human-readable source label
// (e.g. "users.totp_secret_encrypted") used only for logging.
type ageSecretSample struct {
	source     string
	ciphertext []byte
}

// ageSecretSampler abstracts "fetch a small bounded sample of existing
// at-rest ciphertexts" so the self-check's decision logic
// (classifyAgeSelfCheck) can be table-driven tested without a live database.
// dbAgeSecretSampler is the sole production implementation.
type ageSecretSampler interface {
	Sample(ctx context.Context) ([]ageSecretSample, error)
}

// dbAgeSecretSampler is the production ageSecretSampler. It samples up to
// ageSelfCheckSampleLimit confirmed users.totp_secret_encrypted rows, and —
// if there is still room in the budget — the smtp_settings singleton's
// password_enc, if one has ever been set. Both are read-only.
//
// users carries no Row-Level Security (see db/schema.sql), so it is queried
// directly. smtp_settings IS RLS-forced with only an app.agent='on' escape
// policy (m30), so that read runs under pool.InAgentTx, exactly like every
// other production reader of that table (e.g. the mailer's DB resolver).
type dbAgeSecretSampler struct {
	pool *db.Pool
}

func (s dbAgeSecretSampler) Sample(ctx context.Context) ([]ageSecretSample, error) {
	if s.pool == nil {
		return nil, errors.New("age self-check: nil db pool")
	}

	var samples []ageSecretSample

	rows, err := s.pool.Query(ctx,
		`SELECT totp_secret_encrypted
		   FROM users
		  WHERE totp_secret_encrypted IS NOT NULL
		    AND totp_confirmed_at IS NOT NULL
		  ORDER BY id
		  LIMIT $1`,
		ageSelfCheckSampleLimit,
	)
	if err != nil {
		return nil, errors.New("age self-check: sample users.totp_secret_encrypted: " + err.Error())
	}
	for rows.Next() {
		var ct []byte
		if scanErr := rows.Scan(&ct); scanErr != nil {
			rows.Close()
			return samples, errors.New("age self-check: scan users.totp_secret_encrypted: " + scanErr.Error())
		}
		samples = append(samples, ageSecretSample{source: "users.totp_secret_encrypted", ciphertext: ct})
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return samples, errors.New("age self-check: iterate users.totp_secret_encrypted: " + rowsErr.Error())
	}

	if len(samples) >= ageSelfCheckSampleLimit {
		return samples, nil
	}

	// Best-effort second source: a failure here (e.g. smtp_settings has never
	// been configured, or a transient DB hiccup) must not discard whatever
	// users.totp_secret_encrypted samples were already gathered above.
	_ = s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var ct []byte
		qErr := tx.QueryRow(ctx, `SELECT password_enc FROM smtp_settings WHERE password_enc IS NOT NULL LIMIT 1`).Scan(&ct)
		if qErr != nil {
			return nil //nolint:nilerr // best-effort second source; see comment above
		}
		samples = append(samples, ageSecretSample{source: "smtp_settings.password_enc", ciphertext: ct})
		return nil
	})

	return samples, nil
}

// ageSampleOutcome classifies a single sample's decrypt attempt.
type ageSampleOutcome int

const (
	// ageOutcomeOK means the sample decrypted successfully with the resolved
	// identity.
	ageOutcomeOK ageSampleOutcome = iota
	// ageOutcomeWrongIdentity means decryption failed specifically with age's
	// "no identity matched any of the recipients" signature — the fingerprint
	// of a genuinely different key than the one the ciphertext was encrypted
	// under, i.e. a rotated shared identity.
	ageOutcomeWrongIdentity
	// ageOutcomeOther means decryption failed for any other reason (malformed
	// ciphertext, truncated data, etc.) — evidence of a corrupted row, not
	// necessarily a rotated key.
	ageOutcomeOther
)

// isWrongAgeIdentity reports whether err is age's specific "none of the
// supplied identities could unwrap this file" failure
// (age.NoIdentityMatchError, which always wraps age.ErrIncorrectIdentity).
// cryptbox.AgeIdentity.Decrypt wraps the underlying age error with %w, and
// NoIdentityMatchError itself unwraps to its per-stanza ErrIncorrectIdentity
// causes, so errors.Is sees straight through both wrapping layers.
//
// This is deliberately narrower than "any decrypt error": a malformed/
// truncated ciphertext fails at header-parse time with an unrelated error
// (not ErrIncorrectIdentity) and must NOT be classified as a key rotation —
// only a structurally-valid age file whose recipient stanza does not match
// the resolved identity's key is.
func isWrongAgeIdentity(err error) bool {
	return errors.Is(err, age.ErrIncorrectIdentity)
}

// classifyAgeSelfCheck turns a set of per-sample decrypt outcomes into a
// boot-log decision.
//
//   - No samples at all (fresh install, or nothing sampled): (false, false) —
//     the caller must treat this as "do nothing", never a false alarm.
//   - Every sample decrypted OK: (false, true) — nothing to report beyond an
//     optional quiet confirmation.
//   - EVERY sample failed with the SAME wrong-identity signature: (true,
//     false) — this is the rotated-key fingerprint; warn loudly.
//   - Anything else (a mix of OK/failed, or failures that are not the
//     wrong-identity signature): (false, false) — deliberately NOT a warning.
//     A single failing sample could just be one corrupted row; requiring the
//     failure across the WHOLE sample before crying wolf is the point — a
//     genuinely rotated shared key fails every sample encrypted under the
//     old one, not just one.
func classifyAgeSelfCheck(outcomes []ageSampleOutcome) (warn bool, allOK bool) {
	if len(outcomes) == 0 {
		return false, false
	}
	wrongCount, okCount := 0, 0
	for _, o := range outcomes {
		switch o {
		case ageOutcomeWrongIdentity:
			wrongCount++
		case ageOutcomeOK:
			okCount++
		}
	}
	if wrongCount == len(outcomes) {
		return true, false
	}
	return false, okCount == len(outcomes)
}

// ageIdentitySelfCheck is the production entry point, called once at boot
// right after the shared secrets-at-rest identity is resolved. It is
// best-effort and MUST NEVER block or crash boot:
//
//   - a bounded ageSelfCheckTimeout caps how long sampling may run;
//   - a recover() guards against any panic in this diagnostic path;
//   - any sampling error (nil pool, query failure, context deadline) degrades
//     to a single debug line, never a warning and never a returned error.
//
// It logs nothing at all when there is nothing to check (fresh install, or a
// degraded/failed sample that yields zero rows) — that is the explicit
// "never a false alarm on an empty instance" guarantee.
func ageIdentitySelfCheck(ctx context.Context, pool *db.Pool, id *cryptbox.AgeIdentity, logger *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			logger.Debug("secrets-at-rest age self-check: recovered from panic, skipping (best-effort)", slog.Any("panic", r))
		}
	}()
	if pool == nil || id == nil || logger == nil {
		return
	}

	cctx, cancel := context.WithTimeout(ctx, ageSelfCheckTimeout)
	defer cancel()

	runAgeIdentitySelfCheck(cctx, dbAgeSecretSampler{pool: pool}, id, logger)
}

// runAgeIdentitySelfCheck is the testable core of the self-check: sample,
// decrypt, classify, log. Split out from ageIdentitySelfCheck so tests can
// inject a fake ageSecretSampler and a real (but throwaway) cryptbox identity
// pair without a live database.
func runAgeIdentitySelfCheck(ctx context.Context, sampler ageSecretSampler, id *cryptbox.AgeIdentity, logger *slog.Logger) {
	samples, err := sampler.Sample(ctx)
	if err != nil {
		logger.Debug("secrets-at-rest age self-check: sampling failed, skipping (best-effort)", slog.Any("error", err))
	}
	if len(samples) == 0 {
		return
	}

	outcomes := make([]ageSampleOutcome, len(samples))
	for i, s := range samples {
		_, decErr := id.Decrypt(s.ciphertext)
		switch {
		case decErr == nil:
			outcomes[i] = ageOutcomeOK
		case isWrongAgeIdentity(decErr):
			outcomes[i] = ageOutcomeWrongIdentity
		default:
			outcomes[i] = ageOutcomeOther
		}
	}

	warn, allOK := classifyAgeSelfCheck(outcomes)
	switch {
	case warn:
		logger.Warn("secrets-at-rest age identity self-check FAILED: the secrets-at-rest key has changed "+
			"since these secrets were stored — every sampled secret failed to decrypt with the currently "+
			"resolved key. Every TOTP/SMTP/other secret encrypted under the previous key is now unreadable. "+
			"On a PaaS that regenerates env per deploy, pin a stable WPMGR_SITE_DEST_AGE_SECRET (recommended) "+
			"or a stable WPMGR_SESSION_SECRET, then sign in with a recovery code and re-enter secrets (2FA/SMTP)",
			slog.Int("sampled", len(samples)),
			slog.Int("failed", len(samples)),
		)
	case allOK:
		logger.Debug("secrets-at-rest age self-check: all sampled secrets decrypted OK", slog.Int("sampled", len(samples)))
	default:
		// Ambiguous: a mix of outcomes, or failures that were not the
		// wrong-identity signature (e.g. a corrupted single row). Not a clear
		// rotation signal — deliberately not a warning — but worth a debug
		// breadcrumb for anyone investigating a specific report.
		logger.Debug("secrets-at-rest age self-check: mixed/inconclusive result, not a rotation signal", slog.Int("sampled", len(samples)))
	}
}
