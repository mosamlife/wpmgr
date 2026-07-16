package main

// age_selfcheck_test.go — GH #215 diagnosability follow-up regression lock.
//
// Covers:
//   - ageRecipientFingerprint: stable for a fixed identity, differs across
//     two distinct identities, never touches secret key material.
//   - classifyAgeSelfCheck: the pure decision logic (no-rows → silent,
//     all-ok → silent, sample-wrong-key → warn, everything else → silent)
//     table-driven, with no DB and no cryptbox involved.
//   - runAgeIdentitySelfCheck: the same three scenarios end-to-end through a
//     fake ageSecretSampler + real (throwaway) age identities, asserting on
//     the actual log output, still without any live database.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
)

// newTestAgeIdentity returns a fresh ephemeral age identity for test use only
// (cryptbox.NewAgeIdentity("") is documented as test-only: production code
// must always use a stable derivation).
func newTestAgeIdentity(t *testing.T) *cryptbox.AgeIdentity {
	t.Helper()
	id, err := cryptbox.NewAgeIdentity("")
	if err != nil {
		t.Fatalf("NewAgeIdentity: %v", err)
	}
	return id
}

// --- ageRecipientFingerprint -------------------------------------------------

func TestAgeRecipientFingerprint_StableForSameIdentity(t *testing.T) {
	id := newTestAgeIdentity(t)

	first := ageRecipientFingerprint(id)
	second := ageRecipientFingerprint(id)

	if first == "" {
		t.Fatal("fingerprint is empty for a valid identity")
	}
	if len(first) != ageFingerprintHexLen {
		t.Fatalf("fingerprint length = %d, want %d", len(first), ageFingerprintHexLen)
	}
	if first != second {
		t.Fatalf("fingerprint not stable for the same identity: %q vs %q", first, second)
	}
}

func TestAgeRecipientFingerprint_DiffersForDifferentIdentity(t *testing.T) {
	a := newTestAgeIdentity(t)
	b := newTestAgeIdentity(t)

	fa := ageRecipientFingerprint(a)
	fb := ageRecipientFingerprint(b)

	if fa == fb {
		t.Fatalf("two different identities produced the same fingerprint: %q", fa)
	}
}

func TestAgeRecipientFingerprint_NilIdentity(t *testing.T) {
	if got := ageRecipientFingerprint(nil); got != "" {
		t.Fatalf("fingerprint of nil identity = %q, want empty string", got)
	}
}

// --- classifyAgeSelfCheck ----------------------------------------------------

func TestClassifyAgeSelfCheck(t *testing.T) {
	tests := []struct {
		name      string
		outcomes  []ageSampleOutcome
		wantWarn  bool
		wantAllOK bool
	}{
		{
			name:      "no samples (fresh install) is silent",
			outcomes:  nil,
			wantWarn:  false,
			wantAllOK: false,
		},
		{
			name:      "all samples decrypt OK is silent",
			outcomes:  []ageSampleOutcome{ageOutcomeOK, ageOutcomeOK, ageOutcomeOK},
			wantWarn:  false,
			wantAllOK: true,
		},
		{
			name:      "every sample fails with wrong-identity signature warns",
			outcomes:  []ageSampleOutcome{ageOutcomeWrongIdentity, ageOutcomeWrongIdentity, ageOutcomeWrongIdentity},
			wantWarn:  true,
			wantAllOK: false,
		},
		{
			name:      "a single wrong-identity sample among successes does not warn",
			outcomes:  []ageSampleOutcome{ageOutcomeOK, ageOutcomeWrongIdentity, ageOutcomeOK},
			wantWarn:  false,
			wantAllOK: false,
		},
		{
			name:      "all-corrupted (non-identity) failures do not warn",
			outcomes:  []ageSampleOutcome{ageOutcomeOther, ageOutcomeOther},
			wantWarn:  false,
			wantAllOK: false,
		},
		{
			name:      "mixed wrong-identity and other failures do not warn",
			outcomes:  []ageSampleOutcome{ageOutcomeWrongIdentity, ageOutcomeOther},
			wantWarn:  false,
			wantAllOK: false,
		},
		{
			name:      "single sample, wrong identity, still warns (whole sample failed)",
			outcomes:  []ageSampleOutcome{ageOutcomeWrongIdentity},
			wantWarn:  true,
			wantAllOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, allOK := classifyAgeSelfCheck(tt.outcomes)
			if warn != tt.wantWarn {
				t.Errorf("warn = %v, want %v", warn, tt.wantWarn)
			}
			if allOK != tt.wantAllOK {
				t.Errorf("allOK = %v, want %v", allOK, tt.wantAllOK)
			}
		})
	}
}

