package main

// age_identity_test.go — regression lock for the self-host secret-at-rest
// stability fix: resolveAgeIdentity must never return an ephemeral key, must
// let an explicit WPMGR_SITE_DEST_AGE_SECRET take precedence, and must fail
// fast (not silently fall back) if that explicit key is unparseable.

import (
	"strings"
	"testing"

	"filippo.io/age"
)

// validSessionSecret mirrors the shape a real, config.ValidateSessionSecret-
// approved WPMGR_SESSION_SECRET would have (>=32 bytes, not a placeholder).
const validSessionSecret = "correct-horse-battery-staple-32-bytes-min"

// TestResolveAgeIdentity_DerivesFromSessionSecretWhenEnvEmpty is the core
// self-host regression lock: with no explicit WPMGR_SITE_DEST_AGE_SECRET,
// the identity must be the DETERMINISTIC derivation, not
// cryptbox.NewAgeIdentity("")'s fresh-random-key-every-call. Two resolutions
// from the same session secret (simulating two restarts of the same
// self-host install) must produce the identical identity.
func TestResolveAgeIdentity_DerivesFromSessionSecretWhenEnvEmpty(t *testing.T) {
	first, source, err := resolveAgeIdentity("", validSessionSecret)
	if err != nil {
		t.Fatalf("resolveAgeIdentity: %v", err)
	}
	if source != "derived from WPMGR_SESSION_SECRET" {
		t.Fatalf("source = %q, want derivation path", source)
	}

	second, _, err := resolveAgeIdentity("", validSessionSecret)
	if err != nil {
		t.Fatalf("resolveAgeIdentity (second call): %v", err)
	}

	if first.RecipientString() != second.RecipientString() {
		t.Fatalf("two resolutions from the same session secret produced different identities: %q vs %q", first.RecipientString(), second.RecipientString())
	}

	// And it must actually be usable across the simulated restart: the
	// second resolution must decrypt what the first encrypted.
	ciphertext, err := first.Encrypt([]byte("SMTP password"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	plaintext, err := second.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("post-restart identity failed to decrypt pre-restart ciphertext: %v", err)
	}
	if string(plaintext) != "SMTP password" {
		t.Fatalf("decrypted = %q, want %q", plaintext, "SMTP password")
	}
}

// TestResolveAgeIdentity_ExplicitEnvKeyTakesPrecedence asserts that when
// WPMGR_SITE_DEST_AGE_SECRET is set, it is used verbatim rather than the
// session-secret derivation, so existing installs that already rely on the
// explicit key are unaffected by this change.
func TestResolveAgeIdentity_ExplicitEnvKeyTakesPrecedence(t *testing.T) {
	fixture, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate fixture age identity: %v", err)
	}
	explicitKeyString := fixture.String()
	explicitRecipient := fixture.Recipient().String()

	got, source, err := resolveAgeIdentity(explicitKeyString, validSessionSecret)
	if err != nil {
		t.Fatalf("resolveAgeIdentity: %v", err)
	}
	if source != "explicit WPMGR_SITE_DEST_AGE_SECRET" {
		t.Fatalf("source = %q, want explicit path", source)
	}
	if got.RecipientString() != explicitRecipient {
		t.Fatalf("recipient = %q, want %q (the explicit key's own recipient)", got.RecipientString(), explicitRecipient)
	}

	// It must differ from what the derivation path would have produced —
	// proving the explicit key, not the session secret, was actually used.
	derived, _, err := resolveAgeIdentity("", validSessionSecret)
	if err != nil {
		t.Fatalf("resolveAgeIdentity (derivation path): %v", err)
	}
	if got.RecipientString() == derived.RecipientString() {
		t.Fatal("explicit-key path produced the same identity as the derivation path")
	}
}

// TestResolveAgeIdentity_UnparseableExplicitKeyFailsFast asserts a malformed
// explicit WPMGR_SITE_DEST_AGE_SECRET is a hard error — it must NOT silently
// fall back to the session-secret derivation, which would surprise an
// operator who set the env var expecting it to be honored.
func TestResolveAgeIdentity_UnparseableExplicitKeyFailsFast(t *testing.T) {
	_, _, err := resolveAgeIdentity("not-a-valid-age-secret-key", validSessionSecret)
	if err == nil {
		t.Fatal("expected an error for an unparseable explicit age secret key, got nil")
	}
	if !strings.Contains(err.Error(), "WPMGR_SITE_DEST_AGE_SECRET") {
		t.Fatalf("error %q does not identify WPMGR_SITE_DEST_AGE_SECRET as the source of the problem", err.Error())
	}
}

// TestResolveAgeIdentity_DifferentSessionSecretsDeriveDifferentIdentities
// guards against a degenerate derivation that ignores its seed: two
// different session secrets (e.g. two distinct self-host installs) must
// resolve to two different, non-interoperable identities.
func TestResolveAgeIdentity_DifferentSessionSecretsDeriveDifferentIdentities(t *testing.T) {
	a, _, err := resolveAgeIdentity("", "install-A-session-secret-value-32b")
	if err != nil {
		t.Fatalf("resolveAgeIdentity (A): %v", err)
	}
	b, _, err := resolveAgeIdentity("", "install-B-session-secret-value-32b")
	if err != nil {
		t.Fatalf("resolveAgeIdentity (B): %v", err)
	}
	if a.RecipientString() == b.RecipientString() {
		t.Fatal("two different session secrets derived the same identity")
	}
}