// --- fake sampler for end-to-end (no DB) tests ------------------------------

type fakeAgeSecretSampler struct {
	samples []ageSecretSample
	err     error
}

func (f fakeAgeSecretSampler) Sample(context.Context) ([]ageSecretSample, error) {
	return f.samples, f.err
}

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestRunAgeIdentitySelfCheck_NoRowsIsSilent(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)
	id := newTestAgeIdentity(t)

	runAgeIdentitySelfCheck(context.Background(), fakeAgeSecretSampler{}, id, logger)

	if buf.Len() != 0 {
		t.Fatalf("expected no log output for a fresh install (no sampled rows), got: %s", buf.String())
	}
}

func TestRunAgeIdentitySelfCheck_SamplerErrorDegradesToDebugOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)
	id := newTestAgeIdentity(t)

	runAgeIdentitySelfCheck(context.Background(), fakeAgeSecretSampler{err: errors.New("boom: db unreachable")}, id, logger)

	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("a sampler error must never produce a WARN, got: %s", out)
	}
	if !strings.Contains(out, "level=DEBUG") {
		t.Fatalf("expected a debug line for a degraded sample, got: %s", out)
	}
}

func TestRunAgeIdentitySelfCheck_AllOKIsSilentOfWarnings(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)
	id := newTestAgeIdentity(t)

	ciphertext, err := id.Encrypt([]byte("some TOTP secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	samples := []ageSecretSample{
		{source: "users.totp_secret_encrypted", ciphertext: ciphertext},
		{source: "users.totp_secret_encrypted", ciphertext: ciphertext},
	}

	runAgeIdentitySelfCheck(context.Background(), fakeAgeSecretSampler{samples: samples}, id, logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("all sampled secrets decrypted OK, must not warn, got: %s", buf.String())
	}
}

// TestRunAgeIdentitySelfCheck_RotatedKeyWarns is the core GH #215 regression
// lock: ciphertexts encrypted under the OLD identity, sampled and decrypted
// with the NEW (currently-resolved) identity — simulating a self-host
// restart where WPMGR_SESSION_SECRET (or an explicit
// WPMGR_SITE_DEST_AGE_SECRET) changed underneath already-stored secrets —
// must produce exactly one loud WARN naming the remediation.
func TestRunAgeIdentitySelfCheck_RotatedKeyWarns(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	oldIdentity := newTestAgeIdentity(t)
	newIdentity := newTestAgeIdentity(t)

	ciphertext, err := oldIdentity.Encrypt([]byte("some TOTP secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	samples := []ageSecretSample{
		{source: "users.totp_secret_encrypted", ciphertext: ciphertext},
		{source: "users.totp_secret_encrypted", ciphertext: ciphertext},
		{source: "smtp_settings.password_enc", ciphertext: ciphertext},
	}

	runAgeIdentitySelfCheck(context.Background(), fakeAgeSecretSampler{samples: samples}, newIdentity, logger)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a WARN for a fully-rotated key, got: %s", out)
	}
	if !strings.Contains(out, "WPMGR_SITE_DEST_AGE_SECRET") || !strings.Contains(out, "recovery code") {
		t.Fatalf("WARN must name the remediation (pin a stable key, sign in with a recovery code), got: %s", out)
	}
}

// TestRunAgeIdentitySelfCheck_PartialFailureDoesNotWarn guards the
// "never cry wolf on a single possibly-corrupt row" requirement: one sample
// decrypts fine, one does not (simulating a single corrupted/truncated row
// rather than a rotated key) — must not warn.
func TestRunAgeIdentitySelfCheck_PartialFailureDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	id := newTestAgeIdentity(t)
	other := newTestAgeIdentity(t)

	goodCiphertext, err := id.Encrypt([]byte("good secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	badCiphertext, err := other.Encrypt([]byte("secret encrypted under a different key"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	samples := []ageSecretSample{
		{source: "users.totp_secret_encrypted", ciphertext: goodCiphertext},
		{source: "users.totp_secret_encrypted", ciphertext: badCiphertext},
	}

	runAgeIdentitySelfCheck(context.Background(), fakeAgeSecretSampler{samples: samples}, id, logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("a single failing sample among successes must not warn, got: %s", buf.String())
	}
}

// TestAgeIdentitySelfCheck_NeverPanics is a boot-safety smoke test: a nil
// pool must never panic and must not log a warning.
func TestAgeIdentitySelfCheck_NeverPanics(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)
	id := newTestAgeIdentity(t)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ageIdentitySelfCheck panicked with a nil pool: %v", r)
		}
	}()
	ageIdentitySelfCheck(context.Background(), nil, id, logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("a nil pool must never produce a WARN, got: %s", buf.String())
	}
}
